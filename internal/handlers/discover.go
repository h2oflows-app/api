package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DiscoverHandler serves unified ranked search across curated + community runs.
type DiscoverHandler struct {
	db            *pgxpool.Pool
	devFallbackID string // #246 A6: ListPlans is now auth-only (see ownerID below)
}

func NewDiscoverHandler(db *pgxpool.Pool, devFallbackID string) *DiscoverHandler {
	return &DiscoverHandler{db: db, devFallbackID: devFallbackID}
}

func (h *DiscoverHandler) ownerID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

type discoverRun struct {
	ID                   string     `json:"id"`
	Slug                 string     `json:"slug"`
	Name                 string     `json:"name"`
	RiverName            *string    `json:"river_name"`
	StateAbbr            *string    `json:"state_abbr"`
	Handle               string     `json:"handle"`
	IsSpecial            bool       `json:"is_special"`
	ClassMin             *float64   `json:"class_min"`
	ClassMax             *float64   `json:"class_max"`
	LengthMi             *float64   `json:"length_mi"`
	UpvoteCount          int64      `json:"upvote_count"`
	LastForkedAt         *time.Time `json:"last_forked_at"`
	GaugeName            *string    `json:"gauge_name"`
	PutInLng             *float64   `json:"put_in_lng"`
	PutInLat             *float64   `json:"put_in_lat"`
	OriginalAuthorHandle *string    `json:"original_author_handle"`
	ForkCount            int        `json:"fork_count"`
}

// ListRuns handles GET /api/v1/discover/runs.
// Params: q, min_class, max_class, has_gauge, handle, limit, offset.
// Returns curated + community runs interleaved, ranked per V15.
func (h *DiscoverHandler) ListRuns(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	var minClass *float64
	if v := r.URL.Query().Get("min_class"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			minClass = &f
		}
	}
	var maxClass *float64
	if v := r.URL.Query().Get("max_class"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			maxClass = &f
		}
	}

	hasGauge := r.URL.Query().Get("has_gauge") == "true"
	handle := strings.TrimSpace(r.URL.Query().Get("handle"))

	limit := 20
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 50 {
		limit = l
	}
	offset := 0
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o >= 0 {
		offset = o
	}

	query := `
		SELECT
			id, slug, name, river_name, state_abbr, handle, is_special,
			class_min, class_max, length_mi,
			upvote_count, last_forked_at, gauge_name,
			put_in_lng, put_in_lat,
			original_author_handle,
			fork_count,
			text_rank
		FROM (
			-- All runs live in user_reaches; curated h2oflows runs are one of
			-- possibly several special-user accounts (#314).
			SELECT
				ur.id::text                         AS id,
				ur.slug                             AS slug,
				ur.name                             AS name,
				ur.river_name                       AS river_name,
				rv.state_abbr                       AS state_abbr,
				COALESCE(up.handle, 'h2oflows')     AS handle,
				COALESCE(up.is_special, false)      AS is_special,
				ur.class_min                        AS class_min,
				ur.class_max                        AS class_max,
				CASE
					WHEN ur.centerline IS NOT NULL
						THEN ROUND((ST_Length(ur.centerline) / 1609.34)::numeric, 2)::float8
					ELSE ROUND((ST_Distance(ur.put_in::geography, ur.take_out::geography) / 1609.34)::numeric, 2)::float8
				END                                 AS length_mi,
				COALESCE(
					(SELECT COUNT(*) FROM run_upvotes uv WHERE uv.user_reach_id = ur.id), 0
				)::bigint                           AS upvote_count,
				ur.original_forked_at               AS last_forked_at,
				COALESCE(g.name, cg.name)           AS gauge_name,
				ST_X(ur.put_in::geometry)           AS put_in_lng,
				ST_Y(ur.put_in::geometry)           AS put_in_lat,
				ur.original_author_handle           AS original_author_handle,
				(SELECT COUNT(*)::int FROM user_reaches f
				 WHERE f.forked_from_user_reach_id = ur.id
				   AND f.visibility = 'public' AND f.deleted_at IS NULL) AS fork_count,
				CASE
					WHEN LOWER(ur.name) = LOWER($1)             THEN 2
					WHEN LOWER(ur.name) LIKE LOWER($1) || '%'   THEN 1
					ELSE 0
				END                                 AS text_rank
			FROM user_reaches ur
			LEFT JOIN rivers rv ON rv.id = ur.river_id
			LEFT JOIN gauges g ON g.id = ur.primary_gauge_id
			LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
			LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
			WHERE ur.visibility = 'public' AND ur.deleted_at IS NULL
			  AND ur.completeness_score >= 0.2
			  AND ur.forked_from_user_reach_id IS NULL
			  AND ($1 = '' OR ur.name ILIKE '%' || $1 || '%' OR ur.river_name ILIKE '%' || $1 || '%')
			  AND ($2::float8 IS NULL OR ur.class_max >= $2)
			  AND ($3::float8 IS NULL OR ur.class_min <= $3)
			  AND (NOT $4::bool OR (g.id IS NOT NULL OR cg.id IS NOT NULL))
			  AND ($5 = '' OR up.handle = $5)
	` + anonPublicOnMapFilter(r, "ur.owner_id") + `
		) combined
		ORDER BY upvote_count DESC, is_special DESC, text_rank DESC, last_forked_at DESC NULLS LAST
		LIMIT $6 OFFSET $7
	`
	rows, err := h.db.Query(r.Context(), query, q, minClass, maxClass, hasGauge, handle, limit+1, offset)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	items := make([]discoverRun, 0)
	for rows.Next() {
		var run discoverRun
		var textRank int
		if err := rows.Scan(
			&run.ID, &run.Slug, &run.Name, &run.RiverName, &run.StateAbbr, &run.Handle, &run.IsSpecial,
			&run.ClassMin, &run.ClassMax, &run.LengthMi,
			&run.UpvoteCount, &run.LastForkedAt, &run.GaugeName,
			&run.PutInLng, &run.PutInLat,
			&run.OriginalAuthorHandle, &run.ForkCount,
			&textRank,
		); err != nil {
			continue
		}
		_ = textRank
		items = append(items, run)
	}

	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"items":       items,
		"has_more":    hasMore,
		"next_offset": offset + len(items),
	})
}

