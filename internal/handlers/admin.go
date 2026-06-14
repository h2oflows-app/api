package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	gauge "github.com/h2oflow/h2oflow/apps/api/internal/gaugecore"
	"github.com/h2oflow/h2oflow/apps/api/internal/kmlimport"
	"github.com/h2oflow/h2oflow/apps/api/internal/nldi"
	"github.com/jackc/pgx/v5/pgxpool"
)

// adminPoller is the subset of poller.Poller used by admin gauge actions.
type adminPoller interface {
	FetchNowIfStale(ctx context.Context, gaugeID string, maxAge time.Duration) bool
}

type AdminHandler struct {
	db     *pgxpool.Pool
	poller adminPoller
}

func NewAdminHandler(db *pgxpool.Pool) *AdminHandler {
	return &AdminHandler{db: db}
}

func (h *AdminHandler) WithPoller(p adminPoller) *AdminHandler {
	h.poller = p
	return h
}

// SlugCheck reports whether a reach slug is available (not already taken).
// GET /api/v1/admin/slug-check?slug=…&exclude=…
// exclude is the caller's current slug so an in-place rename doesn't self-conflict.
func (h *AdminHandler) SlugCheck(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("slug")
	if slug == "" {
		errorResponse(w, http.StatusBadRequest, "slug is required")
		return
	}
	exclude := r.URL.Query().Get("exclude")
	var exists bool
	_ = h.db.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM reaches WHERE slug = $1 AND slug != $2)`,
		slug, exclude,
	).Scan(&exists)
	jsonResponse(w, http.StatusOK, map[string]bool{"available": !exists})
}

// ── Rivers ────────────────────────────────────────────────────────────────────

// ListRivers returns all rivers with their reach count.
// GET /api/v1/admin/rivers
func (h *AdminHandler) ListRivers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(), `
		SELECT rv.id, rv.slug, rv.name, rv.gnis_id, rv.basin, rv.state_abbr,
		       COUNT(re.id) AS reach_count,
		       COUNT(g.id) FILTER (WHERE g.poll_health = 'degraded')    AS gauges_degraded,
		       COUNT(g.id) FILTER (WHERE g.poll_health = 'stale')       AS gauges_stale,
		       COUNT(g.id) FILTER (WHERE g.poll_health = 'unreachable') AS gauges_unreachable
		FROM rivers rv
		LEFT JOIN reaches re ON re.river_id = rv.id
		LEFT JOIN gauges g ON g.id = re.primary_gauge_id
		GROUP BY rv.id
		ORDER BY rv.name
	`)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type River struct {
		ID                string  `json:"id"`
		Slug              string  `json:"slug"`
		Name              string  `json:"name"`
		GNISID            *string `json:"gnis_id"`
		Basin             *string `json:"basin"`
		StateAbbr         *string `json:"state_abbr"`
		ReachCount        int     `json:"reach_count"`
		GaugesDegraded    int     `json:"gauges_degraded"`
		GaugesStale       int     `json:"gauges_stale"`
		GaugesUnreachable int     `json:"gauges_unreachable"`
	}

	rivers := make([]River, 0)
	for rows.Next() {
		var rv River
		if err := rows.Scan(
			&rv.ID, &rv.Slug, &rv.Name, &rv.GNISID, &rv.Basin, &rv.StateAbbr,
			&rv.ReachCount,
			&rv.GaugesDegraded, &rv.GaugesStale, &rv.GaugesUnreachable,
		); err != nil {
			continue
		}
		rivers = append(rivers, rv)
	}
	jsonResponse(w, http.StatusOK, rivers)
}

// GetRiver returns a single river and its reaches.
// GET /api/v1/admin/rivers/{riverSlug}
func (h *AdminHandler) GetRiver(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "riverSlug")

	type Reach struct {
		ID            string   `json:"id"`
		Slug          string   `json:"slug"`
		Name          string   `json:"name"`
		CommonName    *string  `json:"common_name"`
		ClassMin      *float64 `json:"class_min"`
		ClassMax      *float64 `json:"class_max"`
		HasCenterline bool     `json:"has_centerline"`
		StateAbbr     *string  `json:"state_abbr"`
		RiverOrder    *int16   `json:"river_order"`
	}
	type RiverDetail struct {
		ID        string  `json:"id"`
		Slug      string  `json:"slug"`
		Name      string  `json:"name"`
		GNISID    *string `json:"gnis_id"`
		Basin     *string `json:"basin"`
		StateAbbr *string `json:"state_abbr"`
		HUC8      *string `json:"huc8"`
		Reaches   []Reach `json:"reaches"`
	}

	var rv RiverDetail
	err := h.db.QueryRow(r.Context(), `
		SELECT id, slug, name, gnis_id, basin, state_abbr, huc8 FROM rivers WHERE slug = $1
	`, slug).Scan(&rv.ID, &rv.Slug, &rv.Name, &rv.GNISID, &rv.Basin, &rv.StateAbbr, &rv.HUC8)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "river not found")
		return
	}

	// Pull the HUC-derived basin from the primary gauge of any linked reach.
	// This lets the admin compare the system-derived value against the stored one.
	rows, err := h.db.Query(r.Context(), `
		SELECT id, slug, name, common_name, class_min, class_max,
		       (centerline IS NOT NULL) AS has_centerline, state_abbr, river_order
		FROM reaches
		WHERE river_id = $1
		ORDER BY river_order NULLS LAST, name
	`, rv.ID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	rv.Reaches = make([]Reach, 0)
	for rows.Next() {
		var re Reach
		if err := rows.Scan(&re.ID, &re.Slug, &re.Name, &re.CommonName, &re.ClassMin, &re.ClassMax, &re.HasCenterline, &re.StateAbbr, &re.RiverOrder); err != nil {
			continue
		}
		rv.Reaches = append(rv.Reaches, re)
	}
	jsonResponse(w, http.StatusOK, rv)
}

// CreateRiver creates a new river from a GNIS ID.
// POST /api/v1/admin/rivers
// Only accepts gnis_id — name, state, basin, huc8 are derived from NHD.
func (h *AdminHandler) CreateRiver(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GnisID string `json:"gnis_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.GnisID = strings.TrimSpace(body.GnisID)
	if body.GnisID == "" {
		errorResponse(w, http.StatusBadRequest, "gnis_id is required")
		return
	}

	ctx := r.Context()
	coord, err := nldi.NHDCoordByGNISID(ctx, body.GnisID)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, fmt.Sprintf("GNIS ID not found in NHD: %v", err))
		return
	}
	if coord.Name == "" {
		errorResponse(w, http.StatusBadRequest, "NHD feature has no GNIS name")
		return
	}

	stateAbbr, basin, huc8 := riverMetaFromGNIS(ctx, body.GnisID)
	slug := kmlimport.Slugify(coord.Name)

	var id string
	dbErr := h.db.QueryRow(ctx, `
		INSERT INTO rivers (slug, name, gnis_id, state_abbr, basin, huc8)
		VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), NULLIF($6,''))
		RETURNING id
	`, slug, coord.Name, body.GnisID, stateAbbr, basin, huc8).Scan(&id)
	if dbErr != nil {
		errorResponse(w, http.StatusConflict, "river already exists (GNIS ID or slug conflict)")
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]any{
		"id":         id,
		"slug":       slug,
		"name":       coord.Name,
		"state_abbr": stateAbbr,
		"basin":      basin,
		"huc8":       huc8,
	})
}

