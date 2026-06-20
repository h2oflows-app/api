// Package kmlimport imports reach features from a Google My Maps KML/KMZ export.
//
// Map conventions supported:
//   - Folder-per-reach maps: one folder per reach, folder name matched to DB
//   - Category-organized maps: folders named "Access Points", "Rivers", "Rapids"
//     with reach inferred by pin name + geographic proximity
//
// Pin name prefix → feature type:
//
//	"Rapid: <name>"    → rapids
//	"Wave: <name>"     → rapids (is_surf_wave=true)
//	"Surf: <name>"     → rapids (is_surf_wave=true)
//	"Put-in: <name>"   → reach_access type=put_in
//	"Take-out: <name>" → reach_access type=take_out
//	"Parking: <name>"  → reach_access type=parking
//	"Hazard: <name>"   → rapids (is_permanent_hazard=true)
//
// Hazard descriptions may include a hazard type keyword to classify them:
//
//	"low-head dam", "lowhead", "dam" → hazard_type="low_head_dam"
//	"rebar", "rebar/concrete"        → hazard_type="rebar"
//	"strainer"                        → hazard_type="strainer"
//	"bridge"                          → hazard_type="bridge_piling"
//	(default)                         → hazard_type="other"
package kmlimport

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ── Result types ─────────────────────────────────────────────────────────────

// Result summarises what was imported.
type Result struct {
	MapName string                   `json:"map_name"`
	Reaches map[string]*ReachResult  `json:"reaches"` // keyed by reach slug
	Log     []string                 `json:"log"`
}

// ReachResult holds per-reach counts.
type ReachResult struct {
	Name      string   `json:"name"`
	Rapids    int      `json:"rapids"`
	Hazards   int      `json:"hazards"`
	PutIns    int      `json:"put_ins"`
	TakeOuts  int      `json:"take_outs"`
	Parking   int      `json:"parking"`
	BoatRamps int      `json:"boat_ramps"`
	Campsites int      `json:"campsites"`
	Errors    []string `json:"errors,omitempty"`
}

// ── KML types ─────────────────────────────────────────────────────────────────

// KMLDoc is the parsed representation of a KML/KMZ file.
type KMLDoc struct {
	Name        string
	Description string // optional — may contain "Basin: South Platte" etc.
	Folders     []KMLFolder
}

// KMLFolder is a single layer/folder in the KML.
type KMLFolder struct {
	Name       string
	Placemarks []KMLPlacemark
}

// KMLPlacemark is a single pin or shape.
type KMLPlacemark struct {
	Name        string
	Description string
	Point       *KMLPoint      // nil for LineStrings/Polygons
	LineString  *KMLLineString // non-nil when placemark is a LineString
}

// KMLPoint holds the parsed coordinate string.
type KMLPoint struct {
	Coordinates string
}

// KMLLineString holds raw KML coordinates for a LineString placemark.
type KMLLineString struct {
	Coordinates string
}

// ── ParseKMLBytes ──────────────────────────────────────────────────────────────