// ── GET /discover/plans (#246 A4) ──────────────────────────────────────────

type discoverPlanRun struct {
	UserReachID *string  `json:"user_reach_id,omitempty"`
	Name        *string  `json:"name,omitempty"`
	ClassMin    *float64 `json:"class_min,omitempty"`
	ClassMax    *float64 `json:"class_max,omitempty"`
	FlowBand    *string  `json:"flow_band,omitempty"`
	FlowColor   *string  `json:"flow_color,omitempty"`
	GaugeCFS    *float64 `json:"gauge_cfs,omitempty"`
}

type discoverPlan struct {
	ID         string            `json:"id"`
	Slug       string            `json:"slug"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	HostHandle string            `json:"host_handle"`
	Location   *string           `json:"location,omitempty"`
	StartDate  string            `json:"start_date"`
	EndDate    string            `json:"end_date"`
	RunCount   int               `json:"run_count"`
	Run        *discoverPlanRun  `json:"run,omitempty"`
	Crew       discoverCrewMeter `json:"crew"`
}

type discoverCrewMeter struct {
	Filled int  `json:"filled"`
	Max    *int `json:"max"`
}

// ListPlans handles GET /api/v1/discover/plans — public crew-call browse.
// Gated by the plan's own visibility + looking_for_crew, no
// anonPublicOnMapFilter/public_on_map owner gate (contract decision #7 —
// this intentionally diverges from ListRuns above). #246 A6 anon scoping
// (IMPLEMENTATION_PLAN.md §6 REVISED, PART 3 item 3, binding): the
// anon-OK carve-out is DROPPED here — the calendar domain (plans, including
// crew-call plans) is auth-only, so this now 401s anonymous callers same as
// every other /discover/plans-adjacent calendar read.
// Params: q, min_class, max_class, location, limit, offset.
func (h *DiscoverHandler) ListPlans(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	location := strings.TrimSpace(r.URL.Query().Get("location"))

	var minClass *float64
	if v := r.URL.Query().Get("min_class"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			minClass = &f
		}
	}
	var maxClass *float64
	if v := r.URL.Query().Get("max_class"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			maxClass = &f
		}
	}

	limit := 20
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 50 {
		limit = l
	}
	offset := 0
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o >= 0 {
		offset = o
	}

	// #246 A6 (PART 3 item 3): the caller is always authed now (anon 401s
	// above), so this switches from the anon-caller-only bare CURRENT_DATE
	// to userToday() — same timezone rule every other date guard in this
	// package follows (contract "Timezone rule", binding).
	today, terr := userToday(r.Context(), h.db, ownerID)
	if terr != nil {
		errorResponse(w, http.StatusInternalServerError, "could not resolve local date")
		return
	}

	query := `
		SELECT
			p.id, p.slug, p.name, p.type::text,
			COALESCE(up.handle, 'h2oflows')   AS host_handle,
			p.location, p.start_date::text, p.end_date::text,
			COALESCE(rc.run_count, 0)         AS run_count,
			fr.user_reach_id::text, fr.name, fr.class_min, fr.class_max,
			fr.flow_band, fr.flow_color, fr.gauge_cfs,
			COALESCE(cm.filled, 0)            AS crew_filled,
			p.max_crew
		FROM plans p
		LEFT JOIN user_profiles up ON up.owner_id = p.owner_id
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS run_count
			FROM plan_runs pr2
			WHERE pr2.plan_id = p.id AND pr2.deleted_at IS NULL
		) rc ON true
		LEFT JOIN LATERAL (
			-- earliest upcoming run (run_date >= today), else the earliest
			-- run overall — contract: "first upcoming run summary".
			SELECT pr.user_reach_id, ur.name, ur.class_min, ur.class_max,
			       pr.flow_band, pr.flow_color, pr.gauge_cfs
			FROM plan_runs pr
			LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
			WHERE pr.plan_id = p.id AND pr.deleted_at IS NULL
			ORDER BY (pr.run_date < $5::date), pr.run_date ASC
			LIMIT 1
		) fr ON true
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS filled FROM plan_members pm
			WHERE pm.plan_id = p.id AND pm.status = 'accepted'
		) cm ON true
		WHERE p.visibility = 'public' AND p.looking_for_crew AND p.deleted_at IS NULL
		  -- #246 A6: caller is always authed now — local-today, not bare
		  -- CURRENT_DATE (contract "Timezone rule", binding; PART 3 item 3).
		  AND p.end_date >= $5::date
		  AND ($1 = '' OR p.name ILIKE '%' || $1 || '%' OR COALESCE(p.location, '') ILIKE '%' || $1 || '%')
		  AND ($2::float8 IS NULL OR fr.class_max >= $2)
		  AND ($3::float8 IS NULL OR fr.class_min <= $3)
		  AND ($4 = '' OR COALESCE(p.location, '') ILIKE '%' || $4 || '%')
		ORDER BY p.start_date ASC
		LIMIT $6 OFFSET $7
	`
	todayStr := today.Format("2006-01-02")
	rows, err := h.db.Query(r.Context(), query, q, minClass, maxClass, location, todayStr, limit+1, offset)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	items := make([]discoverPlan, 0)
	for rows.Next() {
		var dp discoverPlan
		var run discoverPlanRun
		if err := rows.Scan(
			&dp.ID, &dp.Slug, &dp.Name, &dp.Type,
			&dp.HostHandle,
			&dp.Location, &dp.StartDate, &dp.EndDate,
			&dp.RunCount,
			&run.UserReachID, &run.Name, &run.ClassMin, &run.ClassMax,
			&run.FlowBand, &run.FlowColor, &run.GaugeCFS,
			&dp.Crew.Filled,
			&dp.Crew.Max,
		); err != nil {
			continue
		}
		if run.UserReachID != nil {
			dp.Run = &run
		}
		items = append(items, dp)
	}

	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"items":       items,
		"has_more":    hasMore,
		"next_offset": offset + len(items),
	})
}