// DeleteRiver permanently deletes a river and unlinks its reaches.
// Reaches are NOT deleted — they remain but lose their river association.
// DELETE /api/v1/admin/rivers/{riverSlug}
func (h *AdminHandler) DeleteRiver(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "riverSlug")

	// Unlink reaches first so the FK constraint doesn't block deletion.
	if _, err := h.db.Exec(r.Context(),
		`UPDATE reaches SET river_id = NULL WHERE river_id = (SELECT id FROM rivers WHERE slug = $1)`,
		slug,
	); err != nil {
		errorResponse(w, http.StatusInternalServerError, "unlink reaches failed")
		return
	}

	tag, err := h.db.Exec(r.Context(), `DELETE FROM rivers WHERE slug = $1`, slug)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "river not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// riverMetaFromGNIS resolves a GNIS stream ID to state abbreviation, canonical
// basin label, and HUC8 by querying NHD → TIGERweb + WBD. Returns zero values
// without error when the lookup fails — callers treat this as best-effort.
func riverMetaFromGNIS(ctx context.Context, gnisID string) (stateAbbr, basin, huc8 string) {
	coord, err := nldi.NHDCoordByGNISID(ctx, gnisID)
	if err != nil {
		log.Printf("riverMetaFromGNIS %s: nhd: %v", gnisID, err)
		return
	}
	type stateResult struct {
		val string
		err error
	}
	type basinResult struct {
		info nldi.BasinInfo
		err  error
	}
	stateCh := make(chan stateResult, 1)
	basinCh := make(chan basinResult, 1)
	go func() { v, e := nldi.StateAt(ctx, coord.Lat, coord.Lng); stateCh <- stateResult{v, e} }()
	go func() { v, e := nldi.BasinAt(ctx, coord.Lat, coord.Lng); basinCh <- basinResult{v, e} }()
	stateRes := <-stateCh
	basinRes := <-basinCh

	stateAbbr = stateRes.val
	if stateAbbr == "" && basinRes.info.States != "" {
		stateAbbr = strings.SplitN(basinRes.info.States, ",", 2)[0]
	}
	basin = gauge.CanonicalBasin(basinRes.info.HUC8)
	huc8 = basinRes.info.HUC8
	return
}

