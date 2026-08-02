package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	gauge "github.com/h2oflow/h2oflow/apps/api/internal/gaugecore"
	"github.com/h2oflow/h2oflow/apps/api/internal/kmlimport"
	"github.com/h2oflow/h2oflow/apps/api/internal/nldi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// resolveOrCreateRiver finds a river by GNIS ID (preferred) or name, backfilling
// gnis_id on existing legacy rows when a new GNIS match is now available. When
// no match exists, INSERTs a new river — populating state_abbr/basin/huc8 via
// NHD + WBD/TIGERweb when gnisID is present, or via put-in coords when not.
// Returns river ID, "" on failure.
func resolveOrCreateRiver(ctx context.Context, db *pgxpool.Pool, riverName, gnisID string, putInLat, putInLng float64) string {
	riverSlug := kmlimport.Slugify(riverName)
	var rid string

	if gnisID != "" {
		_ = db.QueryRow(ctx, `SELECT id FROM rivers WHERE gnis_id = $1`, gnisID).Scan(&rid)
	}

	if rid == "" {
		_ = db.QueryRow(ctx, `SELECT id FROM rivers WHERE lower(name) = lower($1) LIMIT 1`, riverName).Scan(&rid)
	}

	// Backfill GNIS ID + state/basin/huc8 on existing river if we now have a GNIS ID.
	if rid != "" && gnisID != "" {
		var existingGnis, existingState *string
		_ = db.QueryRow(ctx, `SELECT gnis_id, state_abbr FROM rivers WHERE id = $1`, rid).Scan(&existingGnis, &existingState)
		if existingGnis == nil || existingState == nil {
			stateAbbr, basin, huc8 := riverMetaFromGNIS(ctx, gnisID)
			_, _ = db.Exec(ctx, `
				UPDATE rivers SET
					gnis_id    = COALESCE(gnis_id, $2),
					state_abbr = COALESCE(state_abbr, NULLIF($3,'')),
					basin      = COALESCE(basin,      NULLIF($4,'')),
					huc8       = COALESCE(huc8,       NULLIF($5,''))
				WHERE id = $1
			`, rid, gnisID, stateAbbr, basin, huc8)
		}
	}

	if rid == "" {
		var gnisParam interface{}
		var stateAbbr, basin, huc8 string
		if gnisID != "" {
			gnisParam = gnisID
			stateAbbr, basin, huc8 = riverMetaFromGNIS(ctx, gnisID)
		} else if putInLat != 0 && putInLng != 0 {
			stateAbbr, basin, huc8 = riverMetaFromCoords(ctx, putInLat, putInLng)
		}
		_ = db.QueryRow(ctx, `
			INSERT INTO rivers (slug, name, gnis_id, state_abbr, basin, huc8)
			VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), NULLIF($6,''))
			ON CONFLICT (slug) DO UPDATE SET
				name       = EXCLUDED.name,
				gnis_id    = COALESCE(rivers.gnis_id,    EXCLUDED.gnis_id),
				state_abbr = COALESCE(rivers.state_abbr, EXCLUDED.state_abbr),
				basin      = COALESCE(rivers.basin,      EXCLUDED.basin),
				huc8       = COALESCE(rivers.huc8,       EXCLUDED.huc8)
			RETURNING id
		`, riverSlug, riverName, gnisParam, stateAbbr, basin, huc8).Scan(&rid)
	}
	return rid
}

// riverMetaFromCoords resolves state/basin/huc8 from a lat/lng directly —
// used when a stream has no GNIS ID (unnamed tributaries, custom routes).
func riverMetaFromCoords(ctx context.Context, lat, lng float64) (stateAbbr, basin, huc8 string) {
	stateCh := make(chan string, 1)
	type basinResult struct{ info nldi.BasinInfo }
	basinCh := make(chan basinResult, 1)
	go func() { v, _ := nldi.StateAt(ctx, lat, lng); stateCh <- v }()
	go func() { v, _ := nldi.BasinAt(ctx, lat, lng); basinCh <- basinResult{v} }()
	stateAbbr = <-stateCh
	basinRes := <-basinCh
	if stateAbbr == "" && basinRes.info.States != "" {
		stateAbbr = strings.SplitN(basinRes.info.States, ",", 2)[0]
	}
	basin = gauge.CanonicalBasin(basinRes.info.HUC8)
	huc8 = basinRes.info.HUC8
	return
}

// runStateFromCoords resolves the two-letter US state abbreviation for a
// run's OWN put-in coordinate via nldi.StateAt (TIGERweb Census lookup) —
// the same service riverMetaFromCoords above uses for the river-level value.
// Fails soft: a zero coordinate, a network/parse error, or a point outside
// all US state polygons all yield "" so callers can leave/clear
// user_reaches.state_abbr rather than fail the request (#356 — per-run
// state, independent of the shared river row, which multi-state rivers like
// the Colorado need since one river row can no longer speak for every run).
func runStateFromCoords(ctx context.Context, lat, lng float64) string {
	if lat == 0 && lng == 0 {
		return ""
	}
	abbr, err := nldi.StateAt(ctx, lat, lng)
	if err != nil {
		return ""
	}
	return abbr
}

// UserReachHandler handles /api/v1/me/reaches routes.
// All mutations are owner-scoped. devFallbackID is used in development when
// SUPABASE_JWKS_URL is not set so routes remain testable without a real JWT.
type UserReachHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
}

func NewUserReachHandler(db *pgxpool.Pool, devFallbackID string) *UserReachHandler {
	return &UserReachHandler{db: db, devFallbackID: devFallbackID}
}

// ownerID returns the authenticated user's ID, or the dev fallback, or ("", false).
func (h *UserReachHandler) ownerID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

// userReachSlugRe validates user-editable run slugs: lowercase alphanumeric + hyphens,
// starts and ends with alphanumeric.
var userReachSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

// authorOwnerID resolves the owner a run is authored under. When the request
// carries ?as={handle}, the handle must name an existing special user AND the
// caller must hold an app role equal to that handle (loaded from user_roles
// by the LoadAppRoles middleware) — NOT data_admin/site_admin generally. When
// both hold, the run is authored as that special user (official/curated
// content); otherwise it is owned by the authenticated caller. Returns
// ("", false, false) when unauthenticated. ?as=h2oflows keeps working through
// this same path since h2oflows is seeded as a special user (mig 000130) and
// iankco is seeded with the h2oflows role (mig 000131).
//
// The bool second return reports whether the run is being authored as a
// special user, so callers can force official/public visibility.
func (h *UserReachHandler) authorOwnerID(r *http.Request) (ownerID string, asSpecial bool, ok bool) {
	id, ok := h.ownerID(r)
	if !ok {
		return "", false, false
	}
	if as := r.URL.Query().Get("as"); as != "" {
		for _, role := range auth.AppRolesFromContext(r.Context()) {
			if role != as {
				continue
			}
			var specialOwnerID string
			err := h.db.QueryRow(r.Context(),
				`SELECT owner_id FROM user_profiles WHERE is_special AND handle = $1`, as,
			).Scan(&specialOwnerID)
			if err == nil {
				return specialOwnerID, true, true
			}
			break
		}
	}
	return id, false, true
}

// ── Types ────────────────────────────────────────────────────────────────────

type userReachSummary struct {
	ID              string     `json:"id"`
	Slug            string     `json:"slug"`
	Name            string     `json:"name"`
	LongName        *string    `json:"long_name"`
	RiverID         *string    `json:"river_id"`
	RiverName       *string    `json:"river_name"`
	StateAbbr       *string    `json:"state_abbr"`
	BasinGroup      *string    `json:"basin_group"`
	PutInLng        float64    `json:"put_in_lng"`
	PutInLat        float64    `json:"put_in_lat"`
	TakeOutLng      float64    `json:"take_out_lng"`
	TakeOutLat      float64    `json:"take_out_lat"`
	Note            *string    `json:"note"`
	ClassMin        *float64   `json:"class_min"`
	ClassMax        *float64   `json:"class_max"`
	CurrentCFS      *float64   `json:"current_cfs"`
	FlowBand        *string    `json:"flow_band"`
	FlowStatus      string     `json:"flow_status"`
	GaugeID         *string    `json:"gauge_id"`
	GaugeExternalID *string    `json:"gauge_external_id"`
	GaugeSource     *string    `json:"gauge_source"`
	GaugeName       *string    `json:"gauge_name"`
	GaugeLat        *float64   `json:"gauge_lat"`
	GaugeLng        *float64   `json:"gauge_lng"`
	CustomGaugeID   *string    `json:"custom_gauge_id"`
	CustomGaugeSlug *string    `json:"custom_gauge_slug"`
	CustomGaugeName *string    `json:"custom_gauge_name"`
	LastReadAt      *time.Time `json:"last_reading_at"`
	CreatedAt       time.Time  `json:"created_at"`
	Visibility      string     `json:"visibility"`
	IsPrivate       bool       `json:"is_private"` // backward compat; derived from Visibility
	DeletedAt       *time.Time `json:"deleted_at"`
	ForkCount       int        `json:"fork_count"`
	IsFork          bool       `json:"is_fork"`
	UpvoteCount     int        `json:"upvote_count"`
	AuthorHandle    *string    `json:"author_handle"`
	IsSpecial       bool       `json:"is_special"`
}

type userReachRapid struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Description       *string  `json:"description"`
	ClassRating       *float64 `json:"class_rating"`
	IsSurfWave        bool     `json:"is_surf_wave"`
	IsPermanentHazard bool     `json:"is_permanent_hazard"`
	HazardType        *string  `json:"hazard_type"`
	Lng               *float64 `json:"lng"`
	Lat               *float64 `json:"lat"`
}

type userReachAccessPoint struct {
	ID         string   `json:"id"`
	AccessType string   `json:"access_type"`
	Name       *string  `json:"name"`
	Notes      *string  `json:"notes"`
	Lng        *float64 `json:"lng"`
	Lat        *float64 `json:"lat"`
}

type userReachDetail struct {
	userReachSummary
	RiverSlug               *string                `json:"river_slug"`
	RiverStateAbbr          *string                `json:"river_state_abbr"`
	RiverBasin              *string                `json:"river_basin"`
	UpComID                 *string                `json:"up_comid"`
	DownComID               *string                `json:"down_comid"`
	Centerline              *json.RawMessage       `json:"centerline"`
	GaugeID                 *string                `json:"gauge_id"`
	GaugeName               *string                `json:"gauge_name"`
	GaugeSource             *string                `json:"gauge_source"`
	GaugeExternalID         *string                `json:"gauge_external_id"`
	GaugePollHealth         *string                `json:"gauge_poll_health"`
	GaugeLastPollSuccess    *time.Time             `json:"gauge_last_poll_success_at"`
	CustomGaugeID           *string                `json:"custom_gauge_id"`
	CustomGaugeName         *string                `json:"custom_gauge_name"`
	ForkedFromSlug          *string                `json:"forked_from_slug"`
	ForkedFromName          *string                `json:"forked_from_name"`
	OriginalAuthorHandle    *string                `json:"original_author_handle"`
	OriginalForkedAt        *time.Time             `json:"original_forked_at"`
	LastModifiedAfterForkAt *time.Time             `json:"last_modified_after_fork_at"`
	FlowBands               FlowBands              `json:"flow_bands"`
	Rapids                  []userReachRapid       `json:"rapids"`
	AccessPoints            []userReachAccessPoint `json:"access_points"`
	UpvoteCount             int                    `json:"upvote_count"`
	UserUpvoted             bool                   `json:"user_upvoted"`
	IsOwn                   bool                   `json:"is_own"`
	RiverConfirmed          bool                   `json:"river_confirmed"`
	ReferenceCount          int                    `json:"reference_count"`
}

// ── MapAll ────────────────────────────────────────────────────────────────────

