// Package handlers — run_upload.go implements the #155 authenticated public
// run-upload API: X-API-Key authenticated single-or-bulk run creation
// (UploadRuns) and partial update (UploadUpdate). These sit alongside the
// JWT-authenticated /me/reaches and /me/runs handlers in user_reaches.go but
// are scoped to exactly one owner (the API key's owner) — no
// editableOwnerIDs special-account resolution.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/h2oflow/h2oflow/apps/api/internal/kmlimport"
)

// ── Shared DTOs ──────────────────────────────────────────────────────────────

type uploadLatLng struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type uploadGauge struct {
	ExternalID string `json:"external_id"`
	Source     string `json:"source"`
}

type uploadRapid struct {
	Name              string        `json:"name"`
	ClassRating       *float64      `json:"class_rating"`
	HazardType        string        `json:"hazard_type"`
	IsSurfWave        bool          `json:"is_surf_wave"`
	IsPermanentHazard bool          `json:"is_permanent_hazard"`
	Description       *string       `json:"description"`
	Location          *uploadLatLng `json:"location"`
}

type uploadAccess struct {
	Type     string        `json:"type"`
	Name     *string       `json:"name"`
	Notes    *string       `json:"notes"`
	Location *uploadLatLng `json:"location"`
}

type uploadRunInput struct {
	Name      string         `json:"name"`
	RiverName string         `json:"river_name"`
	GnisID    string         `json:"gnis_id"`
	PutIn     uploadLatLng   `json:"put_in"`
	TakeOut   uploadLatLng   `json:"take_out"`
	UpComID   string         `json:"up_comid"`
	DownComID string         `json:"down_comid"`
	ClassMin  *float64       `json:"class_min"`
	ClassMax  *float64       `json:"class_max"`
	Note      *string        `json:"note"`
	FlowBands *FlowBands     `json:"flow_bands"`
	Gauge     *uploadGauge   `json:"gauge"`
	Rapids    []uploadRapid  `json:"rapids"`
	Access    []uploadAccess `json:"access"`
}

// validAccessTypes is the human-facing list of accepted access_type values,
// mirroring editableAccessTypes (user_reaches.go). Kept as a literal for a
// clear validation error message.
const validAccessTypes = "camp, parking, boat_ramp, intermediate, shuttle_drop"

// validateUploadChildren rejects malformed rapids/access before any DB write so
// the uploader gets a loud, specific error rather than silently-dropped rows —
// note put_in/take_out are NOT valid access types here (they're owned by the
// run's put_in/take_out columns).
func validateUploadChildren(rapids []uploadRapid, access []uploadAccess) error {
	for _, rp := range rapids {
		if strings.TrimSpace(rp.Name) == "" {
			return fmt.Errorf("rapid name is required")
		}
	}
	for _, ap := range access {
		if !editableAccessTypes[ap.Type] {
			return fmt.Errorf("invalid access type %q (valid: %s)", ap.Type, validAccessTypes)
		}
	}
	return nil
}

// buildUploadCenterline resolves a centerline for a run being created or
// updated via the upload API: ComID-driven when the caller supplies both
// hints, point-snap-driven (NLDI) otherwise, trimmed to the exact put-in/
// take-out pins via PostGIS. ok=false means no centerline could be resolved
// (NLDI outage, unsnappable points, etc) — callers should store no centerline
// and let the 2-point straight-line fallback render.
func (h *UserReachHandler) buildUploadCenterline(ctx context.Context, upComID, downComID string, putIn, takeOut uploadLatLng) (geojson, up, down string, ok bool) {
	nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var raw string
	var err error
	if upComID != "" && downComID != "" {
		raw, err = kmlimport.FetchCenterlinePreview(nctx, upComID, downComID)
		up, down = upComID, downComID
	} else {
		raw, up, down, err = kmlimport.FetchCenterlineFromPoints(nctx, putIn.Lng, putIn.Lat, takeOut.Lng, takeOut.Lat)
	}
	if err != nil || raw == "" {
		return "", "", "", false
	}
	if trimmed, terr := trimLineGeoJSON(nctx, h.db, raw, putIn.Lng, putIn.Lat, takeOut.Lng, takeOut.Lat); terr == nil && trimmed != "" {
		raw = trimmed
	}
	return raw, up, down, true
}

// ── UploadRuns (POST /api/v1/upload/runs) ───────────────────────────────────

