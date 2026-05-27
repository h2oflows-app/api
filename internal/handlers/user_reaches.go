package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/h2oflow/h2oflow/apps/api/internal/kmlimport"
	"github.com/jackc/pgx/v5/pgxpool"
)

// resolveOrCreateRiver finds a river by GNIS ID (preferred) or name, backfilling
// gnis_id on existing legacy rows when a new GNIS match is now available. When
// no match exists, INSERTs a new river — populating state_abbr/basin/huc8 via
// NHD + WBD/TIGERweb when gnisID is present. Returns river ID, "" on failure.
func resolveOrCreateRiver(ctx context.Context, db *pgxpool.Pool, riverName, gnisID string) string {
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

// ── Types ────────────────────────────────────────────────────────────────────

type userReachFlowRange struct {
	Label    string   `json:"label"`
	MinValue *float64 `json:"min_value"`
	MaxValue *float64 `json:"max_value"`
}

type userReachSummary struct {
	ID          string     `json:"id"`
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	RiverName   *string    `json:"river_name"`
	StateAbbr   *string    `json:"state_abbr"`
	BasinGroup  *string    `json:"basin_group"`
	PutInLng    float64    `json:"put_in_lng"`
	PutInLat    float64    `json:"put_in_lat"`
	TakeOutLng  float64    `json:"take_out_lng"`
	TakeOutLat  float64    `json:"take_out_lat"`
	Note        *string    `json:"note"`
	ClassMin    *float64   `json:"class_min"`
	ClassMax    *float64   `json:"class_max"`
	CurrentCFS  *float64   `json:"current_cfs"`
	FlowBand    *string    `json:"flow_band"`
	FlowStatus  string     `json:"flow_status"`
	GaugeID          *string    `json:"gauge_id"`
	CustomGaugeID    *string    `json:"custom_gauge_id"`
	CustomGaugeSlug  *string    `json:"custom_gauge_slug"`
	CustomGaugeName  *string    `json:"custom_gauge_name"`
	LastReadAt       *time.Time `json:"last_reading_at"`
	CreatedAt        time.Time  `json:"created_at"`
	IsPrivate        bool       `json:"is_private"`
	AuthorHandle     *string    `json:"author_handle"`
	IsOfficial       bool       `json:"is_official"`
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
	RiverSlug       *string              `json:"river_slug"`
	RiverStateAbbr  *string              `json:"river_state_abbr"`
	RiverBasin      *string              `json:"river_basin"`
	UpComID          *string              `json:"up_comid"`
	DownComID        *string              `json:"down_comid"`
	Centerline       *json.RawMessage     `json:"centerline"`
	GaugeID               *string              `json:"gauge_id"`
	GaugeName             *string              `json:"gauge_name"`
	GaugeSource           *string              `json:"gauge_source"`
	GaugeExternalID       *string              `json:"gauge_external_id"`
	GaugePollHealth       *string              `json:"gauge_poll_health"`
	GaugeLastPollSuccess  *time.Time           `json:"gauge_last_poll_success_at"`
	CustomGaugeID         *string              `json:"custom_gauge_id"`
	CustomGaugeName       *string              `json:"custom_gauge_name"`
	ForkedFromSlug          *string    `json:"forked_from_slug"`
	ForkedFromName          *string    `json:"forked_from_name"`
	OriginalAuthorHandle    *string    `json:"original_author_handle"`
	OriginalForkedAt        *time.Time `json:"original_forked_at"`
	LastModifiedAfterForkAt *time.Time `json:"last_modified_after_fork_at"`
	FlowRanges            []userReachFlowRange   `json:"flow_ranges"`
	Rapids                []userReachRapid       `json:"rapids"`
	AccessPoints          []userReachAccessPoint `json:"access_points"`
	UpvoteCount           int                    `json:"upvote_count"`
	UserUpvoted           bool                   `json:"user_upvoted"`
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
				WHEN COALESCE(lr.value, cg.last_value_cfs) IS NULL OR fr.label IS NULL THEN 'unknown'
				WHEN fr.label = 'running' THEN 'runnable'
				WHEN fr.label = 'low'     THEN 'caution'
				WHEN fr.label = 'high'    THEN 'flood'
				ELSE 'unknown'
			END AS flow_status,
			ur.primary_gauge_id::text AS gauge_id,
			ur.class_max
		FROM user_reaches ur
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN LATERAL (
			SELECT value FROM gauge_readings
			WHERE gauge_id = ur.primary_gauge_id
			  AND timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY timestamp DESC LIMIT 1
		) lr ON TRUE
		LEFT JOIN LATERAL (
			SELECT label FROM user_reach_flow_ranges
			WHERE user_reach_id = ur.id
			  AND (min_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) >= min_value)
			  AND (max_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) <  max_value)
			ORDER BY min_value ASC NULLS FIRST
			LIMIT 1
		) fr ON TRUE
		WHERE ur.owner_id = $1
	`, ownerID)
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
		CommonName  *string  `json:"common_name"`
		ClassMax    *float64 `json:"class_max"`
		FlowStatus  string   `json:"flow_status"`
		CurrentCFS  *float64 `json:"current_cfs"`
		GaugeID     *string  `json:"gauge_id"`
		IsUserReach bool     `json:"is_user_reach"`
	}
	type feature struct {
		Type       string          `json:"type"`
		Geometry   json.RawMessage `json:"geometry"`
		Properties featureProps    `json:"properties"`
	}

	features := make([]feature, 0)
	for rows.Next() {
		var (
			id, slug, name  string
			riverName       *string
			centerlineJSON  *string
			putInLng, putInLat   float64
			takeOutLng, takeOutLat float64
			currentCFS      *float64
			flowStatus      string
			gaugeID         *string
			classMax        *float64
		)
		if err := rows.Scan(
			&id, &slug, &name, &riverName,
			&centerlineJSON,
			&putInLng, &putInLat, &takeOutLng, &takeOutLat,
			&currentCFS, &flowStatus, &gaugeID, &classMax,
		); err != nil {
			continue
		}

		var geom json.RawMessage
		if centerlineJSON != nil && *centerlineJSON != "" {
			geom = json.RawMessage(*centerlineJSON)
		} else {
			// Synthesize a 2-point LineString from put_in → take_out.
			type lineString struct {
				Type        string      `json:"type"`
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
				ID:          id,
				Slug:        slug,
				Name:        name,
				RiverName:   riverName,
				CommonName:  nil,
				ClassMax:    classMax,
				FlowStatus:  flowStatus,
				CurrentCFS:  currentCFS,
				GaugeID:     gaugeID,
				IsUserReach: true,
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
	rows, err := h.db.Query(r.Context(), `
		WITH cluster_groups AS (
			SELECT a.id AS run_id, MIN(b.id::text) AS cluster_id
			FROM user_reaches a
			JOIN user_reaches b ON (
				b.is_private = FALSE
				AND a.up_comid IS NOT NULL
				AND b.up_comid = a.up_comid
				AND ST_DWithin(a.put_in::geography,   b.put_in::geography,   1609.34)
				AND ST_DWithin(a.take_out::geography, b.take_out::geography, 1609.34)
			)
			WHERE a.is_private = FALSE
			GROUP BY a.id
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
				WHEN COALESCE(lr.value, cg.last_value_cfs) IS NULL OR fr.label IS NULL THEN 'unknown'
				WHEN fr.label = 'running' THEN 'runnable'
				WHEN fr.label = 'low'     THEN 'caution'
				WHEN fr.label = 'high'    THEN 'flood'
				ELSE 'unknown'
			END AS flow_status,
			ur.primary_gauge_id::text AS gauge_id,
			ur.class_max,
			up.handle AS author_handle,
			(up.owner_id IS NULL) AS is_official,
			COALESCE(cgrp.cluster_id, ur.id::text) AS cluster_id,
			(
				COALESCE((SELECT COUNT(*) FROM run_upvotes uv WHERE uv.user_reach_id = ur.id), 0)
				+ COALESCE((SELECT COUNT(*) FROM reports rp WHERE rp.user_reach_id = ur.id AND rp.deleted_at IS NULL), 0) * 2
				+ (CASE WHEN ur.centerline IS NOT NULL THEN 1 ELSE 0 END
				   + CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN 1 ELSE 0 END
				   + CASE WHEN ur.note IS NOT NULL AND char_length(ur.note) >= 20 THEN 1 ELSE 0 END) * 5
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
			SELECT label FROM user_reach_flow_ranges
			WHERE user_reach_id = ur.id
			  AND (min_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) >= min_value)
			  AND (max_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) <  max_value)
			ORDER BY min_value ASC NULLS FIRST
			LIMIT 1
		) fr ON TRUE
		WHERE ur.is_private = FALSE
	`)
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
		IsCommunity  bool     `json:"is_community"`
		AuthorHandle *string  `json:"author_handle"`
		IsOfficial   bool     `json:"is_official"`
		ClusterID    string   `json:"cluster_id"`
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
			isOfficial             bool
			clusterID              string
			rankScore              int
		)
		if err := rows.Scan(
			&id, &slug, &name, &riverName,
			&centerlineJSON,
			&putInLng, &putInLat, &takeOutLng, &takeOutLat,
			&currentCFS, &flowStatus, &gaugeID, &classMax,
			&authorHandle, &isOfficial,
			&clusterID, &rankScore,
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
				IsCommunity:  true,
				AuthorHandle: authorHandle,
				IsOfficial:   isOfficial,
				ClusterID:    clusterID,
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
			ur.id, ur.slug, ur.name, ur.river_name,
			rv.state_abbr, rv.basin AS basin_group,
			ST_X(ur.put_in::geometry)    AS put_in_lng,
			ST_Y(ur.put_in::geometry)    AS put_in_lat,
			ST_X(ur.take_out::geometry)  AS take_out_lng,
			ST_Y(ur.take_out::geometry)  AS take_out_lat,
			ur.note, ur.created_at,
			ur.class_min, ur.class_max,
			COALESCE(lr.value, cg.last_value_cfs) AS current_cfs,
			COALESCE(lr.timestamp, cg.last_value_at) AS last_reading_at,
			CASE
				WHEN COALESCE(lr.value, cg.last_value_cfs) IS NULL OR fr.label IS NULL THEN 'unknown'
				WHEN fr.label = 'running' THEN 'runnable'
				WHEN fr.label = 'low'     THEN 'caution'
				WHEN fr.label = 'high'    THEN 'flood'
				ELSE 'unknown'
			END AS flow_status,
			fr.label AS flow_band,
			ur.primary_gauge_id::text AS gauge_id,
			ur.custom_gauge_id::text AS custom_gauge_id,
			cg.slug AS custom_gauge_slug,
			cg.name AS custom_gauge_name,
			ur.is_private
		FROM user_reaches ur
		LEFT JOIN rivers rv ON rv.id = ur.river_id
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN LATERAL (
			SELECT value, timestamp FROM gauge_readings
			WHERE gauge_id = ur.primary_gauge_id
			  AND timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY timestamp DESC LIMIT 1
		) lr ON TRUE
		LEFT JOIN LATERAL (
			SELECT label FROM user_reach_flow_ranges
			WHERE user_reach_id = ur.id
			  AND (min_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) >= min_value)
			  AND (max_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) <  max_value)
			ORDER BY min_value ASC NULLS FIRST
			LIMIT 1
		) fr ON TRUE
		WHERE ur.owner_id = $1
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
			&s.ID, &s.Slug, &s.Name, &s.RiverName,
			&s.StateAbbr, &s.BasinGroup,
			&s.PutInLng, &s.PutInLat, &s.TakeOutLng, &s.TakeOutLat,
			&s.Note, &s.CreatedAt,
			&s.ClassMin, &s.ClassMax,
			&s.CurrentCFS, &s.LastReadAt,
			&s.FlowStatus, &s.FlowBand, &s.GaugeID,
			&s.CustomGaugeID, &s.CustomGaugeSlug, &s.CustomGaugeName,
			&s.IsPrivate,
		); err == nil {
			items = append(items, s)
		}
	}
	jsonResponse(w, http.StatusOK, items)
}

// ── Get ──────────────────────────────────────────────────────────────────────

// GET /api/v1/me/reaches/{slug}
func (h *UserReachHandler) Get(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")

	var d userReachDetail
	var geojsonBytes []byte

	err := h.db.QueryRow(r.Context(), `
		SELECT
			ur.id, ur.slug, ur.name, ur.river_name,
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
				WHEN COALESCE(lr.value, cg.last_value_cfs) IS NULL OR fr.label IS NULL THEN 'unknown'
				WHEN fr.label = 'running' THEN 'runnable'
				WHEN fr.label = 'low'     THEN 'caution'
				WHEN fr.label = 'high'    THEN 'flood'
				ELSE 'unknown'
			END AS flow_status,
			fr.label AS flow_band,
			rv.slug AS river_slug,
			rv.state_abbr AS river_state_abbr,
			rv.basin AS river_basin,
			ur.is_private,
			COALESCE(fr_reach.slug, fr_ur.slug) AS forked_from_slug,
			COALESCE(
				COALESCE(fr_reach.common_name, fr_reach.name),
				fr_ur.name
			) AS forked_from_name,
			ur.original_author_handle,
			ur.original_forked_at,
			ur.last_modified_after_fork_at
		FROM user_reaches ur
		LEFT JOIN rivers rv ON rv.id = ur.river_id
		LEFT JOIN gauges g ON g.id = ur.primary_gauge_id
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN reaches fr_reach ON fr_reach.id = ur.forked_from_reach_id
		LEFT JOIN user_reaches fr_ur ON fr_ur.id = ur.forked_from_user_reach_id
		LEFT JOIN LATERAL (
			SELECT value, timestamp FROM gauge_readings
			WHERE gauge_id = ur.primary_gauge_id
			  AND timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY timestamp DESC LIMIT 1
		) lr ON TRUE
		LEFT JOIN LATERAL (
			SELECT label FROM user_reach_flow_ranges
			WHERE user_reach_id = ur.id
			  AND (min_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) >= min_value)
			  AND (max_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) <  max_value)
			ORDER BY min_value ASC NULLS FIRST
			LIMIT 1
		) fr ON TRUE
		WHERE ur.owner_id = $1 AND ur.slug = $2
	`, ownerID, slug).Scan(
		&d.ID, &d.Slug, &d.Name, &d.RiverName,
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
		&d.IsPrivate,
		&d.ForkedFromSlug, &d.ForkedFromName,
		&d.OriginalAuthorHandle, &d.OriginalForkedAt, &d.LastModifiedAfterForkAt,
	)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}

	if geojsonBytes != nil {
		raw := json.RawMessage(geojsonBytes)
		d.Centerline = &raw
	}

	// Flow ranges
	frRows, _ := h.db.Query(r.Context(), `
		SELECT label, min_value, max_value
		FROM user_reach_flow_ranges
		WHERE user_reach_id = $1
		ORDER BY min_value ASC NULLS FIRST
	`, d.ID)
	if frRows != nil {
		defer frRows.Close()
		d.FlowRanges = make([]userReachFlowRange, 0)
		for frRows.Next() {
			var fr userReachFlowRange
			if frRows.Scan(&fr.Label, &fr.MinValue, &fr.MaxValue) == nil {
				d.FlowRanges = append(d.FlowRanges, fr)
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
	_ = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM run_upvotes WHERE user_reach_id = $1 AND user_id = $2)`, d.ID, ownerID).Scan(&d.UserUpvoted)

	jsonResponse(w, http.StatusOK, d)
}

