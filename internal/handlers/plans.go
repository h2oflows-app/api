package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/h2oflow/h2oflow/apps/api/internal/kmlimport"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlanHandler handles /plans, /plan-runs, and /me/calendar* routes (#246 A3
// — Trip Calendar). Split across plans.go (plan CRUD + shared helpers),
// plan_runs.go (calendar-run CRUD), calendar.go (/me/calendar*).
type PlanHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
	rl            *reportRateLimiter // reused: generic (owner,limit,window) limiter — see reports.go
}

func NewPlanHandler(db *pgxpool.Pool, devFallbackID string) *PlanHandler {
	return &PlanHandler{db: db, devFallbackID: devFallbackID, rl: newReportRateLimiter()}
}

func (h *PlanHandler) ownerID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

// ── Shared types/helpers ─────────────────────────────────────────────────

// dbQueryer is the QueryRow+Exec+Query superset satisfied by both
// *pgxpool.Pool and pgx.Tx (extends the pgxQueryer interface defined in
// user_reaches.go with Query — needed by flow.StampFull and the itinerary
// list queries in this file). Lets plan-creation helpers run identically
// inside the POST /plans transaction or standalone from POST /plans/{id}/runs.
type dbQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// apiError carries an HTTP status alongside an error so shared helpers like
// insertPlanRun can signal 400/404/422 distinctly from a bare 500.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func (h *PlanHandler) respondAPIError(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		errorResponse(w, ae.status, ae.msg)
		return
	}
	errorResponse(w, http.StatusInternalServerError, err.Error())
}

var validPlanTypes = map[string]bool{"personal": true, "festival": true, "race": true, "cruise": true}
var validPlanVisibilities = map[string]bool{"public": true, "private": true}

func parseDate(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

// validateRunDateInRange enforces "run_date ∈ [plan.start_date, plan.end_date]"
// (app-enforced — cross-row CHECK isn't expressible in Postgres).
func validateRunDateInRange(runDate, planStart, planEnd string) error {
	rd, err := parseDate(runDate)
	if err != nil {
		return fmt.Errorf("invalid run_date")
	}
	ps, err := parseDate(planStart)
	if err != nil {
		return fmt.Errorf("invalid plan start_date")
	}
	pe, err := parseDate(planEnd)
	if err != nil {
		return fmt.Errorf("invalid plan end_date")
	}
	if rd.Before(ps) || rd.After(pe) {
		return fmt.Errorf("run_date must be within the plan's dates (%s..%s)", planStart, planEnd)
	}
	return nil
}

// userToday resolves ownerID's IANA calendar tz (user_calendar_prefs.tz,
// default America/Denver) and returns (NOW() AT TIME ZONE tz)::date — the
// "user-local today" every future/past guard in this package uses instead
// of bare CURRENT_DATE (contract "Timezone rule", binding).
func userToday(ctx context.Context, q dbQueryer, ownerID string) (time.Time, error) {
	var today time.Time
	err := q.QueryRow(ctx, `
		SELECT (NOW() AT TIME ZONE COALESCE(
			(SELECT tz FROM user_calendar_prefs WHERE owner_id = $1), 'America/Denver'
		))::date
	`, ownerID).Scan(&today)
	return today, err
}

// localNoonUTC returns local noon (12:00) of runDate in ownerID's calendar
// tz, converted to UTC — the flow re-stamp anchor for mark-paddled (contract
// "Timezone rule": "Flow re-stamp targets local noon of run_date in the
// user's tz").
func localNoonUTC(ctx context.Context, q dbQueryer, ownerID, runDate string) (time.Time, error) {
	var t time.Time
	err := q.QueryRow(ctx, `
		SELECT ($1::date + TIME '12:00') AT TIME ZONE COALESCE(
			(SELECT tz FROM user_calendar_prefs WHERE owner_id = $2), 'America/Denver'
		)
	`, runDate, ownerID).Scan(&t)
	return t, err
}

// ensureHandle mirrors ReportHandler.ensureProfile (reports.go) — a plan's
// share URL embeds the host's handle, lazily creating a user_profiles row
// from their email on first use, same as the first report/dashboard/etc.
// Duplicated rather than shared across handler types, matching this
// codebase's per-file-helper convention (see reportRateLimiter).
func (h *PlanHandler) ensureHandle(ctx context.Context, ownerID, email string) (string, error) {
	var handle string
	err := h.db.QueryRow(ctx,
		`SELECT handle FROM user_profiles WHERE owner_id = $1`, ownerID,
	).Scan(&handle)
	if err == nil {
		return handle, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("ensureHandle lookup: %w", err)
	}

	base := handleFromEmail(email)
	for i := 0; i < 20; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", base, i+1)
		}
		if _, insErr := h.db.Exec(ctx,
			`INSERT INTO user_profiles (owner_id, handle) VALUES ($1, $2)`,
			ownerID, candidate,
		); insErr == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("ensureHandle: could not assign unique handle for %s", ownerID)
}