// UploadRuns creates one or many runs under the API key's owning account.
// The body is either a single run object or a JSON array of run objects.
// Each run is processed independently in its own transaction so one bad run
// in a batch never aborts the others; per-item errors are reported in
// "results" rather than failing the whole request.
func (h *UserReachHandler) UploadRuns(w http.ResponseWriter, r *http.Request) { //nolint:cyclop
	ownerID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	raw = bytes.TrimSpace(raw)

	var runs []uploadRunInput
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &runs); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	} else {
		var single uploadRunInput
		if err := json.Unmarshal(raw, &single); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		runs = []uploadRunInput{single}
	}
	if len(runs) == 0 {
		errorResponse(w, http.StatusBadRequest, "no runs in request")
		return
	}

	ctx := r.Context()

	// Batch cap. A brand-new account has no user_api_access row yet (it's
	// lazily created by recordRunWrite on first write below) — maxBatch
	// scans as the Go zero value in that case, so only enforce the cap once
	// a real quota row exists.
	var maxBatch int
	_ = h.db.QueryRow(ctx, `SELECT max_batch FROM user_api_access WHERE owner_id = $1`, ownerID).Scan(&maxBatch)
	if maxBatch > 0 && len(runs) > maxBatch {
		errorResponse(w, http.StatusBadRequest, fmt.Sprintf("batch of %d exceeds max_batch %d", len(runs), maxBatch))
		return
	}

	results := make([]map[string]any, len(runs))
	created, failed := 0, 0

	for i, in := range runs {
		name := strings.TrimSpace(in.Name)
		if name == "" {
			results[i] = map[string]any{"index": i, "status": "error", "error": "name is required"}
			failed++
			continue
		}
		if in.PutIn.Lat == 0 && in.PutIn.Lng == 0 {
			results[i] = map[string]any{"index": i, "status": "error", "error": "put_in coordinates required"}
			failed++
			continue
		}
		if in.TakeOut.Lat == 0 && in.TakeOut.Lng == 0 {
			results[i] = map[string]any{"index": i, "status": "error", "error": "take_out coordinates required"}
			failed++
			continue
		}

		if rlErr := recordRunWrite(ctx, h.db, ownerID); rlErr != nil {
			results[i] = map[string]any{"index": i, "status": "error", "error": "rate limit exceeded"}
			failed++
			for j := i + 1; j < len(runs); j++ {
				results[j] = map[string]any{"index": j, "status": "error", "error": "rate limit exceeded"}
				failed++
			}
			break
		}

		id, slug, centerlineLabel, cerr := h.createOneUploadRun(ctx, ownerID, name, in)
		if cerr != nil {
			results[i] = map[string]any{"index": i, "status": "error", "error": cerr.Error()}
			failed++
			continue
		}
		results[i] = map[string]any{
			"index":           i,
			"name":            name,
			"slug":            slug,
			"id":              id,
			"river_confirmed": false,
			"centerline":      centerlineLabel,
			"status":          "created",
		}
		created++
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"created": created,
		"failed":  failed,
		"results": results,
	})
}