// GET /api/v1/me/reaches/map/all
//
// Returns GeoJSON FeatureCollection of the owner's user reaches. Uses stored
// centerline when present; falls back to a 2-point LineString from put_in →
// take_out so every reach renders on the map.
func (h *UserReachHandler) MapAll(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var slugFilter []string
	if sp := r.URL.Query().Get("slugs"); sp != "" {
		slugFilter = strings.Split(sp, ",")
	}

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
			COALESCE((SELECT COUNT(*) FROM run_upvotes uv WHERE uv.user_reach_id = ur.id), 0) AS upvote_count,
			up.handle AS author_handle
		FROM user_reaches ur
		LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
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
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.label, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_label END)
				END AS band_label,
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.color, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_color END)
				END AS band_color
		) fr
		WHERE ur.owner_id = $1
		  AND ($2::text[] IS NULL OR ur.slug = ANY($2))
	`, ownerID, slugFilter)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type featureProps struct {
		ID           string   `json:"id"`
		Slug         string   `json:"slug"`
		Name         string   `json:"name"`
		RiverName    *string  `json:"river_name"`
		CommonName   *string  `json:"common_name"`
		ClassMax     *float64 `json:"class_max"`
		FlowStatus   string   `json:"flow_status"`
		CurrentCFS   *float64 `json:"current_cfs"`
		GaugeID      *string  `json:"gauge_id"`
		IsUserReach  bool     `json:"is_user_reach"`
		UpvoteCount  int64    `json:"upvote_count"`
		AuthorHandle *string  `json:"author_handle"`
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
			upvoteCount            int64
			authorHandle           *string
		)
		if err := rows.Scan(
			&id, &slug, &name, &riverName,
			&centerlineJSON,
			&putInLng, &putInLat, &takeOutLng, &takeOutLat,
			&currentCFS, &flowStatus, &gaugeID, &classMax, &upvoteCount,
			&authorHandle,
		); err != nil {
			continue
		}

		var geom json.RawMessage
		if centerlineJSON != nil && *centerlineJSON != "" {
			geom = json.RawMessage(*centerlineJSON)
		} else {
			// Synthesize a 2-point LineString from put_in → take_out.
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
				ID:           id,
				Slug:         slug,
				Name:         name,
				RiverName:    riverName,
				CommonName:   nil,
				ClassMax:     classMax,
				FlowStatus:   flowStatus,
				CurrentCFS:   currentCFS,
				GaugeID:      gaugeID,
				IsUserReach:  true,
				AuthorHandle: authorHandle,
				UpvoteCount:  upvoteCount,
			},
		})
	}

	type featureCollection struct {
		Type     string    `json:"type"`
		Features []feature `json:"features"`
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-cache")
	_ = json.NewEncoder(w).Encode(featureCollection{Type: "FeatureCollection", Features: features})
}

// ── MapCommunity ──────────────────────────────────────────────────────────────

// GET /api/v1/user-runs/map/community
//
// Returns GeoJSON FeatureCollection of all public user reaches (is_private=FALSE).
// Same shape as MapAll. Public endpoint — no auth required.
func (h *UserReachHandler) MapCommunity(w http.ResponseWriter, r *http.Request) {
	query := `
		WITH geo_clusters AS (
			-- Geometry-based clustering: same COMID + within 1mi at put-in and take-out.
			SELECT a.id AS run_id, MIN(b.id::text) AS cluster_id
			FROM user_reaches a
			JOIN user_reaches b ON (
				b.visibility = 'public' AND b.deleted_at IS NULL
				AND a.up_comid IS NOT NULL
				AND b.up_comid = a.up_comid
				AND ST_DWithin(a.put_in::geography,   b.put_in::geography,   1609.34)
				AND ST_DWithin(a.take_out::geography, b.take_out::geography, 1609.34)
			)
			WHERE a.visibility = 'public' AND a.deleted_at IS NULL
			GROUP BY a.id
		),
		cluster_groups AS (
			-- Lineage-first clustering: forks under public parent; fall back to geo. (V10)
			SELECT ur.id AS run_id,
				COALESCE(
					CASE
						WHEN ur.forked_from_user_reach_id IS NOT NULL
						  AND EXISTS(
							SELECT 1 FROM user_reaches p
							WHERE p.id = ur.forked_from_user_reach_id
							  AND p.visibility = 'public' AND p.deleted_at IS NULL
						  )
						THEN ur.forked_from_user_reach_id::text
						ELSE NULL
					END,
					gc.cluster_id,
					ur.id::text
				) AS cluster_id
			FROM user_reaches ur
			LEFT JOIN geo_clusters gc ON gc.run_id = ur.id
			WHERE ur.visibility = 'public' AND ur.deleted_at IS NULL
		)
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
			up.handle AS author_handle,
			COALESCE(up.is_special, false) AS is_special,
			COALESCE(cgrp.cluster_id, ur.id::text) AS cluster_id,
			(ur.forked_from_user_reach_id IS NOT NULL) AS is_fork,
			(SELECT COUNT(*)::int FROM user_reaches f
			 WHERE f.forked_from_user_reach_id = ur.id
			   AND f.visibility = 'public' AND f.deleted_at IS NULL) AS fork_count,
			-- Bayesian rank: (C*prior + votes)/(C + votes)*100. C=5. (V19)
			ROUND(
				(5.0 * ur.completeness_score + COALESCE((SELECT COUNT(*) FROM run_upvotes uv WHERE uv.user_reach_id = ur.id), 0)::float)
				/ GREATEST(5.0 + COALESCE((SELECT COUNT(*) FROM run_upvotes uv WHERE uv.user_reach_id = ur.id), 0)::float, 1.0)
				* 100
			)::int AS rank_score
		FROM user_reaches ur
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
		LEFT JOIN cluster_groups cgrp ON cgrp.run_id = ur.id
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
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.label, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_label END)
				END AS band_label,
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.color, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_color END)
				END AS band_color
		) fr
		WHERE ur.visibility = 'public' AND ur.deleted_at IS NULL
	` + anonPublicOnMapFilter(r, "ur.owner_id")
	rows, err := h.db.Query(r.Context(), query)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type featureProps struct {
		ID           string   `json:"id"`
		Slug         string   `json:"slug"`
		Name         string   `json:"name"`
		RiverName    *string  `json:"river_name"`
		CommonName   *string  `json:"common_name"`
		ClassMax     *float64 `json:"class_max"`
		FlowStatus   string   `json:"flow_status"`
		CurrentCFS   *float64 `json:"current_cfs"`
		GaugeID      *string  `json:"gauge_id"`
		IsUserReach  bool     `json:"is_user_reach"`
		AuthorHandle *string  `json:"author_handle"`
		IsSpecial    bool     `json:"is_special"`
		ClusterID    string   `json:"cluster_id"`
		IsFork       bool     `json:"is_fork"`
		ForkCount    int      `json:"fork_count"`
		RankScore    int      `json:"rank_score"`
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
			authorHandle           *string
			isSpecial              bool
			clusterID              string
			isFork                 bool
			forkCount              int
			rankScore              int
		)
		if err := rows.Scan(
			&id, &slug, &name, &riverName,
			&centerlineJSON,
			&putInLng, &putInLat, &takeOutLng, &takeOutLat,
			&currentCFS, &flowStatus, &gaugeID, &classMax,
			&authorHandle, &isSpecial,
			&clusterID, &isFork, &forkCount, &rankScore,
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
				ID:           id,
				Slug:         slug,
				Name:         name,
				RiverName:    riverName,
				CommonName:   nil,
				ClassMax:     classMax,
				FlowStatus:   flowStatus,
				CurrentCFS:   currentCFS,
				GaugeID:      gaugeID,
				IsUserReach:  true,
				AuthorHandle: authorHandle,
				IsSpecial:    isSpecial,
				ClusterID:    clusterID,
				IsFork:       isFork,
				ForkCount:    forkCount,
				RankScore:    rankScore,
			},
		})
	}

	type featureCollection struct {
		Type     string    `json:"type"`
		Features []feature `json:"features"`
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_ = json.NewEncoder(w).Encode(featureCollection{Type: "FeatureCollection", Features: features})
}

// ── List ─────────────────────────────────────────────────────────────────────

// GET /api/v1/me/reaches
func (h *UserReachHandler) List(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT
			ur.id, ur.slug, ur.name, ur.long_name, ur.river_name,
			rv.id::text AS river_id, COALESCE(ur.state_abbr, rv.state_abbr) AS state_abbr, rv.basin AS basin_group,
			ST_X(ur.put_in::geometry)    AS put_in_lng,
			ST_Y(ur.put_in::geometry)    AS put_in_lat,
			ST_X(ur.take_out::geometry)  AS take_out_lng,
			ST_Y(ur.take_out::geometry)  AS take_out_lat,
			ur.note, ur.created_at,
			ur.class_min, ur.class_max,
			COALESCE(lr.value, cg.last_value_cfs) AS current_cfs,
			COALESCE(lr.timestamp, cg.last_value_at) AS last_reading_at,
			CASE
				WHEN fr.band_color IS NULL        THEN 'unknown'
				WHEN fr.band_color LIKE 'red%'    THEN 'caution'
				WHEN fr.band_color LIKE 'blue%'   THEN 'flood'
				ELSE                                   'runnable'
			END AS flow_status,
			fr.band_label AS flow_band,
			ur.primary_gauge_id::text AS gauge_id,
			g.external_id AS gauge_external_id,
			g.source AS gauge_source,
			g.name AS gauge_name,
			ST_Y(g.location::geometry) AS gauge_lat,
			ST_X(g.location::geometry) AS gauge_lng,
			ur.custom_gauge_id::text AS custom_gauge_id,
			cg.slug AS custom_gauge_slug,
			cg.name AS custom_gauge_name,
			ur.visibility,
			COALESCE((SELECT COUNT(*) FROM run_upvotes uv WHERE uv.user_reach_id = ur.id), 0) AS upvote_count,
			up.handle AS author_handle
		FROM user_reaches ur
		LEFT JOIN rivers rv ON rv.id = ur.river_id
		LEFT JOIN gauges g ON g.id = ur.primary_gauge_id
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
		LEFT JOIN LATERAL (
			SELECT value, timestamp FROM gauge_readings
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
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.label, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_label END)
				END AS band_label,
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.color, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_color END)
				END AS band_color
		) fr
		WHERE ur.owner_id = $1 AND ur.deleted_at IS NULL
		ORDER BY ur.name
	`, ownerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	items := make([]userReachSummary, 0)
	for rows.Next() {
		var s userReachSummary
		if err := rows.Scan(
			&s.ID, &s.Slug, &s.Name, &s.LongName, &s.RiverName,
			&s.RiverID, &s.StateAbbr, &s.BasinGroup,
			&s.PutInLng, &s.PutInLat, &s.TakeOutLng, &s.TakeOutLat,
			&s.Note, &s.CreatedAt,
			&s.ClassMin, &s.ClassMax,
			&s.CurrentCFS, &s.LastReadAt,
			&s.FlowStatus, &s.FlowBand, &s.GaugeID,
			&s.GaugeExternalID, &s.GaugeSource, &s.GaugeName,
			&s.GaugeLat, &s.GaugeLng,
			&s.CustomGaugeID, &s.CustomGaugeSlug, &s.CustomGaugeName,
			&s.Visibility, &s.UpvoteCount, &s.AuthorHandle,
		); err == nil {
			s.IsPrivate = s.Visibility != "public"
			items = append(items, s)
		}
	}
	jsonResponse(w, http.StatusOK, items)
}

// ── ReferencedRuns ────────────────────────────────────────────────────────────

