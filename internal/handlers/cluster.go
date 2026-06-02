package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClusterHandler finds geographic clusters of runs (curated + community) that
// share the same section of river. Used for dupe prevention at create time and
// for the "similar runs" sidebar on a user run's detail page.
//
// Cluster definition: put-in within 1 mile AND take-out within 1 mile AND
// same NHD up_comid (user_reaches) or put_in_comid (curated reaches).
//
// Ranking formula (no upvotes yet — added in PR 6.6):
//   reports * 2  + completeness * 5 + system_owned * 50
// Completeness: +1 each for centerline, flow ranges, note ≥ 20 chars (max 3).
type ClusterHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
}

func NewClusterHandler(db *pgxpool.Pool, devFallbackID string) *ClusterHandler {
	return &ClusterHandler{db: db, devFallbackID: devFallbackID}
}

func (h *ClusterHandler) callerID(r *http.Request) string {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id
	}
	return h.devFallbackID
}

// ── Types ─────────────────────────────────────────────────────────────────────

type clusterRun struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	AuthorHandle *string `json:"author_handle"`
	IsOfficial  bool     `json:"is_official"`
	ClassMin    *float64 `json:"class_min"`
	ClassMax    *float64 `json:"class_max"`
	PutInLng    float64  `json:"put_in_lng"`
	PutInLat    float64  `json:"put_in_lat"`
	TakeOutLng  float64  `json:"take_out_lng"`
	TakeOutLat  float64  `json:"take_out_lat"`
	RankScore   int      `json:"rank_score"`
	ReportCount int      `json:"report_count"`
}

