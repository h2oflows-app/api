package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserProfileHandler serves public user profile endpoints.
type UserProfileHandler struct {
	db *pgxpool.Pool
}

func NewUserProfileHandler(db *pgxpool.Pool) *UserProfileHandler {
	return &UserProfileHandler{db: db}
}

type userProfileRun struct {
	ID        string   `json:"id"`
	Slug      string   `json:"slug"`
	Name      string   `json:"name"`
	RiverName *string  `json:"river_name"`
	ClassMin  *float64 `json:"class_min"`
	ClassMax  *float64 `json:"class_max"`
	// PutInElevationFt/PutInLng: river-position ordering basis (mig 000150),
	// same convention as internal/handlers/user_reaches.go — elevation is
	// direction-agnostic upstream/downstream signal, longitude is the
	// fallback for runs still missing an elevation lookup. put_in is
	// NOT NULL (mig 000071) so PutInLng is never null; elevation is
	// nullable (EPQS lookup can fail — mig 000150 comment).
	// RiverSequence: position in the river's NHDPlus flowline order
	// (mig 000151, web#392). The EXACT upstream→downstream key — elevation
	// and longitude below are proxies that fail on flat rivers and on rivers
	// not flowing west→east respectively. NULL when the river is unsequenced
	// or the run sits off the sequenced mainstem, in which case the elevation
	// tier still applies.
	RiverSequence    *int      `json:"river_sequence"`
	PutInElevationFt *float64  `json:"put_in_elevation_ft,omitempty"`
	PutInLng         float64   `json:"put_in_lng"`
	CurrentCFS       *float64  `json:"current_cfs"`
	FlowStatus       string    `json:"flow_status"`
	CreatedAt        time.Time `json:"created_at"`
}

type userProfileResponse struct {
	Handle string           `json:"handle"`
	Runs   []userProfileRun `json:"runs"`
}