// ListUnassignedReaches returns reaches that have no river association.
// GET /api/v1/admin/reaches/unassigned
func (h *AdminHandler) ListUnassignedReaches(w http.ResponseWriter, r *http.Request) {
	type Reach struct {
		ID            string   `json:"id"`
		Slug          string   `json:"slug"`
		Name          string   `json:"name"`
		CommonName    *string  `json:"common_name"`
		RiverName     *string  `json:"river_name"`
		ClassMin      *float64 `json:"class_min"`
		ClassMax      *float64 `json:"class_max"`
		HasCenterline bool     `json:"has_centerline"`
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT id, slug, name, common_name, river_name, class_min, class_max,
		       (centerline IS NOT NULL) AS has_centerline
		FROM reaches
		WHERE river_id IS NULL
		ORDER BY name
	`)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	reaches := make([]Reach, 0)
	for rows.Next() {
		var re Reach
		if err := rows.Scan(&re.ID, &re.Slug, &re.Name, &re.CommonName, &re.RiverName,
			&re.ClassMin, &re.ClassMax, &re.HasCenterline); err != nil {
			continue
		}
		reaches = append(reaches, re)
	}
	jsonResponse(w, http.StatusOK, reaches)
}

// GroupedReaches returns all assigned reaches grouped by reach state → river,
// sorted by river_order NULLS LAST then name. Designed for the admin list view.
// GET /api/v1/admin/reaches/grouped
func (h *AdminHandler) GroupedReaches(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(), `
		SELECT
			re.id, re.slug, re.name, re.common_name, re.river_order,
			COALESCE(re.state_abbr, '') AS state_abbr,
			(re.centerline IS NOT NULL) AS has_centerline,
			rv.id AS river_id, rv.slug AS river_slug, rv.name AS river_name, COALESCE(rv.basin, '') AS river_basin
		FROM reaches re
		JOIN rivers rv ON rv.id = re.river_id
		ORDER BY
			COALESCE(re.state_abbr, 'ZZZ') ASC,
			rv.name ASC,
			re.river_order NULLS LAST,
			re.name ASC
	`)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type ReachRow struct {
		ID            string  `json:"id"`
		Slug          string  `json:"slug"`
		Name          string  `json:"name"`
		CommonName    *string `json:"common_name"`
		RiverOrder    *int16  `json:"river_order"`
		StateAbbr     string  `json:"state_abbr"`
		HasCenterline bool    `json:"has_centerline"`
	}
	type RiverGroup struct {
		RiverID    string     `json:"river_id"`
		RiverSlug  string     `json:"river_slug"`
		RiverName  string     `json:"river_name"`
		RiverBasin string     `json:"river_basin"`
		Reaches    []ReachRow `json:"reaches"`
	}
	type StateGroup struct {
		State  string       `json:"state"`
		Rivers []RiverGroup `json:"rivers"`
	}

	// Preserve insertion order while deduping state and river keys.
	var stateOrder []string
	stateIndex := map[string]int{}
	riverIndex := map[string]map[string]int{} // state → riverID → index
	var groups []StateGroup

	for rows.Next() {
		var re ReachRow
		var riverID, riverSlug, riverName, riverBasin string
		if err := rows.Scan(&re.ID, &re.Slug, &re.Name, &re.CommonName, &re.RiverOrder,
			&re.StateAbbr, &re.HasCenterline,
			&riverID, &riverSlug, &riverName, &riverBasin); err != nil {
			continue
		}

		state := re.StateAbbr
		if state == "" {
			state = "—"
		}

		si, ok := stateIndex[state]
		if !ok {
			si = len(groups)
			stateIndex[state] = si
			stateOrder = append(stateOrder, state)
			groups = append(groups, StateGroup{State: state, Rivers: []RiverGroup{}})
			riverIndex[state] = map[string]int{}
		}

		ri, ok := riverIndex[state][riverID]
		if !ok {
			ri = len(groups[si].Rivers)
			riverIndex[state][riverID] = ri
			groups[si].Rivers = append(groups[si].Rivers, RiverGroup{
				RiverID:    riverID,
				RiverSlug:  riverSlug,
				RiverName:  riverName,
				RiverBasin: riverBasin,
				Reaches:    []ReachRow{},
			})
		}

		groups[si].Rivers[ri].Reaches = append(groups[si].Rivers[ri].Reaches, re)
	}

	_ = stateOrder
	jsonResponse(w, http.StatusOK, groups)
}

// ── User Roles ────────────────────────────────────────────────────────────────

type userRoleRow struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Email     *string   `json:"email"`
	Role      string    `json:"role"`
	RiverID   *string   `json:"river_id"`
	RiverName *string   `json:"river_name"`
	CreatedAt time.Time `json:"created_at"`
}

// ListUserRoles returns all role assignments. Site admin only.
// GET /api/v1/admin/users/roles
func (h *AdminHandler) ListUserRoles(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(), `
		SELECT ur.id, ur.user_id, NULL::text AS email,
		       ur.role, ur.river_id, rv.name AS river_name, ur.created_at
		FROM user_roles ur
		LEFT JOIN rivers rv ON rv.id = ur.river_id
		ORDER BY ur.created_at DESC
	`)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	result := make([]userRoleRow, 0)
	for rows.Next() {
		var ur userRoleRow
		if err := rows.Scan(&ur.ID, &ur.UserID, &ur.Email, &ur.Role, &ur.RiverID, &ur.RiverName, &ur.CreatedAt); err != nil {
			continue
		}
		result = append(result, ur)
	}
	jsonResponse(w, http.StatusOK, result)
}

// AssignRole grants a role to a user. Site admin only.
// POST /api/v1/admin/users/roles
func (h *AdminHandler) AssignRole(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID  string  `json:"user_id"`
		Role    string  `json:"role"`
		RiverID *string `json:"river_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.UserID == "" || body.Role == "" {
		errorResponse(w, http.StatusBadRequest, "user_id and role are required")
		return
	}

	var id string
	err := h.db.QueryRow(r.Context(), `
		INSERT INTO user_roles (user_id, role, river_id)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING
		RETURNING id
	`, body.UserID, body.Role, body.RiverID).Scan(&id)
	if err != nil {
		// ON CONFLICT DO NOTHING means no rows returned if duplicate — that's fine
		jsonResponse(w, http.StatusCreated, map[string]string{"id": id})
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]string{"id": id})
}