// GET /api/v1/me/referenced-runs?dashboard_id={id}
// Returns summaries of OTHER users' public runs the caller has referenced onto a
// dashboard (referenced_user_reach_id rows). Same shape as List, but author_handle
// is the original owner — these render read-only on the dashboard.
func (h *UserReachHandler) ReferencedRuns(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	dashboardID := r.URL.Query().Get("dashboard_id")

	// $2 dashboard filter is optional — NULL means all of the caller's references.
	var dashPtr *string
	if dashboardID != "" {
		dashPtr = &dashboardID
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT
			ur.id, ur.slug, ur.name, ur.long_name, ur.river_name,
			COALESCE(ur.state_abbr, rv.state_abbr) AS state_abbr, rv.basin AS basin_group,
			COALESCE(lr.value, cg.last_value_cfs) AS current_cfs,
			COALESCE(lr.timestamp, cg.last_value_at) AS last_reading_at,
			CASE
				WHEN fr.band_color IS NULL        THEN 'unknown'
				WHEN fr.band_color LIKE 'red%'    THEN 'caution'
				WHEN fr.band_color LIKE 'blue%'   THEN 'flood'
				ELSE                                   'runnable'
			END AS flow_status,
			fr.band_label AS flow_band,
			ur.primary_gauge_id::text AS gauge_id,
			g.external_id AS gauge_external_id,
			g.source AS gauge_source,
			g.name AS gauge_name,
			ST_Y(g.location::geometry) AS gauge_lat,
			ST_X(g.location::geometry) AS gauge_lng,
			ur.custom_gauge_id::text AS custom_gauge_id,
			cg.slug AS custom_gauge_slug,
			cg.name AS custom_gauge_name,
			up.handle AS author_handle
		FROM user_watchlists w
		JOIN user_reaches ur ON ur.id = w.referenced_user_reach_id
		LEFT JOIN rivers rv ON rv.id = ur.river_id
		LEFT JOIN gauges g ON g.id = ur.primary_gauge_id
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
		LEFT JOIN LATERAL (
			SELECT value, timestamp FROM gauge_readings
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
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.label, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_label END)
				END AS band_label,
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.color, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_color END)
				END AS band_color
		) fr
		WHERE w.user_id = $1
		  AND w.referenced_user_reach_id IS NOT NULL
		  AND ($2::uuid IS NULL OR w.dashboard_id = $2::uuid)
		  AND ur.visibility = 'public'
		  AND ur.deleted_at IS NULL
		ORDER BY ur.name
	`, callerID, dashPtr)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	items := make([]userReachSummary, 0)
	for rows.Next() {
		var s userReachSummary
		if err := rows.Scan(
			&s.ID, &s.Slug, &s.Name, &s.LongName, &s.RiverName,
			&s.StateAbbr, &s.BasinGroup,
			&s.CurrentCFS, &s.LastReadAt,
			&s.FlowStatus, &s.FlowBand, &s.GaugeID,
			&s.GaugeExternalID, &s.GaugeSource, &s.GaugeName,
			&s.GaugeLat, &s.GaugeLng,
			&s.CustomGaugeID, &s.CustomGaugeSlug, &s.CustomGaugeName,
			&s.AuthorHandle,
		); err == nil {
			s.Visibility = "public"
			items = append(items, s)
		}
	}
	jsonResponse(w, http.StatusOK, items)
}

// ── Get ──────────────────────────────────────────────────────────────────────

// GET /api/v1/me/reaches/{slug}
func (h *UserReachHandler) Get(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ids := editableOwnerIDs(r.Context(), h.db, callerID)

	var d userReachDetail
	var geojsonBytes []byte

	err := h.db.QueryRow(r.Context(), `
		SELECT
			ur.id, ur.slug, ur.name, ur.long_name, ur.river_name,
			ST_X(ur.put_in::geometry)    AS put_in_lng,
			ST_Y(ur.put_in::geometry)    AS put_in_lat,
			ST_X(ur.take_out::geometry)  AS take_out_lng,
			ST_Y(ur.take_out::geometry)  AS take_out_lat,
			ur.note, ur.created_at,
			ur.class_min, ur.class_max,
			ur.up_comid, ur.down_comid,
			CASE WHEN ur.centerline IS NOT NULL
				 THEN ST_AsGeoJSON(ur.centerline::geometry)
				 ELSE NULL END,
			ur.primary_gauge_id::text,
			g.name AS gauge_name,
			g.source AS gauge_source,
			g.external_id AS gauge_external_id,
			g.poll_health AS gauge_poll_health,
			g.last_poll_success_at AS gauge_last_poll_success_at,
			ur.custom_gauge_id::text,
			cg.name AS custom_gauge_name,
			COALESCE(lr.value, cg.last_value_cfs) AS current_cfs,
			COALESCE(lr.timestamp, cg.last_value_at) AS last_reading_at,
			CASE
				WHEN fr.band_color IS NULL        THEN 'unknown'
				WHEN fr.band_color LIKE 'red%'    THEN 'caution'
				WHEN fr.band_color LIKE 'blue%'   THEN 'flood'
				ELSE                                   'runnable'
			END AS flow_status,
			fr.band_label AS flow_band,
			rv.slug AS river_slug,
			COALESCE(ur.state_abbr, rv.state_abbr) AS river_state_abbr,
			rv.basin AS river_basin,
			ur.visibility,
			fr_reach.slug AS forked_from_slug,
			COALESCE(fr_reach.long_name, fr_reach.name) AS forked_from_name,
			ur.original_author_handle,
			ur.original_forked_at,
			ur.last_modified_after_fork_at,
			up.handle AS author_handle,
			ur.river_confirmed
		FROM user_reaches ur
		LEFT JOIN rivers rv ON rv.id = ur.river_id
		LEFT JOIN gauges g ON g.id = ur.primary_gauge_id
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN user_reaches fr_reach ON fr_reach.id = ur.forked_from_user_reach_id
		LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
		LEFT JOIN LATERAL (
			SELECT value, timestamp FROM gauge_readings
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
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.label, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_label END)
				END AS band_label,
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.color, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_color END)
				END AS band_color
		) fr
		WHERE ur.owner_id = ANY($1::text[]) AND ur.slug = $2
		ORDER BY (ur.owner_id = $3) DESC LIMIT 1
	`, ids, slug, callerID).Scan(
		&d.ID, &d.Slug, &d.Name, &d.LongName, &d.RiverName,
		&d.PutInLng, &d.PutInLat, &d.TakeOutLng, &d.TakeOutLat,
		&d.Note, &d.CreatedAt,
		&d.ClassMin, &d.ClassMax,
		&d.UpComID, &d.DownComID,
		&geojsonBytes,
		&d.GaugeID, &d.GaugeName, &d.GaugeSource, &d.GaugeExternalID,
		&d.GaugePollHealth, &d.GaugeLastPollSuccess,
		&d.CustomGaugeID, &d.CustomGaugeName,
		&d.CurrentCFS, &d.LastReadAt,
		&d.FlowStatus, &d.FlowBand,
		&d.RiverSlug, &d.RiverStateAbbr, &d.RiverBasin,
		&d.Visibility,
		&d.ForkedFromSlug, &d.ForkedFromName,
		&d.OriginalAuthorHandle, &d.OriginalForkedAt, &d.LastModifiedAfterForkAt,
		&d.AuthorHandle,
		&d.RiverConfirmed,
	)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}
	d.IsPrivate = d.Visibility != "public"

	if geojsonBytes != nil {
		raw := json.RawMessage(geojsonBytes)
		d.Centerline = &raw
	}

	// Flow bands
	_ = h.db.QueryRow(r.Context(),
		`SELECT base_label, base_color FROM user_reaches WHERE id = $1`, d.ID,
	).Scan(&d.FlowBands.BaseLabel, &d.FlowBands.BaseColor)
	frRows, _ := h.db.Query(r.Context(), `
		SELECT value, label, color
		FROM user_reach_flow_ranges
		WHERE user_reach_id = $1
		ORDER BY value ASC
	`, d.ID)
	d.FlowBands.Thresholds = make([]FlowBandThreshold, 0)
	if frRows != nil {
		defer frRows.Close()
		for frRows.Next() {
			var t FlowBandThreshold
			if frRows.Scan(&t.Value, &t.Label, &t.Color) == nil {
				d.FlowBands.Thresholds = append(d.FlowBands.Thresholds, t)
			}
		}
	}

	// Rapids
	d.Rapids = make([]userReachRapid, 0)
	rapRows, _ := h.db.Query(r.Context(), `
		SELECT id, name, description, class_rating,
		       is_surf_wave, is_permanent_hazard, hazard_type,
		       ST_X(location::geometry), ST_Y(location::geometry)
		FROM rapids
		WHERE user_reach_id = $1
		ORDER BY name
	`, d.ID)
	if rapRows != nil {
		defer rapRows.Close()
		for rapRows.Next() {
			var rr userReachRapid
			if rapRows.Scan(&rr.ID, &rr.Name, &rr.Description, &rr.ClassRating,
				&rr.IsSurfWave, &rr.IsPermanentHazard, &rr.HazardType,
				&rr.Lng, &rr.Lat) == nil {
				d.Rapids = append(d.Rapids, rr)
			}
		}
	}

	// Access points
	d.AccessPoints = make([]userReachAccessPoint, 0)
	apRows, _ := h.db.Query(r.Context(), `
		SELECT id, access_type, name, notes,
		       ST_X(location::geometry), ST_Y(location::geometry)
		FROM reach_access
		WHERE user_reach_id = $1
		ORDER BY access_type, name
	`, d.ID)
	if apRows != nil {
		defer apRows.Close()
		for apRows.Next() {
			var ap userReachAccessPoint
			if apRows.Scan(&ap.ID, &ap.AccessType, &ap.Name, &ap.Notes,
				&ap.Lng, &ap.Lat) == nil {
				d.AccessPoints = append(d.AccessPoints, ap)
			}
		}
	}

	// Upvotes
	_ = h.db.QueryRow(r.Context(), `SELECT COUNT(*) FROM run_upvotes WHERE user_reach_id = $1`, d.ID).Scan(&d.UpvoteCount)
	_ = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM run_upvotes WHERE user_reach_id = $1 AND user_id = $2)`, d.ID, callerID).Scan(&d.UserUpvoted)

	// Reference count: how many distinct non-owner users have this run on a dashboard.
	_ = h.db.QueryRow(r.Context(),
		`SELECT COUNT(DISTINCT user_id) FROM user_watchlists WHERE referenced_user_reach_id = $1 AND user_id <> $2`,
		d.ID, callerID,
	).Scan(&d.ReferenceCount)

	jsonResponse(w, http.StatusOK, d)
}

// ── GetPublic ─────────────────────────────────────────────────────────────────

// GET /api/v1/user-runs/by-handle/{handle}/{slug}
// Canonical public lookup by author handle + run slug. Auth optional.
func (h *UserReachHandler) GetPublicByHandle(w http.ResponseWriter, r *http.Request) {
	handle := chi.URLParam(r, "handle")
	slug := chi.URLParam(r, "slug")
	callerID, _ := h.ownerID(r)

	var runID string
	err := h.db.QueryRow(r.Context(), `
		SELECT ur.id FROM user_reaches ur
		JOIN user_profiles up ON up.owner_id = ur.owner_id
		WHERE ur.slug = $1 AND LOWER(up.handle) = LOWER($2) AND ur.visibility = 'public'
	`, slug, handle).Scan(&runID)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}
	h.getPublicByID(w, r, runID, callerID)
}

// GET /api/v1/user-runs/{runId}
// Public: returns detail for any non-private user reach by UUID.
// Auth optional — when present, populates user_upvoted.
func (h *UserReachHandler) GetPublic(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")
	callerID, _ := h.ownerID(r)
	h.getPublicByID(w, r, runID, callerID)
}

