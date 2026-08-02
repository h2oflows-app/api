package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/ai"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// jsonCache holds a pre-warmed snapshot of marshalled JSON.
// Used for both /reaches/map/all (GeoJSON) and /reaches (list JSON).
type jsonCache struct {
	mu       sync.RWMutex
	payload  []byte // marshalled JSON
	warmedAt time.Time
}

func (c *jsonCache) set(payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payload = payload
	c.warmedAt = time.Now()
}

func (c *jsonCache) get() ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.payload) == 0 {
		return nil, false
	}
	return c.payload, true
}

type statsCache struct {
	mu       sync.Mutex
	cachedAt time.Time
	reaches  int
	rivers   int
	reports  int
}

func (c *statsCache) get(ttl time.Duration) (reaches, rivers, reports int, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.cachedAt) < ttl {
		return c.reaches, c.rivers, c.reports, true
	}
	return 0, 0, 0, false
}

func (c *statsCache) set(reaches, rivers, reports int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reaches, c.rivers, c.reports = reaches, rivers, reports
	c.cachedAt = time.Now()
}

// gaugeFetcher is the narrow poller interface ReachHandler needs for
// on-demand fetching of stale primary gauges. Keeps this package free of a
// hard dependency on the poller implementation.
type gaugeFetcher interface {
	FetchNowIfStale(ctx context.Context, gaugeID string, maxAge time.Duration) bool
}

// ReachHandler handles reach-related HTTP routes.
type ReachHandler struct {
	db        *pgxpool.Pool
	asker     *ai.ReachAsker
	cache     *jsonCache   // /reaches/map/all GeoJSON
	listCache *jsonCache   // /reaches lightweight JSON
	poller    gaugeFetcher // nil = on-demand fetching disabled
	sc        statsCache
}

func NewReachHandler(db *pgxpool.Pool, asker *ai.ReachAsker) *ReachHandler {
	return &ReachHandler{db: db, asker: asker, cache: &jsonCache{}, listCache: &jsonCache{}}
}

// WithPoller wires a poller for on-demand gauge fetching. Optional — without
// it, reach detail pages still work but show whatever the last poll tick
// captured.
func (h *ReachHandler) WithPoller(p gaugeFetcher) *ReachHandler {
	h.poller = p
	return h
}

// WarmCache fetches all reach features (no bbox filter) and stores the result
// in the in-memory cache. Call once at server startup, then every poll cycle.
// Also warms the lightweight list cache used by GET /reaches.
func (h *ReachHandler) WarmCache(ctx context.Context) {
	// The shared cache represents the authenticated/default (unfiltered) view;
	// anonymous callers bypass it and run a live filtered query instead (see
	// MapAll/List) since a single process-wide cache can't vary per-caller.
	features, err := h.queryAllFeatures(ctx, false)
	if err != nil {
		log.Printf("reach cache: warm failed: %v", err)
		return
	}
	payload, err := json.Marshal(newFeatureCollection(features))
	if err != nil {
		log.Printf("reach cache: marshal failed: %v", err)
		return
	}
	h.cache.set(payload)
	log.Printf("reach cache: warmed %d features", len(features))

	// Warm lightweight list cache (no geometry).
	items, err := h.queryAllListItems(ctx, false)
	if err != nil {
		log.Printf("reach list cache: warm failed: %v", err)
		return
	}
	listPayload, err := json.Marshal(items)
	if err != nil {
		log.Printf("reach list cache: marshal failed: %v", err)
		return
	}
	h.listCache.set(listPayload)
	log.Printf("reach list cache: warmed %d items", len(items))
}

// StartCacheRefresh launches a background goroutine that re-warms the cache
// on the given interval (typically the same as the gauge poll interval).
func (h *ReachHandler) StartCacheRefresh(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.WarmCache(ctx)
			}
		}
	}()
}