// RevokeRole removes a role assignment. Site admin only.
// DELETE /api/v1/admin/users/roles/{roleId}
func (h *AdminHandler) RevokeRole(w http.ResponseWriter, r *http.Request) {
	roleID := chi.URLParam(r, "roleId")
	_, err := h.db.Exec(r.Context(), `DELETE FROM user_roles WHERE id = $1`, roleID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetMyRoles returns the caller's own role assignments.
// GET /api/v1/admin/me/roles
func (h *AdminHandler) GetMyRoles(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT ur.id, ur.user_id, NULL::text, ur.role, ur.river_id, rv.name, ur.created_at
		FROM user_roles ur
		LEFT JOIN rivers rv ON rv.id = ur.river_id
		WHERE ur.user_id = $1
	`, userID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	result := make([]userRoleRow, 0)
	for rows.Next() {
		var ur userRoleRow
		if err := rows.Scan(&ur.ID, &ur.UserID, &ur.Email, &ur.Role, &ur.RiverID, &ur.RiverName, &ur.CreatedAt); err != nil {
			continue
		}
		result = append(result, ur)
	}

	// Include site_admin status from Supabase JWT
	isSiteAdmin := auth.IsSiteAdminFromContext(r.Context())
	jsonResponse(w, http.StatusOK, map[string]any{
		"is_site_admin": isSiteAdmin,
		"is_data_admin": auth.IsDataAdminFromContext(r.Context()),
		"roles":         result,
	})
}

// GNISLookup handles GET /api/v1/admin/rivers/gnis-lookup?gnis_id=X
//
// Resolves a GNIS stream ID to state, basin name, and HUC8. Uses the NHD
// ArcGIS flowline layer to get a representative coordinate, then calls
// TIGERweb for state and WBD for basin info. Returns 404 when the GNIS ID
// has no NHD features.
func (h *AdminHandler) GNISLookup(w http.ResponseWriter, r *http.Request) {
	gnisID := strings.TrimSpace(r.URL.Query().Get("gnis_id"))
	if gnisID == "" {
		errorResponse(w, http.StatusBadRequest, "gnis_id is required")
		return
	}
	ctx := r.Context()

	coord, err := nldi.NHDCoordByGNISID(ctx, gnisID)
	if err != nil {
		errorResponse(w, http.StatusNotFound, fmt.Sprintf("GNIS lookup: %v", err))
		return
	}

	type stateResult struct {
		val string
		err error
	}
	type basinResult struct {
		info nldi.BasinInfo
		err  error
	}
	stateCh := make(chan stateResult, 1)
	basinCh := make(chan basinResult, 1)
	go func() { v, e := nldi.StateAt(ctx, coord.Lat, coord.Lng); stateCh <- stateResult{v, e} }()
	go func() { v, e := nldi.BasinAt(ctx, coord.Lat, coord.Lng); basinCh <- basinResult{v, e} }()

	stateRes := <-stateCh
	basinRes := <-basinCh

	stateAbbr := stateRes.val
	if stateAbbr == "" && basinRes.info.States != "" {
		stateAbbr = strings.SplitN(basinRes.info.States, ",", 2)[0]
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"name":       coord.Name,
		"state_abbr": stateAbbr,
		"basin":      gauge.CanonicalBasin(basinRes.info.HUC8),
		"huc8":       basinRes.info.HUC8,
		"states":     basinRes.info.States,
		"lat":        coord.Lat,
		"lng":        coord.Lng,
	})
}

// ReorderReachesForRiver handles POST /api/v1/admin/rivers/{riverSlug}/reorder-reaches
//
// Assigns river_order 1..N to all reaches on the river, sorted by start_point
// longitude. Direction is auto-detected: if the sum of (end_lng - start_lng) across
// all reaches with both points is positive, the river flows W→E (ASC); otherwise
// E→W (DESC, e.g. Colorado River flowing toward Utah). Falls back to ASC when
// no reaches have both endpoints set.
func (h *AdminHandler) ReorderReachesForRiver(w http.ResponseWriter, r *http.Request) {
	riverSlug := chi.URLParam(r, "riverSlug")
	if riverSlug == "" {
		errorResponse(w, http.StatusBadRequest, "riverSlug required")
		return
	}

	// Detect flow direction: sum (end_lng - start_lng) across reaches with both points.
	var lngDelta float64
	_ = h.db.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(
			ST_X(re.end_point::geometry) - ST_X(re.start_point::geometry)
		), 0)
		FROM reaches re
		JOIN rivers rv ON rv.id = re.river_id
		WHERE rv.slug = $1
		  AND re.start_point IS NOT NULL
		  AND re.end_point IS NOT NULL
	`, riverSlug).Scan(&lngDelta)

	orderDir := "ASC"
	if lngDelta < 0 {
		orderDir = "DESC"
	}

	rows, err := h.db.Query(r.Context(), fmt.Sprintf(`
		SELECT re.id
		FROM reaches re
		JOIN rivers rv ON rv.id = re.river_id
		WHERE rv.slug = $1
		ORDER BY
			ST_X(re.start_point::geometry) %s NULLS LAST,
			re.name ASC
	`, orderDir), riverSlug)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		errorResponse(w, http.StatusInternalServerError, "scan failed")
		return
	}
	if len(ids) == 0 {
		jsonResponse(w, http.StatusOK, map[string]any{"updated": 0})
		return
	}

	tx, err := h.db.Begin(r.Context())
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx failed")
		return
	}
	defer tx.Rollback(r.Context())

	for i, id := range ids {
		if _, err := tx.Exec(r.Context(),
			`UPDATE reaches SET river_order = $1 WHERE id = $2`,
			i+1, id,
		); err != nil {
			errorResponse(w, http.StatusInternalServerError, "update failed")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		errorResponse(w, http.StatusInternalServerError, "commit failed")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"updated": len(ids)})
}