func (h *UserReachHandler) getPublicByID(w http.ResponseWriter, r *http.Request, runID string, callerID string) {
	var d userReachDetail
	var geojsonBytes []byte
	var authorID string

	err := h.db.QueryRow(r.Context(), `
		SELECT
			ur.id, ur.slug, ur.name, ur.long_name, ur.river_name,
			ST_X(ur.put_in::geometry)    AS put_in_lng,
			ST_Y(ur.put_in::geometry)    AS put_in_lat,
			ST_X(ur.take_out::geometry)  AS take_out_lng,
			ST_Y(ur.take_out::geometry)  AS take_out_lat,
			ur.note, ur.created_at,
			ur.class_min, ur.class_max,
			ur.up_comid, ur.down_comid,
			CASE WHEN ur.centerline IS NOT NULL
			     THEN ST_AsGeoJSON(ur.centerline::geometry)
			     ELSE NULL END,
			ur.primary_gauge_id::text,
			g.name AS gauge_name,
			g.source AS gauge_source,
			g.external_id AS gauge_external_id,
			g.poll_health AS gauge_poll_health,
			g.last_poll_success_at,
			ur.custom_gauge_id::text,
			cg.name AS custom_gauge_name,
			COALESCE(lr.value, cg.last_value_cfs) AS current_cfs,
			COALESCE(lr.timestamp, cg.last_value_at) AS last_reading_at,
			CASE
				WHEN fr.band_color IS NULL        THEN 'unknown'
				WHEN fr.band_color LIKE 'red%'    THEN 'caution'
				WHEN fr.band_color LIKE 'blue%'   THEN 'flood'
				ELSE                                   'runnable'
			END AS flow_status,
			fr.band_label AS flow_band,
			rv.slug AS river_slug,
			COALESCE(ur.state_abbr, rv.state_abbr) AS river_state_abbr,
			rv.basin AS river_basin,
			ur.visibility,
			fr_reach.slug AS forked_from_slug,
			COALESCE(fr_reach.long_name, fr_reach.name) AS forked_from_name,
			ur.owner_id,
			up.handle AS author_handle,
			COALESCE(up.is_special, false) AS is_special,
			ur.original_author_handle,
			ur.original_forked_at,
			ur.last_modified_after_fork_at,
			ur.deleted_at
		FROM user_reaches ur
		LEFT JOIN rivers rv ON rv.id = ur.river_id
		LEFT JOIN gauges g ON g.id = ur.primary_gauge_id
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN user_reaches fr_reach ON fr_reach.id = ur.forked_from_user_reach_id
		LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
		LEFT JOIN LATERAL (
			SELECT value, timestamp FROM gauge_readings
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
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.label, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_label END)
				END AS band_label,
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.color, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_color END)
				END AS band_color
		) fr
		WHERE ur.id = $1 AND ur.visibility = 'public'
	`, runID).Scan(
		&d.ID, &d.Slug, &d.Name, &d.LongName, &d.RiverName,
		&d.PutInLng, &d.PutInLat, &d.TakeOutLng, &d.TakeOutLat,
		&d.Note, &d.CreatedAt,
		&d.ClassMin, &d.ClassMax,
		&d.UpComID, &d.DownComID,
		&geojsonBytes,
		&d.GaugeID, &d.GaugeName, &d.GaugeSource, &d.GaugeExternalID,
		&d.GaugePollHealth, &d.GaugeLastPollSuccess,
		&d.CustomGaugeID, &d.CustomGaugeName,
		&d.CurrentCFS, &d.LastReadAt,
		&d.FlowStatus, &d.FlowBand,
		&d.RiverSlug, &d.RiverStateAbbr, &d.RiverBasin,
		&d.Visibility,
		&d.ForkedFromSlug, &d.ForkedFromName,
		&authorID, &d.AuthorHandle, &d.IsSpecial,
		&d.OriginalAuthorHandle, &d.OriginalForkedAt, &d.LastModifiedAfterForkAt,
		&d.DeletedAt,
	)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	d.IsPrivate = d.Visibility != "public"
	if callerID != "" {
		for _, id := range editableOwnerIDs(r.Context(), h.db, callerID) {
			if id == authorID {
				d.IsOwn = true
				break
			}
		}
	}

	if geojsonBytes != nil {
		raw := json.RawMessage(geojsonBytes)
		d.Centerline = &raw
	}

	// Flow bands
	_ = h.db.QueryRow(r.Context(),
		`SELECT base_label, base_color FROM user_reaches WHERE id = $1`, d.ID,
	).Scan(&d.FlowBands.BaseLabel, &d.FlowBands.BaseColor)
	frRows2, _ := h.db.Query(r.Context(), `
		SELECT value, label, color
		FROM user_reach_flow_ranges
		WHERE user_reach_id = $1
		ORDER BY value ASC
	`, d.ID)
	d.FlowBands.Thresholds = make([]FlowBandThreshold, 0)
	if frRows2 != nil {
		defer frRows2.Close()
		for frRows2.Next() {
			var t FlowBandThreshold
			if frRows2.Scan(&t.Value, &t.Label, &t.Color) == nil {
				d.FlowBands.Thresholds = append(d.FlowBands.Thresholds, t)
			}
		}
	}

	// Rapids
	d.Rapids = make([]userReachRapid, 0)
	rapRows, _ := h.db.Query(r.Context(), `
		SELECT id, name, description, class_rating,
		       is_surf_wave, is_permanent_hazard, hazard_type,
		       ST_X(location::geometry), ST_Y(location::geometry)
		FROM rapids WHERE user_reach_id = $1 ORDER BY name
	`, d.ID)
	if rapRows != nil {
		defer rapRows.Close()
		for rapRows.Next() {
			var rr userReachRapid
			if rapRows.Scan(&rr.ID, &rr.Name, &rr.Description, &rr.ClassRating,
				&rr.IsSurfWave, &rr.IsPermanentHazard, &rr.HazardType,
				&rr.Lng, &rr.Lat) == nil {
				d.Rapids = append(d.Rapids, rr)
			}
		}
	}

	// Access points
	d.AccessPoints = make([]userReachAccessPoint, 0)
	apRows, _ := h.db.Query(r.Context(), `
		SELECT id, access_type, name, notes,
		       ST_X(location::geometry), ST_Y(location::geometry)
		FROM reach_access WHERE user_reach_id = $1 ORDER BY access_type, name
	`, d.ID)
	if apRows != nil {
		defer apRows.Close()
		for apRows.Next() {
			var ap userReachAccessPoint
			if apRows.Scan(&ap.ID, &ap.AccessType, &ap.Name, &ap.Notes,
				&ap.Lng, &ap.Lat) == nil {
				d.AccessPoints = append(d.AccessPoints, ap)
			}
		}
	}

	// Upvotes
	_ = h.db.QueryRow(r.Context(), `SELECT COUNT(*) FROM run_upvotes WHERE user_reach_id = $1`, d.ID).Scan(&d.UpvoteCount)
	if callerID != "" {
		_ = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM run_upvotes WHERE user_reach_id = $1 AND user_id = $2)`, d.ID, callerID).Scan(&d.UserUpvoted)
	}

	jsonResponse(w, http.StatusOK, d)
}

// ── Meetup feature picker (#246 "meet up at", product request 2026-07-25) ──
// No existing read returns a LIGHTWEIGHT rapids+access listing for a run:
// Get/GetPublic/getPublicByID return the full userReachDetail (gauge/flow
// bands/upvotes/description/hazard fields and all) as part of the whole run
// detail payload — too heavy for a "pick a feature" dropdown source, so this
// is a new, minimal, dedicated read.

// userReachFeatureOption/userReachAccessFeatureOption are deliberately thin
// {id,name[,access_type]} projections — no description/hazard/location
// fields the full run-detail rapids[]/access_points[] carry.
type userReachFeatureOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type userReachAccessFeatureOption struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	AccessType string `json:"access_type"`
}

// GET /api/v1/user-runs/{runId}/features
// Public for a public run (mirrors GetPublic/getPublicByID's visibility
// gate); an authed caller who may edit the run (owns it, or holds a
// special-user role for it — editableOwnerIDs, #314) can also read it while
// private — the calendar run sheet needs this for a still-unpublished run
// being planned. access[] is pre-filtered to the meetup-eligible access
// types (meetupEligibleAccessTypes, plan_runs.go) — put_in/take_out aren't
// offered as a "meet up at" pick, they're already the implicit endpoints.
func (h *UserReachHandler) ListFeatures(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")
	callerID, _ := h.ownerID(r)
	ctx := r.Context()

	var ownerID, visibility string
	if err := h.db.QueryRow(ctx,
		`SELECT owner_id, visibility::text FROM user_reaches WHERE id = $1::uuid AND deleted_at IS NULL`,
		runID,
	).Scan(&ownerID, &visibility); err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}
	if visibility != "public" {
		allowed := false
		if callerID != "" {
			for _, id := range editableOwnerIDs(ctx, h.db, callerID) {
				if id == ownerID {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			errorResponse(w, http.StatusNotFound, "run not found")
			return
		}
	}

	rapids := make([]userReachFeatureOption, 0)
	rapRows, _ := h.db.Query(ctx,
		`SELECT id, name FROM rapids WHERE user_reach_id = $1::uuid ORDER BY name`, runID)
	if rapRows != nil {
		defer rapRows.Close()
		for rapRows.Next() {
			var f userReachFeatureOption
			if rapRows.Scan(&f.ID, &f.Name) == nil {
				rapids = append(rapids, f)
			}
		}
	}

	access := make([]userReachAccessFeatureOption, 0)
	apRows, _ := h.db.Query(ctx, `
		SELECT id, COALESCE(name, ''), access_type FROM reach_access
		WHERE user_reach_id = $1::uuid
		  AND access_type IN ('camp','parking','boat_ramp','intermediate','shuttle_drop')
		ORDER BY access_type, name
	`, runID)
	if apRows != nil {
		defer apRows.Close()
		for apRows.Next() {
			var f userReachAccessFeatureOption
			if apRows.Scan(&f.ID, &f.Name, &f.AccessType) == nil {
				access = append(access, f)
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{"rapids": rapids, "access": access})
}

// ── Create ───────────────────────────────────────────────────────────────────

// POST /api/v1/me/reaches
func (h *UserReachHandler) Create(w http.ResponseWriter, r *http.Request) {
	ownerID, asSpecial, ok := h.authorOwnerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if rlErr := recordRunWrite(r.Context(), h.db, ownerID); rlErr != nil {
		errorResponse(w, http.StatusTooManyRequests, rlErr.Error())
		return
	}

	type latLng struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	}
	var body struct {
		Name      string   `json:"name"`
		LongName  *string  `json:"long_name"`
		RiverName string   `json:"river_name"`
		GnisID    string   `json:"gnis_id"`
		PutIn     latLng   `json:"put_in"`
		TakeOut   latLng   `json:"take_out"`
		UpComID   string   `json:"up_comid"`
		DownComID string   `json:"down_comid"`
		Note      *string  `json:"note"`
		ClassMin  *float64 `json:"class_min"`
		ClassMax  *float64 `json:"class_max"`
		// is_private kept for backward compat; nil (absent) = public by default; true → private
		IsPrivate *bool      `json:"is_private"`
		FlowBands *FlowBands `json:"flow_bands"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		errorResponse(w, http.StatusBadRequest, "name is required")
		return
	}
	if body.PutIn.Lat == 0 && body.PutIn.Lng == 0 {
		errorResponse(w, http.StatusBadRequest, "put_in coordinates required")
		return
	}
	if body.TakeOut.Lat == 0 && body.TakeOut.Lng == 0 {
		errorResponse(w, http.StatusBadRequest, "take_out coordinates required")
		return
	}

	ctx := r.Context()

	// Auto-assign or create river if river_name provided.
	var riverID *string
	var finalRiverName *string
	if rn := strings.TrimSpace(body.RiverName); rn != "" {
		finalRiverName = &rn
		rid := resolveOrCreateRiver(ctx, h.db, rn, body.GnisID, body.PutIn.Lat, body.PutIn.Lng)
		if rid != "" {
			riverID = &rid
		}
	}

	// Resolve this run's OWN state from its own put-in (#356) — independent
	// of the river row's COALESCE-once state_abbr, which multiple runs on a
	// multi-state river (e.g. Colorado River: AZ, UT, CO reaches) can't all
	// share correctly. put_in is validated non-zero above.
	stateAbbr := runStateFromCoords(ctx, body.PutIn.Lat, body.PutIn.Lng)

	// Generate slug from name.
	baseSlug := kmlimport.Slugify(body.Name)
	slug := baseSlug
	// Ensure uniqueness for this owner.
	for i := 2; i <= 20; i++ {
		var existing string
		err := h.db.QueryRow(ctx,
			`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`, ownerID, slug).Scan(&existing)
		if err != nil {
			break // no conflict
		}
		slug = fmt.Sprintf("%s-%d", baseSlug, i)
	}

	// Per-run privacy dropped — all runs public (community model).
	createVisibility := "public"
	_ = asSpecial // retained; no longer affects visibility (all runs public already)

	var reachID string
	err := h.db.QueryRow(ctx, `
		INSERT INTO user_reaches
			(owner_id, slug, name, long_name, river_id, river_name, put_in, take_out, up_comid, down_comid, note, class_min, class_max, visibility, published_at, river_confirmed, state_abbr)
		VALUES
			($1, $2, $3, $4, $5, $6,
			 ST_SetSRID(ST_MakePoint($7, $8), 4326)::geography,
			 ST_SetSRID(ST_MakePoint($9, $10), 4326)::geography,
			 NULLIF($11,''), NULLIF($12,''), $13, $14, $15,
			 $16::run_visibility,
			 CASE WHEN $16 = 'public' THEN NOW() ELSE NULL END,
			 $17, NULLIF($18,''))
		RETURNING id
	`, ownerID, slug, body.Name, body.LongName, riverID, finalRiverName,
		body.PutIn.Lng, body.PutIn.Lat,
		body.TakeOut.Lng, body.TakeOut.Lat,
		body.UpComID, body.DownComID, body.Note,
		body.ClassMin, body.ClassMax, createVisibility,
		riverID != nil, stateAbbr,
	).Scan(&reachID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("create failed: %v", err))
		return
	}

	// Insert flow bands.
	if fb := body.FlowBands; fb != nil {
		_, _ = h.db.Exec(ctx,
			`UPDATE user_reaches SET base_label = $1, base_color = $2 WHERE id = $3`,
			fb.BaseLabel, fb.BaseColor, reachID)
		for _, t := range fb.Thresholds {
			_, _ = h.db.Exec(ctx, `
				INSERT INTO user_reach_flow_ranges (user_reach_id, label, value, color)
				VALUES ($1, $2, $3, $4)
			`, reachID, t.Label, t.Value, t.Color)
		}
	}

	saveCompleteness(ctx, h.db, reachID)
	jsonResponse(w, http.StatusCreated, map[string]string{"id": reachID, "slug": slug})
}

// ── Import ────────────────────────────────────────────────────────────────────