// Map handles GET /api/v1/reaches/map
//
// Returns a GeoJSON FeatureCollection of reach centerlines with their current
// flow status. MapLibre uses this to color river lines on the dashboard map.
//
// Query params:
//
//	bbox=west,south,east,north  (required — viewport bounds)
//
// Flow status values and their map colors:
//
//	runnable  → green  (flow is within fun/optimal range)
//	caution   → yellow (minimum or pushy — paddle at your level)
//	low       → red    (below minimum, not recommended)
//	flood     → red    (high water or flood stage, dangerous)
//	unknown   → grey   (no current reading or no flow ranges defined)
//
// Reaches with no centerline geometry are omitted — they appear as gauge
// markers via the gauge search endpoint instead.
// mapBaseSQL is the CTE + SELECT + FROM + JOINs shared by the Map handler.
// The WHERE clause is appended dynamically based on query parameters.
// runs-unify 5a: reads from user_reaches (sentinel-owned twins); reaches retired.
const mapBaseSQL = `
	WITH latest_reading AS (
		SELECT DISTINCT ON (gauge_id)
			gauge_id, value
		FROM gauge_readings
		WHERE timestamp > NOW() - INTERVAL '48 hours'
		ORDER BY gauge_id, timestamp DESC
	)
	SELECT
		r.id, r.name, r.slug,
		r.river_name, r.long_name AS common_name, NULL::text AS put_in_name, NULL::text AS take_out_name,
		r.class_min,
		COALESCE(
			(SELECT MAX(class_rating) FROM rapids WHERE user_reach_id = r.id AND class_rating IS NOT NULL),
			r.class_max
		) AS class_max,
		NULL::text AS character,
		ROUND((ST_Length(r.centerline::geography) / 1609.34)::numeric, 2)::float8 AS length_mi,
		ST_AsGeoJSON(r.centerline::geometry)::json AS centerline,
		ST_X(r.put_in::geometry)    AS put_in_lng,
		ST_Y(r.put_in::geometry)    AS put_in_lat,
		ST_X(r.take_out::geometry)  AS take_out_lng,
		ST_Y(r.take_out::geometry)  AS take_out_lat,
		lr.value                         AS current_cfs,
		g.last_reading_at,
		fr.band_label                    AS flow_label,
		g.id                             AS gauge_id,
		g.reach_relationship,
		g.featured                       AS gauge_trusted,
		g.gauge_notes,
		g.info_links,
		CASE
			WHEN fr.band_color IS NULL        THEN 'unknown'
			WHEN fr.band_color LIKE 'red%'    THEN 'caution'
			WHEN fr.band_color LIKE 'blue%'   THEN 'flood'
			ELSE                                   'runnable'
		END AS flow_status
	FROM user_reaches r
	LEFT JOIN gauges g ON g.id = r.primary_gauge_id
	LEFT JOIN latest_reading lr ON lr.gauge_id = g.id
	LEFT JOIN LATERAL (
		SELECT label, color FROM user_reach_flow_ranges
		WHERE user_reach_id = r.id
		  AND lr.value >= value
		ORDER BY value DESC
		LIMIT 1
	) thresh ON TRUE,
	LATERAL (
		SELECT
			COALESCE(thresh.label, CASE WHEN lr.value IS NOT NULL THEN r.base_label END) AS band_label,
			COALESCE(thresh.color, CASE WHEN lr.value IS NOT NULL THEN r.base_color END) AS band_color
	) fr
	WHERE r.owner_id = '00000000-0000-0000-0000-000000000001'
	  AND r.deleted_at IS NULL
`

// Map handles GET /api/v1/reaches/map
//
// Accepts either (or both):
//
//	bbox=west,south,east,north  – spatial viewport filter
//	river_name=Arkansas River   – same-river filter (case-insensitive)
//
// At least one parameter is required. Providing river_name without bbox
// returns all reaches on that river regardless of viewport, which is the
// preferred mode for the reach detail page sidebar.
func (h *ReachHandler) Map(w http.ResponseWriter, r *http.Request) {
	riverName := r.URL.Query().Get("river_name")
	bboxRaw := r.URL.Query().Get("bbox")

	if riverName == "" && bboxRaw == "" {
		errorResponse(w, http.StatusBadRequest, "bbox or river_name is required")
		return
	}

	// mapBaseSQL already includes WHERE owner_id/deleted_at; append extra filters with AND.
	extraWhere := "AND r.centerline IS NOT NULL" + anonPublicOnMapFilter(r, "r.owner_id")
	args := make([]any, 0, 5)
	n := 1

	if bboxRaw != "" {
		bbox, err := parseBBoxParam(r)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		extraWhere += fmt.Sprintf(
			" AND ST_Intersects(r.centerline::geometry, ST_MakeEnvelope($%d, $%d, $%d, $%d, 4326))",
			n, n+1, n+2, n+3,
		)
		args = append(args, bbox.West, bbox.South, bbox.East, bbox.North)
		n += 4
	}

	if riverName != "" {
		extraWhere += fmt.Sprintf(" AND r.river_name ILIKE $%d", n)
		args = append(args, riverName)
	}

	rows, err := h.db.Query(r.Context(), mapBaseSQL+extraWhere, args...)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	features := make([]Feature, 0)
	for rows.Next() {
		var (
			id                string
			name              string
			slug              string
			riverName         *string
			commonName        *string
			putInName         *string
			takeOutName       *string
			classMin          *float64
			classMax          *float64
			character         *string
			lengthMi          *float64
			centerlineJSON    []byte
			putInLng          *float64
			putInLat          *float64
			takeOutLng        *float64
			takeOutLat        *float64
			currentCFS        *float64
			lastReadingAt     *time.Time
			flowLabel         *string
			gaugeID           *string
			reachRelationship *string
			gaugeTrusted      *bool
			gaugeNotes        *string
			infoLinks         []byte
			flowStatus        string
		)
		if err := rows.Scan(
			&id, &name, &slug, &riverName, &commonName, &putInName, &takeOutName,
			&classMin, &classMax, &character, &lengthMi,
			&centerlineJSON,
			&putInLng, &putInLat, &takeOutLng, &takeOutLat,
			&currentCFS, &lastReadingAt, &flowLabel, &gaugeID,
			&reachRelationship, &gaugeTrusted, &gaugeNotes, &infoLinks,
			&flowStatus,
		); err != nil {
			continue
		}

		// put_in and take_out as [lng, lat] pairs for frontend marker rendering on hover.
		var putIn, takeOut *[2]float64
		if putInLng != nil && putInLat != nil {
			putIn = &[2]float64{*putInLng, *putInLat}
		}
		if takeOutLng != nil && takeOutLat != nil {
			takeOut = &[2]float64{*takeOutLng, *takeOutLat}
		}

		features = append(features, Feature{
			Type:     "Feature",
			Geometry: rawGeometry(centerlineJSON),
			Properties: map[string]any{
				"id":                 id,
				"name":               name,
				"slug":               slug,
				"river_name":         riverName,
				"common_name":        commonName,
				"put_in_name":        putInName,
				"take_out_name":      takeOutName,
				"class_min":          classMin,
				"class_max":          classMax,
				"character":          character,
				"length_mi":          lengthMi,
				"put_in":             putIn,
				"take_out":           takeOut,
				"current_cfs":        currentCFS,
				"last_reading_at":    lastReadingAt,
				"flow_label":         flowLabel,
				"gauge_id":           gaugeID,
				"flow_status":        flowStatus,
				"flow_color":         flowColor(flowStatus),
				"reach_relationship": reachRelationship,
				"gauge_trusted":      gaugeTrusted,
				"gauge_notes":        gaugeNotes,
				"info_links":         rawJSON(infoLinks),
			},
		})
	}
	if err := rows.Err(); err != nil {
		errorResponse(w, http.StatusInternalServerError, "scan failed")
		return
	}

	jsonResponse(w, http.StatusOK, newFeatureCollection(features))
}