// GET /api/v1/users/{handle}
func (h *UserProfileHandler) GetProfile(w http.ResponseWriter, r *http.Request) {
	handle := chi.URLParam(r, "handle")

	// Verify handle exists.
	var ownerID string
	err := h.db.QueryRow(r.Context(),
		`SELECT owner_id FROM user_profiles WHERE LOWER(handle) = LOWER($1)`,
		handle,
	).Scan(&ownerID)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "user not found")
		return
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT
			ur.id, ur.slug, ur.name, ur.river_name,
			ur.class_min, ur.class_max,
			ur.put_in_elevation_ft,
			ur.river_sequence,
			ST_X(ur.put_in::geometry) AS put_in_lng,
			COALESCE(lr.value, cg.last_value_cfs) AS current_cfs,
			CASE
				WHEN fr.band_color IS NULL        THEN 'unknown'
				WHEN fr.band_color LIKE 'red%'    THEN 'caution'
				WHEN fr.band_color LIKE 'blue%'   THEN 'flood'
				ELSE                                   'runnable'
			END AS flow_status,
			ur.created_at
		FROM user_reaches ur
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN LATERAL (
			SELECT value FROM gauge_readings
			WHERE gauge_id = ur.primary_gauge_id
			  AND timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY timestamp DESC LIMIT 1
		) lr ON TRUE
		LEFT JOIN LATERAL (
			SELECT label, color FROM user_reach_flow_ranges
			WHERE user_reach_id = ur.id
			  AND COALESCE(lr.value, cg.last_value_cfs) >= value
			ORDER BY value DESC
			LIMIT 1
		) thresh ON TRUE,
		LATERAL (
			SELECT
				COALESCE(thresh.label, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_label END) AS band_label,
				COALESCE(thresh.color, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_color END) AS band_color
		) fr
		WHERE ur.owner_id = $1 AND ur.visibility = 'public' AND ur.deleted_at IS NULL
		  AND ur.forked_from_user_reach_id IS NULL
	`+anonPublicOnMapFilter(r, "ur.owner_id")+`
		ORDER BY ur.river_sequence ASC NULLS LAST, ur.put_in_elevation_ft DESC NULLS LAST, ST_X(ur.put_in::geometry) ASC, ur.created_at DESC
	`, ownerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	runs := make([]userProfileRun, 0)
	for rows.Next() {
		var run userProfileRun
		if err := rows.Scan(
			&run.ID, &run.Slug, &run.Name, &run.RiverName,
			&run.ClassMin, &run.ClassMax,
			&run.PutInElevationFt, &run.RiverSequence, &run.PutInLng,
			&run.CurrentCFS, &run.FlowStatus, &run.CreatedAt,
		); err == nil {
			runs = append(runs, run)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(userProfileResponse{Handle: handle, Runs: runs})
}

// MapAllByHandle handles GET /api/v1/users/{handle}/runs/map/all.
// Returns GeoJSON FeatureCollection of the user's public runs (is_private=FALSE).
// Symmetric with /me/runs/map/all; no auth required.
func (h *UserProfileHandler) MapAllByHandle(w http.ResponseWriter, r *http.Request) {
	handle := chi.URLParam(r, "handle")

	var ownerID string
	if err := h.db.QueryRow(r.Context(),
		`SELECT owner_id FROM user_profiles WHERE LOWER(handle) = LOWER($1)`,
		handle,
	).Scan(&ownerID); err != nil {
		errorResponse(w, http.StatusNotFound, "user not found")
		return
	}

	// Caller may be anonymous (auth.Optional route). Empty string makes the
	// EXISTS subquery evaluate false for every run, so anon always gets false.
	callerID, _ := auth.UserIDFromContext(r.Context())

	rows, err := h.db.Query(r.Context(), `
		SELECT
			ur.id, ur.slug, ur.name, ur.river_name,
			ST_AsGeoJSON(ur.centerline::geometry)  AS centerline_json,
			ST_X(ur.put_in::geometry)              AS put_in_lng,
			ST_Y(ur.put_in::geometry)              AS put_in_lat,
			ST_X(ur.take_out::geometry)            AS take_out_lng,
			ST_Y(ur.take_out::geometry)            AS take_out_lat,
			COALESCE(lr.value, cg.last_value_cfs)  AS current_cfs,
			CASE
				WHEN fr.band_color IS NULL        THEN 'unknown'
				WHEN fr.band_color LIKE 'red%'    THEN 'caution'
				WHEN fr.band_color LIKE 'blue%'   THEN 'flood'
				ELSE                                   'runnable'
			END AS flow_status,
			ur.primary_gauge_id::text AS gauge_id,
			ur.class_max,
			ur.put_in_elevation_ft,
			COALESCE((SELECT COUNT(*) FROM run_upvotes uv WHERE uv.user_reach_id = ur.id), 0) AS upvote_count,
			EXISTS(SELECT 1 FROM run_upvotes uv WHERE uv.user_reach_id = ur.id AND uv.user_id = $2::text) AS user_upvoted
		FROM user_reaches ur
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN LATERAL (
			SELECT value FROM gauge_readings
			WHERE gauge_id = ur.primary_gauge_id
			  AND timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY timestamp DESC LIMIT 1
		) lr ON TRUE
		LEFT JOIN LATERAL (
			SELECT label, color FROM user_reach_flow_ranges
			WHERE user_reach_id = ur.id
			  AND COALESCE(lr.value, cg.last_value_cfs) >= value
			ORDER BY value DESC
			LIMIT 1
		) thresh ON TRUE,
		LATERAL (
			SELECT
				COALESCE(thresh.label, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_label END) AS band_label,
				COALESCE(thresh.color, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_color END) AS band_color
		) fr
		WHERE ur.owner_id = $1 AND ur.visibility = 'public' AND ur.deleted_at IS NULL
		  AND ur.forked_from_user_reach_id IS NULL
	`+anonPublicOnMapFilter(r, "ur.owner_id")+`
		ORDER BY ur.river_sequence ASC NULLS LAST, ur.put_in_elevation_ft DESC NULLS LAST, ST_X(ur.put_in::geometry) ASC, ur.created_at DESC
	`, ownerID, callerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type featureProps struct {
		ID          string   `json:"id"`
		Slug        string   `json:"slug"`
		Name        string   `json:"name"`
		RiverName   *string  `json:"river_name"`
		ClassMax    *float64 `json:"class_max"`
		FlowStatus  string   `json:"flow_status"`
		CurrentCFS  *float64 `json:"current_cfs"`
		GaugeID     *string  `json:"gauge_id"`
		IsUserReach bool     `json:"is_user_reach"`
		UpvoteCount int64    `json:"upvote_count"`
		UserUpvoted bool     `json:"user_upvoted"`
		// PutInLng/PutInElevationFt: same river-position sort basis as
		// userProfileRun (mig 000150) — the explore/profile sidebar
		// (RunsMap.vue's ReachListItem) sorts client-side using these.
		// PutInLng is never null (put_in is NOT NULL, mig 000071);
		// PutInElevationFt can be (EPQS lookup failure).
		PutInLng         float64  `json:"put_in_lng"`
		PutInElevationFt *float64 `json:"put_in_elevation_ft,omitempty"`
	}
	type feature struct {
		Type       string          `json:"type"`
		Geometry   json.RawMessage `json:"geometry"`
		Properties featureProps    `json:"properties"`
	}

	features := make([]feature, 0)
	for rows.Next() {
		var (
			id, slug, name         string
			riverName              *string
			centerlineJSON         *string
			putInLng, putInLat     float64
			takeOutLng, takeOutLat float64
			currentCFS             *float64
			flowStatus             string
			gaugeID                *string
			classMax               *float64
			putInElevationFt       *float64
			upvoteCount            int64
			userUpvoted            bool
		)
		if err := rows.Scan(
			&id, &slug, &name, &riverName,
			&centerlineJSON,
			&putInLng, &putInLat, &takeOutLng, &takeOutLat,
			&currentCFS, &flowStatus, &gaugeID, &classMax, &putInElevationFt, &upvoteCount, &userUpvoted,
		); err != nil {
			continue
		}

		var geom json.RawMessage
		if centerlineJSON != nil && *centerlineJSON != "" {
			geom = json.RawMessage(*centerlineJSON)
		} else {
			type lineString struct {
				Type        string       `json:"type"`
				Coordinates [][2]float64 `json:"coordinates"`
			}
			raw, _ := json.Marshal(lineString{
				Type:        "LineString",
				Coordinates: [][2]float64{{putInLng, putInLat}, {takeOutLng, takeOutLat}},
			})
			geom = json.RawMessage(raw)
		}

		features = append(features, feature{
			Type:     "Feature",
			Geometry: geom,
			Properties: featureProps{
				ID:               id,
				Slug:             slug,
				Name:             name,
				RiverName:        riverName,
				ClassMax:         classMax,
				FlowStatus:       flowStatus,
				CurrentCFS:       currentCFS,
				GaugeID:          gaugeID,
				IsUserReach:      true,
				UpvoteCount:      upvoteCount,
				UserUpvoted:      userUpvoted,
				PutInLng:         putInLng,
				PutInElevationFt: putInElevationFt,
			},
		})
	}

	type featureCollection struct {
		Type     string    `json:"type"`
		Features []feature `json:"features"`
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=30")
	_ = json.NewEncoder(w).Encode(featureCollection{Type: "FeatureCollection", Features: features})
}

// GET /api/v1/users/search?q= — handle prefix search, limit 10, public.
func (h *UserProfileHandler) Search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q"))), "@")
	type result struct {
		Handle string `json:"handle"`
	}
	out := make([]result, 0)
	if len(q) < 2 {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	rows, err := h.db.Query(r.Context(),
		`SELECT handle FROM user_profiles WHERE LOWER(handle) LIKE $1 ORDER BY handle LIMIT 10`,
		q+"%",
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var handle string
			if rows.Scan(&handle) == nil {
				out = append(out, result{Handle: handle})
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