// POST /api/v1/me/reaches/import
// Accepts a share payload (same shape as Create body) and creates a new reach
// owned by the authenticated user. Slug is re-generated to avoid collisions.
func (h *UserReachHandler) Import(w http.ResponseWriter, r *http.Request) { //nolint:cyclop
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	type latLng struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	}
	var body struct {
		Name            string        `json:"name"`
		LongName        *string       `json:"long_name"`
		RiverName       string        `json:"river_name"`
		GnisID          string        `json:"gnis_id"`
		PutIn           latLng        `json:"put_in"`
		TakeOut         latLng        `json:"take_out"`
		UpComID         string        `json:"up_comid"`
		DownComID       string        `json:"down_comid"`
		Note            *string       `json:"note"`
		FlowBands       *FlowBands    `json:"flow_bands"`
		GaugeExternalID string        `json:"gauge_external_id"`
		GaugeSource     string        `json:"gauge_source"`
		CustomGauge     *sharePayload `json:"custom_gauge"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid share payload")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		errorResponse(w, http.StatusBadRequest, "name is required")
		return
	}
	if body.PutIn.Lat == 0 && body.PutIn.Lng == 0 {
		errorResponse(w, http.StatusBadRequest, "put_in coordinates required")
		return
	}
	if body.TakeOut.Lat == 0 && body.TakeOut.Lng == 0 {
		errorResponse(w, http.StatusBadRequest, "take_out coordinates required")
		return
	}

	ctx := r.Context()

	var riverID *string
	var finalRiverName *string
	if rn := strings.TrimSpace(body.RiverName); rn != "" {
		finalRiverName = &rn
		rid := resolveOrCreateRiver(ctx, h.db, rn, body.GnisID, body.PutIn.Lat, body.PutIn.Lng)
		if rid != "" {
			riverID = &rid
		}
	}

	// Resolve this run's OWN state from its own put-in (#356) — see Create.
	stateAbbr := runStateFromCoords(ctx, body.PutIn.Lat, body.PutIn.Lng)

	baseSlug := kmlimport.Slugify(body.Name)
	slug := baseSlug
	for i := 2; i <= 20; i++ {
		var existing string
		err := h.db.QueryRow(ctx,
			`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`, ownerID, slug).Scan(&existing)
		if err != nil {
			break
		}
		slug = fmt.Sprintf("%s-%d", baseSlug, i)
	}

	var reachID string
	err := h.db.QueryRow(ctx, `
		INSERT INTO user_reaches
			(owner_id, slug, name, long_name, river_id, river_name, put_in, take_out, up_comid, down_comid, note, river_confirmed, state_abbr)
		VALUES
			($1, $2, $3, $4, $5, $6,
			 ST_SetSRID(ST_MakePoint($7, $8), 4326)::geography,
			 ST_SetSRID(ST_MakePoint($9, $10), 4326)::geography,
			 NULLIF($11,''), NULLIF($12,''), $13, $14, NULLIF($15,''))
		RETURNING id
	`, ownerID, slug, body.Name, body.LongName, riverID, finalRiverName,
		body.PutIn.Lng, body.PutIn.Lat,
		body.TakeOut.Lng, body.TakeOut.Lat,
		body.UpComID, body.DownComID, body.Note,
		riverID != nil, stateAbbr,
	).Scan(&reachID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("import failed: %v", err))
		return
	}

	if fb := body.FlowBands; fb != nil {
		_, _ = h.db.Exec(ctx,
			`UPDATE user_reaches SET base_label = $1, base_color = $2 WHERE id = $3`,
			fb.BaseLabel, fb.BaseColor, reachID)
		for _, t := range fb.Thresholds {
			_, _ = h.db.Exec(ctx, `
				INSERT INTO user_reach_flow_ranges (user_reach_id, label, value, color)
				VALUES ($1, $2, $3, $4)
			`, reachID, t.Label, t.Value, t.Color)
		}
	}

	// Restore gauge association: custom gauge takes precedence over primary gauge.
	if body.CustomGauge != nil && body.CustomGauge.Name != "" {
		cgID, _ := importCustomGaugeForOwner(ctx, h.db, ownerID, *body.CustomGauge)
		if cgID != "" {
			_, _ = h.db.Exec(ctx,
				`UPDATE user_reaches SET custom_gauge_id = $1::uuid, updated_at = NOW() WHERE id = $2`,
				cgID, reachID)
		}
	} else if body.GaugeExternalID != "" && body.GaugeSource != "" {
		var gaugeID string
		if lookupErr := h.db.QueryRow(ctx,
			`SELECT id::text FROM gauges WHERE external_id = $1 AND source = $2`,
			body.GaugeExternalID, body.GaugeSource,
		).Scan(&gaugeID); lookupErr == nil {
			_, _ = h.db.Exec(ctx,
				`UPDATE user_reaches SET primary_gauge_id = $1::uuid, updated_at = NOW() WHERE id = $2`,
				gaugeID, reachID)
		}
	}

	saveCompleteness(ctx, h.db, reachID)
	jsonResponse(w, http.StatusCreated, map[string]string{"id": reachID, "slug": slug})
}

// ── Slug check ───────────────────────────────────────────────────────────────

// GET /api/v1/me/runs/slug-check?slug=X&exclude=current-slug
func (h *UserReachHandler) SlugCheck(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := r.URL.Query().Get("slug")
	if slug == "" {
		errorResponse(w, http.StatusBadRequest, "slug is required")
		return
	}
	if !userReachSlugRe.MatchString(slug) {
		jsonResponse(w, http.StatusOK, map[string]bool{"available": false})
		return
	}
	exclude := r.URL.Query().Get("exclude")
	var exists bool
	_ = h.db.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM user_reaches WHERE owner_id = $1 AND slug = $2 AND deleted_at IS NULL AND slug != $3)`,
		ownerID, slug, exclude,
	).Scan(&exists)
	jsonResponse(w, http.StatusOK, map[string]bool{"available": !exists})
}

// ── Update ───────────────────────────────────────────────────────────────────

// PATCH /api/v1/me/reaches/{slug}
func (h *UserReachHandler) Update(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()
	ids := editableOwnerIDs(ctx, h.db, callerID)

	// Resolve the actual owner of this slug among the caller's editable
	// accounts (their own, plus any special account their role grants them).
	// Prefer the caller's own row on a slug collision.
	var ownerID string
	if err := h.db.QueryRow(ctx,
		`SELECT owner_id FROM user_reaches WHERE owner_id = ANY($1::text[]) AND slug = $2
		 ORDER BY (owner_id = $3) DESC LIMIT 1`,
		ids, slug, callerID,
	).Scan(&ownerID); err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}
	if rlErr := recordRunWrite(ctx, h.db, ownerID); rlErr != nil {
		errorResponse(w, http.StatusTooManyRequests, rlErr.Error())
		return
	}

	type latLng struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	}
	var body struct {
		Name      *string  `json:"name"`
		NewSlug   *string  `json:"slug"`
		Note      *string  `json:"note"`
		RiverName *string  `json:"river_name"`
		GnisID    *string  `json:"gnis_id"`
		PutIn     *latLng  `json:"put_in"`
		TakeOut   *latLng  `json:"take_out"`
		UpComID   *string  `json:"up_comid"`
		DownComID *string  `json:"down_comid"`
		ClassMin  *float64 `json:"class_min"`
		ClassMax  *float64 `json:"class_max"`
		// is_private kept for backward compat; maps to visibility column
		IsPrivate  *bool   `json:"is_private"`
		Visibility *string `json:"visibility"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Treat empty river_name as NULL (clear it).
	var riverName *string
	if body.RiverName != nil && strings.TrimSpace(*body.RiverName) != "" {
		rn := strings.TrimSpace(*body.RiverName)
		riverName = &rn
	}

	// Validate and uniqueness-check new slug when provided. Scoped to the
	// resolved target owner (not necessarily the caller) — renaming a
	// special-user's run must not collide within THAT account's namespace.
	var newSlug *string
	if body.NewSlug != nil && *body.NewSlug != slug {
		ns := strings.TrimSpace(*body.NewSlug)
		if !userReachSlugRe.MatchString(ns) {
			errorResponse(w, http.StatusBadRequest, "slug must be lowercase alphanumeric with hyphens")
			return
		}
		// Reject if another run already uses this slug.
		var conflict bool
		_ = h.db.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM user_reaches WHERE owner_id = $1 AND slug = $2 AND deleted_at IS NULL)`,
			ownerID, ns,
		).Scan(&conflict)
		if conflict {
			errorResponse(w, http.StatusConflict, "slug already in use")
			return
		}
		newSlug = &ns
	}

	// Resolve visibility update: accept both old is_private and new visibility field.
	var newVisibility *string
	if body.Visibility != nil {
		newVisibility = body.Visibility
	} else if body.IsPrivate != nil {
		v := "public"
		if *body.IsPrivate {
			v = "private"
		}
		newVisibility = &v
	}
	// Normalize legacy 'unlisted' → 'public' (unlisted retired).
	if newVisibility != nil && *newVisibility == "unlisted" {
		v := "public"
		newVisibility = &v
	}

	// Core fields update. Set last_modified_after_fork_at when run is a fork.
	// When visibility is set to 'public' for the first time, also stamp published_at.
	tag, err := h.db.Exec(ctx, `
		UPDATE user_reaches
		SET
			name         = COALESCE($3, name),
			slug         = CASE WHEN $4 THEN $5 ELSE slug END,
			note         = $6,
			river_name   = CASE WHEN $7 THEN $8 ELSE river_name END,
			class_min    = CASE WHEN $9 THEN $10::numeric ELSE class_min END,
			class_max    = CASE WHEN $11 THEN $12::numeric ELSE class_max END,
			visibility   = CASE WHEN $13 THEN $14::run_visibility ELSE visibility END,
			published_at = CASE
				WHEN $13 AND $14 = 'public' AND published_at IS NULL THEN NOW()
				ELSE published_at
			END,
			updated_at   = NOW(),
			last_modified_after_fork_at = CASE
				WHEN original_forked_at IS NOT NULL THEN NOW()
				ELSE last_modified_after_fork_at
			END
		WHERE owner_id = $1 AND slug = $2
	`, ownerID, slug, body.Name, newSlug != nil, newSlug, body.Note,
		body.RiverName != nil, riverName,
		body.ClassMin != nil, body.ClassMin,
		body.ClassMax != nil, body.ClassMax,
		newVisibility != nil, newVisibility)
	if err != nil || tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}
	// Subsequent lookups must use the new slug when renamed.
	if newSlug != nil {
		slug = *newSlug
	}

	// Re-resolve river when river_name is being set, so state/basin/huc8 get populated.
	// Use this request's new put-in when the caller supplied one (geometry can
	// arrive in the same PATCH as river_name); otherwise there is no real
	// coordinate to resolve against here. Previously this hardcoded 0,0
	// ("null island") on every call — resolveOrCreateRiver only consults
	// putInLat/putInLng at all when it's creating a brand-new river row with
	// no gnis_id, so that garbage point could silently seed a new river's
	// state/basin/huc8 from the Gulf of Guinea instead of the run's actual
	// location. 0,0 is still passed when we truly have no coordinate (no
	// put_in in this PATCH) — resolveOrCreateRiver already treats that as
	// "unknown, skip coord resolution" (putInLat != 0 && putInLng != 0 guard),
	// so it's a meaningful sentinel here, not fed-in garbage.
	if riverName != nil {
		gnisID := ""
		if body.GnisID != nil {
			gnisID = strings.TrimSpace(*body.GnisID)
		}
		var putInLat, putInLng float64
		if body.PutIn != nil {
			putInLat, putInLng = body.PutIn.Lat, body.PutIn.Lng
		}
		rid := resolveOrCreateRiver(ctx, h.db, *riverName, gnisID, putInLat, putInLng)
		if rid != "" {
			_, _ = h.db.Exec(ctx,
				`UPDATE user_reaches SET river_id = $3 WHERE owner_id = $1 AND slug = $2`,
				ownerID, slug, rid)
		}
	}

	// Geometry update — only when all four geometry fields provided. The
	// put-in moving means this run's OWN state may have changed too (#356),
	// so recompute state_abbr from the NEW put-in on every geometry update —
	// unconditionally replacing rather than merging: if the fresh lookup
	// fails softly (network error, or point outside all US polygons) we set
	// NULL rather than keep whatever state matched the OLD put-in, since a
	// stale-but-plausible value is worse than a visibly-missing one once we
	// know the location changed.
	if body.PutIn != nil && body.TakeOut != nil && body.UpComID != nil && body.DownComID != nil {
		newStateAbbr := runStateFromCoords(ctx, body.PutIn.Lat, body.PutIn.Lng)
		_, _ = h.db.Exec(ctx, `
			UPDATE user_reaches
			SET
				put_in     = ST_SetSRID(ST_MakePoint($3, $4), 4326)::geography,
				take_out   = ST_SetSRID(ST_MakePoint($5, $6), 4326)::geography,
				up_comid   = NULLIF($7, ''),
				down_comid = NULLIF($8, ''),
				state_abbr = NULLIF($9, ''),
				updated_at = NOW()
			WHERE owner_id = $1 AND slug = $2
		`, ownerID, slug,
			body.PutIn.Lng, body.PutIn.Lat,
			body.TakeOut.Lng, body.TakeOut.Lat,
			*body.UpComID, *body.DownComID, newStateAbbr)
	}

	// Recompute completeness after any field change. (V18)
	var runID string
	if h.db.QueryRow(ctx, `SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`,
		ownerID, slug).Scan(&runID) == nil {
		saveCompleteness(ctx, h.db, runID)
	}

	jsonResponse(w, http.StatusOK, map[string]string{"slug": slug})
}

// ── Delete ───────────────────────────────────────────────────────────────────

