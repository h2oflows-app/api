package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
)

// jsonResponse writes v as JSON with the given status code.
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorResponse writes a standard error JSON body.
func errorResponse(w http.ResponseWriter, status int, msg string) {
	jsonResponse(w, status, map[string]string{"error": msg})
}

// FeatureCollection is a GeoJSON FeatureCollection.
// MapLibre consumes this directly as a source — no frontend transformation needed.
type FeatureCollection struct {
	Type     string    `json:"type"`
	Features []Feature `json:"features"`
}

// Feature is a GeoJSON Feature. Geometry is any JSON-serialisable geometry
// (PointGeometry for gauge markers, rawGeometry for reach centerlines).
type Feature struct {
	Type       string         `json:"type"`
	Geometry   any            `json:"geometry"`
	Properties map[string]any `json:"properties"`
}

// PointGeometry is a GeoJSON Point. Coordinates are [longitude, latitude]
// per the GeoJSON spec (not lat/lng).
type PointGeometry struct {
	Type        string     `json:"type"`
	Coordinates [2]float64 `json:"coordinates"`
}

func newFeatureCollection(features []Feature) FeatureCollection {
	if features == nil {
		features = []Feature{} // never return null — MapLibre expects an array
	}
	return FeatureCollection{Type: "FeatureCollection", Features: features}
}

// ── Anon public_on_map gating (#314) ────────────────────────────────────────
//
// All runs are public (per Umbrella N Channels), but anonymous (unauthenticated)
// callers should only see special-user (curated/partner) accounts that have
// opted into public_on_map — not every community member's personal runs.
// Authenticated callers see the full existing result set, unchanged.

// isAnonymousCaller reports whether the request has no authenticated user
// attached to context — i.e. auth.Optional ran but found no valid bearer
// token (or the route has no auth middleware at all).
func isAnonymousCaller(r *http.Request) bool {
	_, ok := auth.UserIDFromContext(r.Context())
	return !ok
}

// specialPublicOnMapClause returns a self-contained SQL " AND <ownerCol> IN
// (...)" clause (no placeholders) restricting rows to special-user accounts
// that have opted into public_on_map. ownerCol must be a trusted, hardcoded
// column reference (e.g. "ur.owner_id") — never derived from user input.
func specialPublicOnMapClause(ownerCol string) string {
	return fmt.Sprintf(
		" AND %s IN (SELECT owner_id FROM user_profiles WHERE is_special AND public_on_map) ",
		ownerCol,
	)
}

// anonPublicOnMapFilter returns specialPublicOnMapClause(ownerCol) when the
// caller is anonymous, or "" (no added restriction) when authenticated.
func anonPublicOnMapFilter(r *http.Request, ownerCol string) string {
	if !isAnonymousCaller(r) {
		return ""
	}
	return specialPublicOnMapClause(ownerCol)
}
