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

// ── GET /discover/plans (#246 A4, regrouped per-run #246 A7) ──────────────

// discoverPlanRun is one looking-for-crew run under a discoverPlan (contract
// §8): "items are PLANS but each carries runs_looking_for_crew: [...] (only
// future/today runs w/ looking_for_crew, parent public); a plan appears iff
// it has >=1 such run."
type discoverPlanRun struct {
	PlanRunID string            `json:"plan_run_id"`
	Name      string            `json:"name"`
	RunDate   string            `json:"run_date"`
	RunTime   *string           `json:"run_time,omitempty"`
	ClassMin  *float64          `json:"class_min,omitempty"`
	ClassMax  *float64          `json:"class_max,omitempty"`
	FlowBand  *string           `json:"flow_band,omitempty"`
	FlowColor *string           `json:"flow_color,omitempty"`
	GaugeCFS  *float64          `json:"gauge_cfs,omitempty"`
	Crew      discoverCrewMeter `json:"crew"`
}

type discoverPlan struct {
	ID                 string            `json:"id"`
	Slug               string            `json:"slug"`
	Name               string            `json:"name"`
	Type               string            `json:"type"`
	HostHandle         string            `json:"host_handle"`
	Location           *string           `json:"location,omitempty"`
	StartDate          string            `json:"start_date"`
	EndDate            string            `json:"end_date"`
	RunCount           int               `json:"run_count"`
	RunsLookingForCrew []discoverPlanRun `json:"runs_looking_for_crew"`
}

type discoverCrewMeter struct {
	Filled int  `json:"filled"`
	Max    *int `json:"max"`
}

// ListPlans handles GET /api/v1/discover/plans — public crew-call browse.
// #246 A7: the looking_for_crew gate moved from plans to plan_runs — a plan
// appears iff >=1 of its LIVE runs is looking_for_crew, future/today, and
// (when class filters are given) within [min_class,max_class]; the plans-
// level looking_for_crew column is GONE (dropped, mig 000144). Still no
// anonPublicOnMapFilter/public_on_map owner gate (contract decision #7 —
// diverges from ListRuns above). #246 A6 anon scoping (IMPLEMENTATION_PLAN.md
// §6 REVISED, PART 3 item 3, binding): auth-only, 401s anon callers same as
// every other /discover/plans-adjacent calendar read.
// Params: q, min_class, max_class, location, limit, offset.
func (h *DiscoverHandler) ListPlans(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

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
	// above), so this uses userToday() — same timezone rule every other date
	// guard in this package follows (contract "Timezone rule", binding).
	today, terr := userToday(ctx, h.db, ownerID)
	if terr != nil {
		errorResponse(w, http.StatusInternalServerError, "could not resolve local date")
		return
	}
	todayStr := today.Format("2006-01-02")

	// Step 1: which plans qualify — INNER JOIN to at least one plan_run that
	// is live, looking_for_crew, future/today, and (if class filters given)
	// within range; DISTINCT collapses a plan with multiple qualifying runs
	// down to one row (the SELECT list carries no per-run columns, so DISTINCT
	// is safe here).
	planQuery := `
		SELECT DISTINCT
			p.id, p.slug, p.name, p.type::text,
			COALESCE(up.handle, 'h2oflows')   AS host_handle,
			p.location, p.start_date::text, p.end_date::text,
			COALESCE(rc.run_count, 0)         AS run_count
		FROM plans p
		LEFT JOIN user_profiles up ON up.owner_id = p.owner_id
		JOIN plan_runs pr ON pr.plan_id = p.id AND pr.deleted_at IS NULL
		  AND pr.looking_for_crew AND pr.run_date >= $5::date
		LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS run_count
			FROM plan_runs pr2
			WHERE pr2.plan_id = p.id AND pr2.deleted_at IS NULL
		) rc ON true
		WHERE p.visibility = 'public' AND p.deleted_at IS NULL
		  AND ($1 = '' OR p.name ILIKE '%' || $1 || '%' OR COALESCE(p.location, '') ILIKE '%' || $1 || '%')
		  AND ($2::float8 IS NULL OR ur.class_max >= $2)
		  AND ($3::float8 IS NULL OR ur.class_min <= $3)
		  AND ($4 = '' OR COALESCE(p.location, '') ILIKE '%' || $4 || '%')
		ORDER BY p.start_date::text ASC
		LIMIT $6 OFFSET $7
	`
	rows, err := h.db.Query(ctx, planQuery, q, minClass, maxClass, location, todayStr, limit+1, offset)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}

	items := make([]discoverPlan, 0)
	order := make([]string, 0)
	for rows.Next() {
		var dp discoverPlan
		if err := rows.Scan(
			&dp.ID, &dp.Slug, &dp.Name, &dp.Type, &dp.HostHandle,
			&dp.Location, &dp.StartDate, &dp.EndDate, &dp.RunCount,
		); err != nil {
			continue
		}
		dp.RunsLookingForCrew = []discoverPlanRun{}
		items = append(items, dp)
		order = append(order, dp.ID)
	}
	rows.Close()

	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
		order = order[:limit]
	}

	// Step 2: load the qualifying runs_looking_for_crew[] for exactly the
	// matched plans (same looking_for_crew/future/class filters, so the
	// displayed list matches what gated the plan's inclusion above).
	if len(order) > 0 {
		byPlan := make(map[string]*discoverPlan, len(items))
		for i := range items {
			byPlan[items[i].ID] = &items[i]
		}

		runRows, rerr := h.db.Query(ctx, `
			SELECT pr.plan_id::text, pr.id::text, COALESCE(ur.name, 'Paddle'), pr.run_date::text, pr.run_time::text,
			       ur.class_min, ur.class_max, pr.flow_band, pr.flow_color, pr.gauge_cfs,
			       pr.max_crew, COALESCE(cm.filled, 0)
			FROM plan_runs pr
			LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
			LEFT JOIN LATERAL (
				SELECT COUNT(*) AS filled FROM plan_members pm
				WHERE pm.plan_run_id = pr.id AND pm.status = 'accepted'
			) cm ON true
			WHERE pr.plan_id = ANY($1::uuid[]) AND pr.deleted_at IS NULL
			  AND pr.looking_for_crew AND pr.run_date >= $2::date
			  AND ($3::float8 IS NULL OR ur.class_max >= $3)
			  AND ($4::float8 IS NULL OR ur.class_min <= $4)
			ORDER BY pr.plan_id, pr.run_date
		`, order, todayStr, minClass, maxClass)
		if rerr == nil {
			defer runRows.Close()
			for runRows.Next() {
				var planID string
				var dr discoverPlanRun
				if serr := runRows.Scan(
					&planID, &dr.PlanRunID, &dr.Name, &dr.RunDate, &dr.RunTime,
					&dr.ClassMin, &dr.ClassMax, &dr.FlowBand, &dr.FlowColor, &dr.GaugeCFS,
					&dr.Crew.Max, &dr.Crew.Filled,
				); serr != nil {
					continue
				}
				if dp, ok := byPlan[planID]; ok {
					dp.RunsLookingForCrew = append(dp.RunsLookingForCrew, dr)
				}
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"items":       items,
		"has_more":    hasMore,
		"next_offset": offset + len(items),
	})
}