// ParseKMLBytes parses a KML or KMZ file from raw bytes.
func ParseKMLBytes(data []byte) (*KMLDoc, error) {
	// KMZ is a ZIP archive — extract the first .kml file inside.
	if isZIP(data) {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("open kmz: %w", err)
		}
		for _, f := range zr.File {
			if strings.HasSuffix(strings.ToLower(f.Name), ".kml") {
				rc, err := f.Open()
				if err != nil {
					return nil, fmt.Errorf("open %s inside kmz: %w", f.Name, err)
				}
				data, err = io.ReadAll(rc)
				rc.Close()
				if err != nil {
					return nil, fmt.Errorf("read %s inside kmz: %w", f.Name, err)
				}
				break
			}
		}
	}

	type xmlPoint struct {
		Coordinates string `xml:"coordinates"`
	}
	type xmlLineString struct {
		Coordinates string `xml:"coordinates"`
	}
	type xmlPlacemark struct {
		Name        string         `xml:"name"`
		Description string         `xml:"description"`
		Point       *xmlPoint      `xml:"Point"`
		LineString  *xmlLineString `xml:"LineString"`
	}
	// xmlFolder is declared as a named type so it can reference itself for
	// nested sub-folders (Google My Maps sometimes wraps all reaches in one
	// outer folder; we need to recurse into those).
	type xmlFolder struct {
		Name       string         `xml:"name"`
		Placemarks []xmlPlacemark `xml:"Placemark"`
		SubFolders []xmlFolder    `xml:"Folder"`
	}
	type xmlDocument struct {
		Name        string      `xml:"name"`
		Description string      `xml:"description"`
		Folders     []xmlFolder `xml:"Folder"`
	}
	type xmlKML struct {
		Document xmlDocument `xml:"Document"`
	}

	var raw xmlKML
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&raw); err != nil {
		return nil, err
	}

	// convertPM converts a raw xmlPlacemark to a KMLPlacemark.
	convertPM := func(xp xmlPlacemark) KMLPlacemark {
		pm := KMLPlacemark{
			Name:        strings.TrimSpace(xp.Name),
			Description: StripHTML(strings.TrimSpace(xp.Description)),
		}
		if xp.Point != nil {
			pm.Point = &KMLPoint{Coordinates: strings.TrimSpace(xp.Point.Coordinates)}
		}
		if xp.LineString != nil {
			pm.LineString = &KMLLineString{Coordinates: strings.TrimSpace(xp.LineString.Coordinates)}
		}
		return pm
	}

	// flattenFolders recursively collects reach folders.
	// A folder that has sub-folders but no placemarks is treated as a wrapper
	// and replaced by its children (one-level KML nesting is common in
	// Google My Maps exports where the user organises reaches inside a layer).
	// A folder that has placemarks (with or without sub-folders) is kept as-is;
	// any sub-folders it also has are then flattened separately.
	var flattenFolders func([]xmlFolder) []KMLFolder
	flattenFolders = func(folders []xmlFolder) []KMLFolder {
		var out []KMLFolder
		for _, xf := range folders {
			hasPins     := len(xf.Placemarks) > 0
			hasSubs     := len(xf.SubFolders) > 0

			if hasPins {
				// This folder has placemarks — treat it as a reach folder.
				kf := KMLFolder{Name: xf.Name}
				for _, xp := range xf.Placemarks {
					kf.Placemarks = append(kf.Placemarks, convertPM(xp))
				}
				out = append(out, kf)
			}
			if hasSubs {
				// Recurse into sub-folders (whether or not this folder also had pins).
				out = append(out, flattenFolders(xf.SubFolders)...)
			}
		}
		return out
	}

	doc := &KMLDoc{
		Name:        raw.Document.Name,
		Description: StripHTML(strings.TrimSpace(raw.Document.Description)),
	}
	doc.Folders = flattenFolders(raw.Document.Folders)
	return doc, nil
}

// isZIP checks the ZIP magic bytes.
func isZIP(data []byte) bool {
	return len(data) >= 4 && data[0] == 'P' && data[1] == 'K' && data[2] == 0x03 && data[3] == 0x04
}

// kmlCoordsToLineStringGeoJSON converts a KML coordinate string (space/newline-
// separated "lng,lat,ele" triples) to a GeoJSON LineString JSON string.
func kmlCoordsToLineStringGeoJSON(raw string) (string, error) {
	var coords [][2]float64
	for _, token := range strings.Fields(raw) {
		parts := strings.Split(token, ",")
		if len(parts) < 2 {
			continue
		}
		lng, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		lat, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err1 != nil || err2 != nil {
			continue
		}
		coords = append(coords, [2]float64{lng, lat})
	}
	if len(coords) < 2 {
		return "", fmt.Errorf("LineString has fewer than 2 valid coordinates")
	}
	// Build GeoJSON manually to avoid importing encoding/json at the top level.
	var sb strings.Builder
	sb.WriteString(`{"type":"LineString","coordinates":[`)
	for i, c := range coords {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(fmt.Sprintf("[%g,%g]", c[0], c[1]))
	}
	sb.WriteString(`]}`)
	return sb.String(), nil
}

