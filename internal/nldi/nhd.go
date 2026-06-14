package nldi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// FirstCoord returns the first [lng, lat] pair from a GeoJSON geometry, or nil
// if the geometry is empty or an unrecognised type.
func FirstCoord(g Geometry) []float64 {
	switch g.Type {
	case "LineString":
		// coordinates: [[lng, lat], ...]
		if outer, ok := g.Coordinates.([]interface{}); ok && len(outer) > 0 {
			if pt, ok := outer[0].([]interface{}); ok && len(pt) >= 2 {
				lng, _ := pt[0].(float64)
				lat, _ := pt[1].(float64)
				return []float64{lng, lat}
			}
		}
	case "MultiLineString":
		// coordinates: [[[lng, lat], ...], ...]
		if lines, ok := g.Coordinates.([]interface{}); ok && len(lines) > 0 {
			if line, ok := lines[0].([]interface{}); ok && len(line) > 0 {
				if pt, ok := line[0].([]interface{}); ok && len(pt) >= 2 {
					lng, _ := pt[0].(float64)
					lat, _ := pt[1].(float64)
					return []float64{lng, lat}
				}
			}
		}
	case "Point":
		if pt, ok := g.Coordinates.([]interface{}); ok && len(pt) >= 2 {
			lng, _ := pt[0].(float64)
			lat, _ := pt[1].(float64)
			return []float64{lng, lat}
		}
	}
	return nil
}

const nhdArcGISURL = "https://hydro.nationalmap.gov/arcgis/rest/services/nhd/MapServer/6/query"