// createOneUploadRun creates a single run for UploadRuns: resolves the river,
// builds a centerline, and writes the run plus its flow bands / rapids /
// access points / gauge link inside one transaction. Returns the new run's
// id, slug, and a "nldi"/"fallback" centerline label on success.
func (h *UserReachHandler) createOneUploadRun(ctx context.Context, ownerID, name string, in uploadRunInput) (id, slug, centerlineLabel string, err error) {
	// Validate children up front — fail fast before the slow NLDI call.
	if verr := validateUploadChildren(in.Rapids, in.Access); verr != nil {
		return "", "", "", verr
	}

	// Auto-assign or create river if river_name provided.
	var riverID *string
	var finalRiverName *string
	if rn := strings.TrimSpace(in.RiverName); rn != "" {
		finalRiverName = &rn
		if rid := resolveOrCreateRiver(ctx, h.db, rn, in.GnisID, in.PutIn.Lat, in.PutIn.Lng); rid != "" {
			riverID = &rid
		}
	}

	// Centerline: NLDI (from ComIDs or from snapped points) with a 2-point
	// fallback when NLDI can't resolve one.
	clGeoJSON, upC, downC, clOK := h.buildUploadCenterline(ctx, in.UpComID, in.DownComID, in.PutIn, in.TakeOut)
	centerlineLabel = "fallback"
	finalUpComID, finalDownComID := in.UpComID, in.DownComID
	if clOK {
		centerlineLabel = "nldi"
		finalUpComID, finalDownComID = upC, downC
	}

	// Generate a unique slug for this owner.
	baseSlug := kmlimport.Slugify(name)
	slug = baseSlug
	for i := 2; i <= 20; i++ {
		var existing string
		qerr := h.db.QueryRow(ctx, `SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`, ownerID, slug).Scan(&existing)
		if qerr != nil {
			break // no conflict
		}
		slug = fmt.Sprintf("%s-%d", baseSlug, i)
	}

	tx, err := h.db.Begin(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("tx begin failed: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := tx.QueryRow(ctx, `
		INSERT INTO user_reaches
			(owner_id, slug, name, river_id, river_name, put_in, take_out, up_comid, down_comid, note, class_min, class_max, visibility, published_at, river_confirmed)
		VALUES
			($1, $2, $3, $4, $5,
			 ST_SetSRID(ST_MakePoint($6, $7), 4326)::geography,
			 ST_SetSRID(ST_MakePoint($8, $9), 4326)::geography,
			 NULLIF($10,''), NULLIF($11,''), $12, $13, $14,
			 'public', NOW(), false)
		RETURNING id
	`, ownerID, slug, name, riverID, finalRiverName,
		in.PutIn.Lng, in.PutIn.Lat,
		in.TakeOut.Lng, in.TakeOut.Lat,
		finalUpComID, finalDownComID, in.Note,
		in.ClassMin, in.ClassMax,
	).Scan(&id); err != nil {
		return "", "", "", fmt.Errorf("create failed: %w", err)
	}

	if clOK {
		if _, err := tx.Exec(ctx,
			`UPDATE user_reaches SET centerline = ST_GeomFromGeoJSON($1)::geography WHERE id = $2`,
			clGeoJSON, id); err != nil {
			return "", "", "", fmt.Errorf("centerline update failed: %w", err)
		}
	}

	if fb := in.FlowBands; fb != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE user_reaches SET base_label = $1, base_color = $2 WHERE id = $3`,
			fb.BaseLabel, fb.BaseColor, id); err != nil {
			return "", "", "", fmt.Errorf("flow bands update failed: %w", err)
		}
		for _, t := range fb.Thresholds {
			if _, err := tx.Exec(ctx, `
				INSERT INTO user_reach_flow_ranges (user_reach_id, label, value, color)
				VALUES ($1, $2, $3, $4)
			`, id, t.Label, t.Value, t.Color); err != nil {
				return "", "", "", fmt.Errorf("flow range insert failed: %w", err)
			}
		}
	}

	for _, rp := range in.Rapids {
		rpName := strings.TrimSpace(rp.Name)
		if rpName == "" {
			continue // skip rapids with empty name rather than failing the whole run
		}
		var insertErr error
		if rp.Location != nil {
			_, insertErr = tx.Exec(ctx, `
				INSERT INTO rapids (user_reach_id, name, location, description, class_rating,
				                    is_surf_wave, is_permanent_hazard, hazard_type, data_source, verified)
				VALUES ($1, $2, ST_SetSRID(ST_MakePoint($3, $4), 4326)::geography,
				        NULLIF($5, ''), $6, $7, $8, NULLIF($9, ''), 'community', true)
			`, id, rpName, rp.Location.Lng, rp.Location.Lat, rp.Description, rp.ClassRating,
				rp.IsSurfWave, rp.IsPermanentHazard, rp.HazardType)
		} else {
			_, insertErr = tx.Exec(ctx, `
				INSERT INTO rapids (user_reach_id, name, description, class_rating,
				                    is_surf_wave, is_permanent_hazard, hazard_type, data_source, verified)
				VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, NULLIF($7, ''), 'community', true)
			`, id, rpName, rp.Description, rp.ClassRating,
				rp.IsSurfWave, rp.IsPermanentHazard, rp.HazardType)
		}
		if insertErr != nil {
			return "", "", "", fmt.Errorf("rapid insert failed: %w", insertErr)
		}
	}

	for _, ap := range in.Access {
		if !editableAccessTypes[ap.Type] {
			continue // bulk upload: skip disallowed/unknown access types rather than failing the run
		}
		var insertErr error
		if ap.Location != nil {
			_, insertErr = tx.Exec(ctx, `
				INSERT INTO reach_access (user_reach_id, access_type, name, location, notes, data_source, verified)
				VALUES ($1, $2, NULLIF($3, ''), ST_SetSRID(ST_MakePoint($4, $5), 4326)::geography,
				        NULLIF($6, ''), 'community', true)
			`, id, ap.Type, ap.Name, ap.Location.Lng, ap.Location.Lat, ap.Notes)
		} else {
			_, insertErr = tx.Exec(ctx, `
				INSERT INTO reach_access (user_reach_id, access_type, name, notes, data_source, verified)
				VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), 'community', true)
			`, id, ap.Type, ap.Name, ap.Notes)
		}
		if insertErr != nil {
			return "", "", "", fmt.Errorf("access point insert failed: %w", insertErr)
		}
	}

	if in.Gauge != nil && in.Gauge.ExternalID != "" && in.Gauge.Source != "" {
		var gaugeID string
		if gerr := tx.QueryRow(ctx,
			`SELECT id::text FROM gauges WHERE external_id = $1 AND source = $2`,
			in.Gauge.ExternalID, in.Gauge.Source,
		).Scan(&gaugeID); gerr == nil {
			if _, err := tx.Exec(ctx,
				`UPDATE user_reaches SET primary_gauge_id = $1::uuid, updated_at = NOW() WHERE id = $2`,
				gaugeID, id); err != nil {
				return "", "", "", fmt.Errorf("gauge link failed: %w", err)
			}
		}
		// No matching gauge: not an error, the link is simply skipped.
	}

	if err := tx.Commit(ctx); err != nil {
		return "", "", "", fmt.Errorf("commit failed: %w", err)
	}

	saveCompleteness(ctx, h.db, id)
	return id, slug, centerlineLabel, nil
}

