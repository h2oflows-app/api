package handlers

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/h2oflow/h2oflow/apps/api/internal/nldi"
	"golang.org/x/sync/errgroup"
)

// ── BasinMap ──────────────────────────────────────────────────────────────────

type basinReachItem struct {
	Slug        string      `json:"slug"`
	Name        string      `json:"name"`
	RiverName   *string     `json:"river_name"`
	CommonName  *string     `json:"common_name"`
	RiverOrder  *int16      `json:"river_order"`
	ClassMin    *float64    `json:"class_min"`
	ClassMax    *float64    `json:"class_max"`
	AnchorComID *string     `json:"anchor_comid"`
	StartComID  *string     `json:"start_comid"`
	EndComID    *string     `json:"end_comid"`
	Centerline  rawGeometry `json:"centerline"`
	StartPoint  *[2]float64 `json:"start_point"`
	EndPoint    *[2]float64 `json:"end_point"`
	FlowStatus  string      `json:"flow_status"`
}

type basinMapResponse struct {
	BasinSlug string           `json:"basin_slug"`
	Reaches   []basinReachItem `json:"reaches"`
}

// basinSlugExpr is the SQL expression that converts a river basin name or
// gauge watershed name into the same normalized slug the frontend produces via
// slugifyBasin(cleanBasinName(name)): strip Upper/Middle/Lower prefix, strip
// River/Basin suffix, lowercase, replace non-alnum runs with '-', trim dashes.
const basinSlugExpr = `trim('-' FROM lower(regexp_replace(
	trim(regexp_replace(
		regexp_replace(
			COALESCE(%s, ''),
			'^(Upper|Middle|Lower)\s+', '', 'i'
		),
		'\s+(Rivers?|Basins?)$', '', 'i'
	)),
	'[^a-z0-9]+', '-', 'gi'
)))`