func (h *PlanHandler) uniquePlanSlug(ctx context.Context, ownerID, base string) string {
	candidate := base
	for i := 2; i < 100; i++ {
		var exists bool
		h.db.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM plans WHERE owner_id=$1 AND slug=$2)`,
			ownerID, candidate,
		).Scan(&exists)
		if !exists {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return fmt.Sprintf("%s-%d", base, time.Now().UnixMilli())
}

func (h *PlanHandler) uniqueRunSlug(ctx context.Context, q dbQueryer, ownerID, base string) string {
	candidate := base
	for i := 2; i < 100; i++ {
		var exists bool
		q.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM plan_runs WHERE owner_id=$1 AND slug=$2)`,
			ownerID, candidate,
		).Scan(&exists)
		if !exists {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return fmt.Sprintf("%s-%d", base, time.Now().UnixMilli())
}

// ── Response shapes ──────────────────────────────────────────────────────

type planDetail struct {
	ID             string  `json:"id"`
	Slug           string  `json:"slug"`
	Name           string  `json:"name"`
	Type           string  `json:"type"`
	Visibility     string  `json:"visibility"`
	StartDate      string  `json:"start_date"`
	EndDate        string  `json:"end_date"`
	Location       *string `json:"location,omitempty"`
	LookingForCrew bool    `json:"looking_for_crew"`
	MaxCrew        *int    `json:"max_crew,omitempty"`
	HostOwnerID    string  `json:"host_owner_id"`
	HostHandle     string  `json:"host_handle"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

type planRunSummary struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	UserReachID *string  `json:"user_reach_id,omitempty"`
	Name        *string  `json:"name,omitempty"`
	RunDate     string   `json:"run_date"`
	RunTime     *string  `json:"run_time,omitempty"`
	SortOrder   int16    `json:"sort_order"`
	GaugeCFS    *float64 `json:"gauge_cfs,omitempty"`
	FlowBand    *string  `json:"flow_band,omitempty"`
	FlowColor   *string  `json:"flow_color,omitempty"`
	Paddled     bool     `json:"paddled"`
	PaddledAt   *string  `json:"paddled_at,omitempty"`
	Notes       *string  `json:"notes,omitempty"`
	Companions  *string  `json:"companions,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
}

type planMember struct {
	Handle string `json:"handle"`
	Status string `json:"status"`
}

type itineraryDay struct {
	Date string           `json:"date"`
	Runs []planRunSummary `json:"runs"`
}

// ── POST /plans ───────────────────────────────────────────────────────────