// MapAll handles GET /api/v1/reaches/map/all
//
// Returns the full reach GeoJSON dataset (no bbox filter). Served from an
// in-memory cache warmed at startup and refreshed every poll cycle. The
// frontend loads this once and filters client-side on every viewport change,
// eliminating per-pan/zoom round-trips.
func (h *ReachHandler) MapAll(w http.ResponseWriter, r *http.Request) {
	// Anonymous callers bypass the shared cache entirely — it holds the
	// unfiltered (authenticated) view and can't vary per-caller — and run a
	// live, publicly-scoped query instead (#314).
	anon := isAnonymousCaller(r)
	if !anon {
		if payload, ok := h.cache.get(); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=60")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
	}
	// Cache cold (first request before WarmCache finishes), or anonymous — query directly.
	features, err := h.queryAllFeatures(r.Context(), anon)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	jsonResponse(w, http.StatusOK, newFeatureCollection(features))
}

// queryAllFeatures runs the reach-map query without a bbox filter and returns
// the raw Feature slice. Shared by MapAll and WarmCache. When anonOnly is
// true, results are additionally restricted to special-user accounts opted
// into public_on_map (#314).
func (h *ReachHandler) queryAllFeatures(ctx context.Context, anonOnly bool) ([]Feature, error) {
	extra := ""
	if anonOnly {
		extra = specialPublicOnMapClause("r.owner_id")
	}
	rows, err := h.db.Query(ctx, `
		WITH latest_reading AS (
			SELECT DISTINCT ON (gauge_id)
				gauge_id, value
			FROM gauge_readings
			WHERE timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY gauge_id, timestamp DESC
		)
		SELECT
			r.id, r.name, r.slug,
			COALESCE(r.river_name, rv.name) AS river_name,
			r.long_name AS common_name, NULL::text AS put_in_name, NULL::text AS take_out_name,
			r.class_min,
			COALESCE(
				(SELECT MAX(class_rating) FROM rapids WHERE user_reach_id = r.id AND class_rating IS NOT NULL),
				r.class_max
			) AS class_max,
			NULL::text AS character,
			ROUND((ST_Length(r.centerline::geography) / 1609.34)::numeric, 2)::float8 AS length_mi,
			ST_AsGeoJSON(r.centerline::geometry)::json AS centerline,
			ST_X(r.put_in::geometry)        AS put_in_lng,
			ST_Y(r.put_in::geometry)        AS put_in_lat,
			ST_X(r.take_out::geometry)      AS take_out_lng,
			ST_Y(r.take_out::geometry)      AS take_out_lat,
			lr.value                        AS current_cfs,
			g.last_reading_at,
			fr.band_label                   AS flow_label,
			g.id                            AS gauge_id,
			g.reach_relationship,
			g.featured                      AS gauge_trusted,
			g.gauge_notes,
			g.info_links,
			CASE
				WHEN fr.band_color IS NULL        THEN 'unknown'
				WHEN fr.band_color LIKE 'red%'    THEN 'caution'
				WHEN fr.band_color LIKE 'blue%'   THEN 'flood'
				ELSE                                   'runnable'
			END AS flow_status
		-- runs-unify 5a: curated map served from the h2oflows sentinel-owned twins
		-- in user_reaches (the legacy reaches table is retired in Phase 5).
		FROM user_reaches r
		LEFT JOIN rivers rv ON rv.id = r.river_id
		LEFT JOIN gauges g ON g.id = r.primary_gauge_id
		LEFT JOIN latest_reading lr ON lr.gauge_id = g.id
		LEFT JOIN LATERAL (
			SELECT label, color FROM user_reach_flow_ranges
			WHERE user_reach_id = r.id
			  AND lr.value >= value
			ORDER BY value DESC
			LIMIT 1
		) thresh ON TRUE,
		LATERAL (
			SELECT
				COALESCE(thresh.label, CASE WHEN lr.value IS NOT NULL THEN r.base_label END) AS band_label,
				COALESCE(thresh.color, CASE WHEN lr.value IS NOT NULL THEN r.base_color END) AS band_color
		) fr
		WHERE r.centerline IS NOT NULL
		  AND r.owner_id = '00000000-0000-0000-0000-000000000001'
		  AND r.deleted_at IS NULL
	`+extra)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	features := make([]Feature, 0)
	for rows.Next() {
		var (
			id, name, slug         string
			riverName, commonName  *string
			putInName, takeOutName *string
			classMin, classMax     *float64
			character              *string
			lengthMi               *float64
			centerlineJSON         []byte
			putInLng, putInLat     *float64
			takeOutLng, takeOutLat *float64
			currentCFS             *float64
			lastReadingAt          *time.Time
			flowLabel, gaugeID     *string
			reachRelationship      *string
			gaugeTrusted           *bool
			gaugeNotes             *string
			infoLinks              []byte
			flowStatus             string
		)
		if err := rows.Scan(
			&id, &name, &slug,
			&riverName, &commonName, &putInName, &takeOutName,
			&classMin, &classMax, &character, &lengthMi,
			&centerlineJSON,
			&putInLng, &putInLat, &takeOutLng, &takeOutLat,
			&currentCFS, &lastReadingAt, &flowLabel, &gaugeID,
			&reachRelationship, &gaugeTrusted, &gaugeNotes, &infoLinks,
			&flowStatus,
		); err != nil {
			continue
		}
		var putIn, takeOut *[2]float64
		if putInLng != nil && putInLat != nil {
			putIn = &[2]float64{*putInLng, *putInLat}
		}
		if takeOutLng != nil && takeOutLat != nil {
			takeOut = &[2]float64{*takeOutLng, *takeOutLat}
		}
		features = append(features, Feature{
			Type:     "Feature",
			Geometry: rawGeometry(centerlineJSON),
			Properties: map[string]any{
				"id": id, "name": name, "slug": slug,
				"river_name": riverName, "common_name": commonName,
				"put_in_name": putInName, "take_out_name": takeOutName,
				"class_min": classMin, "class_max": classMax,
				"character": character, "length_mi": lengthMi,
				"put_in": putIn, "take_out": takeOut,
				"current_cfs": currentCFS, "last_reading_at": lastReadingAt,
				"flow_label": flowLabel, "gauge_id": gaugeID,
				"flow_status": flowStatus, "flow_color": flowColor(flowStatus),
				"reach_relationship": reachRelationship,
				"gauge_trusted":      gaugeTrusted, "gauge_notes": gaugeNotes,
				"info_links": rawJSON(infoLinks),
			},
		})
	}
	return features, rows.Err()
}