// ── Importer ──────────────────────────────────────────────────────────────────

// Importer runs KML imports against a live database pool.
type Importer struct {
	pool    *pgxpool.Pool
	DryRun  bool
	cleared map[string]bool // reaches whose import data has been cleared this run
}

// New creates a new Importer.
func New(pool *pgxpool.Pool, dryRun bool) *Importer {
	return &Importer{pool: pool, DryRun: dryRun, cleared: map[string]bool{}}
}

// reachStats returns-or-creates a ReachResult for the given reach.
func (res *Result) reachStats(slug, name string) *ReachResult {
	if _, ok := res.Reaches[slug]; !ok {
		res.Reaches[slug] = &ReachResult{Name: name}
	}
	return res.Reaches[slug]
}


// slugify converts a display name to a URL-safe slug.
// "Browns Canyon" → "browns-canyon", "Cache La Poudre" → "cache-la-poudre"
// Slugify converts a string to a URL-safe slug. Exported so admin handlers
// can generate consistent slugs without re-implementing the logic.
func Slugify(s string) string { return slugify(s) }

func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}


// ── DB upserts ────────────────────────────────────────────────────────────────

// stripClassSuffix removes a trailing "(IV+)" / "(III)" style class annotation
// from a rapid name so "Phone Boof (IV)" is stored as "Phone Boof".
func stripClassSuffix(name string) string {
	open := strings.LastIndex(name, "(")
	close := strings.LastIndex(name, ")")
	if open < 0 || close != len(name)-1 {
		return name
	}
	inner := strings.TrimSpace(name[open+1 : close])
	// Only strip if the parenthetical looks like a class rating.
	if ParseClassRating(inner) != nil {
		return strings.TrimSpace(name[:open])
	}
	return name
}

// inferHazardType classifies a permanent hazard from its name/description text.
func inferHazardType(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "low-head") || strings.Contains(lower, "lowhead") ||
		strings.Contains(lower, "low head") || strings.Contains(lower, "weir"):
		return "low_head_dam"
	case strings.Contains(lower, "dam"):
		return "dam"
	case strings.Contains(lower, "rebar") || strings.Contains(lower, "rebar/concrete") ||
		strings.Contains(lower, "rebar / concrete"):
		return "rebar"
	case strings.Contains(lower, "strainer"):
		return "strainer"
	case strings.Contains(lower, "bridge") || strings.Contains(lower, "piling"):
		return "bridge_piling"
	default:
		return "other"
	}
}

// ── Parsing helpers ───────────────────────────────────────────────────────────

// SplitPrefixWithHint wraps SplitPrefix with folder-name and description hints.
func SplitPrefixWithHint(name, description, folderHint string) (prefix, rest string) {
	prefix, rest = SplitPrefix(name)
	if prefix != "" {
		return
	}
	descLower := strings.ToLower(description)
	switch {
	case strings.Contains(descLower, "boat ramp") || strings.Contains(descLower, "boat launch") ||
		strings.Contains(descLower, "launch ramp"):
		return "boat-ramp", name
	case strings.Contains(descLower, "parking") || strings.Contains(descLower, "can park") ||
		strings.Contains(descLower, "park as well") || strings.Contains(descLower, "park here"):
		return "parking", name
	case strings.Contains(descLower, "take-out") || strings.Contains(descLower, "takeout") ||
		strings.Contains(descLower, "take out"):
		return "take-out", name
	case strings.Contains(descLower, "put-in") || strings.Contains(descLower, "put in") ||
		strings.Contains(descLower, "put_in"):
		return "put-in", name
	case strings.Contains(descLower, "surf wave") || strings.Contains(descLower, "surf spot") ||
		strings.Contains(descLower, "surfable") || strings.Contains(descLower, "play wave"):
		return "wave", name
	case strings.Contains(descLower, "class") || strings.Contains(descLower, "line is") ||
		strings.Contains(descLower, "boof") || strings.Contains(descLower, "ledge"):
		return "rapid", name
	}
	switch strings.ToLower(folderHint) {
	case "rapids", "waves", "surf waves":
		return "rapid", name
	case "access points", "access":
		return "put-in", name
	case "hazards", "permanent hazards":
		return "hazard", name
	case "campsites", "camps", "camping":
		return "campsite", name
	}
	return "", name
}