func (h *PlanHandler) Create(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !h.rl.allow(ownerID, 20, time.Hour) {
		errorResponse(w, http.StatusTooManyRequests, "rate limit: 20 plans per hour")
		return
	}

	var body struct {
		Name           string              `json:"name"`
		Type           *string             `json:"type"`
		StartDate      string              `json:"start_date"`
		EndDate        string              `json:"end_date"`
		Location       *string             `json:"location"`
		Visibility     *string             `json:"visibility"`
		LookingForCrew *bool               `json:"looking_for_crew"`
		MaxCrew        *int                `json:"max_crew"`
		Runs           []createPlanRunBody `json:"runs"`
		// TODO(A4): invite machinery (email/handle invites + .ics) lands in
		// the next PR — accepted here so old-client bodies don't 400, but
		// silently ignored.
		Invites json.RawMessage `json:"invites"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		errorResponse(w, http.StatusBadRequest, "name is required")
		return
	}
	if body.StartDate == "" || body.EndDate == "" {
		errorResponse(w, http.StatusBadRequest, "start_date and end_date are required")
		return
	}
	sd, err := parseDate(body.StartDate)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid start_date")
		return
	}
	ed, err := parseDate(body.EndDate)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid end_date")
		return
	}
	if ed.Before(sd) {
		errorResponse(w, http.StatusBadRequest, "end_date must be on or after start_date")
		return
	}

	planType := "personal"
	if body.Type != nil {
		if !validPlanTypes[*body.Type] {
			errorResponse(w, http.StatusBadRequest, "invalid type")
			return
		}
		planType = *body.Type
	}
	visibility := "public"
	if body.Visibility != nil {
		if !validPlanVisibilities[*body.Visibility] {
			errorResponse(w, http.StatusBadRequest, "invalid visibility")
			return
		}
		visibility = *body.Visibility
	}
	lookingForCrew := body.LookingForCrew != nil && *body.LookingForCrew
	if lookingForCrew && (body.MaxCrew == nil || *body.MaxCrew <= 0) {
		errorResponse(w, http.StatusBadRequest, "max_crew is required (and must be > 0) when looking_for_crew is true")
		return
	}

	ctx := r.Context()

	slugBase := kmlimport.Slugify(name)
	if slugBase == "" {
		slugBase = "plan"
	}
	slug := h.uniquePlanSlug(ctx, ownerID, slugBase)

	tx, err := h.db.Begin(ctx)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx failed")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var planID string
	err = tx.QueryRow(ctx, `
		INSERT INTO plans
			(owner_id, slug, name, type, visibility, start_date, end_date,
			 location, looking_for_crew, max_crew)
		VALUES ($1,$2,$3,$4::plan_type,$5::plan_visibility,$6::date,$7::date,$8,$9,$10)
		RETURNING id
	`, ownerID, slug, name, planType, visibility, body.StartDate, body.EndDate,
		body.Location, lookingForCrew, body.MaxCrew,
	).Scan(&planID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("create plan failed: %v", err))
		return
	}

	for _, runBody := range body.Runs {
		if _, _, err := h.insertPlanRun(ctx, tx, planID, ownerID, body.StartDate, body.EndDate, runBody); err != nil {
			h.respondAPIError(w, err)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		errorResponse(w, http.StatusInternalServerError, "commit failed")
		return
	}

	email, _ := auth.EmailFromContext(ctx)
	handle, herr := h.ensureHandle(ctx, ownerID, email)
	if herr != nil {
		// Plan is already committed — degrade the URL rather than fail
		// the whole request over a handle-assignment hiccup.
		handle = ownerID
	}

	jsonResponse(w, http.StatusCreated, map[string]string{
		"id":   planID,
		"slug": slug,
		"url":  fmt.Sprintf("/plans/%s/%s", handle, slug),
	})
}

// ── GET /plans/{handle}/{slug} + GET /plans/{id} ──────────────────────────

func (h *PlanHandler) GetByHandleSlug(w http.ResponseWriter, r *http.Request) {
	handle := chi.URLParam(r, "handle")
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()

	var planID string
	err := h.db.QueryRow(ctx, `
		SELECT p.id FROM plans p
		JOIN user_profiles up ON up.owner_id = p.owner_id
		WHERE LOWER(up.handle) = LOWER($1) AND p.slug = $2 AND p.deleted_at IS NULL
	`, handle, slug).Scan(&planID)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}
	h.renderPlan(w, r, planID)
}

func (h *PlanHandler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var planID string
	if err := h.db.QueryRow(r.Context(),
		`SELECT id FROM plans WHERE id = $1::uuid AND deleted_at IS NULL`, id,
	).Scan(&planID); err != nil {
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}
	h.renderPlan(w, r, planID)
}

func (h *PlanHandler) renderPlan(w http.ResponseWriter, r *http.Request, planID string) {
	ctx := r.Context()

	var pd planDetail
	var createdAt, updatedAt time.Time
	err := h.db.QueryRow(ctx, `
		SELECT p.id, p.slug, p.name, p.type::text, p.visibility::text,
		       p.start_date::text, p.end_date::text, p.location,
		       p.looking_for_crew, p.max_crew,
		       p.owner_id, COALESCE(up.handle, ''),
		       p.created_at, p.updated_at
		FROM plans p
		LEFT JOIN user_profiles up ON up.owner_id = p.owner_id
		WHERE p.id = $1 AND p.deleted_at IS NULL
	`, planID).Scan(
		&pd.ID, &pd.Slug, &pd.Name, &pd.Type, &pd.Visibility,
		&pd.StartDate, &pd.EndDate, &pd.Location,
		&pd.LookingForCrew, &pd.MaxCrew,
		&pd.HostOwnerID, &pd.HostHandle,
		&createdAt, &updatedAt,
	)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}
	pd.CreatedAt = createdAt.Format(time.RFC3339)
	pd.UpdatedAt = updatedAt.Format(time.RFC3339)

	if pd.Visibility != "public" {
		callerID, callerOK := h.ownerID(r)
		allowed := callerOK && callerID == pd.HostOwnerID
		if !allowed && callerOK {
			h.db.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM plan_members
					WHERE plan_id = $1 AND member_owner_id = $2 AND status IN ('invited','accepted'))
			`, planID, callerID).Scan(&allowed)
		}
		if !allowed {
			errorResponse(w, http.StatusNotFound, "plan not found")
			return
		}
	}

	rows, err := h.db.Query(ctx, `
		SELECT pr.id, pr.slug, pr.user_reach_id::text, ur.name, pr.run_date::text, pr.run_time::text,
		       pr.sort_order, pr.gauge_cfs, pr.flow_band, pr.flow_color, pr.paddled, pr.paddled_at,
		       pr.notes, pr.companions, pr.created_at
		FROM plan_runs pr
		LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
		WHERE pr.plan_id = $1 AND pr.deleted_at IS NULL
		ORDER BY pr.run_date, pr.sort_order
	`, planID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	var dateOrder []string
	runsByDate := map[string][]planRunSummary{}
	for rows.Next() {
		var run planRunSummary
		var paddledAtRaw *time.Time
		var createdAtRaw time.Time
		if err := rows.Scan(
			&run.ID, &run.Slug, &run.UserReachID, &run.Name, &run.RunDate, &run.RunTime,
			&run.SortOrder, &run.GaugeCFS, &run.FlowBand, &run.FlowColor, &run.Paddled, &paddledAtRaw,
			&run.Notes, &run.Companions, &createdAtRaw,
		); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		if paddledAtRaw != nil {
			s := paddledAtRaw.Format(time.RFC3339)
			run.PaddledAt = &s
		}
		run.CreatedAt = createdAtRaw.Format(time.RFC3339)

		if _, seen := runsByDate[run.RunDate]; !seen {
			dateOrder = append(dateOrder, run.RunDate)
		}
		runsByDate[run.RunDate] = append(runsByDate[run.RunDate], run)
	}

	itinerary := make([]itineraryDay, 0, len(dateOrder))
	for _, d := range dateOrder {
		itinerary = append(itinerary, itineraryDay{Date: d, Runs: runsByDate[d]})
	}

	members := []planMember{}
	memberRows, err := h.db.Query(ctx, `
		SELECT COALESCE(up.handle, pm.invite_handle, '') AS handle, pm.status::text
		FROM plan_members pm
		LEFT JOIN user_profiles up ON up.owner_id = pm.member_owner_id
		WHERE pm.plan_id = $1
		ORDER BY pm.created_at
	`, planID)
	if err == nil {
		defer memberRows.Close()
		for memberRows.Next() {
			var m planMember
			if memberRows.Scan(&m.Handle, &m.Status) == nil {
				members = append(members, m)
			}
		}
	}

	var filled int
	h.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM plan_members WHERE plan_id = $1 AND status = 'accepted'`, planID,
	).Scan(&filled)

	jsonResponse(w, http.StatusOK, map[string]any{
		"plan":      pd,
		"itinerary": itinerary,
		"members":   members,
		"crew":      map[string]any{"filled": filled, "max": pd.MaxCrew},
	})
}

// ── GET /me/plans?from=&to=&type= ─────────────────────────────────────────

func (h *PlanHandler) ListMine(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	from := q.Get("from")
	to := q.Get("to")
	planType := q.Get("type")

	conds := []string{"p.owner_id = $1", "p.deleted_at IS NULL"}
	args := []any{ownerID}
	if from != "" {
		if _, err := parseDate(from); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid from")
			return
		}
		args = append(args, from)
		conds = append(conds, fmt.Sprintf("p.end_date >= $%d::date", len(args)))
	}
	if to != "" {
		if _, err := parseDate(to); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid to")
			return
		}
		args = append(args, to)
		conds = append(conds, fmt.Sprintf("p.start_date <= $%d::date", len(args)))
	}
	if planType != "" {
		if !validPlanTypes[planType] {
			errorResponse(w, http.StatusBadRequest, "invalid type")
			return
		}
		args = append(args, planType)
		conds = append(conds, fmt.Sprintf("p.type = $%d::plan_type", len(args)))
	}

	query := fmt.Sprintf(`
		SELECT p.id, p.slug, p.name, p.type::text, p.visibility::text,
		       p.start_date::text, p.end_date::text, p.location,
		       p.looking_for_crew, p.max_crew,
		       (SELECT COUNT(*) FROM plan_runs pr WHERE pr.plan_id = p.id AND pr.deleted_at IS NULL) AS run_count
		FROM plans p
		WHERE %s
		ORDER BY p.start_date
	`, strings.Join(conds, " AND "))

	rows, err := h.db.Query(ctx, query, args...)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type myPlan struct {
		ID             string  `json:"id"`
		Slug           string  `json:"slug"`
		Name           string  `json:"name"`
		Type           string  `json:"type"`
		Visibility     string  `json:"visibility"`
		StartDate      string  `json:"start_date"`
		EndDate        string  `json:"end_date"`
		Location       *string `json:"location,omitempty"`
		LookingForCrew bool    `json:"looking_for_crew"`
		MaxCrew        *int    `json:"max_crew,omitempty"`
		RunCount       int     `json:"run_count"`
	}

	plans := []myPlan{}
	for rows.Next() {
		var p myPlan
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Type, &p.Visibility,
			&p.StartDate, &p.EndDate, &p.Location, &p.LookingForCrew, &p.MaxCrew, &p.RunCount); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		plans = append(plans, p)
	}

	jsonResponse(w, http.StatusOK, map[string]any{"plans": plans})
}

// ── PATCH /plans/{id} ──────────────────────────────────────────────────────

func (h *PlanHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var body struct {
		Name           *string `json:"name"`
		Type           *string `json:"type"`
		Visibility     *string `json:"visibility"`
		LookingForCrew *bool   `json:"looking_for_crew"`
		MaxCrew        *int    `json:"max_crew"`
		StartDate      *string `json:"start_date"`
		EndDate        *string `json:"end_date"`
		Location       *string `json:"location"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	ctx := r.Context()

	var curType, curVisibility, curStart, curEnd string
	var curLookingForCrew bool
	var curMaxCrew *int
	err := h.db.QueryRow(ctx, `
		SELECT type::text, visibility::text, start_date::text, end_date::text, looking_for_crew, max_crew
		FROM plans WHERE id = $1::uuid AND owner_id = $2 AND deleted_at IS NULL
	`, id, ownerID).Scan(&curType, &curVisibility, &curStart, &curEnd, &curLookingForCrew, &curMaxCrew)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}

	newType := curType
	if body.Type != nil {
		if !validPlanTypes[*body.Type] {
			errorResponse(w, http.StatusBadRequest, "invalid type")
			return
		}
		newType = *body.Type
	}
	newVisibility := curVisibility
	if body.Visibility != nil {
		if !validPlanVisibilities[*body.Visibility] {
			errorResponse(w, http.StatusBadRequest, "invalid visibility")
			return
		}
		newVisibility = *body.Visibility
	}
	newStart := curStart
	if body.StartDate != nil {
		newStart = *body.StartDate
	}
	newEnd := curEnd
	if body.EndDate != nil {
		newEnd = *body.EndDate
	}
	sd, err1 := parseDate(newStart)
	ed, err2 := parseDate(newEnd)
	if err1 != nil || err2 != nil {
		errorResponse(w, http.StatusBadRequest, "invalid start_date/end_date")
		return
	}
	if ed.Before(sd) {
		errorResponse(w, http.StatusBadRequest, "end_date must be on or after start_date")
		return
	}
	newLookingForCrew := curLookingForCrew
	if body.LookingForCrew != nil {
		newLookingForCrew = *body.LookingForCrew
	}
	newMaxCrew := curMaxCrew
	if body.MaxCrew != nil {
		newMaxCrew = body.MaxCrew
	}
	if newLookingForCrew && (newMaxCrew == nil || *newMaxCrew <= 0) {
		errorResponse(w, http.StatusBadRequest, "max_crew is required (and must be > 0) when looking_for_crew is true")
		return
	}

	_, err = h.db.Exec(ctx, `
		UPDATE plans SET
			name             = COALESCE($1, name),
			type             = $2::plan_type,
			visibility       = $3::plan_visibility,
			start_date       = $4::date,
			end_date         = $5::date,
			location         = COALESCE($6, location),
			looking_for_crew = $7,
			max_crew         = $8,
			updated_at       = NOW()
		WHERE id = $9::uuid AND owner_id = $10
	`, body.Name, newType, newVisibility, newStart, newEnd, body.Location,
		newLookingForCrew, newMaxCrew, id, ownerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("update failed: %v", err))
		return
	}

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── DELETE /plans/{id} ────────────────────────────────────────────────────
// Tombstones the plan AND its child plan_runs in one tx (contract invariant).

func (h *PlanHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	tx, err := h.db.Begin(ctx)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx failed")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	tag, err := tx.Exec(ctx,
		`UPDATE plans SET deleted_at = NOW(), updated_at = NOW() WHERE id = $1::uuid AND owner_id = $2 AND deleted_at IS NULL`,
		id, ownerID,
	)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}

	if _, err := tx.Exec(ctx,
		`UPDATE plan_runs SET deleted_at = NOW(), updated_at = NOW() WHERE plan_id = $1::uuid AND deleted_at IS NULL`,
		id,
	); err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		errorResponse(w, http.StatusInternalServerError, "commit failed")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