// List handles GET /api/v1/reaches
//
// Returns a lightweight JSON array of all reaches with current flow data,
// grouped by the frontend into basin → river → reach. No geometry is included.
// Served from an in-memory cache warmed at startup and refreshed every poll cycle.
func (h *ReachHandler) List(w http.ResponseWriter, r *http.Request) {
	// Anonymous callers bypass the shared cache — see MapAll (#314).
	anon := isAnonymousCaller(r)
	if !anon {
		if payload, ok := h.listCache.get(); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=60")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
	}
	// Cache cold, or anonymous — query directly.
	items, err := h.queryAllListItems(r.Context(), anon)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	jsonResponse(w, http.StatusOK, items)
}

// reachListItem is the lightweight struct returned by GET /reaches.
type reachListItem struct {
	Slug            string   `json:"slug"`
	Name            string   `json:"name"`
	RiverName       *string  `json:"river_name"`
	CommonName      *string  `json:"common_name"`
	PutInName       *string  `json:"put_in_name"`
	TakeOutName     *string  `json:"take_out_name"`
	Basin           *string  `json:"basin"`
	StateAbbr       *string  `json:"state_abbr"`
	ClassMin        *float64 `json:"class_min"`
	ClassMax        *float64 `json:"class_max"`
	CurrentCFS      *float64 `json:"current_cfs"`
	FlowLabel       *string  `json:"flow_label"`
	FlowStatus      string   `json:"flow_status"`
	GaugeID         *string  `json:"gauge_id"`
	GaugeExternalID *string  `json:"gauge_external_id"`
	GaugeSource     *string  `json:"gauge_source"`
	GaugeName       *string  `json:"gauge_name"`
	GaugeStatus     *string  `json:"gauge_status"`
	IsSpecial       bool     `json:"is_special"`
	AuthorHandle    *string  `json:"author_handle"`
}