// ── Admin gauge management ────────────────────────────────────────────────────

// ListAdminGauges handles GET /admin/gauges
//
// Query params:
//
//	q=           text search on name or external_id
//	source=      filter by source (usgs, dwr, …)
//	poll_health= filter by poll_health (healthy, degraded, stale, unreachable)
//	status=      filter by status (active, inactive, seasonal, retired, …)
//	orphaned=    true = only gauges with no associated reaches
//	show_retired=true  include retired gauges (default excluded)
//	limit=       max rows (1–200, default 50)
//	offset=      pagination offset
func (h *AdminHandler) ListAdminGauges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	search := q.Get("q")
	source := q.Get("source")
	pollHealth := q.Get("poll_health")
	status := q.Get("status")
	orphaned := q.Get("orphaned") == "true"
	showRetired := q.Get("show_retired") == "true"
	limit := clampInt(parseIntOr(q.Get("limit"), 50), 1, 200)
	offset := max(parseIntOr(q.Get("offset"), 0), 0)

	args := []any{}
	wheres := []string{}
	idx := 1

	if !showRetired && status == "" {
		wheres = append(wheres, fmt.Sprintf("g.status != $%d", idx))
		args = append(args, "retired")
		idx++
	}
	if status != "" {
		wheres = append(wheres, fmt.Sprintf("g.status = $%d", idx))
		args = append(args, status)
		idx++
	}
	if source != "" {
		wheres = append(wheres, fmt.Sprintf("g.source = $%d", idx))
		args = append(args, source)
		idx++
	}
	if pollHealth != "" {
		wheres = append(wheres, fmt.Sprintf("g.poll_health = $%d", idx))
		args = append(args, pollHealth)
		idx++
	}
	if search != "" {
		wheres = append(wheres, fmt.Sprintf("(g.name ILIKE $%d OR g.external_id ILIKE $%d)", idx, idx+1))
		pat := "%" + search + "%"
		args = append(args, pat, pat)
		idx += 2
	}
	if orphaned {
		wheres = append(wheres, `NOT EXISTS (
			SELECT 1 FROM reaches r2
			WHERE r2.primary_gauge_id = g.id OR r2.secondary_gauge_id = g.id
		)`)
	}

	where := ""
	if len(wheres) > 0 {
		where = "WHERE " + strings.Join(wheres, " AND ")
	}

	countSQL := fmt.Sprintf(`SELECT COUNT(*) FROM gauges g %s`, where)
	var total int
	if err := h.db.QueryRow(r.Context(), countSQL, args...).Scan(&total); err != nil {
		errorResponse(w, http.StatusInternalServerError, "count failed")
		return
	}

	args = append(args, limit, offset)
	rows, err := h.db.Query(r.Context(), fmt.Sprintf(`
		SELECT
			g.id, g.external_id, g.source, g.name,
			g.status, g.auto_managed,
			g.poll_health, g.consecutive_poll_failures,
			g.last_reading_at, g.last_poll_success_at, g.last_poll_failure_at,
			g.seasonal_start_mmdd, g.seasonal_end_mmdd,
			g.state_abbr,
			g.current_cfs,
			g.poll_interval_seconds,
			g.detected_interval_seconds,
			(SELECT COUNT(*) FROM reaches r2
			 WHERE r2.primary_gauge_id = g.id
			) AS reach_count
		FROM gauges g
		%s
		ORDER BY
			CASE g.poll_health
				WHEN 'unreachable' THEN 0
				WHEN 'stale'       THEN 1
				WHEN 'degraded'    THEN 2
				ELSE 3
			END,
			g.name
		LIMIT $%d OFFSET $%d
	`, where, idx, idx+1), args...)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type gaugeRow struct {
		ID                      string     `json:"id"`
		ExternalID              string     `json:"external_id"`
		Source                  string     `json:"source"`
		Name                    string     `json:"name"`
		Status                  string     `json:"status"`
		AutoManaged             bool       `json:"auto_managed"`
		PollHealth              string     `json:"poll_health"`
		ConsecutiveFailures     int        `json:"consecutive_poll_failures"`
		LastReadingAt           *time.Time `json:"last_reading_at"`
		LastPollSuccessAt       *time.Time `json:"last_poll_success_at"`
		LastPollFailureAt       *time.Time `json:"last_poll_failure_at"`
		SeasonalStartMMDD       *string    `json:"seasonal_start_mmdd"`
		SeasonalEndMMDD         *string    `json:"seasonal_end_mmdd"`
		StateAbbr               *string    `json:"state_abbr"`
		LastReadingCfs          *float64   `json:"last_reading_cfs"`
		PollIntervalSeconds     *int       `json:"poll_interval_seconds"`
		DetectedIntervalSeconds *int       `json:"detected_interval_seconds"`
		ReachCount              int        `json:"reach_count"`
	}

	gauges := make([]gaugeRow, 0)
	for rows.Next() {
		var g gaugeRow
		if err := rows.Scan(
			&g.ID, &g.ExternalID, &g.Source, &g.Name,
			&g.Status, &g.AutoManaged,
			&g.PollHealth, &g.ConsecutiveFailures,
			&g.LastReadingAt, &g.LastPollSuccessAt, &g.LastPollFailureAt,
			&g.SeasonalStartMMDD, &g.SeasonalEndMMDD,
			&g.StateAbbr,
			&g.LastReadingCfs,
			&g.PollIntervalSeconds,
			&g.DetectedIntervalSeconds,
			&g.ReachCount,
		); err != nil {
			continue
		}
		gauges = append(gauges, g)
	}

	// Health summary counts (unfiltered except for show_retired)
	type healthCounts struct {
		Healthy     int `json:"healthy"`
		Degraded    int `json:"degraded"`
		Stale       int `json:"stale"`
		Unreachable int `json:"unreachable"`
		Total       int `json:"total"`
	}
	var summary healthCounts
	summaryWhere := ""
	summaryArgs := []any{}
	if !showRetired {
		summaryWhere = "WHERE status != $1"
		summaryArgs = append(summaryArgs, "retired")
	}
	_ = h.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT
			COUNT(*) FILTER (WHERE poll_health = 'healthy'),
			COUNT(*) FILTER (WHERE poll_health = 'degraded'),
			COUNT(*) FILTER (WHERE poll_health = 'stale'),
			COUNT(*) FILTER (WHERE poll_health = 'unreachable'),
			COUNT(*)
		FROM gauges %s
	`, summaryWhere), summaryArgs...).Scan(
		&summary.Healthy, &summary.Degraded, &summary.Stale, &summary.Unreachable, &summary.Total,
	)

	jsonResponse(w, http.StatusOK, map[string]any{
		"gauges":  gauges,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
		"summary": summary,
	})
}

// PollAdminGauge handles POST /admin/gauges/:id/poll — force immediate fetch.
func (h *AdminHandler) PollAdminGauge(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if h.poller == nil {
		errorResponse(w, http.StatusServiceUnavailable, "poller unavailable")
		return
	}
	fetched := h.poller.FetchNowIfStale(r.Context(), id, 0)
	jsonResponse(w, http.StatusOK, map[string]any{"fetched": fetched})
}

// RetireAdminGauge handles POST /admin/gauges/:id/retire.
func (h *AdminHandler) RetireAdminGauge(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_, err := h.db.Exec(r.Context(), `
		UPDATE gauges SET status = 'retired', auto_managed = FALSE WHERE id = $1
	`, id)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "update failed")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"status": "retired"})
}

// ReactivateAdminGauge handles POST /admin/gauges/:id/reactivate.
func (h *AdminHandler) ReactivateAdminGauge(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_, err := h.db.Exec(r.Context(), `
		UPDATE gauges SET
			status = 'active',
			consecutive_poll_failures = 0,
			poll_health = 'healthy',
			auto_managed = TRUE
		WHERE id = $1
	`, id)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "update failed")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"status": "active"})
}

// SetAdminGaugeSeasonal handles PATCH /admin/gauges/:id/seasonal.
// Body: { "start_mmdd": "04-01", "end_mmdd": "10-31" } or {} to clear.
func (h *AdminHandler) SetAdminGaugeSeasonal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		StartMMDD *string `json:"start_mmdd"`
		EndMMDD   *string `json:"end_mmdd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.StartMMDD != nil && body.EndMMDD != nil {
		_, err := h.db.Exec(r.Context(), `
			UPDATE gauges SET
				status = 'seasonal',
				seasonal_start_mmdd = $2,
				seasonal_end_mmdd   = $3
			WHERE id = $1
		`, id, *body.StartMMDD, *body.EndMMDD)
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, "update failed")
			return
		}
		jsonResponse(w, http.StatusOK, map[string]any{"status": "seasonal"})
	} else {
		// Clear seasonal — revert to active
		_, err := h.db.Exec(r.Context(), `
			UPDATE gauges SET
				status = 'active',
				seasonal_start_mmdd = NULL,
				seasonal_end_mmdd   = NULL
			WHERE id = $1
		`, id)
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, "update failed")
			return
		}
		jsonResponse(w, http.StatusOK, map[string]any{"status": "active"})
	}
}