// SplitPrefix splits "Rapid: Zoom Flume" → ("rapid", "Zoom Flume").
func SplitPrefix(name string) (prefix, rest string) {
	lower := strings.ToLower(name)
	for _, p := range []string{"Rapid", "Wave", "Surf", "Put-in", "Take-out", "Parking", "Boat Ramp", "Hazard", "Campsite"} {
		if strings.HasPrefix(lower, strings.ToLower(p)+":") {
			prefix := strings.ToLower(p)
			if prefix == "surf" {
				prefix = "wave"
			}
			if prefix == "boat ramp" {
				prefix = "boat-ramp"
			}
			return prefix, strings.TrimSpace(name[len(p)+1:])
		}
	}
	switch {
	case strings.Contains(lower, "put-in") || strings.Contains(lower, "put in") ||
		strings.Contains(lower, "putin") || strings.Contains(lower, "put_in"):
		return "put-in", name
	case strings.Contains(lower, "take-out") || strings.Contains(lower, "takeout") ||
		strings.Contains(lower, "take out") || strings.Contains(lower, "takout"):
		return "take-out", name
	case strings.Contains(lower, "parking") || strings.Contains(lower, "trailhead"):
		return "parking", name
	case strings.HasPrefix(lower, "boat ramp") || strings.HasSuffix(lower, "boat ramp"):
		return "boat-ramp", name
	case strings.Contains(lower, "surf wave") || strings.Contains(lower, "play wave") ||
		strings.Contains(lower, "surf spot"):
		return "wave", name
	case strings.Contains(lower, "rapid") || strings.Contains(lower, "falls") ||
		strings.Contains(lower, "drop") || strings.Contains(lower, "hole"):
		return "rapid", name
	}
	return "", name
}

// ParseCoords parses "lon,lat[,alt]" from a KML coordinates string.
func ParseCoords(raw string) (lon, lat float64, ok bool) {
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		return 0, 0, false
	}
	fields := strings.Split(parts[0], ",")
	if len(fields) < 2 {
		return 0, 0, false
	}
	lon, err1 := strconv.ParseFloat(fields[0], 64)
	lat, err2 := strconv.ParseFloat(fields[1], 64)
	return lon, lat, err1 == nil && err2 == nil
}

// ParseClassRating extracts a numeric class rating from one or more text fields.
// Priority: parenthesized notation "(IV+)", "(III-)" > "class III+" prefix.
// Accepts variadic strings so callers can pass both name and description.
func ParseClassRating(texts ...string) *float64 {
	lower := strings.ToLower(strings.Join(texts, " "))
	if v := parseParenClass(lower); v != nil {
		return v
	}
	return parseClassPrefix(lower)
}

// parseParenClass finds the first "(IV+)" / "(III-)" / "(III)" style annotation.
func parseParenClass(lower string) *float64 {
	for i := 0; i < len(lower); i++ {
		if lower[i] != '(' {
			continue
		}
		rest := lower[i+1:]
		if strings.HasPrefix(rest, "class ") {
			rest = rest[6:]
		}
		var base float64
		var eaten int
		switch {
		case strings.HasPrefix(rest, "v"):
			base, eaten = 5, 1
		case strings.HasPrefix(rest, "iv"):
			base, eaten = 4, 2
		case strings.HasPrefix(rest, "iii"):
			base, eaten = 3, 3
		case strings.HasPrefix(rest, "ii"):
			base, eaten = 2, 2
		case strings.HasPrefix(rest, "i"):
			base, eaten = 1, 1
		default:
			continue
		}
		rest = rest[eaten:]
		if strings.HasPrefix(rest, "+") {
			base += 0.5
			rest = rest[1:]
		} else if strings.HasPrefix(rest, "-") {
			base -= 0.5
			rest = rest[1:]
		}
		if strings.HasPrefix(rest, ")") {
			return &base
		}
	}
	return nil
}