// ── UploadUpdate (PATCH /api/v1/upload/runs/{slug}) ─────────────────────────

// UploadUpdate partially updates one of the API key owner's own runs. Unlike
// the JWT-authenticated Update, this never resolves editableOwnerIDs — API
// keys may only edit runs under their own account.
func (h *UserReachHandler) UploadUpdate(w http.ResponseWriter, r *http.Request) { //nolint:cyclop
	ownerID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()

	var reachID string
	if err := h.db.QueryRow(ctx,
		`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2 AND deleted_at IS NULL`,
		ownerID, slug,
	).Scan(&reachID); err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	if rlErr := recordRunWrite(ctx, h.db, ownerID); rlErr != nil {
		errorResponse(w, http.StatusTooManyRequests, rlErr.Error())
		return
	}

	var body struct {
		Name      *string         `json:"name"`
		NewSlug   *string         `json:"slug"`
		Note      *string         `json:"note"`
		RiverName *string         `json:"river_name"`
		GnisID    *string         `json:"gnis_id"`
		PutIn     *uploadLatLng   `json:"put_in"`
		TakeOut   *uploadLatLng   `json:"take_out"`
		UpComID   *string         `json:"up_comid"`
		DownComID *string         `json:"down_comid"`
		ClassMin  *float64        `json:"class_min"`
		ClassMax  *float64        `json:"class_max"`
		FlowBands *FlowBands      `json:"flow_bands"`
		Gauge     *uploadGauge    `json:"gauge"`
		Rapids    *[]uploadRapid  `json:"rapids"`
		Access    *[]uploadAccess `json:"access"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate any supplied rapids/access before touching the DB.
	var rapidsToCheck []uploadRapid
	if body.Rapids != nil {
		rapidsToCheck = *body.Rapids
	}
	var accessToCheck []uploadAccess
	if body.Access != nil {
		accessToCheck = *body.Access
	}
	if verr := validateUploadChildren(rapidsToCheck, accessToCheck); verr != nil {
		errorResponse(w, http.StatusBadRequest, verr.Error())
		return
	}

	// Treat empty river_name as NULL (clear it), matching Update's convention.
	var riverName *string
	if body.RiverName != nil && strings.TrimSpace(*body.RiverName) != "" {
		rn := strings.TrimSpace(*body.RiverName)
		riverName = &rn
	}

	// Validate + uniqueness-check a slug rename.
	var newSlug *string
	if body.NewSlug != nil && *body.NewSlug != slug {
		ns := strings.TrimSpace(*body.NewSlug)
		if !userReachSlugRe.MatchString(ns) {
			errorResponse(w, http.StatusBadRequest, "slug must be lowercase alphanumeric with hyphens")
			return
		}
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

	// Core non-geo update. Visibility is untouched — API-uploaded runs stay
	// public, unlike the JWT Update which also handles visibility/is_private.
	tag, err := h.db.Exec(ctx, `
		UPDATE user_reaches
		SET
			name       = COALESCE($3, name),
			slug       = CASE WHEN $4 THEN $5 ELSE slug END,
			note       = COALESCE($6, note),
			river_name = CASE WHEN $7 THEN $8 ELSE river_name END,
			class_min  = CASE WHEN $9 THEN $10::numeric ELSE class_min END,
			class_max  = CASE WHEN $11 THEN $12::numeric ELSE class_max END,
			updated_at = NOW(),
			last_modified_after_fork_at = CASE
				WHEN original_forked_at IS NOT NULL THEN NOW()
				ELSE last_modified_after_fork_at
			END
		WHERE owner_id = $1 AND slug = $2
	`, ownerID, slug, body.Name, newSlug != nil, newSlug, body.Note,
		body.RiverName != nil, riverName,
		body.ClassMin != nil, body.ClassMin,
		body.ClassMax != nil, body.ClassMax)
	if err != nil || tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}
	if newSlug != nil {
		slug = *newSlug
	}

	// Re-resolve river when river_name is being set, so state/basin/huc8 get populated.
	if riverName != nil {
		gnisID := ""
		if body.GnisID != nil {
			gnisID = strings.TrimSpace(*body.GnisID)
		}
		if rid := resolveOrCreateRiver(ctx, h.db, *riverName, gnisID, 0, 0); rid != "" {
			_, _ = h.db.Exec(ctx,
				`UPDATE user_reaches SET river_id = $3 WHERE owner_id = $1 AND slug = $2`,
				ownerID, slug, rid)
		}
	}

	if fb := body.FlowBands; fb != nil {
		_, _ = h.db.Exec(ctx,
			`UPDATE user_reaches SET base_label = $1, base_color = $2 WHERE id = $3`,
			fb.BaseLabel, fb.BaseColor, reachID)
		_, _ = h.db.Exec(ctx, `DELETE FROM user_reach_flow_ranges WHERE user_reach_id = $1`, reachID)
		for _, t := range fb.Thresholds {
			_, _ = h.db.Exec(ctx, `
				INSERT INTO user_reach_flow_ranges (user_reach_id, label, value, color)
				VALUES ($1, $2, $3, $4)
			`, reachID, t.Label, t.Value, t.Color)
		}
	}

	// Geometry recalc — only touched when the caller supplies a new put-in
	// and/or take-out point.
	centerlineRecomputed := false
	if body.PutIn != nil || body.TakeOut != nil {
		var curPutLat, curPutLng, curTakeLat, curTakeLng float64
		_ = h.db.QueryRow(ctx, `
			SELECT ST_Y(put_in::geometry), ST_X(put_in::geometry), ST_Y(take_out::geometry), ST_X(take_out::geometry)
			FROM user_reaches WHERE owner_id = $1 AND slug = $2
		`, ownerID, slug).Scan(&curPutLat, &curPutLng, &curTakeLat, &curTakeLng)

		newPut := uploadLatLng{Lat: curPutLat, Lng: curPutLng}
		if body.PutIn != nil {
			newPut = *body.PutIn
		}
		newTake := uploadLatLng{Lat: curTakeLat, Lng: curTakeLng}
		if body.TakeOut != nil {
			newTake = *body.TakeOut
		}

		upHint := ""
		if body.UpComID != nil {
			upHint = *body.UpComID
		}
		downHint := ""
		if body.DownComID != nil {
			downHint = *body.DownComID
		}

		cl, upC, downC, clOK := h.buildUploadCenterline(ctx, upHint, downHint, newPut, newTake)

		_, _ = h.db.Exec(ctx, `
			UPDATE user_reaches
			SET
				put_in          = ST_SetSRID(ST_MakePoint($3, $4), 4326)::geography,
				take_out        = ST_SetSRID(ST_MakePoint($5, $6), 4326)::geography,
				up_comid        = NULLIF($7, ''),
				down_comid      = NULLIF($8, ''),
				river_confirmed = false,
				updated_at      = NOW()
			WHERE owner_id = $1 AND slug = $2
		`, ownerID, slug, newPut.Lng, newPut.Lat, newTake.Lng, newTake.Lat, upC, downC)

		if clOK {
			_, _ = h.db.Exec(ctx,
				`UPDATE user_reaches SET centerline = ST_GeomFromGeoJSON($1)::geography WHERE owner_id = $2 AND slug = $3`,
				cl, ownerID, slug)
		} else {
			_, _ = h.db.Exec(ctx,
				`UPDATE user_reaches SET centerline = NULL WHERE owner_id = $1 AND slug = $2`,
				ownerID, slug)
		}
		centerlineRecomputed = clOK
	}

	// Rapids/access are POINTER-to-slice: nil (absent) leaves them untouched,
	// [] (present-but-empty) wholesale-clears them.
	if body.Rapids != nil {
		_, _ = h.db.Exec(ctx, `DELETE FROM rapids WHERE user_reach_id = $1`, reachID)
		for _, rp := range *body.Rapids {
			rpName := strings.TrimSpace(rp.Name)
			if rpName == "" {
				continue
			}
			if rp.Location != nil {
				_, _ = h.db.Exec(ctx, `
					INSERT INTO rapids (user_reach_id, name, location, description, class_rating,
					                    is_surf_wave, is_permanent_hazard, hazard_type, data_source, verified)
					VALUES ($1, $2, ST_SetSRID(ST_MakePoint($3, $4), 4326)::geography,
					        NULLIF($5, ''), $6, $7, $8, NULLIF($9, ''), 'community', true)
				`, reachID, rpName, rp.Location.Lng, rp.Location.Lat, rp.Description, rp.ClassRating,
					rp.IsSurfWave, rp.IsPermanentHazard, rp.HazardType)
			} else {
				_, _ = h.db.Exec(ctx, `
					INSERT INTO rapids (user_reach_id, name, description, class_rating,
					                    is_surf_wave, is_permanent_hazard, hazard_type, data_source, verified)
					VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, NULLIF($7, ''), 'community', true)
				`, reachID, rpName, rp.Description, rp.ClassRating,
					rp.IsSurfWave, rp.IsPermanentHazard, rp.HazardType)
			}
		}
	}

	if body.Access != nil {
		_, _ = h.db.Exec(ctx,
			`DELETE FROM reach_access WHERE user_reach_id = $1 AND access_type NOT IN ('put_in','take_out')`,
			reachID)
		for _, ap := range *body.Access {
			if !editableAccessTypes[ap.Type] {
				continue
			}
			if ap.Location != nil {
				_, _ = h.db.Exec(ctx, `
					INSERT INTO reach_access (user_reach_id, access_type, name, location, notes, data_source, verified)
					VALUES ($1, $2, NULLIF($3, ''), ST_SetSRID(ST_MakePoint($4, $5), 4326)::geography,
					        NULLIF($6, ''), 'community', true)
				`, reachID, ap.Type, ap.Name, ap.Location.Lng, ap.Location.Lat, ap.Notes)
			} else {
				_, _ = h.db.Exec(ctx, `
					INSERT INTO reach_access (user_reach_id, access_type, name, notes, data_source, verified)
					VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), 'community', true)
				`, reachID, ap.Type, ap.Name, ap.Notes)
			}
		}
	}

	// Gauge relink — same external_id+source lookup as UploadRuns' create path.
	if body.Gauge != nil && body.Gauge.ExternalID != "" && body.Gauge.Source != "" {
		var gaugeID string
		if gerr := h.db.QueryRow(ctx,
			`SELECT id::text FROM gauges WHERE external_id = $1 AND source = $2`,
			body.Gauge.ExternalID, body.Gauge.Source,
		).Scan(&gaugeID); gerr == nil {
			_, _ = h.db.Exec(ctx,
				`UPDATE user_reaches SET primary_gauge_id = $1::uuid, updated_at = NOW() WHERE id = $2`,
				gaugeID, reachID)
		}
	}

	saveCompleteness(ctx, h.db, reachID)

	var riverConfirmed bool
	_ = h.db.QueryRow(ctx, `SELECT river_confirmed FROM user_reaches WHERE owner_id = $1 AND slug = $2`,
		ownerID, slug).Scan(&riverConfirmed)

	jsonResponse(w, http.StatusOK, map[string]any{
		"slug":                  slug,
		"river_confirmed":       riverConfirmed,
		"centerline_recomputed": centerlineRecomputed,
	})
}