// DELETE /api/v1/me/reaches/{slug}
// Tombstones when referenced by other users; hard-deletes when zero refs. (V11, V13)
func (h *UserReachHandler) Delete(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()
	ids := editableOwnerIDs(ctx, h.db, callerID)

	var runID string
	if err := h.db.QueryRow(ctx,
		`SELECT id FROM user_reaches
		 WHERE owner_id = ANY($1::text[]) AND slug = $2 AND deleted_at IS NULL
		 ORDER BY (owner_id = $3) DESC LIMIT 1`,
		ids, slug, callerID,
	).Scan(&runID); err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}

	// Count external references from other users' dashboards. (V11)
	var refCount int
	_ = h.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM user_watchlists WHERE referenced_user_reach_id = $1`,
		runID,
	).Scan(&refCount)

	if refCount > 0 {
		// Referenced — soft delete (tombstone). (V11)
		_, _ = h.db.Exec(ctx,
			`UPDATE user_reaches SET deleted_at = NOW(), updated_at = NOW() WHERE id = $1`,
			runID)
	} else {
		// Zero refs — hard delete. Votes deleted first due to RESTRICT FK. (V13)
		hardDeleteRun(ctx, h.db, runID)
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Flow ranges ──────────────────────────────────────────────────────────────

// GET /api/v1/me/reaches/{slug}/flow-ranges
func (h *UserReachHandler) GetFlowRanges(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")

	var reachID string
	if err := h.db.QueryRow(r.Context(),
		`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`, ownerID, slug).Scan(&reachID); err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}

	var fb FlowBands
	if err := h.db.QueryRow(r.Context(),
		`SELECT base_label, base_color FROM user_reaches WHERE id = $1`, reachID,
	).Scan(&fb.BaseLabel, &fb.BaseColor); err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	rows, _ := h.db.Query(r.Context(), `
		SELECT value, label, color
		FROM user_reach_flow_ranges
		WHERE user_reach_id = $1
		ORDER BY value ASC
	`, reachID)
	fb.Thresholds = make([]FlowBandThreshold, 0)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var t FlowBandThreshold
			if rows.Scan(&t.Value, &t.Label, &t.Color) == nil {
				fb.Thresholds = append(fb.Thresholds, t)
			}
		}
	}
	jsonResponse(w, http.StatusOK, fb)
}

// GET /api/v1/users/{handle}/runs/{slug}/flow-ranges
// Public flow ranges for a public user run, resolved by owner handle + slug.
// No auth — used by the gauge graph to color another user's (or one's own)
// run in reach mode. Slug alone is ambiguous across users, so handle scopes it.
func (h *UserReachHandler) GetPublicFlowRangesByHandle(w http.ResponseWriter, r *http.Request) {
	handle := chi.URLParam(r, "handle")
	slug := chi.URLParam(r, "slug")

	var reachID string
	if err := h.db.QueryRow(r.Context(), `
		SELECT ur.id
		FROM user_reaches ur
		JOIN user_profiles up ON up.owner_id = ur.owner_id
		WHERE LOWER(up.handle) = LOWER($1)
		  AND ur.slug = $2
		  AND ur.visibility = 'public'
		  AND ur.deleted_at IS NULL
	`, handle, slug).Scan(&reachID); err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	var fb FlowBands
	if err := h.db.QueryRow(r.Context(),
		`SELECT base_label, base_color FROM user_reaches WHERE id = $1`, reachID,
	).Scan(&fb.BaseLabel, &fb.BaseColor); err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	rows, _ := h.db.Query(r.Context(), `
		SELECT value, label, color
		FROM user_reach_flow_ranges
		WHERE user_reach_id = $1
		ORDER BY value ASC
	`, reachID)
	fb.Thresholds = make([]FlowBandThreshold, 0)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var t FlowBandThreshold
			if rows.Scan(&t.Value, &t.Label, &t.Color) == nil {
				fb.Thresholds = append(fb.Thresholds, t)
			}
		}
	}
	jsonResponse(w, http.StatusOK, fb)
}

// PUT /api/v1/me/reaches/{slug}/flow-ranges
func (h *UserReachHandler) SetFlowRanges(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()
	ids := editableOwnerIDs(ctx, h.db, callerID)

	var reachID, resolvedOwnerID string
	if err := h.db.QueryRow(ctx,
		`SELECT id, owner_id FROM user_reaches WHERE owner_id = ANY($1::text[]) AND slug = $2
		 ORDER BY (owner_id = $3) DESC LIMIT 1`,
		ids, slug, callerID).Scan(&reachID, &resolvedOwnerID); err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}
	if rlErr := recordRunWrite(ctx, h.db, resolvedOwnerID); rlErr != nil {
		errorResponse(w, http.StatusTooManyRequests, rlErr.Error())
		return
	}

	var body FlowBands
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if msg := validateFlowBands(body); msg != "" {
		errorResponse(w, http.StatusBadRequest, msg)
		return
	}

	_, err := h.db.Exec(ctx,
		`UPDATE user_reaches SET base_label = $1, base_color = $2 WHERE id = $3`,
		body.BaseLabel, body.BaseColor, reachID,
	)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "update failed: "+err.Error())
		return
	}
	_, _ = h.db.Exec(ctx, `DELETE FROM user_reach_flow_ranges WHERE user_reach_id = $1`, reachID)
	for _, t := range body.Thresholds {
		_, _ = h.db.Exec(ctx, `
			INSERT INTO user_reach_flow_ranges (user_reach_id, label, value, color)
			VALUES ($1, $2, $3, $4)
		`, reachID, t.Label, t.Value, t.Color)
	}

	saveCompleteness(ctx, h.db, reachID)
	w.WriteHeader(http.StatusNoContent)
}

// ── Rapids & Access Points ──────────────────────────────────────────────────
//
// Bulk-replace endpoints backing the run editor's rapids/access-points steps.
// Wholesale DELETE-then-INSERT inside a transaction, same shape as
// SetFlowRanges above. Every inserted row is stamped data_source='community',
// verified=true — NOT 'import'/'ai_seed' — so a future KML re-import (which
// only clears rows with data_source IN ('import','ai_seed'), see
// kmlimport.Importer.clearImportDataForTarget) never clobbers a manual edit.

// editableAccessTypes are the reach_access.access_type values the feature
// editor may write. put_in/take_out are excluded: those are owned by the
// run's put_in/take_out columns and set via wizard steps 1-2, never here.
var editableAccessTypes = map[string]bool{
	"camp":         true,
	"parking":      true,
	"boat_ramp":    true,
	"intermediate": true,
	"shuttle_drop": true,
}

type rapidInput struct {
	Name              string   `json:"name"`
	Description       *string  `json:"description"`
	ClassRating       *float64 `json:"class_rating"`
	IsSurfWave        bool     `json:"is_surf_wave"`
	IsPermanentHazard bool     `json:"is_permanent_hazard"`
	HazardType        *string  `json:"hazard_type"`
	Lng               *float64 `json:"lng"`
	Lat               *float64 `json:"lat"`
}

// PUT /api/v1/me/reaches/{slug}/rapids
// Body: { "rapids": [ {rapid}, ... ] }
// Wholesale-replaces every rapid on the run.
func (h *UserReachHandler) SetRapids(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ids := editableOwnerIDs(r.Context(), h.db, callerID)

	var reachID, resolvedOwnerID string
	if err := h.db.QueryRow(r.Context(),
		`SELECT id, owner_id FROM user_reaches WHERE owner_id = ANY($1::text[]) AND slug = $2
		 ORDER BY (owner_id = $3) DESC LIMIT 1`,
		ids, slug, callerID).Scan(&reachID, &resolvedOwnerID); err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}
	if rlErr := recordRunWrite(r.Context(), h.db, resolvedOwnerID); rlErr != nil {
		errorResponse(w, http.StatusTooManyRequests, rlErr.Error())
		return
	}

	var body struct {
		Rapids []rapidInput `json:"rapids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	for i := range body.Rapids {
		body.Rapids[i].Name = strings.TrimSpace(body.Rapids[i].Name)
		if body.Rapids[i].Name == "" {
			errorResponse(w, http.StatusBadRequest, fmt.Sprintf("rapids[%d].name is required", i))
			return
		}
	}

	ctx := r.Context()
	tx, err := h.db.Begin(ctx)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx begin failed")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `DELETE FROM rapids WHERE user_reach_id = $1`, reachID); err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed: "+err.Error())
		return
	}
	for _, rp := range body.Rapids {
		var insertErr error
		if rp.Lng != nil && rp.Lat != nil {
			_, insertErr = tx.Exec(ctx, `
				INSERT INTO rapids (user_reach_id, name, location, description, class_rating,
				                    is_surf_wave, is_permanent_hazard, hazard_type, data_source, verified)
				VALUES ($1, $2, ST_SetSRID(ST_MakePoint($3, $4), 4326)::geography,
				        NULLIF($5, ''), $6, $7, $8, NULLIF($9, ''), 'community', true)
			`, reachID, rp.Name, rp.Lng, rp.Lat, rp.Description, rp.ClassRating,
				rp.IsSurfWave, rp.IsPermanentHazard, rp.HazardType)
		} else {
			_, insertErr = tx.Exec(ctx, `
				INSERT INTO rapids (user_reach_id, name, description, class_rating,
				                    is_surf_wave, is_permanent_hazard, hazard_type, data_source, verified)
				VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, NULLIF($7, ''), 'community', true)
			`, reachID, rp.Name, rp.Description, rp.ClassRating,
				rp.IsSurfWave, rp.IsPermanentHazard, rp.HazardType)
		}
		if insertErr != nil {
			if isUniqueViolation(insertErr) {
				errorResponse(w, http.StatusConflict, fmt.Sprintf("duplicate rapid name: %q", rp.Name))
			} else {
				errorResponse(w, http.StatusInternalServerError, "insert failed: "+insertErr.Error())
			}
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		errorResponse(w, http.StatusInternalServerError, "commit failed")
		return
	}

	saveCompleteness(ctx, h.db, reachID)
	w.WriteHeader(http.StatusNoContent)
}

type accessPointInput struct {
	AccessType string   `json:"access_type"`
	Name       *string  `json:"name"`
	Notes      *string  `json:"notes"`
	Lng        *float64 `json:"lng"`
	Lat        *float64 `json:"lat"`
}