// parseClassPrefix finds "class III+" / "class IV-" style annotations.
func parseClassPrefix(lower string) *float64 {
	idx := strings.Index(lower, "class ")
	if idx < 0 {
		return nil
	}
	rest := lower[idx+6:]
	var base float64
	switch {
	case strings.HasPrefix(rest, "v"):
		base, rest = 5, rest[1:]
	case strings.HasPrefix(rest, "iv"):
		base, rest = 4, rest[2:]
	case strings.HasPrefix(rest, "iii"):
		base, rest = 3, rest[3:]
	case strings.HasPrefix(rest, "ii"):
		base, rest = 2, rest[2:]
	case strings.HasPrefix(rest, "i"):
		base, rest = 1, rest[1:]
	default:
		return nil
	}
	if strings.HasPrefix(rest, "+") {
		base += 0.5
	} else if strings.HasPrefix(rest, "-") {
		base -= 0.5
	}
	return &base
}

// StripHTML removes basic HTML tags from Google Maps description fields.
func StripHTML(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// ── User-reach KML import ─────────────────────────────────────────────────────

// pinTarget routes a pin write to either a curated reach or a user reach.
// col is "reach_id" or "user_reach_id" — set by this package, never from user input.
type pinTarget struct {
	col string
	id  string
}

func curatedTarget(reachID string) pinTarget    { return pinTarget{"reach_id", reachID} }
func userReachTarget(urID string) pinTarget     { return pinTarget{"user_reach_id", urID} }

// ImportForUserReach imports all pin placemarks from doc into a single user reach.
// No folder structure is required — all point placemarks across all folders are
// treated as pins for the target reach. Metadata placemarks (flow ranges, gauge,
// description, etc.) are ignored; those are managed through the user reach UI.
func (imp *Importer) ImportForUserReach(ctx context.Context, ownerID, reachSlug string, doc *KMLDoc) (*Result, error) {
	var urID, urName string
	if err := imp.pool.QueryRow(ctx,
		`SELECT id, name FROM user_reaches WHERE owner_id = $1 AND slug = $2`,
		ownerID, reachSlug,
	).Scan(&urID, &urName); err != nil {
		return nil, fmt.Errorf("user reach %q not found for owner", reachSlug)
	}

	res := &Result{
		MapName: doc.Name,
		Reaches: map[string]*ReachResult{},
	}
	target := userReachTarget(urID)

	// Collect all point placemarks regardless of folder structure.
	var pins []KMLPlacemark
	for _, folder := range doc.Folders {
		for _, pm := range folder.Placemarks {
			if pm.Point != nil {
				pins = append(pins, pm)
			}
		}
	}

	if len(pins) == 0 {
		res.Log = append(res.Log, "⚠  no point placemarks found in document")
		return res, nil
	}

	// Clear prior import-sourced data for this user reach.
	if !imp.cleared[urID] {
		if err := imp.clearImportDataForTarget(ctx, target); err != nil {
			res.Log = append(res.Log, fmt.Sprintf("⚠  clear failed: %v", err))
		} else {
			imp.cleared[urID] = true
			res.Log = append(res.Log, fmt.Sprintf("↺  [%s] cleared previous import data", urName))
		}
	}

	st := res.reachStats(reachSlug, urName)

	for _, pm := range pins {
		lon, lat, ok := ParseCoords(pm.Point.Coordinates)
		if !ok {
			res.Log = append(res.Log, fmt.Sprintf("⚠  %q — bad coordinates", pm.Name))
			continue
		}
		prefix, pinName := SplitPrefixWithHint(pm.Name, pm.Description, "")
		desc := strings.TrimSpace(pm.Description)

		switch prefix {
		case "rapid", "wave":
			isSurf := prefix == "wave"
			if err := imp.upsertRapidForTarget(ctx, target, pinName, desc, isSurf, false, "", lon, lat); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("rapid %q: %v", pinName, err))
				res.Log = append(res.Log, fmt.Sprintf("✗ [%s] rapid %q: %v", urName, pinName, err))
			} else {
				st.Rapids++
				res.Log = append(res.Log, fmt.Sprintf("✓ [%s] %s: %s", urName, prefix, pinName))
			}
		case "hazard":
			htype := inferHazardType(desc + " " + pinName)
			if err := imp.upsertRapidForTarget(ctx, target, pinName, desc, false, true, htype, lon, lat); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("hazard %q: %v", pinName, err))
				res.Log = append(res.Log, fmt.Sprintf("✗ [%s] hazard %q: %v", urName, pinName, err))
			} else {
				st.Hazards++
				res.Log = append(res.Log, fmt.Sprintf("✓ [%s] hazard (%s): %s", urName, htype, pinName))
			}
		case "put-in":
			if err := imp.upsertAccessForTarget(ctx, target, "put_in", pinName, desc, lon, lat); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("put-in %q: %v", pinName, err))
				res.Log = append(res.Log, fmt.Sprintf("✗ [%s] put-in %q: %v", urName, pinName, err))
			} else {
				st.PutIns++
				res.Log = append(res.Log, fmt.Sprintf("✓ [%s] put-in: %s", urName, pinName))
			}
		case "take-out":
			if err := imp.upsertAccessForTarget(ctx, target, "take_out", pinName, desc, lon, lat); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("take-out %q: %v", pinName, err))
				res.Log = append(res.Log, fmt.Sprintf("✗ [%s] take-out %q: %v", urName, pinName, err))
			} else {
				st.TakeOuts++
				res.Log = append(res.Log, fmt.Sprintf("✓ [%s] take-out: %s", urName, pinName))
			}
		case "parking":
			if err := imp.upsertParkingForTarget(ctx, target, pinName, desc, lon, lat); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("parking %q: %v", pinName, err))
				res.Log = append(res.Log, fmt.Sprintf("✗ [%s] parking %q: %v", urName, pinName, err))
			} else {
				st.Parking++
				res.Log = append(res.Log, fmt.Sprintf("✓ [%s] parking: %s", urName, pinName))
			}
		case "campsite":
			if err := imp.upsertAccessForTarget(ctx, target, "camp", pinName, desc, lon, lat); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("campsite %q: %v", pinName, err))
				res.Log = append(res.Log, fmt.Sprintf("✗ [%s] campsite %q: %v", urName, pinName, err))
			} else {
				st.Campsites++
				res.Log = append(res.Log, fmt.Sprintf("✓ [%s] campsite: %s", urName, pinName))
			}
		case "boat-ramp":
			if err := imp.upsertAccessForTarget(ctx, target, "boat_ramp", pinName, desc, lon, lat); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("boat-ramp %q: %v", pinName, err))
				res.Log = append(res.Log, fmt.Sprintf("✗ [%s] boat-ramp %q: %v", urName, pinName, err))
			} else {
				st.BoatRamps++
				res.Log = append(res.Log, fmt.Sprintf("✓ [%s] boat-ramp: %s", urName, pinName))
			}
		default:
			res.Log = append(res.Log, fmt.Sprintf("⚠  [%s] %q — unknown type, skipping", urName, pm.Name))
		}
	}

	return res, nil
}