// NHDStreamNameAt queries the National Map NHD ArcGIS service for the GNIS
// stream name and ID at the given coordinate. Returns empty strings without an
// error when no named feature is found.
func NHDStreamNameAt(ctx context.Context, lat, lng float64) (name, gnisID string, err error) {
	params := url.Values{
		"geometry":     {fmt.Sprintf("%f,%f", lng, lat)},
		"geometryType": {"esriGeometryPoint"},
		"spatialRel":   {"esriSpatialRelIntersects"},
		"outFields":    {"gnis_name,gnis_id"},
		"distance":     {"100"},
		"units":        {"esriSRUnit_Meter"},
		"inSR":         {"4326"},
		"f":            {"json"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nhdArcGISURL+"?"+params.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "h2oflows/1.0 (https://h2oflows.org)")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("nhd arcgis: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Features []struct {
			Attributes struct {
				GnisName string `json:"gnis_name"`
				GnisID   string `json:"gnis_id"`
			} `json:"attributes"`
		} `json:"features"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", fmt.Errorf("nhd arcgis parse: %w", err)
	}
	for _, f := range result.Features {
		if f.Attributes.GnisName != "" {
			return f.Attributes.GnisName, f.Attributes.GnisID, nil
		}
	}
	return "", "", nil
}

// NHDNameResult is one GNIS entry returned by NHDLookupByName.
type NHDNameResult struct {
	GnisID string
	Name   string
	HUC8   string // first 8 chars of reachcode, may be empty
}

// riverSuffixes are stripped (lowercase, longest-first) when normalizing names
// for fuzzy matching between our rivers table and NHD gnis_name.
var riverSuffixes = []string{
	" reservoir", " slough", " stream", " branch", " gulch",
	" river", " creek", " canal", " bayou", " ditch", " wash",
	" draw", " fork", " lake", " run",
}

// normalizeRiverName lowercases, trims, and strips common geographic suffixes
// so "Cache La Poudre" matches "Cache la Poudre River".
func normalizeRiverName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, sfx := range riverSuffixes {
		if strings.HasSuffix(s, sfx) {
			s = strings.TrimSuffix(s, sfx)
			break
		}
	}
	return strings.TrimSpace(s)
}

// NamesMatch reports whether two river names refer to the same waterway by
// comparing their normalized forms (suffixes stripped from both sides).
// Used to verify spatial-fallback results against the DB river name.
func NamesMatch(a, b string) bool {
	return normalizeRiverName(a) == normalizeRiverName(b)
}

// nhdFetchByWhere executes an NHD layer-6 query with retry (3 attempts, 2s backoff).
func nhdFetchByWhere(ctx context.Context, where string) ([]NHDNameResult, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*2) * time.Second)
		}
		results, err := nhdFetchByWhereOnce(ctx, where)
		if err == nil {
			return results, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// nhdFetchByWhereOnce executes a single NHD layer-6 query with the given WHERE
// clause and returns de-duplicated NHDNameResults.
func nhdFetchByWhereOnce(ctx context.Context, where string) ([]NHDNameResult, error) {
	params := url.Values{
		"where":             {where},
		"outFields":         {"gnis_id,gnis_name,reachcode"},
		"returnGeometry":    {"false"},
		"resultRecordCount": {"50"},
		"f":                 {"json"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nhdArcGISURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "h2oflows/1.0 (https://h2oflows.org)")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nhd name lookup: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Features []struct {
			Attributes struct {
				GnisID    string `json:"gnis_id"`
				GnisName  string `json:"gnis_name"`
				ReachCode string `json:"reachcode"`
			} `json:"attributes"`
		} `json:"features"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("nhd name lookup parse: %w", err)
	}

	seen := map[string]NHDNameResult{}
	for _, f := range result.Features {
		id := f.Attributes.GnisID
		if id == "" {
			continue
		}
		if _, ok := seen[id]; !ok {
			huc8 := ""
			if len(f.Attributes.ReachCode) >= 8 {
				huc8 = f.Attributes.ReachCode[:8]
			}
			seen[id] = NHDNameResult{GnisID: id, Name: f.Attributes.GnisName, HUC8: huc8}
		}
	}

	out := make([]NHDNameResult, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	return out, nil
}

// NHDLookupByName queries NHD layer 6 for flowlines matching name.
// Strategy:
//  1. Exact case-insensitive match on gnis_name.
//  2. If no results, LIKE query on gnis_name LIKE '%name%', then filter
//     candidates whose normalized name (suffixes stripped) equals the
//     normalized input — handles "Cache La Poudre" ↔ "Cache la Poudre River".
//
// Returns all distinct GNIS IDs found — callers check for uniqueness:
// 1 result = safe to backfill, >1 = ambiguous, 0 = no match.
func NHDLookupByName(ctx context.Context, name string) ([]NHDNameResult, error) {
	escaped := strings.ReplaceAll(name, "'", "''")

	// Pass 1: exact match.
	exact, err := nhdFetchByWhere(ctx, fmt.Sprintf("UPPER(gnis_name) = UPPER('%s')", escaped))
	if err != nil {
		return nil, err
	}
	if len(exact) > 0 {
		return exact, nil
	}

	// Pass 2: LIKE fallback, then normalize-filter.
	candidates, err := nhdFetchByWhere(ctx, fmt.Sprintf("UPPER(gnis_name) LIKE UPPER('%%%s%%')", escaped))
	if err != nil {
		return nil, err
	}
	// Only normalize the NHD name — our DB names never include type suffixes.
	normInput := strings.ToLower(strings.TrimSpace(name))
	var matched []NHDNameResult
	for _, c := range candidates {
		if normalizeRiverName(c.Name) == normInput {
			matched = append(matched, c)
		}
	}
	return matched, nil
}

// GNISLookupResult holds the coordinate and HUC8 derived from an NHD GNIS ID query.
type GNISLookupResult struct {
	Name  string
	Lat   float64
	Lng   float64
	HUC8  string // first 8 chars of reachcode
}

// NHDCoordByGNISID queries NHD layer 6 for the first flowline feature with the
// given GNIS ID and returns its first path vertex (WGS84), the GNIS name, and
// the HUC8 derived from the reachcode. Returns an error when no feature is found.
func NHDCoordByGNISID(ctx context.Context, gnisID string) (*GNISLookupResult, error) {
	params := url.Values{
		"where":           {fmt.Sprintf("gnis_id='%s'", gnisID)},
		"outFields":       {"gnis_id,gnis_name,reachcode"},
		"returnGeometry":  {"true"},
		"resultRecordCount": {"1"},
		"inSR":            {"4326"},
		"outSR":           {"4326"},
		"f":               {"json"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nhdArcGISURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "h2oflows/1.0 (https://h2oflows.org)")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nhd gnis lookup: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Features []struct {
			Attributes struct {
				GnisName  string `json:"gnis_name"`
				ReachCode string `json:"reachcode"`
			} `json:"attributes"`
			Geometry struct {
				Paths [][][]float64 `json:"paths"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("nhd gnis lookup parse: %w", err)
	}
	if len(result.Features) == 0 {
		return nil, fmt.Errorf("nhd: no feature found for GNIS ID %q", gnisID)
	}
	f := result.Features[0]
	if len(f.Geometry.Paths) == 0 || len(f.Geometry.Paths[0]) == 0 {
		return nil, fmt.Errorf("nhd: feature has no geometry for GNIS ID %q", gnisID)
	}
	pt := f.Geometry.Paths[0][0] // [lng, lat]
	if len(pt) < 2 {
		return nil, fmt.Errorf("nhd: malformed coordinate for GNIS ID %q", gnisID)
	}
	huc8 := ""
	if len(f.Attributes.ReachCode) >= 8 {
		huc8 = f.Attributes.ReachCode[:8]
	}
	return &GNISLookupResult{Name: f.Attributes.GnisName, Lat: pt[1], Lng: pt[0], HUC8: huc8}, nil
}
