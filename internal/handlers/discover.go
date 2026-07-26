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
	devFallbackID string // ListPlans is auth-only (see ownerID below)
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

// ── GET /discover/plans (web#354 A1 — regrouped by RUN, not event) ───────

// discoverCrewMeter is the crew embed on a discoverCrewRun: {filled,max}
// (looking_for_crew is implied by appearing in this list at all).
type discoverCrewMeter struct {
	Filled int  `json:"filled"`
	Max    *int `json:"max"`
}

// discoverCrewRun is one crew-seeking calendar_run — web#354 A1 regroups
// this endpoint from "plans, each carrying runs_looking_for_crew[]" to a
// flat, run_date-sorted list of runs directly: events are owner-only now
// (no visibility concept to gate a public browse on), so a run's own
// looking_for_crew + future/today is the only qualifying signal. Crew count
// still reads plan_members (unchanged in A1, keyed by plan_run_id).
type discoverCrewRun struct {
	RunID      string            `json:"plan_run_id"`
	Name       string            `json:"name"`
	HostHandle string            `json:"host_handle"`
	RunDate    string            `json:"run_date"`
	RunTime    *string           `json:"run_time,omitempty"`
	ClassMin   *float64          `json:"class_min,omitempty"`
	ClassMax   *float64          `json:"class_max,omitempty"`
	FlowBand   *string           `json:"flow_band,omitempty"`
	FlowColor  *string           `json:"flow_color,omitempty"`
	GaugeCFS   *float64          `json:"gauge_cfs,omitempty"`
	MeetupSpot *string           `json:"meetup_spot,omitempty"`
	Crew       discoverCrewMeter `json:"crew"`
}

// ListPlans handles GET /api/v1/discover/plans — public crew-call browse.
// web#354 A1: regrouped by run (events dropped visibility/type entirely and
// are owner-only now, so there is no more per-event public gate to browse
// by). Flat, run_date-sorted list of calendar_runs where looking_for_crew
// AND run_date >= the caller's local today AND deleted_at IS NULL. Class
// filters stay; auth-only (unchanged — anon 401s, no calendar-domain
// existence oracle). `q`/`location` no longer have an event name/location to
// match against (events aren't joined at all here anymore) — both now match
// against the run's own reach name / meetup_spot, the closest surviving
// analogues; documented here since this is a behavior change, not a pure
// rename.
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

	// Caller is always authed (anon 401s above), so this uses userToday() —
	// same timezone rule every other date guard in this package follows
	// (contract "Timezone rule", binding).
	today, terr := userToday(ctx, h.db, ownerID)
	if terr != nil {
		errorResponse(w, http.StatusInternalServerError, "could not resolve local date")
		return
	}
	todayStr := today.Format("2006-01-02")

	rows, err := h.db.Query(ctx, `
		SELECT cr.id::text, COALESCE(ur.name, 'Paddle'), COALESCE(up.handle, 'h2oflows'),
		       cr.run_date::text, cr.run_time::text,
		       ur.class_min, ur.class_max, cr.flow_band, cr.flow_color, cr.gauge_cfs,
		       cr.meetup_spot, cr.max_crew, COALESCE(cm.filled, 0)
		FROM calendar_runs cr
		LEFT JOIN user_reaches ur ON ur.id = cr.user_reach_id
		LEFT JOIN user_profiles up ON up.owner_id = cr.owner_id
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS filled FROM plan_members pm
			WHERE pm.plan_run_id = cr.id AND pm.status = 'accepted'
		) cm ON true
		WHERE cr.looking_for_crew AND cr.run_date >= $5::date AND cr.deleted_at IS NULL
		  AND ($1 = '' OR COALESCE(ur.name, '') ILIKE '%' || $1 || '%' OR COALESCE(cr.meetup_spot, '') ILIKE '%' || $1 || '%')
		  AND ($2::float8 IS NULL OR ur.class_max >= $2)
		  AND ($3::float8 IS NULL OR ur.class_min <= $3)
		  AND ($4 = '' OR COALESCE(cr.meetup_spot, '') ILIKE '%' || $4 || '%')
		ORDER BY cr.run_date, cr.run_time NULLS LAST
		LIMIT $6 OFFSET $7
	`, q, minClass, maxClass, location, todayStr, limit+1, offset)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	items := make([]discoverCrewRun, 0)
	for rows.Next() {
		var dr discoverCrewRun
		if err := rows.Scan(
			&dr.RunID, &dr.Name, &dr.HostHandle, &dr.RunDate, &dr.RunTime,
			&dr.ClassMin, &dr.ClassMax, &dr.FlowBand, &dr.FlowColor, &dr.GaugeCFS,
			&dr.MeetupSpot, &dr.Crew.Max, &dr.Crew.Filled,
		); err != nil {
			continue
		}
		items = append(items, dr)
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