func (imp *Importer) upsertRapidForTarget(ctx context.Context, t pinTarget, name, desc string, isSurfWave, isPermanentHazard bool, hazardType string, lon, lat float64) error {
	if imp.DryRun {
		return nil
	}
	classRating := ParseClassRating(name, desc)
	cleanName := stripClassSuffix(name)
	tag, err := imp.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE rapids
		SET location             = ST_SetSRID(ST_MakePoint($3, $4), 4326)::geography,
		    description          = CASE WHEN $5 <> '' THEN $5 ELSE description END,
		    class_rating         = CASE WHEN $6::numeric IS NOT NULL THEN $6::numeric ELSE class_rating END,
		    is_surf_wave         = is_surf_wave OR $7,
		    is_permanent_hazard  = is_permanent_hazard OR $8,
		    hazard_type          = CASE WHEN $9 <> '' THEN $9 ELSE hazard_type END
		WHERE %s = $1 AND LOWER(name) = LOWER($2)
	`, t.col), t.id, cleanName, lon, lat, desc, classRating, isSurfWave, isPermanentHazard, hazardType)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		_, err = imp.pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO rapids (%s, name, location, description, class_rating,
			                    is_surf_wave, is_permanent_hazard, hazard_type,
			                    data_source, verified)
			VALUES ($1, $2, ST_SetSRID(ST_MakePoint($3, $4), 4326)::geography,
			        NULLIF($5,''), $6::numeric, $7, $8, NULLIF($9,''), 'import', true)
			ON CONFLICT (%s, name) WHERE %s IS NOT NULL DO UPDATE
			  SET location            = EXCLUDED.location,
			      description         = COALESCE(EXCLUDED.description, rapids.description),
			      class_rating        = COALESCE(EXCLUDED.class_rating, rapids.class_rating),
			      is_surf_wave        = rapids.is_surf_wave OR EXCLUDED.is_surf_wave,
			      is_permanent_hazard = rapids.is_permanent_hazard OR EXCLUDED.is_permanent_hazard,
			      hazard_type         = COALESCE(EXCLUDED.hazard_type, rapids.hazard_type)
		`, t.col, t.col, t.col), t.id, cleanName, lon, lat, desc, classRating, isSurfWave, isPermanentHazard, hazardType)
	}
	return err
}