// queryAllListItems returns a lightweight slice of all reaches with current
// flow data. No geometry, no gauge notes/links — just what the browse page
// needs. When anonOnly is true, results are additionally restricted to
// special-user accounts opted into public_on_map (#314).
func (h *ReachHandler) queryAllListItems(ctx context.Context, anonOnly bool) ([]reachListItem, error) {
	extra := ""
	if anonOnly {
		extra = specialPublicOnMapClause("r.owner_id")
	}
	rows, err := h.db.Query(ctx, `
		WITH latest_reading AS (
			SELECT DISTINCT ON (gauge_id)
				gauge_id, value
			FROM gauge_readings
			WHERE timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY gauge_id, timestamp DESC
		)
		SELECT
			r.slug,
			r.name,
			COALESCE(r.river_name, rv.name) AS river_name,
			r.long_name AS common_name,
			NULL::text  AS put_in_name,
			NULL::text  AS take_out_name,
			rv.basin,
			COALESCE(r.state_abbr, rv.state_abbr) AS state_abbr,
			r.class_min,
			COALESCE(
				(SELECT MAX(class_rating) FROM rapids WHERE user_reach_id = r.id AND class_rating IS NOT NULL),
				r.class_max
			) AS class_max,
			lr.value           AS current_cfs,
			fr.band_label      AS flow_label,
			g.id               AS gauge_id,
			g.external_id      AS gauge_external_id,
			g.source       AS gauge_source,
			g.name         AS gauge_name,
			g.status       AS gauge_status,
			CASE
				WHEN fr.band_color IS NULL        THEN 'unknown'
				WHEN fr.band_color LIKE 'red%'    THEN 'caution'
				WHEN fr.band_color LIKE 'blue%'   THEN 'flood'
				ELSE                                   'runnable'
			END AS flow_status,
			COALESCE(up.is_special, false) AS is_special,
			up.handle AS author_handle
		FROM user_reaches r
		LEFT JOIN rivers rv ON rv.id = r.river_id
		LEFT JOIN gauges g ON g.id = r.primary_gauge_id
		LEFT JOIN user_profiles up ON up.owner_id = r.owner_id
		LEFT JOIN latest_reading lr ON lr.gauge_id = g.id
		LEFT JOIN LATERAL (
			SELECT label, color FROM user_reach_flow_ranges
			WHERE user_reach_id = r.id
			  AND lr.value >= value
			ORDER BY value DESC
			LIMIT 1
		) thresh ON TRUE,
		LATERAL (
			SELECT
				COALESCE(thresh.label, CASE WHEN lr.value IS NOT NULL THEN r.base_label END) AS band_label,
				COALESCE(thresh.color, CASE WHEN lr.value IS NOT NULL THEN r.base_color END) AS band_color
		) fr
		WHERE r.owner_id = '00000000-0000-0000-0000-000000000001'
		  AND r.deleted_at IS NULL
	`+extra+`
		ORDER BY rv.basin NULLS LAST,
		         r.river_name NULLS LAST,
		         g.elevation_ft DESC NULLS LAST,
		         ST_X(r.put_in::geometry) ASC NULLS LAST
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]reachListItem, 0)
	for rows.Next() {
		var item reachListItem
		if err := rows.Scan(
			&item.Slug, &item.Name,
			&item.RiverName, &item.CommonName, &item.PutInName, &item.TakeOutName,
			&item.Basin, &item.StateAbbr,
			&item.ClassMin, &item.ClassMax,
			&item.CurrentCFS, &item.FlowLabel, &item.GaugeID,
			&item.GaugeExternalID, &item.GaugeSource, &item.GaugeName,
			&item.GaugeStatus, &item.FlowStatus,
			&item.IsSpecial, &item.AuthorHandle,
		); err != nil {
			continue
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ---- Response types ---------------------------------------------------------

type reachDetail struct {
	ID                    string         `json:"id"`
	Slug                  string         `json:"slug"`
	Name                  string         `json:"name"`
	RiverName             *string        `json:"river_name"`
	CommonName            *string        `json:"common_name"`
	PutInName             *string        `json:"put_in_name"`
	TakeOutName           *string        `json:"take_out_name"`
	PutInLat              *float64       `json:"put_in_lat"`
	PutInLng              *float64       `json:"put_in_lng"`
	TakeOutLat            *float64       `json:"take_out_lat"`
	TakeOutLng            *float64       `json:"take_out_lng"`
	Region                *string        `json:"region"`
	ClassMin              *float64       `json:"class_min"`
	ClassMax              *float64       `json:"class_max"`
	ClassHardest          *float64       `json:"class_hardest"`
	Character             *string        `json:"character"`
	LengthMi              *float64       `json:"length_mi"`
	GradientFPM           *float64       `json:"gradient_fpm"`
	Description           *string        `json:"description"`
	DescriptionSource     *string        `json:"description_source"`
	DescriptionConfidence *int           `json:"description_ai_confidence"`
	DescriptionVerified   bool           `json:"description_verified"`
	AWReachID             *string        `json:"aw_reach_id"`
	WatershedName         *string        `json:"watershed_name"`
	PermitRequired        bool           `json:"permit_required"`
	MultiDayDays          int            `json:"multi_day_days"`
	Centerline            rawGeometry    `json:"centerline"`
	CenterlineSource      string         `json:"centerline_source"`
	Gauge                 gaugeSnippet   `json:"gauge"`
	Gauges                []gaugeSnippet `json:"gauges"`
	Rapids                []rapidRow     `json:"rapids"`
	Access                []accessRow    `json:"access"`
	Related               []relatedReach `json:"related"`
	IsSpecial             bool           `json:"is_special"`
	AuthorHandle          *string        `json:"author_handle"`
}

type gaugeSnippet struct {
	ID            *string    `json:"id"`
	ExternalID    *string    `json:"external_id"`
	Source        *string    `json:"source"`
	Name          *string    `json:"name"`
	Featured      *bool      `json:"featured"`
	Relationship  *string    `json:"reach_relationship"`
	CurrentCFS    *float64   `json:"current_cfs"`
	LastReadingAt *time.Time `json:"last_reading_at"`
	FlowStatus    string     `json:"flow_status"`
	FlowBandLabel *string    `json:"flow_band_label"`
	Lng           *float64   `json:"lng"`
	Lat           *float64   `json:"lat"`
}

type rapidRow struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	RiverMile            *float64 `json:"river_mile"`
	ClassRating          *float64 `json:"class_rating"`
	ClassAtLow           *float64 `json:"class_at_low"`
	ClassAtHigh          *float64 `json:"class_at_high"`
	Description          *string  `json:"description"`
	PortageDescription   *string  `json:"portage_description"`
	IsPortageRecommended bool     `json:"is_portage_recommended"`
	IsSurfWave           bool     `json:"is_surf_wave"`
	IsPermanentHazard    bool     `json:"is_permanent_hazard"`
	HazardType           *string  `json:"hazard_type"`
	DataSource           string   `json:"data_source"`
	AIConfidence         *int     `json:"ai_confidence"`
	Verified             bool     `json:"verified"`
	Lng                  *float64 `json:"lng"`
	Lat                  *float64 `json:"lat"`
	RiverOrder           *float64 `json:"river_order"`
}

type accessRow struct {
	ID                 string        `json:"id"`
	AccessType         string        `json:"access_type"`
	Name               *string       `json:"name"`
	Directions         *string       `json:"directions"`
	RoadType           *string       `json:"road_type"`
	EntryStyle         *string       `json:"entry_style"`
	ApproachDistMi     *float64      `json:"approach_dist_mi"`
	ApproachNotes      *string       `json:"approach_notes"`
	ParkingFee         *float64      `json:"parking_fee"`
	PermitRequired     bool          `json:"permit_required"`
	PermitInfo         *string       `json:"permit_info"`
	PermitURL          *string       `json:"permit_url"`
	SeasonalCloseStart *string       `json:"seasonal_close_start"`
	SeasonalCloseEnd   *string       `json:"seasonal_close_end"`
	Notes              *string       `json:"notes"`
	WaterLng           *float64      `json:"water_lng"`
	WaterLat           *float64      `json:"water_lat"`
	ParkingLng         *float64      `json:"parking_lng"`
	ParkingLat         *float64      `json:"parking_lat"`
	HikeToWaterMin     *int          `json:"hike_to_water_min"`
	DataSource         string        `json:"data_source"`
	AIConfidence       *int          `json:"ai_confidence"`
	Verified           bool          `json:"verified"`
	Waypoints          []waypointRow `json:"waypoints"`
	RiverOrder         *float64      `json:"river_order"`
}

type relatedReach struct {
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Relationship string `json:"relationship"` // upstream | downstream | tributary | continuation
}

type waypointRow struct {
	Sequence    int      `json:"sequence"`
	Label       string   `json:"label"`
	Description *string  `json:"description"`
	Lng         *float64 `json:"lng"`
	Lat         *float64 `json:"lat"`
	Verified    bool     `json:"verified"`
}

// GetFlowRanges handles GET /api/v1/reaches/{slug}/flow-ranges
//
// Returns flow bands for a reach in the new flexible shape (V15).
// runs-unify 5a: resolves slug via user_reaches (sentinel-owned twins).
func (h *ReachHandler) GetFlowRanges(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	var reachID string
	var fb FlowBands
	err := h.db.QueryRow(r.Context(),
		`SELECT id, base_label, base_color FROM user_reaches
		 WHERE slug = $1 AND owner_id = '00000000-0000-0000-0000-000000000001'
		   AND deleted_at IS NULL`, slug,
	).Scan(&reachID, &fb.BaseLabel, &fb.BaseColor)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "reach not found")
		return
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT fr.value, fr.label, fr.color
		FROM user_reach_flow_ranges fr
		WHERE fr.user_reach_id = $1
		ORDER BY fr.value ASC
	`, reachID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	fb.Thresholds = make([]FlowBandThreshold, 0)
	for rows.Next() {
		var t FlowBandThreshold
		if rows.Scan(&t.Value, &t.Label, &t.Color) == nil {
			fb.Thresholds = append(fb.Thresholds, t)
		}
	}

	jsonResponse(w, http.StatusOK, fb)
}

// --- Helpers ----------------------------------------------------------------

// flowColor maps a flow status to its hex color for MapLibre line-color expressions.
// These values should stay in sync with the frontend's style constants.
func flowColor(status string) string {
	switch status {
	case "runnable":
		return "#22c55e" // emerald-500 — go paddle
	case "caution":
		return "#eab308" // yellow-500 — minimum/pushy
	case "low":
		return "#ef4444" // red-500 — too low, not runnable
	case "flood":
		return "#3b82f6" // blue-500 — too much water
	default:
		return "#6b7280" // gray-500 — unknown/no data
	}
}

// rawGeometry wraps a pre-serialized GeoJSON geometry blob so it passes through
// the JSON encoder without double-encoding. The centerline comes out of PostGIS
// as a JSON string via ST_AsGeoJSON; we want it embedded as an object, not a string.
type rawGeometry []byte

func (g rawGeometry) MarshalJSON() ([]byte, error) {
	if len(g) == 0 {
		return []byte("null"), nil
	}
	return g, nil
}

// rawJSON passes a JSONB []byte from Postgres straight through the JSON encoder.
// Used for info_links so the array of {label,url} objects isn't double-encoded.
type rawJSON []byte

func (j rawJSON) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("[]"), nil
	}
	return j, nil
}

