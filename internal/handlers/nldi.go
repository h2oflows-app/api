package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/kmlimport"
	"github.com/h2oflow/h2oflow/apps/api/internal/nldi"
	"github.com/jackc/pgx/v5/pgxpool"
	"log"
)

type NLDIHandler struct {
	db           *pgxpool.Pool
	anthropicKey string
	cacheWarmer  func()
}

func NewNLDIHandler(db *pgxpool.Pool) *NLDIHandler { return &NLDIHandler{db: db} }

func (h *NLDIHandler) WithAnthropicKey(key string) *NLDIHandler {
	h.anthropicKey = key
	return h
}

func (h *NLDIHandler) WithCacheWarmer(fn func()) *NLDIHandler {
	h.cacheWarmer = fn
	return h
}

func (h *NLDIHandler) warmCache() {
	if h.cacheWarmer != nil {
		go h.cacheWarmer()
	}
}

// WatershedExplorer handles GET /api/v1/admin/nldi/watershed
//
// Query params:
//
//	lat, lng    float64  — coordinate to snap (required)
//	distance    int      — km radius for upstream navigation (default 150, max 500)
//
// Response: { snap, upstream_flowlines, downstream_flowlines, upstream_gauges }
func (h *NLDIHandler) WatershedExplorer(w http.ResponseWriter, r *http.Request) {
	lat, lng, err := parseLatLng(r)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	distanceKm := 150
	if d := r.URL.Query().Get("distance"); d != "" {
		if v, err := strconv.Atoi(d); err == nil && v > 0 && v <= 500 {
			distanceKm = v
		}
	}

	ctx := r.Context()
	c := nldi.New()

	snap, err := c.SnapToComID(ctx, lat, lng)
	if err != nil {
		errorResponse(w, http.StatusBadGateway, fmt.Sprintf("snap to NHD: %v", err))
		return
	}

	upFlowlines, err := c.UpstreamFlowlines(ctx, snap.ComID, distanceKm)
	if err != nil {
		errorResponse(w, http.StatusBadGateway, fmt.Sprintf("upstream flowlines: %v", err))
		return
	}

	downFlowlines, err := c.DownstreamFlowlines(ctx, snap.ComID, distanceKm)
	if err != nil {
		errorResponse(w, http.StatusBadGateway, fmt.Sprintf("downstream flowlines: %v", err))
		return
	}

	upGauges, err := c.UpstreamGauges(ctx, snap.ComID, distanceKm)
	if err != nil {
		upGauges = &nldi.Collection{Type: "FeatureCollection"}
	}

	type snapInfo struct {
		ComID string  `json:"comid"`
		Name  string  `json:"name"`
		Lat   float64 `json:"lat"`
		Lng   float64 `json:"lng"`
	}
	type response struct {
		Snap                snapInfo        `json:"snap"`
		UpstreamFlowlines   nldi.Collection `json:"upstream_flowlines"`
		DownstreamFlowlines nldi.Collection `json:"downstream_flowlines"`
		UpstreamGauges      nldi.Collection `json:"upstream_gauges"`
	}

	body := response{
		Snap:                snapInfo{ComID: snap.ComID, Name: snap.Name, Lat: lat, Lng: lng},
		UpstreamFlowlines:   *upFlowlines,
		DownstreamFlowlines: *downFlowlines,
		UpstreamGauges:      *upGauges,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// ── Reach authoring ───────────────────────────────────────────────────────────

// createReachRequest uses ComID-first authoring: admin selects upstream and
// downstream ComID segments from the NHD network on the map. Access point
// coords are not required here — they come from KML import later, at which
// point the NLDI centerline is trimmed and stored.
type createReachRequest struct {
	Slug           string   `json:"slug"` // optional — auto-derived from river+name if blank
	Name           string   `json:"name"`
	CommonName     string   `json:"common_name"`
	RiverName      string   `json:"river_name"`
	UpComID        string   `json:"up_comid"`   // reach-start ComID — required
	DownComID      string   `json:"down_comid"` // reach-end ComID — required
	StartLat       *float64 `json:"start_lat"`  // clicked lat for reach start
	StartLng       *float64 `json:"start_lng"`
	EndLat         *float64 `json:"end_lat"` // clicked lat for reach end
	EndLng         *float64 `json:"end_lng"`
	ClassMin       *float64 `json:"class_min"`
	ClassMax       *float64 `json:"class_max"`
	Description    string   `json:"description"`
	PermitRequired bool     `json:"permit_required"`
	MultiDayDays   int      `json:"multi_day_days"` // 0 or 1 = single day
}

// NearbyGauges handles GET /api/v1/admin/nldi/nearby-gauges
// Returns active gauges from three sources in parallel:
//   - Local DB PostGIS radius (all seeded sources)
//   - Colorado DWR API live query (catches unseeded DWR stations)
//   - NLDI flow-network gauges (optional, requires comid param; catches
//     upstream USGS gauges far along the network)
//
// Query params: lat, lng (required), comid (optional), distance (km, default 100, max 500)
func (h *NLDIHandler) NearbyGauges(w http.ResponseWriter, r *http.Request) {
	lat, lng, err := parseLatLng(r)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	comid := r.URL.Query().Get("comid")
	distanceKm := 100
	if d := r.URL.Query().Get("distance"); d != "" {
		if v, err := strconv.Atoi(d); err == nil && v > 0 && v <= 500 {
			distanceKm = v
		}
	}

	ctx := r.Context()

	type gaugeFeature struct {
		Type       string `json:"type"`
		Geometry   any    `json:"geometry"`
		Properties any    `json:"properties"`
	}

	// Channel for collecting all gauge features from parallel sources.
	type result struct {
		features []gaugeFeature
		err      error
	}
	dbCh := make(chan result, 1)
	dwrCh := make(chan result, 1)
	nldiCh := make(chan result, 1)

	// 1. Local DB — all seeded gauges within radius.
	go func() {
		rows, err := h.db.Query(ctx, `
			SELECT external_id, source, name,
			       ST_Y(location::geometry),
			       ST_X(location::geometry)
			FROM gauges
			WHERE status = 'active'
			  AND location IS NOT NULL
			  AND ST_DWithin(
			        location,
			        ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography,
			        $3
			      )
			ORDER BY location <-> ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography
			LIMIT 60
		`, lng, lat, distanceKm*1000)
		if err != nil {
			dbCh <- result{err: err}
			return
		}
		defer rows.Close()
		var feats []gaugeFeature
		for rows.Next() {
			var extID, source string
			var name *string
			var gLat, gLng float64
			if rows.Scan(&extID, &source, &name, &gLat, &gLng) != nil {
				continue
			}
			n := ""
			if name != nil {
				n = *name
			}
			feats = append(feats, gaugeFeature{
				Type:       "Feature",
				Geometry:   map[string]any{"type": "Point", "coordinates": []float64{gLng, gLat}},
				Properties: map[string]any{"identifier": extID, "source": source, "name": n},
			})
		}
		dbCh <- result{features: feats}
	}()

	// 2. DWR live API — discharge stations within radius.
	go func() {
		stations, err := nldi.DWRNearby(ctx, lat, lng, distanceKm)
		if err != nil {
			log.Printf("NearbyGauges: DWR error lat=%.4f lng=%.4f: %v", lat, lng, err)
			dwrCh <- result{err: err}
			return
		}
		feats := make([]gaugeFeature, 0, len(stations))
		for _, st := range stations {
			feats = append(feats, gaugeFeature{
				Type:       "Feature",
				Geometry:   map[string]any{"type": "Point", "coordinates": []float64{st.Lng, st.Lat}},
				Properties: map[string]any{"identifier": st.ExternalID, "source": "dwr", "name": st.Name},
			})
		}
		dwrCh <- result{features: feats}
	}()

	// 3. NLDI flow-network gauges — upstream + short downstream if comid given.
	go func() {
		if comid == "" {
			nldiCh <- result{}
			return
		}
		c := nldi.New()
		upGauges, _ := c.UpstreamGauges(ctx, comid, distanceKm)
		downKm := distanceKm
		if downKm > 50 {
			downKm = 50
		}
		downGauges, _ := c.DownstreamGauges(ctx, comid, downKm)
		var feats []gaugeFeature
		for _, coll := range []*nldi.Collection{upGauges, downGauges} {
			if coll == nil {
				continue
			}
			for _, f := range coll.Features {
				raw, _ := json.Marshal(f.Geometry.Coordinates)
				var coords []float64
				if json.Unmarshal(raw, &coords) != nil || len(coords) < 2 {
					continue
				}
				feats = append(feats, gaugeFeature{
					Type:       "Feature",
					Geometry:   map[string]any{"type": "Point", "coordinates": coords},
					Properties: map[string]any{"identifier": f.Props.Identifier, "source": "usgs", "name": f.Props.Name},
				})
			}
		}
		nldiCh <- result{features: feats}
	}()

	// Merge — deduplicate by (source, identifier).
	seen := map[string]bool{}
	features := make([]gaugeFeature, 0)
	for _, res := range []result{<-dbCh, <-dwrCh, <-nldiCh} {
		for _, f := range res.features {
			p, _ := f.Properties.(map[string]any)
			key := fmt.Sprintf("%v|%v", p["source"], p["identifier"])
			if seen[key] {
				continue
			}
			seen[key] = true
			features = append(features, f)
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"type":     "FeatureCollection",
		"features": features,
	})
}

// normalizeExternalID strips the source prefix (e.g. "USGS-", "DWR-") that the
// NLDI API sometimes includes in the identifier field. NWIS and DWR polling APIs
// expect the bare station number (e.g. "09342500", not "USGS-09342500").
func normalizeExternalID(extID, source string) string {
	if extID == "" || source == "" {
		return extID
	}
	prefix := strings.ToUpper(source) + "-"
	if strings.HasPrefix(strings.ToUpper(extID), prefix) {
		return extID[len(prefix):]
	}
	return extID
}

// UpstreamTributaries handles GET /api/v1/admin/nldi/upstream-tributaries
//
// Snaps lat/lng to the nearest NHD ComID (anchor), then returns all upstream
// tributary flowlines (UT navigation). Used to discover ComIDs for small
// creeks that don't snap reliably via comid/position alone — once the larger
// river's anchor is found, all its tributaries appear as clickable segments.
//
// Query params:
//
//	lat, lng    float64  — coordinate to snap (required)
//	distance    int      — km radius (default 50, max 200)
func (h *NLDIHandler) UpstreamTributaries(w http.ResponseWriter, r *http.Request) {
	lat, lng, err := parseLatLng(r)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	distanceKm := 50
	if d := r.URL.Query().Get("distance"); d != "" {
		if v, err2 := strconv.Atoi(d); err2 == nil && v > 0 && v <= 200 {
			distanceKm = v
		}
	}

	ctx := r.Context()
	c := nldi.New()

	snap, err := c.SnapToComID(ctx, lat, lng)
	if err != nil {
		errorResponse(w, http.StatusBadGateway, fmt.Sprintf("snap to NHD: %v", err))
		return
	}

	tributaries, err := c.UpstreamFlowlines(ctx, snap.ComID, distanceKm)
	if err != nil {
		errorResponse(w, http.StatusBadGateway, fmt.Sprintf("upstream tributaries: %v", err))
		return
	}

	type snapInfo struct {
		ComID string  `json:"comid"`
		Name  string  `json:"name"`
		Lat   float64 `json:"lat"`
		Lng   float64 `json:"lng"`
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"snap":        snapInfo{ComID: snap.ComID, Name: snap.Name, Lat: lat, Lng: lng},
		"tributaries": tributaries,
	})
}

// DownstreamMainstem handles GET /api/v1/admin/nldi/downstream
//
// Returns the downstream mainstem flowlines from a known ComID. Used after the
// upstream ComID is selected in the author flow — displays the full downstream
// river so the user can click anywhere along it to set the take-out ComID,
// even for very long reaches (e.g. Grand Canyon ~300 mi).
//
// Query params:
//
//	comid       string — NHD ComID to trace downstream from (required)
//	distance    int    — km radius (default 500, max 1000)
func (h *NLDIHandler) DownstreamMainstem(w http.ResponseWriter, r *http.Request) {
	comid := strings.TrimSpace(r.URL.Query().Get("comid"))
	if comid == "" {
		errorResponse(w, http.StatusBadRequest, "comid is required")
		return
	}
	distanceKm := 500
	if d := r.URL.Query().Get("distance"); d != "" {
		if v, err := strconv.Atoi(d); err == nil && v > 0 && v <= 1000 {
			distanceKm = v
		}
	}

	ctx := r.Context()
	c := nldi.New()

	flowlines, err := c.DownstreamFlowlines(ctx, comid, distanceKm)
	if err != nil {
		errorResponse(w, http.StatusBadGateway, fmt.Sprintf("downstream flowlines: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"downstream_flowlines": flowlines,
	})
}

// RiverName handles GET /api/v1/admin/nldi/river-name?comid=<comid>[&lat=<lat>&lng=<lng>]
//
// Returns the GNIS stream name at the given location. When lat/lng are provided
// (the exact point the user clicked on the flowline) we query the National Map
// NHD ArcGIS service directly at that coordinate — no NLDI navigation needed,
// and the result is the feature the user actually clicked, not a random tributary.
// When only comid is provided we fall back to fetching a short upstream slice to
// extract a coordinate (legacy path, less reliable for large rivers).
func (h *NLDIHandler) RiverName(w http.ResponseWriter, r *http.Request) {
	comid := strings.TrimSpace(r.URL.Query().Get("comid"))
	if comid == "" {
		errorResponse(w, http.StatusBadRequest, "comid is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	var lat, lng float64

	// Prefer caller-supplied coordinates (the exact map click point).
	if latStr := r.URL.Query().Get("lat"); latStr != "" {
		lat, _ = strconv.ParseFloat(latStr, 64)
		lng, _ = strconv.ParseFloat(r.URL.Query().Get("lng"), 64)
	}

	// Fall back: navigate a short upstream slice to extract a coordinate.
	if lat == 0 && lng == 0 {
		c := nldi.New()
		up, err := c.UpstreamFlowlines(ctx, comid, 10)
		if err != nil || up == nil || len(up.Features) == 0 {
			up, _ = c.DownstreamFlowlines(ctx, comid, 5)
		}
		if up != nil {
			for _, f := range up.Features {
				if pt := nldi.FirstCoord(f.Geometry); pt != nil {
					lat, lng = pt[1], pt[0]
					break
				}
			}
		}
	}

	if lat == 0 && lng == 0 {
		jsonResponse(w, http.StatusOK, map[string]any{"river_name": ""})
		return
	}

	name, gnisID, err := nldi.NHDStreamNameAt(ctx, lat, lng)
	if err != nil {
		// Non-fatal: return empty rather than a 502.
		jsonResponse(w, http.StatusOK, map[string]any{"river_name": ""})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"river_name": name, "gnis_id": gnisID})
}

type updateReachCenterlineRequest struct {
	PutIn   latLng `json:"put_in"`
	TakeOut latLng `json:"take_out"`
	DryRun  bool   `json:"dry_run"`
}

type latLng struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// PreviewCenterline handles GET /api/v1/admin/nldi/preview-centerline
// Returns the GeoJSON LineString between two NHD ComIDs without writing to the
// database. When start_lat/start_lng/end_lat/end_lng are provided the line is
// trimmed via PostGIS ST_LineSubstring to match the exact reach extent.
func (h *NLDIHandler) PreviewCenterline(w http.ResponseWriter, r *http.Request) {
	upComID := r.URL.Query().Get("up_comid")
	downComID := r.URL.Query().Get("down_comid")
	if upComID == "" || downComID == "" {
		errorResponse(w, http.StatusBadRequest, "up_comid and down_comid are required")
		return
	}
	geojson, err := kmlimport.FetchCenterlinePreview(r.Context(), upComID, downComID)
	if err != nil {
		errorResponse(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// Apply trim if coordinates supplied.
	q := r.URL.Query()
	if q.Get("start_lat") != "" && q.Get("end_lat") != "" {
		startLat, e1 := strconv.ParseFloat(q.Get("start_lat"), 64)
		startLng, e2 := strconv.ParseFloat(q.Get("start_lng"), 64)
		endLat, e3 := strconv.ParseFloat(q.Get("end_lat"), 64)
		endLng, e4 := strconv.ParseFloat(q.Get("end_lng"), 64)
		if e1 == nil && e2 == nil && e3 == nil && e4 == nil {
			if trimmed, err := trimLineGeoJSON(r.Context(), h.db, geojson, startLng, startLat, endLng, endLat); err == nil {
				geojson = trimmed
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{"geojson": json.RawMessage(geojson)})
}

// trimLineGeoJSON applies PostGIS ST_LineSubstring to trim the raw GeoJSON line
// to the closest points on the line to putIn and takeOut coordinates.
func trimLineGeoJSON(ctx context.Context, db *pgxpool.Pool, geojson string, putInLon, putInLat, takeOutLon, takeOutLat float64) (string, error) {
	var result string
	err := db.QueryRow(ctx, `
		SELECT ST_AsGeoJSON(
			ST_LineSubstring(
				line,
				LEAST(ST_LineLocatePoint(line, put_pt), ST_LineLocatePoint(line, take_pt)),
				GREATEST(ST_LineLocatePoint(line, put_pt), ST_LineLocatePoint(line, take_pt))
			)
		)
		FROM (
			SELECT
				ST_GeomFromGeoJSON($1)                                      AS line,
				ST_ClosestPoint(ST_GeomFromGeoJSON($1),
				    ST_SetSRID(ST_MakePoint($2, $3), 4326))                 AS put_pt,
				ST_ClosestPoint(ST_GeomFromGeoJSON($1),
				    ST_SetSRID(ST_MakePoint($4, $5), 4326))                 AS take_pt
		) sub
	`, geojson, putInLon, putInLat, takeOutLon, takeOutLat).Scan(&result)
	return result, err
}

// buildSlug produces a URL-safe slug from river name + reach name,
// matching the KML importer convention.
func buildSlug(riverName, reachName string) string {
	r := kmlimport.Slugify(riverName)
	n := kmlimport.Slugify(reachName)
	if r == "" {
		return n
	}
	if n == "" {
		return r
	}
	return r + "-" + n
}

// fillReachState queries TIGERweb for the US state at the given coordinate and
// updates reaches.state_abbr. Best-effort — failures are logged and not fatal.
func fillReachState(ctx context.Context, db *pgxpool.Pool, reachSlug string, lat, lng float64) {
	stateAbbr, err := nldi.StateAt(ctx, lat, lng)
	if err != nil {
		log.Printf("fillReachState %s: %v", reachSlug, err)
		return
	}
	if stateAbbr == "" {
		return
	}
	if _, err := db.Exec(ctx, `UPDATE reaches SET state_abbr = $1 WHERE slug = $2`, stateAbbr, reachSlug); err != nil {
		log.Printf("fillReachState %s: update: %v", reachSlug, err)
	}
}

// ── Shared helpers ────────────────────────────────────────────────────────────

func parseLatLng(r *http.Request) (lat, lng float64, err error) {
	latStr := r.URL.Query().Get("lat")
	lngStr := r.URL.Query().Get("lng")
	if latStr == "" || lngStr == "" {
		return 0, 0, fmt.Errorf("lat and lng are required")
	}
	lat, err = strconv.ParseFloat(latStr, 64)
	if err != nil || lat < -90 || lat > 90 {
		return 0, 0, fmt.Errorf("invalid lat: %s", latStr)
	}
	lng, err = strconv.ParseFloat(lngStr, 64)
	if err != nil || lng < -180 || lng > 180 {
		return 0, 0, fmt.Errorf("invalid lng: %s", lngStr)
	}
	return lat, lng, nil
}