func (imp *Importer) upsertAccessForTarget(ctx context.Context, t pinTarget, accessType, name, notes string, lon, lat float64) error {
	if imp.DryRun {
		return nil
	}
	_, err := imp.pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO reach_access
			(%s, access_type, name, notes,
			 location, data_source, verified)
		VALUES
			($1, $2, $3, NULLIF($4, ''),
			 ST_SetSRID(ST_MakePoint($5, $6), 4326)::geography, 'import', true)
		ON CONFLICT (%s, access_type, name) WHERE %s IS NOT NULL DO UPDATE
		  SET location = EXCLUDED.location,
		      notes    = COALESCE(EXCLUDED.notes, reach_access.notes),
		      verified = true
	`, t.col, t.col, t.col), t.id, accessType, name, notes, lon, lat)
	return err
}

func (imp *Importer) upsertParkingForTarget(ctx context.Context, t pinTarget, name, notes string, lon, lat float64) error {
	if imp.DryRun {
		return nil
	}
	_, err := imp.pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO reach_access
			(%s, access_type, name, notes,
			 location, parking_location, data_source, verified)
		VALUES
			($1, 'parking', $2, NULLIF($3, ''),
			 ST_SetSRID(ST_MakePoint($4, $5), 4326)::geography,
			 ST_SetSRID(ST_MakePoint($4, $5), 4326)::geography,
			 'import', true)
		ON CONFLICT (%s, access_type, name) WHERE %s IS NOT NULL DO UPDATE
		  SET location         = EXCLUDED.location,
		      parking_location = EXCLUDED.parking_location,
		      notes            = COALESCE(EXCLUDED.notes, reach_access.notes),
		      verified         = true
	`, t.col, t.col, t.col), t.id, name, notes, lon, lat)
	return err
}

func (imp *Importer) clearImportDataForTarget(ctx context.Context, t pinTarget) error {
	if imp.DryRun {
		return nil
	}
	if _, err := imp.pool.Exec(ctx,
		fmt.Sprintf(`DELETE FROM rapids WHERE %s = $1 AND data_source IN ('import', 'ai_seed')`, t.col),
		t.id,
	); err != nil {
		return err
	}
	_, err := imp.pool.Exec(ctx,
		fmt.Sprintf(`DELETE FROM reach_access WHERE %s = $1 AND data_source IN ('import', 'ai_seed')`, t.col),
		t.id,
	)
	return err
}