// PUT /api/v1/me/reaches/{slug}/access
// Body: { "access": [ {access}, ... ] }
// Wholesale-replaces every non-put_in/take_out access point on the run;
// put_in/take_out rows (owned by the run's put_in/take_out columns) are
// preserved untouched.
func (h *UserReachHandler) SetAccessPoints(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ids := editableOwnerIDs(r.Context(), h.db, callerID)

	var reachID, resolvedOwnerID string
	if err := h.db.QueryRow(r.Context(),
		`SELECT id, owner_id FROM user_reaches WHERE owner_id = ANY($1::text[]) AND slug = $2
		 ORDER BY (owner_id = $3) DESC LIMIT 1`,
		ids, slug, callerID).Scan(&reachID, &resolvedOwnerID); err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}
	if rlErr := recordRunWrite(r.Context(), h.db, resolvedOwnerID); rlErr != nil {
		errorResponse(w, http.StatusTooManyRequests, rlErr.Error())
		return
	}

	var body struct {
		Access []accessPointInput `json:"access"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	for i, ap := range body.Access {
		if editableAccessTypes[ap.AccessType] {
			continue
		}
		if ap.AccessType == "put_in" || ap.AccessType == "take_out" {
			errorResponse(w, http.StatusBadRequest,
				fmt.Sprintf("access[%d].access_type %q is owned by the run's put-in/take-out fields and cannot be set here", i, ap.AccessType))
		} else {
			errorResponse(w, http.StatusBadRequest,
				fmt.Sprintf("access[%d].access_type %q is invalid", i, ap.AccessType))
		}
		return
	}

	ctx := r.Context()
	tx, err := h.db.Begin(ctx)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx begin failed")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx,
		`DELETE FROM reach_access WHERE user_reach_id = $1 AND access_type NOT IN ('put_in','take_out')`,
		reachID); err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed: "+err.Error())
		return
	}
	for _, ap := range body.Access {
		var insertErr error
		if ap.Lng != nil && ap.Lat != nil {
			_, insertErr = tx.Exec(ctx, `
				INSERT INTO reach_access (user_reach_id, access_type, name, location, notes, data_source, verified)
				VALUES ($1, $2, NULLIF($3, ''), ST_SetSRID(ST_MakePoint($4, $5), 4326)::geography,
				        NULLIF($6, ''), 'community', true)
			`, reachID, ap.AccessType, ap.Name, ap.Lng, ap.Lat, ap.Notes)
		} else {
			_, insertErr = tx.Exec(ctx, `
				INSERT INTO reach_access (user_reach_id, access_type, name, notes, data_source, verified)
				VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), 'community', true)
			`, reachID, ap.AccessType, ap.Name, ap.Notes)
		}
		if insertErr != nil {
			if isUniqueViolation(insertErr) {
				name := ""
				if ap.Name != nil {
					name = *ap.Name
				}
				errorResponse(w, http.StatusConflict, fmt.Sprintf("duplicate access point: %s %q", ap.AccessType, name))
			} else {
				errorResponse(w, http.StatusInternalServerError, "insert failed: "+insertErr.Error())
			}
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		errorResponse(w, http.StatusInternalServerError, "commit failed")
		return
	}

	saveCompleteness(ctx, h.db, reachID)
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/v1/me/reaches/{slug}/gauge
func (h *UserReachHandler) ClearGauge(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()
	ids := editableOwnerIDs(ctx, h.db, callerID)

	var reachID string
	if err := h.db.QueryRow(ctx,
		`SELECT id FROM user_reaches WHERE owner_id = ANY($1::text[]) AND slug = $2
		 ORDER BY (owner_id = $3) DESC LIMIT 1`,
		ids, slug, callerID).Scan(&reachID); err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}
	_, err := h.db.Exec(ctx, `
		UPDATE user_reaches
		SET primary_gauge_id = NULL, custom_gauge_id = NULL, updated_at = NOW()
		WHERE id = $1
	`, reachID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "update failed")
		return
	}
	saveCompleteness(ctx, h.db, reachID)
	w.WriteHeader(http.StatusNoContent)
}

// ── Centerline ───────────────────────────────────────────────────────────────

// POST /api/v1/me/reaches/{slug}/centerline
// Body: { "up_comid": "...", "down_comid": "...", "start_lat": ..., ... }
// Delegates to the NLDI handler's centerline fetch logic via internal fetch.
func (h *UserReachHandler) SetCenterline(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ids := editableOwnerIDs(r.Context(), h.db, callerID)

	var reachID string
	if err := h.db.QueryRow(r.Context(),
		`SELECT id FROM user_reaches WHERE owner_id = ANY($1::text[]) AND slug = $2
		 ORDER BY (owner_id = $3) DESC LIMIT 1`,
		ids, slug, callerID).Scan(&reachID); err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}

	var body struct {
		GeoJSON json.RawMessage `json:"geojson"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.GeoJSON == nil {
		errorResponse(w, http.StatusBadRequest, "geojson required")
		return
	}

	_, err := h.db.Exec(r.Context(), `
		UPDATE user_reaches
		SET centerline = ST_GeomFromGeoJSON($2)::geography, updated_at = NOW()
		WHERE id = $1
	`, reachID, string(body.GeoJSON))
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("centerline update failed: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/v1/me/reaches/{slug}/centerline
func (h *UserReachHandler) ClearCenterline(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ids := editableOwnerIDs(r.Context(), h.db, callerID)

	var reachID string
	if err := h.db.QueryRow(r.Context(),
		`SELECT id FROM user_reaches WHERE owner_id = ANY($1::text[]) AND slug = $2
		 ORDER BY (owner_id = $3) DESC LIMIT 1`,
		ids, slug, callerID).Scan(&reachID); err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}
	_, err := h.db.Exec(r.Context(), `
		UPDATE user_reaches
		SET centerline = NULL, up_comid = NULL, down_comid = NULL, updated_at = NOW()
		WHERE id = $1
	`, reachID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "update failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Gauge ─────────────────────────────────────────────────────────────────────

// PUT /api/v1/me/reaches/{slug}/gauge
// Body (pick one):
//
//	{ "gauge_id": "<uuid>" }
//	{ "custom_gauge_id": "<uuid>" }
//	{ "external_id": "...", "source": "...", "name": "...", "lat": 0.0, "lng": 0.0 }
func (h *UserReachHandler) SetGauge(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ids := editableOwnerIDs(r.Context(), h.db, callerID)

	var reachID string
	if err := h.db.QueryRow(r.Context(),
		`SELECT id FROM user_reaches WHERE owner_id = ANY($1::text[]) AND slug = $2
		 ORDER BY (owner_id = $3) DESC LIMIT 1`,
		ids, slug, callerID).Scan(&reachID); err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}

	var body struct {
		GaugeID       *string  `json:"gauge_id"`
		CustomGaugeID *string  `json:"custom_gauge_id"`
		ExternalID    *string  `json:"external_id"`
		Source        *string  `json:"source"`
		Name          *string  `json:"name"`
		Lat           *float64 `json:"lat"`
		Lng           *float64 `json:"lng"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid body")
		return
	}

	// Upsert path: caller provides external_id+source+name+lat+lng
	if body.ExternalID != nil && body.Source != nil {
		if body.Name == nil || body.Lat == nil || body.Lng == nil {
			errorResponse(w, http.StatusBadRequest, "name, lat, lng required with external_id")
			return
		}
		extID := normalizeExternalID(strings.TrimSpace(*body.ExternalID), *body.Source)
		body.ExternalID = &extID
		var gaugeID string
		err := h.db.QueryRow(r.Context(), `
			INSERT INTO gauges (external_id, source, name, location)
			VALUES ($1, $2, $3, ST_SetSRID(ST_MakePoint($5, $4), 4326)::geography)
			ON CONFLICT (external_id, source) DO UPDATE
			  SET name = EXCLUDED.name, location = EXCLUDED.location
			RETURNING id
		`, body.ExternalID, body.Source, body.Name, body.Lat, body.Lng).Scan(&gaugeID)
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, "failed to upsert gauge")
			return
		}
		body.GaugeID = &gaugeID
	}

	if body.GaugeID == nil && body.CustomGaugeID == nil {
		errorResponse(w, http.StatusBadRequest, "gauge_id, custom_gauge_id, or external_id+source required")
		return
	}
	if body.GaugeID != nil && body.CustomGaugeID != nil {
		errorResponse(w, http.StatusBadRequest, "only one of gauge_id or custom_gauge_id allowed")
		return
	}

	var err error
	if body.GaugeID != nil {
		_, err = h.db.Exec(r.Context(), `
			UPDATE user_reaches
			SET primary_gauge_id = $2::uuid, custom_gauge_id = NULL, updated_at = NOW()
			WHERE id = $1
		`, reachID, body.GaugeID)
	} else {
		_, err = h.db.Exec(r.Context(), `
			UPDATE user_reaches
			SET custom_gauge_id = $2::uuid, primary_gauge_id = NULL, updated_at = NOW()
			WHERE id = $1
		`, reachID, body.CustomGaugeID)
	}
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "gauge update failed")
		return
	}
	saveCompleteness(r.Context(), h.db, reachID)
	w.WriteHeader(http.StatusNoContent)
}

// ImportKML handles POST /api/v1/me/reaches/{slug}/kml
// Accepts multipart/form-data with a "file" field containing a KML or KMZ file.
// Imports all point placemarks as pins into the authenticated owner's user reach.
func (h *UserReachHandler) ImportKML(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "read file: "+err.Error())
		return
	}

	doc, err := kmlimport.ParseKMLBytes(data)
	if err != nil {
		errorResponse(w, http.StatusUnprocessableEntity, "parse KML: "+err.Error())
		return
	}

	imp := kmlimport.New(h.db, false)
	res, err := imp.ImportForUserReach(r.Context(), ownerID, slug, doc)
	if err != nil {
		errorResponse(w, http.StatusNotFound, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, res)
}

// ── ForkUserRun ───────────────────────────────────────────────────────────────

// POST /api/v1/user-runs/{runId}/fork
// Clones a public (or the caller's own) user_reach into a new user_reach
// owned by the caller.
func (h *UserReachHandler) ForkUserRun(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID := chi.URLParam(r, "runId")
	ctx := r.Context()

	newID, newSlug, err := forkRunTx(ctx, h.db, ownerID, runID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			errorResponse(w, http.StatusNotFound, "run not found")
			return
		}
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	var gaugeID *string
	_ = h.db.QueryRow(ctx, `SELECT primary_gauge_id::text FROM user_reaches WHERE id = $1`, newID).Scan(&gaugeID)

	saveCompleteness(ctx, h.db, newID)
	jsonResponse(w, http.StatusCreated, map[string]string{"id": newID, "slug": newSlug, "gauge_id": func() string {
		if gaugeID != nil {
			return *gaugeID
		}
		return ""
	}()})
}

// ── Fork ─────────────────────────────────────────────────────────────────────

// pgxQueryer is satisfied by both *pgxpool.Pool and pgx.Tx — lets fork logic
// run inside a transaction (watchlist auto-fork) or standalone (ForkReach).
type pgxQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// forkRunTx forks any forkable run — public, or the caller's own — into a new
// user_reaches row owned by ownerID. Always snapshots
// original_author_handle/original_author_owner_id so provenance survives
// repeated forking (the old curated-only fork helper this replaces skipped
// that for curated sources; #314 unifies the two fork paths and fixes that
// gap). Copies
// centerline, base flow bands + thresholds, and every rapid/access-point
// feature (copyRunFeatures). The fork is always created public.
//
// Shared by ForkUserRun (self-service), ForkReach (special-user curated-slug
// routes), and the watchlist auto-fork path (watchlist.go).
func forkRunTx(ctx context.Context, q pgxQueryer, ownerID, srcRunID string) (newID, newSlug string, err error) {
	type srcRow struct {
		ID           string
		Name         string
		OwnerID      string
		AuthorHandle *string
		RiverID      *string
		RiverName    *string
		Note         *string
		ClassMin     *float64
		ClassMax     *float64
		GaugeID      *string
		PutInLng     float64
		PutInLat     float64
		TakeOutLng   float64
		TakeOutLat   float64
		Centerline   []byte
	}
	var src srcRow
	err = q.QueryRow(ctx, `
		SELECT
			ur.id::text, ur.name, ur.owner_id::text,
			up.handle,
			ur.river_id::text, ur.river_name, ur.note,
			ur.class_min, ur.class_max,
			ur.primary_gauge_id::text,
			ST_X(ur.put_in::geometry),  ST_Y(ur.put_in::geometry),
			ST_X(ur.take_out::geometry), ST_Y(ur.take_out::geometry),
			ST_AsGeoJSON(ur.centerline::geometry)
		FROM user_reaches ur
		LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
		WHERE ur.id = $1 AND ur.deleted_at IS NULL
		  AND (ur.visibility = 'public' OR ur.owner_id = $2)
	`, srcRunID, ownerID).Scan(
		&src.ID, &src.Name, &src.OwnerID, &src.AuthorHandle,
		&src.RiverID, &src.RiverName, &src.Note,
		&src.ClassMin, &src.ClassMax,
		&src.GaugeID,
		&src.PutInLng, &src.PutInLat,
		&src.TakeOutLng, &src.TakeOutLat,
		&src.Centerline,
	)
	if err != nil {
		return "", "", fmt.Errorf("run not found: %w", err)
	}

	baseSlug := kmlimport.Slugify(src.Name)
	newSlug = baseSlug
	for i := 2; i <= 20; i++ {
		var existing string
		if e := q.QueryRow(ctx,
			`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`, ownerID, newSlug,
		).Scan(&existing); e != nil {
			break
		}
		newSlug = fmt.Sprintf("%s-%d", baseSlug, i)
	}

	if err = q.QueryRow(ctx, `
		INSERT INTO user_reaches
			(owner_id, slug, name, river_id, river_name, note,
			 put_in, take_out,
			 class_min, class_max, primary_gauge_id,
			 forked_from_user_reach_id,
			 original_author_handle, original_author_owner_id, original_forked_at,
			 river_confirmed, visibility, published_at)
		VALUES
			($1, $2, $3, $4::uuid, $5, $17,
			 ST_SetSRID(ST_MakePoint($6, $7), 4326)::geography,
			 ST_SetSRID(ST_MakePoint($8, $9), 4326)::geography,
			 $10, $11, $12::uuid,
			 $13::uuid,
			 $14, $15::uuid, NOW(),
			 $16, 'public'::run_visibility, NOW())
		RETURNING id
	`, ownerID, newSlug, src.Name, src.RiverID, src.RiverName,
		src.PutInLng, src.PutInLat,
		src.TakeOutLng, src.TakeOutLat,
		src.ClassMin, src.ClassMax, src.GaugeID,
		src.ID,
		src.AuthorHandle, src.OwnerID,
		src.RiverID != nil,
		src.Note,
	).Scan(&newID); err != nil {
		return "", "", fmt.Errorf("fork insert failed: %w", err)
	}

	if len(src.Centerline) > 0 {
		_, _ = q.Exec(ctx, `
			UPDATE user_reaches
			SET centerline = ST_GeomFromGeoJSON($2)::geography
			WHERE id = $1
		`, newID, string(src.Centerline))
	}

	// Copy base band config + thresholds verbatim (V12).
	_, _ = q.Exec(ctx, `
		UPDATE user_reaches dst
		SET base_label = src.base_label, base_color = src.base_color
		FROM user_reaches src
		WHERE dst.id = $1 AND src.id = $2
	`, newID, src.ID)
	_, _ = q.Exec(ctx, `
		INSERT INTO user_reach_flow_ranges (user_reach_id, label, value, color)
		SELECT $1, label, value, color
		FROM user_reach_flow_ranges
		WHERE user_reach_id = $2
		ON CONFLICT (user_reach_id, value) DO NOTHING
	`, newID, src.ID)

	// Copy all features (rapids + access points) so a fork carries the whole run (#312).
	copyRunFeatures(ctx, q, newID, src.ID)

	return newID, newSlug, nil
}

// copyRunFeatures clones every rapids + reach_access row from srcID's run onto
// dstID's freshly-forked run. Feature rows are re-stamped data_source='community'
// so the fork owns its own copies (see the run-features editor CRUD). Best-effort:
// a fork still succeeds if this fails. dstID is a brand-new run with no existing
// features, so there are no unique-constraint conflicts to handle.
func copyRunFeatures(ctx context.Context, q pgxQueryer, dstID, srcID string) {
	_, _ = q.Exec(ctx, `
		INSERT INTO rapids
			(user_reach_id, name, river_mile, location, class_rating, class_at_low, class_at_high,
			 description, portage_description, is_portage_recommended,
			 is_surf_wave, is_permanent_hazard, hazard_type, data_source, verified)
		SELECT $1, name, river_mile, location, class_rating, class_at_low, class_at_high,
			 description, portage_description, is_portage_recommended,
			 is_surf_wave, is_permanent_hazard, hazard_type, 'community', verified
		FROM rapids WHERE user_reach_id = $2
	`, dstID, srcID)

	_, _ = q.Exec(ctx, `
		INSERT INTO reach_access
			(user_reach_id, access_type, name, location, directions, road_type,
			 parking_spaces, parking_fee, permit_required, permit_info, permit_url,
			 seasonal_close_start, seasonal_close_end, notes, parking_location,
			 parking_notes, hike_to_water_min, entry_style, approach_dist_mi, approach_notes,
			 data_source, verified)
		SELECT $1, access_type, name, location, directions, road_type,
			 parking_spaces, parking_fee, permit_required, permit_info, permit_url,
			 seasonal_close_start, seasonal_close_end, notes, parking_location,
			 parking_notes, hike_to_water_min, entry_style, approach_dist_mi, approach_notes,
			 'community', verified
		FROM reach_access WHERE user_reach_id = $2
	`, dstID, srcID)
}

// POST /api/v1/me/reaches/fork-reach/{slug} (and /me/runs/fork-reach/{slug})
// Clones a special-user (curated) run into a new user_reaches row owned by
// the authenticated user. These slug routes exist specifically for curated
// forks — special-user account slugs only, not arbitrary community runs
// (use ForkUserRun by ID for those).
func (h *UserReachHandler) ForkReach(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	sourceSlug := chi.URLParam(r, "slug")
	ctx := r.Context()

	var srcRunID string
	if err := h.db.QueryRow(ctx, `
		SELECT id FROM user_reaches
		WHERE slug = $1
		  AND owner_id IN (SELECT owner_id FROM user_profiles WHERE is_special)
		  AND deleted_at IS NULL
	`, sourceSlug).Scan(&srcRunID); err != nil {
		errorResponse(w, http.StatusNotFound, "reach not found")
		return
	}

	newID, newSlug, err := forkRunTx(ctx, h.db, ownerID, srcRunID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			errorResponse(w, http.StatusNotFound, "reach not found")
			return
		}
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	saveCompleteness(ctx, h.db, newID)
	jsonResponse(w, http.StatusCreated, map[string]string{"id": newID, "slug": newSlug})
}

// ── ListCommunity ─────────────────────────────────────────────────────────────

type communityRunsResponse struct {
	Items      []userReachSummary `json:"items"`
	HasMore    bool               `json:"has_more"`
	NextOffset int                `json:"next_offset,omitempty"`
}

// GET /api/v1/user-runs/community
func (h *UserReachHandler) ListCommunity(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 20
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 50 {
		limit = l
	}
	offset := 0
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o > 0 {
		offset = o
	}

	search := "%" + q + "%"
	query := `
		SELECT
			ur.id, ur.slug, ur.name, ur.long_name, ur.river_name,
			COALESCE(ur.state_abbr, rv.state_abbr) AS state_abbr, rv.basin AS basin_group,
			ST_X(ur.put_in::geometry)   AS put_in_lng,
			ST_Y(ur.put_in::geometry)   AS put_in_lat,
			ST_X(ur.take_out::geometry) AS take_out_lng,
			ST_Y(ur.take_out::geometry) AS take_out_lat,
			ur.note, ur.created_at,
			ur.class_min, ur.class_max,
			COALESCE(lr.value, cg.last_value_cfs)       AS current_cfs,
			COALESCE(lr.timestamp, cg.last_value_at)    AS last_reading_at,
			CASE
				WHEN fr.band_color IS NULL        THEN 'unknown'
				WHEN fr.band_color LIKE 'red%'    THEN 'caution'
				WHEN fr.band_color LIKE 'blue%'   THEN 'flood'
				ELSE                                   'runnable'
			END AS flow_status,
			fr.band_label AS flow_band,
			ur.primary_gauge_id::text,
			ur.custom_gauge_id::text,
			cg.slug AS custom_gauge_slug,
			cg.name AS custom_gauge_name,
			ur.visibility,
			up.handle AS author_handle,
			COALESCE(up.is_special, false) AS is_special,
			(SELECT COUNT(*)::int FROM user_reaches f
			 WHERE f.forked_from_user_reach_id = ur.id
			   AND f.visibility = 'public' AND f.deleted_at IS NULL) AS fork_count
		FROM user_reaches ur
		LEFT JOIN rivers rv ON rv.id = ur.river_id
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
		LEFT JOIN LATERAL (
			SELECT value, timestamp FROM gauge_readings
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
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.label, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_label END)
				END AS band_label,
				CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
					COALESCE(thresh.color, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_color END)
				END AS band_color
		) fr
		WHERE ur.visibility = 'public' AND ur.deleted_at IS NULL
		  AND ur.completeness_score >= 0.2
		  AND ur.forked_from_user_reach_id IS NULL
		  AND ($1 = '%%' OR ur.name ILIKE $1 OR ur.river_name ILIKE $1)
	` + anonPublicOnMapFilter(r, "ur.owner_id") + `
		ORDER BY
			(5.0 * ur.completeness_score + COALESCE((SELECT COUNT(*) FROM run_upvotes uv WHERE uv.user_reach_id = ur.id), 0)::float)
			/ GREATEST(5.0 + COALESCE((SELECT COUNT(*) FROM run_upvotes uv WHERE uv.user_reach_id = ur.id), 0)::float, 1.0) DESC,
			ur.created_at DESC, ur.id
		LIMIT $2 OFFSET $3
	`
	rows, err := h.db.Query(r.Context(), query, search, limit+1, offset)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	items := make([]userReachSummary, 0)
	for rows.Next() {
		var s userReachSummary
		if err := rows.Scan(
			&s.ID, &s.Slug, &s.Name, &s.LongName, &s.RiverName,
			&s.StateAbbr, &s.BasinGroup,
			&s.PutInLng, &s.PutInLat, &s.TakeOutLng, &s.TakeOutLat,
			&s.Note, &s.CreatedAt,
			&s.ClassMin, &s.ClassMax,
			&s.CurrentCFS, &s.LastReadAt,
			&s.FlowStatus, &s.FlowBand,
			&s.GaugeID, &s.CustomGaugeID, &s.CustomGaugeSlug, &s.CustomGaugeName,
			&s.Visibility, &s.AuthorHandle, &s.IsSpecial, &s.ForkCount,
		); err == nil {
			s.IsPrivate = s.Visibility != "public"
			items = append(items, s)
		}
	}

	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	jsonResponse(w, http.StatusOK, communityRunsResponse{
		Items:      items,
		HasMore:    hasMore,
		NextOffset: offset + len(items),
	})
}

// ── Completeness helpers ──────────────────────────────────────────────────────

// saveCompleteness recomputes and stores completeness_score for a run. (V18)
// Called after any mutation that could change presence of scored fields.
func saveCompleteness(ctx context.Context, db *pgxpool.Pool, runID string) {
	_, _ = db.Exec(ctx, `
		UPDATE user_reaches ur
		SET completeness_score = (
			CASE WHEN ur.note IS NOT NULL AND char_length(ur.note) > 0             THEN 0.2 ELSE 0 END
		  + CASE WHEN ur.primary_gauge_id IS NOT NULL OR ur.custom_gauge_id IS NOT NULL THEN 0.2 ELSE 0 END
		  + CASE WHEN ur.class_min IS NOT NULL AND ur.class_max IS NOT NULL         THEN 0.2 ELSE 0 END
		  + CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN 0.2 ELSE 0 END
		  + CASE WHEN EXISTS(SELECT 1 FROM rapids WHERE user_reach_id = ur.id)      THEN 0.2 ELSE 0 END
		)
		WHERE ur.id = $1
	`, runID)
}

// ── GC helpers ───────────────────────────────────────────────────────────────

// hardDeleteRun deletes upvotes (RESTRICT FK) then hard-deletes the run. (V13)
func hardDeleteRun(ctx context.Context, db *pgxpool.Pool, runID string) {
	_, _ = db.Exec(ctx, `DELETE FROM run_upvotes WHERE user_reach_id = $1`, runID)
	_, _ = db.Exec(ctx, `DELETE FROM user_reaches WHERE id = $1`, runID)
}

// gcTombstonedRun hard-deletes a tombstoned run when it has zero references. (V13)
// Called after a reference is removed from user_watchlists.
func gcTombstonedRun(ctx context.Context, db *pgxpool.Pool, runID string) {
	var isEligible bool
	_ = db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM user_reaches
			WHERE id = $1 AND deleted_at IS NOT NULL
		) AND NOT EXISTS(
			SELECT 1 FROM user_watchlists WHERE referenced_user_reach_id = $1
		)
	`, runID).Scan(&isEligible)
	if isEligible {
		hardDeleteRun(ctx, db, runID)
	}
}