// nearbyRuns finds curated + public community runs near the given put-in and
// take-out, filtered by up_comid (may be empty to skip ComID filter).
// excludeID excludes a specific run ID (for the "view cluster" use case).
func (h *ClusterHandler) nearbyRuns(r *http.Request, putInLat, putInLng, takeOutLat, takeOutLng float64, upComID, excludeID string) ([]clusterRun, error) {
	ctx := r.Context()
	const radiusMeters = 1609.34 // 1 mile

	comidFilter := ""
	if upComID != "" {
		comidFilter = "AND ur.up_comid = $7"
	}

	// Community (user_reaches) — public, non-deleted, not the excluded run.
	communityQ := `
		SELECT
			ur.id::text,
			ur.slug,
			ur.name,
			up.handle AS author_handle,
			FALSE AS is_official,
			ur.class_min,
			ur.class_max,
			ST_X(ur.put_in::geometry)   AS put_in_lng,
			ST_Y(ur.put_in::geometry)   AS put_in_lat,
			ST_X(ur.take_out::geometry) AS take_out_lng,
			ST_Y(ur.take_out::geometry) AS take_out_lat,
			(
				COALESCE((SELECT COUNT(*) FROM run_upvotes uv WHERE uv.user_reach_id = ur.id), 0)
				+ COALESCE((SELECT COUNT(*) FROM reports rp WHERE rp.user_reach_id = ur.id AND rp.deleted_at IS NULL), 0) * 2
				+ (CASE WHEN ur.centerline IS NOT NULL THEN 1 ELSE 0 END
				   + CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN 1 ELSE 0 END
				   + CASE WHEN ur.note IS NOT NULL AND char_length(ur.note) >= 20 THEN 1 ELSE 0 END) * 5
			)::int AS rank_score,
			COALESCE((SELECT COUNT(*) FROM reports rp WHERE rp.user_reach_id = ur.id AND rp.deleted_at IS NULL), 0)::int AS report_count
		FROM user_reaches ur
		LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
		WHERE ur.is_private = FALSE
		  AND ($6 = '' OR ur.id::text <> $6)
		  AND ST_DWithin(ur.put_in::geography,   ST_MakePoint($2, $1)::geography, $5)
		  AND ST_DWithin(ur.take_out::geography, ST_MakePoint($4, $3)::geography, $5)
	` + comidFilter

	// Curated reaches — always public.
	curatedQ := `
		SELECT
			r.id::text,
			r.slug,
			COALESCE(r.common_name, r.name) AS name,
			NULL AS author_handle,
			TRUE AS is_official,
			r.class_min,
			r.class_max,
			ST_X(r.start_point::geometry) AS put_in_lng,
			ST_Y(r.start_point::geometry) AS put_in_lat,
			ST_X(r.end_point::geometry)   AS take_out_lng,
			ST_Y(r.end_point::geometry)   AS take_out_lat,
			(
				50
				+ COALESCE((SELECT COUNT(*) FROM reports rp WHERE rp.reach_id = r.id AND rp.deleted_at IS NULL), 0) * 2
				+ (CASE WHEN r.centerline IS NOT NULL THEN 1 ELSE 0 END
				   + CASE WHEN EXISTS(SELECT 1 FROM flow_ranges WHERE reach_id = r.id) THEN 1 ELSE 0 END
				   + CASE WHEN r.description IS NOT NULL AND char_length(r.description) >= 20 THEN 1 ELSE 0 END) * 5
			)::int AS rank_score,
			COALESCE((SELECT COUNT(*) FROM reports rp WHERE rp.reach_id = r.id AND rp.deleted_at IS NULL), 0)::int AS report_count
		FROM reaches r
		WHERE ($6 = '' OR r.id::text <> $6)
		  AND r.start_point IS NOT NULL AND r.end_point IS NOT NULL
		  AND ST_DWithin(r.start_point::geography, ST_MakePoint($2, $1)::geography, $5)
		  AND ST_DWithin(r.end_point::geography,   ST_MakePoint($4, $3)::geography, $5)
	`
	// Note: curated reaches don't filter by ComID here — start/end proximity is sufficient.

	fullQ := `(` + communityQ + `) UNION ALL (` + curatedQ + `) ORDER BY rank_score DESC LIMIT 20`

	args := []any{putInLat, putInLng, takeOutLat, takeOutLng, radiusMeters, excludeID}
	if upComID != "" {
		args = append(args, upComID)
	}

	rows, err := h.db.Query(ctx, fullQ, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []clusterRun
	for rows.Next() {
		var cr clusterRun
		if err := rows.Scan(
			&cr.ID, &cr.Slug, &cr.Name,
			&cr.AuthorHandle, &cr.IsOfficial,
			&cr.ClassMin, &cr.ClassMax,
			&cr.PutInLng, &cr.PutInLat, &cr.TakeOutLng, &cr.TakeOutLat,
			&cr.RankScore, &cr.ReportCount,
		); err == nil {
			runs = append(runs, cr)
		}
	}
	if runs == nil {
		runs = []clusterRun{}
	}
	return runs, nil
}

// ── DupeCheck — POST /api/v1/user-runs/dupe-check ────────────────────────────
// Called from the new-run form after put-in/take-out are set. No auth required.

func (h *ClusterHandler) DupeCheck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PutInLat   float64 `json:"put_in_lat"`
		PutInLng   float64 `json:"put_in_lng"`
		TakeOutLat float64 `json:"take_out_lat"`
		TakeOutLng float64 `json:"take_out_lng"`
		UpComID    string  `json:"up_comid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.PutInLat == 0 && body.PutInLng == 0 {
		errorResponse(w, http.StatusBadRequest, "put_in coordinates required")
		return
	}

	runs, err := h.nearbyRuns(r, body.PutInLat, body.PutInLng, body.TakeOutLat, body.TakeOutLng, body.UpComID, "")
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"runs": runs})
}

// ── ClusterForRun — GET /api/v1/user-runs/{runId}/cluster ─────────────────────
// Returns runs in the same geographic cluster as the given user run (excluding self).

func (h *ClusterHandler) ClusterForRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")

	var putInLat, putInLng, takeOutLat, takeOutLng float64
	var upComID, isPrivate string
	err := h.db.QueryRow(r.Context(), `
		SELECT
			ST_Y(put_in::geometry),  ST_X(put_in::geometry),
			ST_Y(take_out::geometry), ST_X(take_out::geometry),
			COALESCE(up_comid, ''),
			is_private::text
		FROM user_reaches WHERE id = $1
	`, runID).Scan(&putInLat, &putInLng, &takeOutLat, &takeOutLng, &upComID, &isPrivate)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	// Private runs: only owner can see cluster.
	if isPrivate == "true" {
		callerID := h.callerID(r)
		var ownerID string
		_ = h.db.QueryRow(r.Context(), `SELECT owner_id FROM user_reaches WHERE id = $1`, runID).Scan(&ownerID)
		if callerID != ownerID {
			errorResponse(w, http.StatusForbidden, "run is private")
			return
		}
	}

	// Parse optional radius override (metres, default 1609).
	radius := 1609.34
	if q := r.URL.Query().Get("radius_m"); q != "" {
		if v, err := strconv.ParseFloat(q, 64); err == nil && v > 0 && v <= 10000 {
			radius = v
		}
	}
	_ = radius // used in nearbyRuns via constant — override not threaded through yet

	runs, err := h.nearbyRuns(r, putInLat, putInLng, takeOutLat, takeOutLng, upComID, runID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"runs": runs})
}