// parseBBoxParam parses bbox=west,south,east,north from the query string.
// GlobalAsk handles POST /api/v1/ask
//
// Accepts {"question": "..."}, identifies the reach from the question text,
// then answers using that reach's embedded content.
// Returns the answer plus the matched reach slug and name.
func (h *ReachHandler) GlobalAsk(w http.ResponseWriter, r *http.Request) {
	if h.asker == nil {
		errorResponse(w, http.StatusServiceUnavailable, "river assistant not configured")
		return
	}

	// web#354 A1 major fix (findings[1]): POST /ask is registered under
	// auth.Optional (main.go) — fully anon-reachable, no ownerID gate. authed
	// is threaded down through asker.Answer -> loadReachReports so an
	// anonymous caller never receives the calendar_runs notes corpus (§1:
	// "Anon: sees no calendar items"); the curated reach/gauge context this
	// handler loads below is unaffected either way.
	_, authed := auth.UserIDFromContext(r.Context())

	var body struct {
		Question string `json:"question"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Question) == "" {
		errorResponse(w, http.StatusBadRequest, "question is required")
		return
	}

	// Load reaches with rapids and access names for richer identification.
	rows, err := h.db.Query(r.Context(), `
		SELECT r.slug, r.name, COALESCE(r.long_name,''), ''::text AS region,
		       COALESCE(array_agg(DISTINCT rap.name) FILTER (WHERE rap.name IS NOT NULL), '{}') AS rapids,
		       COALESCE(array_agg(DISTINCT ra.name)  FILTER (WHERE ra.name  IS NOT NULL), '{}') AS access_names
		-- runs-unify 5c: identify against the h2oflows sentinel twins in user_reaches.
		FROM user_reaches r
		LEFT JOIN rapids rap ON rap.user_reach_id = r.id
		LEFT JOIN reach_access ra ON ra.user_reach_id = r.id
		WHERE r.owner_id = '00000000-0000-0000-0000-000000000001' AND r.deleted_at IS NULL
		GROUP BY r.id, r.slug, r.name, r.long_name
		ORDER BY r.name
	`)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "could not load reaches")
		return
	}
	defer rows.Close()
	var reachContexts []ai.IdentifyReachContext
	for rows.Next() {
		var rc ai.IdentifyReachContext
		if err := rows.Scan(&rc.Slug, &rc.Name, &rc.CommonName, &rc.Region, &rc.Rapids, &rc.Access); err != nil {
			errorResponse(w, http.StatusInternalServerError, "could not scan reaches")
			return
		}
		reachContexts = append(reachContexts, rc)
	}

	askCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Step 1 — identify reach(es).
	identified, err := h.asker.IdentifyReach(askCtx, body.Question, reachContexts)
	if err != nil {
		log.Printf("global ask identify: %v", err)
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("could not identify reach: %v", err))
		return
	}

	if len(identified.Slugs) == 0 {
		jsonResponse(w, http.StatusOK, map[string]any{
			"results": []any{},
			"answer":  "I couldn't identify a specific reach from your question. Try asking about a named run — for example, \"What flows are good for Browns Canyon?\"",
		})
		return
	}

	// Step 2 — answer each matched reach in parallel (up to 3).
	type reachResult struct {
		Answer         string   `json:"answer"`
		ReachSlug      string   `json:"reach_slug"`
		ReachName      string   `json:"reach_name"`
		PutInLat       *float64 `json:"put_in_lat"`
		PutInLng       *float64 `json:"put_in_lng"`
		PrimaryGaugeID *string  `json:"primary_gauge_id"`
	}
	results := make([]reachResult, len(identified.Slugs))
	var wg sync.WaitGroup
	for i, slug := range identified.Slugs {
		wg.Add(1)
		go func(i int, slug string) {
			defer wg.Done()
			var reachID, reachName string
			var putInLat, putInLng *float64
			var primaryGaugeID *string
			if err := h.db.QueryRow(askCtx, `
				SELECT id, name,
				       ST_Y(put_in::geometry) AS put_in_lat,
				       ST_X(put_in::geometry) AS put_in_lng,
				       primary_gauge_id
				FROM user_reaches WHERE slug = $1
				  AND owner_id = '00000000-0000-0000-0000-000000000001' AND deleted_at IS NULL
			`, slug).Scan(&reachID, &reachName, &putInLat, &putInLng, &primaryGaugeID); err != nil {
				log.Printf("global ask: reach not found for slug %q: %v", slug, err)
				return
			}
			answer, err := h.asker.Answer(askCtx, reachID, reachName, identified.Question, authed)
			if err != nil {
				log.Printf("global ask answer [%s]: %v", slug, err)
				return
			}
			results[i] = reachResult{
				Answer:         answer,
				ReachSlug:      slug,
				ReachName:      reachName,
				PutInLat:       putInLat,
				PutInLng:       putInLng,
				PrimaryGaugeID: primaryGaugeID,
			}
		}(i, slug)
	}
	wg.Wait()

	// Filter slots that failed (empty Answer).
	var finalResults []reachResult
	for _, rr := range results {
		if rr.Answer != "" {
			finalResults = append(finalResults, rr)
		}
	}
	if len(finalResults) == 0 {
		errorResponse(w, http.StatusInternalServerError, "could not generate answer")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"results": finalResults})
}

// Stats handles GET /api/v1/stats — cached 60 s. The underlying counts are
// cached process-wide regardless of caller (cheap, non-sensitive aggregates
// on their own), but the "reports" figure — derived from calendar_runs, the
// calendar domain — is only ever included in the RESPONSE for an
// authenticated caller (web#354 A1 major fix, findings[2]/§1: "Anon: sees no
// calendar items"). This route is registered under auth.Optional
// (main.go), so anon reaches it directly.
func (h *ReachHandler) Stats(w http.ResponseWriter, r *http.Request) {
	_, authed := auth.UserIDFromContext(r.Context())

	const ttl = 60 * time.Second
	if reaches, rivers, reports, ok := h.sc.get(ttl); ok {
		jsonResponse(w, http.StatusOK, buildStatsResponse(reaches, rivers, reports, authed))
		return
	}

	var reaches, rivers, reports int
	// runs-unify 5a: count sentinel-owned twins in user_reaches (the curated runs).
	h.db.QueryRow(r.Context(), `SELECT COUNT(*) FROM user_reaches WHERE owner_id = '00000000-0000-0000-0000-000000000001' AND deleted_at IS NULL`).Scan(&reaches)
	h.db.QueryRow(r.Context(), `SELECT COUNT(*) FROM rivers`).Scan(&rivers)
	// #246 A6 repoint (PART 2 item 4): calendar_runs (was plan_runs) is the
	// superset of `reports` post-000143 backfill — count live rows. web#354
	// A1 dropped calendar_runs.plan_id (decoupled from events) entirely, so
	// the old "live parent plan" join is gone too — deleted_at IS NULL alone
	// now determines liveness.
	h.db.QueryRow(r.Context(), `
		SELECT COUNT(*) FROM calendar_runs cr
		WHERE cr.deleted_at IS NULL
	`).Scan(&reports)
	h.sc.set(reaches, rivers, reports)
	jsonResponse(w, http.StatusOK, buildStatsResponse(reaches, rivers, reports, authed))
}

// buildStatsResponse omits "reports" entirely for an anonymous caller
// (authed=false) — web#354 A1 major fix (findings[2]): calendar_runs-derived
// numbers are auth-only, matching every other paddled-run reader post-A1.
func buildStatsResponse(reaches, rivers, reports int, authed bool) map[string]any {
	m := map[string]any{
		"reaches": reaches,
		"rivers":  rivers,
	}
	if authed && reports > 0 {
		m["reports"] = reports
	}
	return m
}

// Required for the reach map endpoint — without a viewport bound the result
// set could be enormous.
func parseBBoxParam(r *http.Request) (*searchBBox, error) {
	raw := r.URL.Query().Get("bbox")
	if raw == "" {
		return nil, fmt.Errorf("bbox is required")
	}
	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		return nil, fmt.Errorf("bbox must be west,south,east,north")
	}
	floats, err := parseFloats(parts)
	if err != nil {
		return nil, fmt.Errorf("bbox: %w", err)
	}
	return &searchBBox{
		West: floats[0], South: floats[1],
		East: floats[2], North: floats[3],
	}, nil
}