// BasinMap handles GET /api/v1/reaches/basin/{slug}/map
//
// Returns centerlines, class, comIDs, and flow status for a set of reaches
// identified by the ?slugs= comma-separated list AND (when authenticated) all
// of the caller's user_reaches whose river/watershed slug matches the basin.
// The {slug} path param is the normalized basin name.
func (h *ReachHandler) BasinMap(w http.ResponseWriter, r *http.Request) {
	basinSlug := chi.URLParam(r, "slug")

	// Ensure a non-nil slice so ANY($1) on an empty list is '{}'::text[], not NULL.
	slugs := parseSlugsParam(r.URL.Query().Get("slugs"))
	if slugs == nil {
		slugs = []string{}
	}

	var ownerID *string
	if uid, ok := auth.UserIDFromContext(r.Context()); ok {
		ownerID = &uid
	}

	// runs-unify 5a: the basin-wide branch returns the h2oflows sentinel twins
	// (curated runs) regardless of auth/slugs, so there is no early-return — a
	// public basin page must list curated runs even when unauthenticated.

	// runs-unify 5a: reaches table retired; all curated runs live in user_reaches.
	// The former "reaches r" UNION branch is removed — sentinel twins cover curated runs.
	// The NOT EXISTS (SELECT 1 FROM reaches …) dedup guards are also removed.
	rows, err := h.db.Query(r.Context(), `
		WITH latest_reading AS (
			SELECT DISTINCT ON (gauge_id)
				gauge_id, value
			FROM gauge_readings
			WHERE timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY gauge_id, timestamp DESC
		)
		-- User_reaches matched by requested slug list (dashboard gauge contexts).
		SELECT
			ur.slug,
			ur.name,
			ur.river_name,
			NULL::text     AS common_name,
			NULL::smallint AS river_order,
			ur.class_min,
			ur.class_max,
			ur.up_comid    AS anchor_comid,
			ur.up_comid    AS start_comid,
			ur.down_comid  AS end_comid,
			ST_AsGeoJSON(ur.centerline::geometry) AS centerline,
			ST_X(ur.put_in::geometry)    AS start_lng,
			ST_Y(ur.put_in::geometry)    AS start_lat,
			ST_X(ur.take_out::geometry)  AS end_lng,
			ST_Y(ur.take_out::geometry)  AS end_lat,
			CASE
				WHEN fr_u.band_color IS NULL       THEN 'unknown'
				WHEN fr_u.band_color LIKE 'red%'   THEN 'caution'
				WHEN fr_u.band_color LIKE 'blue%'  THEN 'flood'
				ELSE                                    'runnable'
			END AS flow_status
		FROM user_reaches ur
		LEFT JOIN gauges g_u ON g_u.id = ur.primary_gauge_id
		LEFT JOIN latest_reading lr_u ON lr_u.gauge_id = g_u.id
		LEFT JOIN LATERAL (
			SELECT label, color FROM user_reach_flow_ranges
			WHERE user_reach_id = ur.id
			  AND lr_u.value >= value
			ORDER BY value DESC
			LIMIT 1
		) thresh_u ON TRUE,
		LATERAL (
			SELECT
				COALESCE(thresh_u.label, CASE WHEN lr_u.value IS NOT NULL THEN ur.base_label END) AS band_label,
				COALESCE(thresh_u.color, CASE WHEN lr_u.value IS NOT NULL THEN ur.base_color END) AS band_color
		) fr_u
		WHERE ur.slug = ANY($1)
		  -- runs-unify 5a: include the h2oflows sentinel twins (former curated
		  -- reaches branch) alongside the caller's own runs.
		  AND (ur.owner_id = $2 OR ur.owner_id = '00000000-0000-0000-0000-000000000001')
		  AND ur.centerline IS NOT NULL

		UNION ALL

		-- All user_reaches whose river/watershed basin slug matches the URL param.
		-- This is the primary source: runs appear even without a dashboard gauge link.
		SELECT
			ur3.slug,
			ur3.name,
			ur3.river_name,
			NULL::text     AS common_name,
			NULL::smallint AS river_order,
			ur3.class_min,
			ur3.class_max,
			ur3.up_comid   AS anchor_comid,
			ur3.up_comid   AS start_comid,
			ur3.down_comid AS end_comid,
			ST_AsGeoJSON(ur3.centerline::geometry) AS centerline,
			ST_X(ur3.put_in::geometry)    AS start_lng,
			ST_Y(ur3.put_in::geometry)    AS start_lat,
			ST_X(ur3.take_out::geometry)  AS end_lng,
			ST_Y(ur3.take_out::geometry)  AS end_lat,
			CASE
				WHEN fr3.band_color IS NULL       THEN 'unknown'
				WHEN fr3.band_color LIKE 'red%'   THEN 'caution'
				WHEN fr3.band_color LIKE 'blue%'  THEN 'flood'
				ELSE                                   'runnable'
			END AS flow_status
		FROM user_reaches ur3
		LEFT JOIN rivers rv3 ON rv3.id = ur3.river_id
		LEFT JOIN gauges g3 ON g3.id = ur3.primary_gauge_id
		LEFT JOIN latest_reading lr3 ON lr3.gauge_id = g3.id
		LEFT JOIN LATERAL (
			SELECT label, color FROM user_reach_flow_ranges
			WHERE user_reach_id = ur3.id
			  AND lr3.value >= value
			ORDER BY value DESC
			LIMIT 1
		) thresh3 ON TRUE,
		LATERAL (
			SELECT
				COALESCE(thresh3.label, CASE WHEN lr3.value IS NOT NULL THEN ur3.base_label END) AS band_label,
				COALESCE(thresh3.color, CASE WHEN lr3.value IS NOT NULL THEN ur3.base_color END) AS band_color
		) fr3
		WHERE (ur3.owner_id = $2 OR ur3.owner_id = '00000000-0000-0000-0000-000000000001')
		  AND ur3.centerline IS NOT NULL
		  AND trim('-' FROM lower(regexp_replace(
		      trim(regexp_replace(
		          regexp_replace(
		              COALESCE(rv3.basin, g3.watershed_name, ''),
		              '^(Upper|Middle|Lower)\s+', '', 'i'
		          ),
		          '\s+(Rivers?|Basins?)$', '', 'i'
		      )),
		      '[^a-z0-9]+', '-', 'gi'
		  ))) = $3
		  AND NOT (ur3.slug = ANY($1))

		ORDER BY river_order ASC NULLS LAST,
		         start_lng ASC NULLS LAST
	`, slugs, ownerID, basinSlug)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	items := make([]basinReachItem, 0, len(slugs))
	for rows.Next() {
		var (
			item               basinReachItem
			startLng, startLat *float64
			endLng, endLat     *float64
		)
		if err := rows.Scan(
			&item.Slug, &item.Name, &item.RiverName, &item.CommonName, &item.RiverOrder,
			&item.ClassMin, &item.ClassMax,
			&item.AnchorComID, &item.StartComID, &item.EndComID,
			&item.Centerline,
			&startLng, &startLat, &endLng, &endLat,
			&item.FlowStatus,
		); err != nil {
			continue
		}
		if startLng != nil && startLat != nil {
			item.StartPoint = &[2]float64{*startLng, *startLat}
		}
		if endLng != nil && endLat != nil {
			item.EndPoint = &[2]float64{*endLng, *endLat}
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		errorResponse(w, http.StatusInternalServerError, "scan failed")
		return
	}

	jsonResponse(w, http.StatusOK, basinMapResponse{BasinSlug: basinSlug, Reaches: items})
}

// ── BasinNetwork ──────────────────────────────────────────────────────────────

type networkCacheEntry struct {
	upstream nldi.Collection
	cachedAt time.Time
}

type dmCacheEntry struct {
	sampled  []string // sampled COMIDs along DM
	cachedAt time.Time
}

var (
	networkCache    sync.Map
	networkCacheTTL = 24 * time.Hour
	dmCache         sync.Map
	dmCacheTTL      = 7 * 24 * time.Hour
)

type basinNetworkResponse struct {
	Tributaries   nldi.Collection `json:"tributaries"`
	Gauges        nldi.Collection `json:"gauges"`
	NLDIAvailable bool            `json:"nldi_available"`
	NLDIError     string          `json:"nldi_error,omitempty"`
}

const (
	tributaryKm     = 10  // UT radius for direct tributary flowlines
	gaugeKm         = 25  // UT radius for upstream gauges (put-in anchors only)
	longReachKm     = 30  // threshold to enable DM mainstem sampling
	sampleSpacingKm = 20  // one tributary anchor per ~20 km
	dmBufferKm      = 25  // overshoot beyond reach length for DM request
	dmMaxKm         = 600 // hard cap on a single DM call
	maxAnchorsTotal = 40  // safety cap on total NLDI fanout per request
)

// BasinNetwork handles GET /api/v1/reaches/basin/{slug}/network
//
// For each reach's start_comid it fetches:
//   - Short UT flowlines (10 km) — direct tributaries near the put-in
//   - UT gauge sites (25 km) — USGS gauges near the put-in
//
// For long reaches (≥ 30 km centerline) it also walks the downstream
// mainstem (DM) from start_comid and samples an additional tributary
// anchor every ~20 km, giving full tributary coverage along the entire run.
//
// Results are cached per comid (UT: 24 h, DM: 7 d). On NLDI downtime, returns
// 200 with empty collections and nldi_available=false.
func (h *ReachHandler) BasinNetwork(w http.ResponseWriter, r *http.Request) {
	empty := nldi.Collection{Type: "FeatureCollection", Features: []nldi.Feature{}}
	emptyOK := basinNetworkResponse{Tributaries: empty, Gauges: empty, NLDIAvailable: true}

	basinSlug := chi.URLParam(r, "slug")

	// Ensure non-nil slice so ANY($1) on empty list is '{}'::text[], not NULL.
	slugs := parseSlugsParam(r.URL.Query().Get("slugs"))
	if slugs == nil {
		slugs = []string{}
	}

	var ownerIDNet *string
	if uid, ok := auth.UserIDFromContext(r.Context()); ok {
		ownerIDNet = &uid
	}

	// runs-unify 5a: no early-return — the basin-wide branch returns curated
	// sentinel twins even when unauthenticated with no slug list.

	type reachAnchor struct {
		startCID string
		lengthKm float64
	}

	// runs-unify 5a: reaches table retired; all curated runs live in user_reaches.
	// The former "FROM reaches" UNION branch is removed; NOT EXISTS dedup guards removed.
	rows, err := h.db.Query(r.Context(), `
		SELECT
			ur2.up_comid AS start_comid,
			COALESCE(ST_Length(ur2.centerline::geography) / 1000.0, 0) AS length_km
		FROM user_reaches ur2
		WHERE ur2.slug = ANY($1)
		  AND ur2.up_comid IS NOT NULL
		  AND (ur2.owner_id = $2 OR ur2.owner_id = '00000000-0000-0000-0000-000000000001')

		UNION ALL

		SELECT
			ur3.up_comid AS start_comid,
			COALESCE(ST_Length(ur3.centerline::geography) / 1000.0, 0) AS length_km
		FROM user_reaches ur3
		LEFT JOIN rivers rv3 ON rv3.id = ur3.river_id
		LEFT JOIN gauges g3 ON g3.id = ur3.primary_gauge_id
		WHERE (ur3.owner_id = $2 OR ur3.owner_id = '00000000-0000-0000-0000-000000000001')
		  AND ur3.up_comid IS NOT NULL
		  AND trim('-' FROM lower(regexp_replace(
		      trim(regexp_replace(
		          regexp_replace(
		              COALESCE(rv3.basin, g3.watershed_name, ''),
		              '^(Upper|Middle|Lower)\s+', '', 'i'
		          ),
		          '\s+(Rivers?|Basins?)$', '', 'i'
		      )),
		      '[^a-z0-9]+', '-', 'gi'
		  ))) = $3
		  AND NOT (ur3.slug = ANY($1))
	`, slugs, ownerIDNet, basinSlug)
	if err != nil {
		jsonResponse(w, http.StatusOK, emptyOK)
		return
	}
	defer rows.Close()

	seenCID := make(map[string]float64) // comid → max length_km seen
	for rows.Next() {
		var ra reachAnchor
		if err := rows.Scan(&ra.startCID, &ra.lengthKm); err != nil {
			continue
		}
		if prev, ok := seenCID[ra.startCID]; !ok || ra.lengthKm > prev {
			seenCID[ra.startCID] = ra.lengthKm
		}
	}
	_ = rows.Err()

	if len(seenCID) == 0 {
		jsonResponse(w, http.StatusOK, emptyOK)
		return
	}

	c := nldi.New()

	// Build full anchor list: put-in COMIDs + mid-reach samples for long reaches.
	type anchor struct {
		comid   string
		isPutIn bool
	}
	var anchors []anchor
	seenAnchor := make(map[string]struct{})

	addAnchor := func(comid string, isPutIn bool) {
		if _, dup := seenAnchor[comid]; dup {
			return
		}
		seenAnchor[comid] = struct{}{}
		anchors = append(anchors, anchor{comid, isPutIn})
	}

	for comid, lengthKm := range seenCID {
		addAnchor(comid, true)
		if lengthKm >= longReachKm {
			for _, mid := range sampledAnchors(r.Context(), c, comid, lengthKm) {
				addAnchor(mid, false)
			}
		}
	}

	var (
		mu           sync.Mutex
		allFlowlines []nldi.Feature
		allGauges    []nldi.Feature
		nldiErr      error
	)

	g, gctx := errgroup.WithContext(r.Context())
	g.SetLimit(8)

	for _, a := range anchors {
		a := a
		g.Go(func() error {
			if fl := cachedFlowlines(gctx, c, a.comid); fl != nil {
				mu.Lock()
				allFlowlines = append(allFlowlines, fl...)
				mu.Unlock()
			} else {
				mu.Lock()
				if nldiErr == nil {
					nldiErr = fmt.Errorf("flowlines unavailable for %s", a.comid)
				}
				mu.Unlock()
			}
			if a.isPutIn {
				if gs := cachedGauges(gctx, c, a.comid); gs != nil {
					mu.Lock()
					allGauges = append(allGauges, gs...)
					mu.Unlock()
				}
			}
			return nil
		})
	}
	_ = g.Wait()

	resp := basinNetworkResponse{
		Tributaries:   nldi.Collection{Type: "FeatureCollection", Features: dedupeFlowlines(allFlowlines)},
		Gauges:        nldi.Collection{Type: "FeatureCollection", Features: dedupeByIdentifier(allGauges)},
		NLDIAvailable: nldiErr == nil,
	}
	if nldiErr != nil {
		resp.NLDIError = "River network data is temporarily unavailable (NLDI). The map will display your dashboard reaches without the tributary overlay."
	}
	jsonResponse(w, http.StatusOK, resp)
}

// sampledAnchors returns mid-reach COMIDs by walking the DM mainstem from
// startCID and sampling every sampleSpacingKm. Returns nil on NLDI failure.
func sampledAnchors(ctx context.Context, c *nldi.Client, startCID string, lengthKm float64) []string {
	bucket := int(math.Ceil(lengthKm/50)) * 50
	key := fmt.Sprintf("dm|%s|%d|%d", startCID, bucket, sampleSpacingKm)

	if v, ok := dmCache.Load(key); ok {
		e := v.(dmCacheEntry)
		if time.Since(e.cachedAt) < dmCacheTTL {
			return e.sampled
		}
		dmCache.Delete(key)
	}

	distance := int(math.Ceil(lengthKm)) + dmBufferKm
	if distance > dmMaxKm {
		distance = dmMaxKm
	}
	coll, err := c.DownstreamFlowlines(ctx, startCID, distance)
	if err != nil || coll == nil || len(coll.Features) == 0 {
		log.Printf("basin network: DM comid %s: %v", startCID, err)
		return nil
	}

	sampled := nldi.SampleDownstreamComIDs(coll.Features, float64(sampleSpacingKm), maxAnchorsTotal)
	// skip first — it's the put-in comid, already added as isPutIn anchor
	if len(sampled) > 1 {
		sampled = sampled[1:]
	} else {
		sampled = nil
	}
	dmCache.Store(key, dmCacheEntry{sampled: sampled, cachedAt: time.Now()})
	return sampled
}

// cachedFlowlines fetches UT flowlines for comid, using networkCache.
// Returns nil if NLDI fails (caller records the error).
func cachedFlowlines(ctx context.Context, c *nldi.Client, comid string) []nldi.Feature {
	key := fmt.Sprintf("fl|%s|%d", comid, tributaryKm)
	if v, ok := networkCache.Load(key); ok {
		e := v.(networkCacheEntry)
		if time.Since(e.cachedAt) < networkCacheTTL {
			return e.upstream.Features
		}
		networkCache.Delete(key)
	}
	fl, err := c.UpstreamFlowlines(ctx, comid, tributaryKm)
	if err != nil {
		log.Printf("basin network: flowlines comid %s: %v", comid, err)
		return nil
	}
	networkCache.Store(key, networkCacheEntry{upstream: *fl, cachedAt: time.Now()})
	return fl.Features
}

// cachedGauges fetches UT gauge sites for comid, using networkCache.
func cachedGauges(ctx context.Context, c *nldi.Client, comid string) []nldi.Feature {
	key := fmt.Sprintf("g|%s|%d", comid, gaugeKm)
	if v, ok := networkCache.Load(key); ok {
		e := v.(networkCacheEntry)
		if time.Since(e.cachedAt) < networkCacheTTL {
			return e.upstream.Features
		}
		networkCache.Delete(key)
	}
	g, err := c.UpstreamGauges(ctx, comid, gaugeKm)
	if err != nil {
		log.Printf("basin network: gauges comid %s: %v", comid, err)
		return nil
	}
	networkCache.Store(key, networkCacheEntry{upstream: *g, cachedAt: time.Now()})
	return g.Features
}

// dedupeByIdentifier removes duplicate NLDI point features (gauges) by identifier.
func dedupeByIdentifier(features []nldi.Feature) []nldi.Feature {
	seen := make(map[string]struct{}, len(features))
	out := make([]nldi.Feature, 0, len(features))
	for _, f := range features {
		id := f.Props.Identifier
		if id == "" {
			out = append(out, f)
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, f)
	}
	return out
}

// dedupeFlowlines removes duplicate NLDI features by nhdplus_comid.
func dedupeFlowlines(features []nldi.Feature) []nldi.Feature {
	seen := make(map[string]struct{}, len(features))
	out := make([]nldi.Feature, 0, len(features))
	for _, f := range features {
		id := ""
		if f.Props.NhdplusComID != nil {
			id = string(*f.Props.NhdplusComID)
		}
		if id == "" {
			out = append(out, f)
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, f)
	}
	return out
}

// parseSlugsParam splits a comma-separated slugs query param, trims whitespace,
// and drops empty strings.
func parseSlugsParam(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := parts[:0]
	for _, s := range parts {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}