// ── GetPublic ─────────────────────────────────────────────────────────────────

// GET /api/v1/user-runs/{runId}
// Public: returns detail for any non-private user reach by UUID.
// Auth optional — when present, populates user_upvoted.
func (h *UserReachHandler) GetPublic(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")
	callerID, _ := h.ownerID(r) // optional

	var d userReachDetail
	var geojsonBytes []byte
	var authorID string

	err := h.db.QueryRow(r.Context(), `
		SELECT
			ur.id, ur.slug, ur.name, ur.river_name,
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
				WHEN COALESCE(lr.value, cg.last_value_cfs) IS NULL OR fr.label IS NULL THEN 'unknown'
				WHEN fr.label = 'running' THEN 'runnable'
				WHEN fr.label = 'low'     THEN 'caution'
				WHEN fr.label = 'high'    THEN 'flood'
				ELSE 'unknown'
			END AS flow_status,
			fr.label AS flow_band,
			rv.slug AS river_slug,
			rv.state_abbr AS river_state_abbr,
			rv.basin AS river_basin,
			ur.is_private,
			COALESCE(fr_reach.slug, fr_ur.slug) AS forked_from_slug,
			COALESCE(
				COALESCE(fr_reach.common_name, fr_reach.name),
				fr_ur.name
			) AS forked_from_name,
			ur.owner_id,
			up.handle AS author_handle,
			ur.original_author_handle,
			ur.original_forked_at,
			ur.last_modified_after_fork_at
		FROM user_reaches ur
		LEFT JOIN rivers rv ON rv.id = ur.river_id
		LEFT JOIN gauges g ON g.id = ur.primary_gauge_id
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN reaches fr_reach ON fr_reach.id = ur.forked_from_reach_id
		LEFT JOIN user_reaches fr_ur ON fr_ur.id = ur.forked_from_user_reach_id
		LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
		LEFT JOIN LATERAL (
			SELECT value, timestamp FROM gauge_readings
			WHERE gauge_id = ur.primary_gauge_id
			  AND timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY timestamp DESC LIMIT 1
		) lr ON TRUE
		LEFT JOIN LATERAL (
			SELECT label FROM user_reach_flow_ranges
			WHERE user_reach_id = ur.id
			  AND (min_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) >= min_value)
			  AND (max_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) <  max_value)
			ORDER BY min_value ASC NULLS FIRST
			LIMIT 1
		) fr ON TRUE
		WHERE ur.id = $1 AND ur.is_private = FALSE
	`, runID).Scan(
		&d.ID, &d.Slug, &d.Name, &d.RiverName,
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
		&d.IsPrivate,
		&d.ForkedFromSlug, &d.ForkedFromName,
		&authorID, &d.AuthorHandle,
		&d.OriginalAuthorHandle, &d.OriginalForkedAt, &d.LastModifiedAfterForkAt,
	)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	d.IsOfficial = (d.AuthorHandle == nil)

	if geojsonBytes != nil {
		raw := json.RawMessage(geojsonBytes)
		d.Centerline = &raw
	}

	// Flow ranges
	frRows, _ := h.db.Query(r.Context(), `
		SELECT label, min_value, max_value
		FROM user_reach_flow_ranges
		WHERE user_reach_id = $1
		ORDER BY min_value ASC NULLS FIRST
	`, d.ID)
	if frRows != nil {
		defer frRows.Close()
		d.FlowRanges = make([]userReachFlowRange, 0)
		for frRows.Next() {
			var fr userReachFlowRange
			if frRows.Scan(&fr.Label, &fr.MinValue, &fr.MaxValue) == nil {
				d.FlowRanges = append(d.FlowRanges, fr)
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

// ── Create ───────────────────────────────────────────────────────────────────

// POST /api/v1/me/reaches
func (h *UserReachHandler) Create(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	type latLng struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	}
	type bandRange struct {
		MinValue *float64 `json:"min_value"`
		MaxValue *float64 `json:"max_value"`
	}
	var body struct {
		Name      string   `json:"name"`
		RiverName string   `json:"river_name"`
		GnisID    string   `json:"gnis_id"`
		PutIn     latLng   `json:"put_in"`
		TakeOut   latLng   `json:"take_out"`
		UpComID   string   `json:"up_comid"`
		DownComID string   `json:"down_comid"`
		Note      *string  `json:"note"`
		ClassMin  *float64 `json:"class_min"`
		ClassMax  *float64 `json:"class_max"`
		IsPrivate bool     `json:"is_private"`
		FlowRanges *struct {
			Low     *bandRange `json:"low"`
			Running *bandRange `json:"running"`
			High    *bandRange `json:"high"`
		} `json:"flow_ranges"`
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
		rid := resolveOrCreateRiver(ctx, h.db, rn, body.GnisID)
		if rid != "" {
			riverID = &rid
		}
	}

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

	var reachID string
	err := h.db.QueryRow(ctx, `
		INSERT INTO user_reaches
			(owner_id, slug, name, river_id, river_name, put_in, take_out, up_comid, down_comid, note, class_min, class_max, is_private)
		VALUES
			($1, $2, $3, $4, $5,
			 ST_SetSRID(ST_MakePoint($6, $7), 4326)::geography,
			 ST_SetSRID(ST_MakePoint($8, $9), 4326)::geography,
			 NULLIF($10,''), NULLIF($11,''), $12, $13, $14, $15)
		RETURNING id
	`, ownerID, slug, body.Name, riverID, finalRiverName,
		body.PutIn.Lng, body.PutIn.Lat,
		body.TakeOut.Lng, body.TakeOut.Lat,
		body.UpComID, body.DownComID, body.Note,
		body.ClassMin, body.ClassMax, body.IsPrivate,
	).Scan(&reachID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("create failed: %v", err))
		return
	}

	// Insert flow ranges.
	if fr := body.FlowRanges; fr != nil {
		type entry struct {
			label string
			r     *bandRange
		}
		for _, e := range []entry{{"low", fr.Low}, {"running", fr.Running}, {"high", fr.High}} {
			if e.r == nil {
				continue
			}
			_, _ = h.db.Exec(ctx, `
				INSERT INTO user_reach_flow_ranges (user_reach_id, label, min_value, max_value)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (user_reach_id, label) DO UPDATE
					SET min_value = EXCLUDED.min_value, max_value = EXCLUDED.max_value
			`, reachID, e.label, e.r.MinValue, e.r.MaxValue)
		}
	}

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
	type bandRange struct {
		MinValue *float64 `json:"min_value"`
		MaxValue *float64 `json:"max_value"`
	}
	var body struct {
		Name      string  `json:"name"`
		RiverName string  `json:"river_name"`
		GnisID    string  `json:"gnis_id"`
		PutIn     latLng  `json:"put_in"`
		TakeOut   latLng  `json:"take_out"`
		UpComID   string  `json:"up_comid"`
		DownComID string  `json:"down_comid"`
		Note      *string `json:"note"`
		FlowRanges *struct {
			Low     *bandRange `json:"low"`
			Running *bandRange `json:"running"`
			High    *bandRange `json:"high"`
		} `json:"flow_ranges"`
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
		rid := resolveOrCreateRiver(ctx, h.db, rn, body.GnisID)
		if rid != "" {
			riverID = &rid
		}
	}

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
			(owner_id, slug, name, river_id, river_name, put_in, take_out, up_comid, down_comid, note)
		VALUES
			($1, $2, $3, $4, $5,
			 ST_SetSRID(ST_MakePoint($6, $7), 4326)::geography,
			 ST_SetSRID(ST_MakePoint($8, $9), 4326)::geography,
			 NULLIF($10,''), NULLIF($11,''), $12)
		RETURNING id
	`, ownerID, slug, body.Name, riverID, finalRiverName,
		body.PutIn.Lng, body.PutIn.Lat,
		body.TakeOut.Lng, body.TakeOut.Lat,
		body.UpComID, body.DownComID, body.Note,
	).Scan(&reachID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("import failed: %v", err))
		return
	}

	if fr := body.FlowRanges; fr != nil {
		type entry struct {
			label string
			r     *bandRange
		}
		for _, e := range []entry{{"low", fr.Low}, {"running", fr.Running}, {"high", fr.High}} {
			if e.r == nil {
				continue
			}
			_, _ = h.db.Exec(ctx, `
				INSERT INTO user_reach_flow_ranges (user_reach_id, label, min_value, max_value)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (user_reach_id, label) DO UPDATE
					SET min_value = EXCLUDED.min_value, max_value = EXCLUDED.max_value
			`, reachID, e.label, e.r.MinValue, e.r.MaxValue)
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

	jsonResponse(w, http.StatusCreated, map[string]string{"id": reachID, "slug": slug})
}

// ── Update ───────────────────────────────────────────────────────────────────

// PATCH /api/v1/me/reaches/{slug}
func (h *UserReachHandler) Update(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")

	type latLng struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	}
	var body struct {
		Name      *string  `json:"name"`
		Note      *string  `json:"note"`
		RiverName *string  `json:"river_name"`
		GnisID    *string  `json:"gnis_id"`
		PutIn     *latLng  `json:"put_in"`
		TakeOut   *latLng  `json:"take_out"`
		UpComID   *string  `json:"up_comid"`
		DownComID *string  `json:"down_comid"`
		ClassMin  *float64 `json:"class_min"`
		ClassMax  *float64 `json:"class_max"`
		IsPrivate *bool    `json:"is_private"`
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

	ctx := r.Context()

	// Core fields update. Set last_modified_after_fork_at when run is a fork (V4).
	tag, err := h.db.Exec(ctx, `
		UPDATE user_reaches
		SET
			name       = COALESCE($3, name),
			note       = $4,
			river_name = CASE WHEN $5 THEN $6 ELSE river_name END,
			class_min  = CASE WHEN $7 THEN $8::numeric ELSE class_min END,
			class_max  = CASE WHEN $9 THEN $10::numeric ELSE class_max END,
			is_private = CASE WHEN $11 THEN $12 ELSE is_private END,
			updated_at = NOW(),
			last_modified_after_fork_at = CASE
				WHEN original_forked_at IS NOT NULL THEN NOW()
				ELSE last_modified_after_fork_at
			END
		WHERE owner_id = $1 AND slug = $2
	`, ownerID, slug, body.Name, body.Note, body.RiverName != nil, riverName,
		body.ClassMin != nil, body.ClassMin,
		body.ClassMax != nil, body.ClassMax,
		body.IsPrivate != nil, body.IsPrivate)
	if err != nil || tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}

	// Re-resolve river when river_name is being set, so state/basin/huc8 get populated.
	if riverName != nil {
		gnisID := ""
		if body.GnisID != nil {
			gnisID = strings.TrimSpace(*body.GnisID)
		}
		rid := resolveOrCreateRiver(ctx, h.db, *riverName, gnisID)
		if rid != "" {
			_, _ = h.db.Exec(ctx,
				`UPDATE user_reaches SET river_id = $3 WHERE owner_id = $1 AND slug = $2`,
				ownerID, slug, rid)
		}
	}

	// Geometry update — only when all four geometry fields provided.
	if body.PutIn != nil && body.TakeOut != nil && body.UpComID != nil && body.DownComID != nil {
		_, _ = h.db.Exec(ctx, `
			UPDATE user_reaches
			SET
				put_in     = ST_SetSRID(ST_MakePoint($3, $4), 4326)::geography,
				take_out   = ST_SetSRID(ST_MakePoint($5, $6), 4326)::geography,
				up_comid   = NULLIF($7, ''),
				down_comid = NULLIF($8, ''),
				updated_at = NOW()
			WHERE owner_id = $1 AND slug = $2
		`, ownerID, slug,
			body.PutIn.Lng, body.PutIn.Lat,
			body.TakeOut.Lng, body.TakeOut.Lat,
			*body.UpComID, *body.DownComID)
	}

}

// ── Delete ───────────────────────────────────────────────────────────────────

// DELETE /api/v1/me/reaches/{slug}
func (h *UserReachHandler) Delete(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")

	tag, err := h.db.Exec(r.Context(),
		`DELETE FROM user_reaches WHERE owner_id = $1 AND slug = $2`, ownerID, slug)
	if err != nil || tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
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

	rows, _ := h.db.Query(r.Context(), `
		SELECT label, min_value, max_value
		FROM user_reach_flow_ranges
		WHERE user_reach_id = $1
		ORDER BY min_value ASC NULLS FIRST
	`, reachID)
	items := make([]userReachFlowRange, 0)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var fr userReachFlowRange
			if rows.Scan(&fr.Label, &fr.MinValue, &fr.MaxValue) == nil {
				items = append(items, fr)
			}
		}
	}
	jsonResponse(w, http.StatusOK, items)
}

// PUT /api/v1/me/reaches/{slug}/flow-ranges
func (h *UserReachHandler) SetFlowRanges(w http.ResponseWriter, r *http.Request) {
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

	type bandRange struct {
		MinValue *float64 `json:"min_value"`
		MaxValue *float64 `json:"max_value"`
	}
	var body struct {
		Low     *bandRange `json:"low"`
		Running *bandRange `json:"running"`
		High    *bandRange `json:"high"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	ctx := r.Context()
	_, _ = h.db.Exec(ctx, `DELETE FROM user_reach_flow_ranges WHERE user_reach_id = $1`, reachID)
	type entry struct {
		label string
		r     *bandRange
	}
	for _, e := range []entry{{"low", body.Low}, {"running", body.Running}, {"high", body.High}} {
		if e.r == nil {
			continue
		}
		_, _ = h.db.Exec(ctx, `
			INSERT INTO user_reach_flow_ranges (user_reach_id, label, min_value, max_value)
			VALUES ($1, $2, $3, $4)
		`, reachID, e.label, e.r.MinValue, e.r.MaxValue)
	}

	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/v1/me/reaches/{slug}/gauge
func (h *UserReachHandler) ClearGauge(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	tag, err := h.db.Exec(r.Context(), `
		UPDATE user_reaches
		SET primary_gauge_id = NULL, custom_gauge_id = NULL, updated_at = NOW()
		WHERE owner_id = $1 AND slug = $2
	`, ownerID, slug)
	if err != nil || tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Centerline ───────────────────────────────────────────────────────────────

// POST /api/v1/me/reaches/{slug}/centerline
// Body: { "up_comid": "...", "down_comid": "...", "start_lat": ..., ... }
// Delegates to the NLDI handler's centerline fetch logic via internal fetch.
func (h *UserReachHandler) SetCenterline(w http.ResponseWriter, r *http.Request) {
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
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	tag, err := h.db.Exec(r.Context(), `
		UPDATE user_reaches
		SET centerline = NULL, up_comid = NULL, down_comid = NULL, updated_at = NOW()
		WHERE owner_id = $1 AND slug = $2
	`, ownerID, slug)
	if err != nil || tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "user reach not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Gauge ─────────────────────────────────────────────────────────────────────

// PUT /api/v1/me/reaches/{slug}/gauge
// Body (pick one):
//   { "gauge_id": "<uuid>" }
//   { "custom_gauge_id": "<uuid>" }
//   { "external_id": "...", "source": "...", "name": "...", "lat": 0.0, "lng": 0.0 }
func (h *UserReachHandler) SetGauge(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")

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

	var tag interface{ RowsAffected() int64 }
	var err error
	if body.GaugeID != nil {
		tag, err = h.db.Exec(r.Context(), `
			UPDATE user_reaches
			SET primary_gauge_id = $3::uuid, custom_gauge_id = NULL, updated_at = NOW()
			WHERE owner_id = $1 AND slug = $2
		`, ownerID, slug, body.GaugeID)
	} else {
		tag, err = h.db.Exec(r.Context(), `
			UPDATE user_reaches
			SET custom_gauge_id = $3::uuid, primary_gauge_id = NULL, updated_at = NOW()
			WHERE owner_id = $1 AND slug = $2
		`, ownerID, slug, body.CustomGaugeID)
	}
	if err != nil || tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "user reach not found or gauge invalid")
		return
	}
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
// Clones a public user_reach into a new user_reach owned by the caller.
func (h *UserReachHandler) ForkUserRun(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID := chi.URLParam(r, "runId")
	ctx := r.Context()

	type srcRow struct {
		ID           string
		Name         string
		OwnerID      string
		AuthorHandle *string
		RiverID      *string
		RiverName    *string
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
	err := h.db.QueryRow(ctx, `
		SELECT
			ur.id::text, ur.name, ur.owner_id::text,
			up.handle,
			ur.river_id::text, ur.river_name,
			ur.class_min, ur.class_max,
			ur.primary_gauge_id::text,
			ST_X(ur.put_in::geometry),  ST_Y(ur.put_in::geometry),
			ST_X(ur.take_out::geometry), ST_Y(ur.take_out::geometry),
			ST_AsGeoJSON(ur.centerline::geometry)
		FROM user_reaches ur
		LEFT JOIN user_profiles up ON up.owner_id = ur.owner_id
		WHERE ur.id = $1 AND ur.is_private = FALSE
	`, runID).Scan(
		&src.ID, &src.Name, &src.OwnerID, &src.AuthorHandle,
		&src.RiverID, &src.RiverName,
		&src.ClassMin, &src.ClassMax,
		&src.GaugeID,
		&src.PutInLng, &src.PutInLat,
		&src.TakeOutLng, &src.TakeOutLat,
		&src.Centerline,
	)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	baseSlug := kmlimport.Slugify(src.Name)
	slug := baseSlug
	for i := 2; i <= 20; i++ {
		var existing string
		if e := h.db.QueryRow(ctx,
			`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`, ownerID, slug,
		).Scan(&existing); e != nil {
			break
		}
		slug = fmt.Sprintf("%s-%d", baseSlug, i)
	}

	var newID string
	if err = h.db.QueryRow(ctx, `
		INSERT INTO user_reaches
			(owner_id, slug, name, river_id, river_name,
			 put_in, take_out,
			 class_min, class_max, primary_gauge_id,
			 forked_from_user_reach_id,
			 original_author_handle, original_author_owner_id, original_forked_at)
		VALUES
			($1, $2, $3, $4::uuid, $5,
			 ST_SetSRID(ST_MakePoint($6, $7), 4326)::geography,
			 ST_SetSRID(ST_MakePoint($8, $9), 4326)::geography,
			 $10, $11, $12::uuid,
			 $13::uuid,
			 $14, $15::uuid, NOW())
		RETURNING id
	`, ownerID, slug, src.Name, src.RiverID, src.RiverName,
		src.PutInLng, src.PutInLat,
		src.TakeOutLng, src.TakeOutLat,
		src.ClassMin, src.ClassMax, src.GaugeID,
		src.ID,
		src.AuthorHandle, src.OwnerID,
	).Scan(&newID); err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("fork failed: %v", err))
		return
	}

	if len(src.Centerline) > 0 {
		_, _ = h.db.Exec(ctx, `
			UPDATE user_reaches
			SET centerline = ST_GeomFromGeoJSON($2)::geography
			WHERE id = $1
		`, newID, string(src.Centerline))
	}

	jsonResponse(w, http.StatusCreated, map[string]string{"id": newID, "slug": slug, "gauge_id": func() string {
		if src.GaugeID != nil {
			return *src.GaugeID
		}
		return ""
	}()})
}

// ── Fork ─────────────────────────────────────────────────────────────────────

// POST /api/v1/me/reaches/fork-reach/{slug}
// Clones a curated reach (reaches table) into a new user_reaches row owned by
// the authenticated user. Copies name, geometry, centerline, class, and gauge.
func (h *UserReachHandler) ForkReach(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	sourceSlug := chi.URLParam(r, "slug")
	ctx := r.Context()

	// Load the source curated reach.
	type srcRow struct {
		ID          string
		Name        string
		CommonName  *string
		RiverID     *string
		RiverName   *string
		ClassMin    *float64
		ClassMax    *float64
		GaugeID     *string
		PutInLng    float64
		PutInLat    float64
		TakeOutLng  float64
		TakeOutLat  float64
		Centerline  []byte
	}
	var src srcRow
	err := h.db.QueryRow(ctx, `
		SELECT
			r.id,
			r.name,
			r.common_name,
			r.river_id::text,
			COALESCE(r.river_name, rv.name),
			r.class_min,
			r.class_max,
			r.primary_gauge_id::text,
			ST_X(r.start_point::geometry),
			ST_Y(r.start_point::geometry),
			ST_X(r.end_point::geometry),
			ST_Y(r.end_point::geometry),
			ST_AsGeoJSON(r.centerline::geometry)
		FROM reaches r
		LEFT JOIN rivers rv ON rv.id = r.river_id
		WHERE r.slug = $1
	`, sourceSlug).Scan(
		&src.ID, &src.Name, &src.CommonName,
		&src.RiverID, &src.RiverName,
		&src.ClassMin, &src.ClassMax,
		&src.GaugeID,
		&src.PutInLng, &src.PutInLat,
		&src.TakeOutLng, &src.TakeOutLat,
		&src.Centerline,
	)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "reach not found")
		return
	}

	// Use common_name as the fork's display name if available.
	forkName := src.Name
	if src.CommonName != nil && *src.CommonName != "" {
		forkName = *src.CommonName
	}

	// Unique slug for the fork (append owner suffix then counter).
	baseSlug := kmlimport.Slugify(forkName)
	slug := baseSlug
	for i := 2; i <= 20; i++ {
		var existing string
		if e := h.db.QueryRow(ctx,
			`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`, ownerID, slug,
		).Scan(&existing); e != nil {
			break
		}
		slug = fmt.Sprintf("%s-%d", baseSlug, i)
	}

	var newID string
	if err = h.db.QueryRow(ctx, `
		INSERT INTO user_reaches
			(owner_id, slug, name, river_id, river_name,
			 put_in, take_out,
			 class_min, class_max, primary_gauge_id,
			 forked_from_reach_id, original_forked_at)
		VALUES
			($1, $2, $3, $4::uuid, $5,
			 ST_SetSRID(ST_MakePoint($6, $7), 4326)::geography,
			 ST_SetSRID(ST_MakePoint($8, $9), 4326)::geography,
			 $10, $11, $12::uuid,
			 $13::uuid, NOW())
		RETURNING id
	`, ownerID, slug, forkName, src.RiverID, src.RiverName,
		src.PutInLng, src.PutInLat,
		src.TakeOutLng, src.TakeOutLat,
		src.ClassMin, src.ClassMax, src.GaugeID,
		src.ID,
	).Scan(&newID); err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("fork failed: %v", err))
		return
	}

	// Copy centerline if source has one.
	if len(src.Centerline) > 0 {
		_, _ = h.db.Exec(ctx, `
			UPDATE user_reaches
			SET centerline = ST_GeomFromGeoJSON($2)::geography
			WHERE id = $1
		`, newID, string(src.Centerline))
	}

	jsonResponse(w, http.StatusCreated, map[string]string{"id": newID, "slug": slug})
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
	rows, err := h.db.Query(r.Context(), `
		SELECT
			ur.id, ur.slug, ur.name, ur.river_name,
			rv.state_abbr, rv.basin AS basin_group,
			ST_X(ur.put_in::geometry)   AS put_in_lng,
			ST_Y(ur.put_in::geometry)   AS put_in_lat,
			ST_X(ur.take_out::geometry) AS take_out_lng,
			ST_Y(ur.take_out::geometry) AS take_out_lat,
			ur.note, ur.created_at,
			ur.class_min, ur.class_max,
			COALESCE(lr.value, cg.last_value_cfs)       AS current_cfs,
			COALESCE(lr.timestamp, cg.last_value_at)    AS last_reading_at,
			CASE
				WHEN COALESCE(lr.value, cg.last_value_cfs) IS NULL OR fr.label IS NULL THEN 'unknown'
				WHEN fr.label = 'running' THEN 'runnable'
				WHEN fr.label = 'low'     THEN 'caution'
				WHEN fr.label = 'high'    THEN 'flood'
				ELSE 'unknown'
			END AS flow_status,
			fr.label AS flow_band,
			ur.primary_gauge_id::text,
			ur.custom_gauge_id::text,
			cg.slug AS custom_gauge_slug,
			cg.name AS custom_gauge_name,
			ur.is_private,
			up.handle AS author_handle,
			(up.owner_id IS NULL) AS is_official
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
			SELECT label FROM user_reach_flow_ranges
			WHERE user_reach_id = ur.id
			  AND (min_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) >= min_value)
			  AND (max_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) <  max_value)
			ORDER BY min_value ASC NULLS FIRST
			LIMIT 1
		) fr ON TRUE
		WHERE ur.is_private = FALSE
		  AND ($1 = '%%' OR ur.name ILIKE $1 OR ur.river_name ILIKE $1)
		ORDER BY ur.created_at DESC, ur.id
		LIMIT $2 OFFSET $3
	`, search, limit+1, offset)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	items := make([]userReachSummary, 0)
	for rows.Next() {
		var s userReachSummary
		if err := rows.Scan(
			&s.ID, &s.Slug, &s.Name, &s.RiverName,
			&s.StateAbbr, &s.BasinGroup,
			&s.PutInLng, &s.PutInLat, &s.TakeOutLng, &s.TakeOutLat,
			&s.Note, &s.CreatedAt,
			&s.ClassMin, &s.ClassMax,
			&s.CurrentCFS, &s.LastReadAt,
			&s.FlowStatus, &s.FlowBand,
			&s.GaugeID, &s.CustomGaugeID, &s.CustomGaugeSlug, &s.CustomGaugeName,
			&s.IsPrivate, &s.AuthorHandle, &s.IsOfficial,
		); err == nil {
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