// ── Publish ───────────────────────────────────────────────────────────────────

// POST /api/v1/me/runs/{slug}/publish
// One-way ratchet: private|unlisted → public. Idempotent if already public.
// Rejects: setting visibility below public after publish (V2).
func (h *UserReachHandler) Publish(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()

	var curVisibility string
	var reachID string
	if err := h.db.QueryRow(ctx,
		`SELECT id, visibility FROM user_reaches WHERE owner_id = $1 AND slug = $2 AND deleted_at IS NULL`,
		ownerID, slug,
	).Scan(&reachID, &curVisibility); err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	if curVisibility == "public" {
		// Already public — idempotent, return current state.
		jsonResponse(w, http.StatusOK, map[string]string{"visibility": "public"})
		return
	}

	// V2: private|unlisted → public (one-way ratchet).
	_, err := h.db.Exec(ctx, `
		UPDATE user_reaches
		SET visibility   = 'public',
		    published_at = NOW(),
		    updated_at   = NOW()
		WHERE id = $1
	`, reachID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "publish failed")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]string{"visibility": "public"})
}

// ── Adopt ─────────────────────────────────────────────────────────────────────

// POST /api/v1/user-runs/{runId}/adopt
// Transfers a tombstoned run to the caller when caller has an active reference.
// Preserves votes; un-tombstones; keeps visibility public. (V14)
func (h *UserReachHandler) Adopt(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID := chi.URLParam(r, "runId")
	ctx := r.Context()

	// Verify run is tombstoned.
	var curOwnerID, slug string
	var deletedAt *string
	if err := h.db.QueryRow(ctx,
		`SELECT owner_id::text, slug, deleted_at::text FROM user_reaches WHERE id = $1`,
		runID,
	).Scan(&curOwnerID, &slug, &deletedAt); err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}
	if deletedAt == nil {
		errorResponse(w, http.StatusConflict, "run is not tombstoned; cannot adopt live run")
		return
	}
	if curOwnerID == callerID {
		errorResponse(w, http.StatusConflict, "already own this run")
		return
	}

	// Verify caller has active reference to this run. (V14)
	var hasRef bool
	if err := h.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM user_watchlists WHERE user_id = $1 AND referenced_user_reach_id = $2::uuid)`,
		callerID, runID,
	).Scan(&hasRef); err != nil || !hasRef {
		errorResponse(w, http.StatusForbidden, "must have run on your dashboard to adopt it")
		return
	}

	// Ensure slug is unique in adopter's namespace; re-slug if conflict.
	finalSlug := slug
	var conflict string
	if err := h.db.QueryRow(ctx,
		`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`,
		callerID, slug,
	).Scan(&conflict); err == nil {
		// Conflict — append suffix until unique.
		base := slug
		for i := 2; i <= 20; i++ {
			candidate := fmt.Sprintf("%s-%d", base, i)
			if e := h.db.QueryRow(ctx,
				`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`,
				callerID, candidate,
			).Scan(&conflict); e != nil {
				finalSlug = candidate
				break
			}
		}
	}

	// Transfer ownership; un-tombstone; record adoption. Votes preserved in run_upvotes.
	// owner_id is TEXT; adopted_from_owner_id is UUID (may be NULL when curOwner is non-UUID dev value).
	_, err := h.db.Exec(ctx, `
		UPDATE user_reaches
		SET owner_id               = $2,
		    slug                   = $3,
		    adopted_from_owner_id  = CASE WHEN $4 ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
		                                  THEN $4::uuid ELSE NULL END,
		    deleted_at             = NULL,
		    visibility             = 'public',
		    updated_at             = NOW()
		WHERE id = $1
	`, runID, callerID, finalSlug, curOwnerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "adopt failed")
		return
	}

	saveCompleteness(ctx, h.db, runID)
	jsonResponse(w, http.StatusOK, map[string]string{"id": runID, "slug": finalSlug})
}
