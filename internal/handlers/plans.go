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
	"github.com/h2oflow/h2oflow/apps/api/internal/mail"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlanHandler handles /plans, /plan-runs, and /me/calendar* routes (#246 A3
// — Trip Calendar). Split across plans.go (plan CRUD + shared helpers),
// plan_runs.go (calendar-run CRUD), calendar.go (/me/calendar*).
//
// # Membership rule (#246 A7, binding)
//
// Crew/RSVP rows (plan_members) are RUN-scoped (plan_run_id NOT NULL)
// whenever the parent plan has at least one live run — invites and join
// requests fan out to one row per targeted run at write time (InviteToPlan/
// JoinRun), never lazily. A plan with ZERO live runs has nowhere to fan out
// to, so its invites/requests stay PLAN-scoped (plan_run_id NULL) — the
// plan page simply lists them as plan members with no per-run RSVP button.
// This is a static split decided once at invite/join time, not a live
// state: a runless plan that later gains its first run does NOT retroactively
// fan out its existing plan-level rows (deliberately simple — see
// migrations/000144_crew_per_run.up.sql's one-time fan-out, which only ever
// runs against pre-existing data).
//
// "Plan member" for ACCESS checks (private-plan read in renderPlan/
// renderPlanRun, the calendar plans[] role in calendar.go, the log-mine gate
// in LogMine, moderation's FlagPlanRun) means: EXISTS any plan_members row
// for that plan_id with the right status, regardless of plan_run_id (run-
// scoped or plan-level, doesn't matter) — every access-check query in this
// package already matches on plan_id alone with no plan_run_id filter, so
// they needed no code change for this rule; this comment documents that
// invariant so it isn't accidentally narrowed to a single run later.
//
// "Crew" (the accept/decline meter with a cap), by contrast, is ALWAYS
// per-run: runFilled/plan_runs.max_crew/looking_for_crew below, never a
// plan-level aggregate — a plan itself has no crew concept anymore (the
// columns moved to plan_runs; see plans_test/plan_runs.go).
type PlanHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
	rl            *reportRateLimiter // reused: generic (owner,limit,window) limiter — see reports.go
	mailer        mail.Mailer        // #246 A4: invites[] on POST /plans; defaults to NoopMailer until WithMailer
}

func NewPlanHandler(db *pgxpool.Pool, devFallbackID string) *PlanHandler {
	return &PlanHandler{db: db, devFallbackID: devFallbackID, rl: newReportRateLimiter(), mailer: mail.NoopMailer{}}
}

// WithMailer wires the shared Mailer for invites embedded in POST /plans'
// `invites` array — mirrors the WithPoller()/WithAnthropicKey() wither
// pattern used by other handlers in main.go.
func (h *PlanHandler) WithMailer(m mail.Mailer) *PlanHandler {
	if m != nil {
		h.mailer = m
	}
	return h
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

// maxDateStr/minDateStr compare "YYYY-MM-DD" date strings lexically — valid
// because ISO-8601 date strings sort identically as strings and as dates.
// Used to intersect a caller-supplied [from,to] range with the fixed 14-day
// Tier-A nudge lookback window (nudges.go/calendar.go) without a round trip
// through time.Time.
func maxDateStr(a, b string) string {
	if a > b {
		return a
	}
	return b
}

func minDateStr(a, b string) string {
	if a < b {
		return a
	}
	return b
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
// Package-level (not a *PlanHandler method) so it's reusable from
// findOrCreatePaddledLog (plan_runs.go), which needs it from both
// PlanHandler.LogMine and NudgeHandler.Confirm (#246 A5 task 1: "extract
// shared helper rather than duplicating").
func ensureHandle(ctx context.Context, db *pgxpool.Pool, ownerID, email string) (string, error) {
	var handle string
	err := db.QueryRow(ctx,
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
		if _, insErr := db.Exec(ctx,
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

// uniqueRunSlug is package-level (not a *PlanHandler method) so it's callable
// from findOrCreatePaddledLog (plan_runs.go) on behalf of both
// PlanHandler.LogMine and NudgeHandler.Confirm.
func uniqueRunSlug(ctx context.Context, q dbQueryer, ownerID, base string) string {
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

// runFilled counts plan_members rows with status='accepted' for planRunID —
// the shared crew-meter read used by renderPlan/renderPlanRun (per itinerary
// run) and RunCrewList/RunCrewAccept/JoinRun/AcceptInvite (invites.go). #246
// A7: crew moved from plans to plan_runs (product decision, see the package
// comment above) — this replaces the old plan-scoped crewFilled. Host is
// never counted (no membership row for the host). #246 A4 carry-over fix
// (task 6a, preserved): scan errors propagate as a real error rather than
// silently failing open to filled=0, which would let an over-cap
// join/accept through on a transient DB error at any of this function's
// gating call sites.
func runFilled(ctx context.Context, q dbQueryer, planRunID string) (int, error) {
	var filled int
	if err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM plan_members WHERE plan_run_id = $1::uuid AND status = 'accepted'`, planRunID,
	).Scan(&filled); err != nil {
		return 0, fmt.Errorf("runFilled: %w", err)
	}
	return filled, nil
}

// ── Response shapes ──────────────────────────────────────────────────────

type planDetail struct {
	ID          string  `json:"id"`
	Slug        string  `json:"slug"`
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	Visibility  string  `json:"visibility"`
	StartDate   string  `json:"start_date"`
	EndDate     string  `json:"end_date"`
	Location    *string `json:"location,omitempty"`
	HostOwnerID string  `json:"host_owner_id"`
	HostHandle  string  `json:"host_handle"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// runCrewMeter is the per-run crew embed (#246 A7 — crew moved off plans
// onto plan_runs): {filled,max,looking_for_crew}, always present on a
// planRunSummary (not conditional on LookingForCrew) so the web can render
// "not looking for crew" vs "0 of N" without a second field to check.
type runCrewMeter struct {
	Filled         int  `json:"filled"`
	Max            *int `json:"max,omitempty"`
	LookingForCrew bool `json:"looking_for_crew"`
}

type planRunSummary struct {
	ID          string       `json:"id"`
	Slug        string       `json:"slug"`
	UserReachID *string      `json:"user_reach_id,omitempty"`
	Name        *string      `json:"name,omitempty"`
	RunDate     string       `json:"run_date"`
	RunTime     *string      `json:"run_time,omitempty"`
	SortOrder   int16        `json:"sort_order"`
	GaugeCFS    *float64     `json:"gauge_cfs,omitempty"`
	FlowBand    *string      `json:"flow_band,omitempty"`
	FlowColor   *string      `json:"flow_color,omitempty"`
	Paddled     bool         `json:"paddled"`
	PaddledAt   *string      `json:"paddled_at,omitempty"`
	Notes       *string      `json:"notes,omitempty"`
	Companions  *string      `json:"companions,omitempty"`
	CreatedAt   string       `json:"created_at,omitempty"`
	Crew        runCrewMeter `json:"crew"`
	// MeetupSpot/MeetupFeatureType/MeetupFeatureID: "meet up at" (product
	// request 2026-07-25) — MeetupSpot is the always-present display text
	// (typed or feature-name snapshot); the FeatureType/ID pair (present
	// only when a #312 rapid/access feature was picked, one or neither, per
	// the DB's XOR) lets the edit sheet re-resolve and re-highlight the pick.
	MeetupSpot        *string `json:"meetup_spot,omitempty"`
	MeetupFeatureType *string `json:"meetup_feature_type,omitempty"`
	MeetupFeatureID   *string `json:"meetup_feature_id,omitempty"`
	// Per-viewer RSVP on THIS run (the caller's own plan_members row, if
	// any): status invited|requested|accepted|declined + the row id the
	// accept/dismiss endpoints take. Drives the itinerary's per-run accept
	// buttons + "You're in" state (#246 A7; W5 review blocker — the web
	// cannot derive these client-side).
	MyRSVP     *string `json:"my_rsvp,omitempty"`
	MyMemberID *string `json:"my_member_id,omitempty"`
}

type planMember struct {
	Handle string `json:"handle"`
	Status string `json:"status"`
	// PlanRunID is nil for a plan-level row (runless-plan invite/request —
	// see the membership-rule package comment above), set to the specific
	// run this membership row RSVPs to otherwise (#246 A7).
	PlanRunID *string `json:"plan_run_id,omitempty"`
	// InviteEmail is populated ONLY when the viewer is the host — email-only
	// invitees have no handle (member_owner_id NULL until accept), and without
	// this the host's members row rendered them invisible. Never exposed to
	// non-host viewers (invitee privacy).
	InviteEmail *string `json:"invite_email,omitempty"`
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
		Name       string              `json:"name"`
		Type       *string             `json:"type"`
		StartDate  string              `json:"start_date"`
		EndDate    string              `json:"end_date"`
		Location   *string             `json:"location"`
		Visibility *string             `json:"visibility"`
		Runs       []createPlanRunBody `json:"runs"`
		Invites    []inviteBody        `json:"invites"`
		// LookingForCrew/MaxCrew: #246 A7 moved crew from plans to plan_runs
		// (see createPlanRunBody's own fields of the same name). Decoded and
		// SILENTLY IGNORED here rather than rejected — an un-migrated web
		// client (pre-W5) still POSTs these at the plan level, and this
		// endpoint must not 400 it (sweep note, IMPLEMENTATION_PLAN.md §6/§9).
		LookingForCrew *bool `json:"looking_for_crew"`
		MaxCrew        *int  `json:"max_crew"`
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

	ctx := r.Context()

	// user_profiles must exist before the plans INSERT — plans.owner_id has
	// a hard FK to user_profiles(owner_id) (mig 000138), unlike reports.owner_id
	// which has no FK. Ensure the profile row up front rather than after
	// commit, mirroring reports.go:202, or a first-time user's first trip
	// 500s on the FK and never reaches ensureHandle at all.
	email, _ := auth.EmailFromContext(ctx)
	handle, herr := ensureHandle(ctx, h.db, ownerID, email)
	if herr != nil {
		errorResponse(w, http.StatusInternalServerError, "could not assign user profile")
		return
	}

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
			(owner_id, slug, name, type, visibility, start_date, end_date, location)
		VALUES ($1,$2,$3,$4::plan_type,$5::plan_visibility,$6::date,$7::date,$8)
		RETURNING id
	`, ownerID, slug, name, planType, visibility, body.StartDate, body.EndDate,
		body.Location,
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

	// invites[] reuses the same inviteOne code path as the standalone POST
	// /plans/{id}/invite (internal/handlers/invites.go) — run inside this
	// same tx so a failed invite rolls back the whole plan create. Any email
	// sends are collected and fired AFTER commit (task 6: "email sends fire
	// after commit, not inside the tx").
	var pendingMails []pendingInviteMail
	if len(body.Invites) > 0 {
		loc := ""
		if body.Location != nil {
			loc = *body.Location
		}
		allRuns, rerr := loadPlanRuns(ctx, tx, planID)
		if rerr != nil {
			errorResponse(w, http.StatusInternalServerError, "load runs failed")
			return
		}
		planInfo := invitedPlanInfo{
			ID: planID, Slug: slug, Name: name, Location: loc,
			StartDate: body.StartDate, EndDate: body.EndDate, HostHandle: handle,
			AllRuns: allRuns,
		}
		for _, ib := range body.Invites {
			targets, terr := resolveInviteTargets(planInfo.AllRuns, ib.PlanRunIDs)
			if terr != nil {
				h.respondAPIError(w, terr)
				return
			}
			_, pending, ierr := inviteOne(ctx, tx, planInfo, ownerID, ib, targets)
			if ierr != nil {
				h.respondAPIError(w, ierr)
				return
			}
			if pending != nil {
				pendingMails = append(pendingMails, *pending)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		errorResponse(w, http.StatusInternalServerError, "commit failed")
		return
	}

	for _, p := range pendingMails {
		go sendInviteMail(h.mailer, p)
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
		// Anon gets a uniform 401 whether or not the plan exists — a 404-vs-401
		// split would let anon probe which handle/slug pairs exist.
		if _, ok := h.ownerID(r); !ok {
			errorResponse(w, http.StatusUnauthorized, "authentication required")
			return
		}
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
		// Uniform 401 for anon — see GetByHandleSlug.
		if _, ok := h.ownerID(r); !ok {
			errorResponse(w, http.StatusUnauthorized, "authentication required")
			return
		}
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}
	h.renderPlan(w, r, planID)
}

func (h *PlanHandler) renderPlan(w http.ResponseWriter, r *http.Request, planID string) {
	ctx := r.Context()

	// #246 A6 anon scoping (IMPLEMENTATION_PLAN.md §6 REVISED — binding): the
	// calendar domain is auth-only. Anon (no ownerID) gets 401 UNLESS a
	// matching invite token is presented for THIS plan_id — the one
	// carve-out (?invite=<raw token>, the exact param name the invite email
	// link already embeds, invites.go sendInviteMail's acceptURL). A valid
	// token grants read access to this plan regardless of its visibility:
	// the whole point is letting an invited, logged-out recipient see what
	// they were invited to before creating an account (accept itself still
	// requires one — contract decision #8). Authed behavior is unchanged
	// below (public readable to any authed user; private → host/invited/
	// accepted member only, 404 no-leak) EXCEPT the token also has to be
	// honored for an AUTHED caller (review finding): an email invite's
	// plan_members row keeps member_owner_id NULL until accept, so an
	// authed-but-not-yet-bound invitee matches neither the host nor the
	// member_owner_id check below and would 404 despite holding a valid
	// token — resolve the token once, up front, for both branches.
	callerID, callerOK := h.ownerID(r)
	tokenRuns, tokenGrant := inviteTokenMemberIDs(ctx, h.db, planID, r.URL.Query().Get("invite"))
	if !callerOK && !tokenGrant {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var pd planDetail
	var createdAt, updatedAt time.Time
	err := h.db.QueryRow(ctx, `
		SELECT p.id, p.slug, p.name, p.type::text, p.visibility::text,
		       p.start_date::text, p.end_date::text, p.location,
		       p.owner_id, COALESCE(up.handle, ''),
		       p.created_at, p.updated_at
		FROM plans p
		LEFT JOIN user_profiles up ON up.owner_id = p.owner_id
		WHERE p.id = $1 AND p.deleted_at IS NULL
	`, planID).Scan(
		&pd.ID, &pd.Slug, &pd.Name, &pd.Type, &pd.Visibility,
		&pd.StartDate, &pd.EndDate, &pd.Location,
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
		allowed := tokenGrant || (callerOK && callerID == pd.HostOwnerID)
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

	// #246 A7: per-run crew {filled,max,looking_for_crew} embedded via the
	// cm LATERAL — crew is no longer a plan-level aggregate (see the
	// membership-rule package comment above runFilled).
	rows, err := h.db.Query(ctx, `
		SELECT pr.id, pr.slug, pr.user_reach_id::text, ur.name, pr.run_date::text, pr.run_time::text,
		       pr.sort_order, pr.gauge_cfs, pr.flow_band, pr.flow_color, pr.paddled, pr.paddled_at,
		       pr.notes, pr.companions, pr.created_at,
		       pr.looking_for_crew, pr.max_crew, COALESCE(cm.filled, 0),
		       pr.meetup_spot, pr.meetup_rapid_id::text, pr.meetup_access_id::text,
		       me.status, me.id
		FROM plan_runs pr
		LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS filled FROM plan_members pm
			WHERE pm.plan_run_id = pr.id AND pm.status = 'accepted'
		) cm ON true
		LEFT JOIN LATERAL (
			-- the caller's own RSVP row on this run ($2 = '' for anon/token
			-- viewers -> never matches); unique per (plan_run_id, member)
			SELECT pm.id::text, pm.status::text FROM plan_members pm
			WHERE pm.plan_run_id = pr.id AND pm.member_owner_id = $2
		) me ON true
		WHERE pr.plan_id = $1 AND pr.deleted_at IS NULL
		ORDER BY pr.run_date, pr.sort_order
	`, planID, callerID)
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
		var meetupRapidID, meetupAccessID *string
		if err := rows.Scan(
			&run.ID, &run.Slug, &run.UserReachID, &run.Name, &run.RunDate, &run.RunTime,
			&run.SortOrder, &run.GaugeCFS, &run.FlowBand, &run.FlowColor, &run.Paddled, &paddledAtRaw,
			&run.Notes, &run.Companions, &createdAtRaw,
			&run.Crew.LookingForCrew, &run.Crew.Max, &run.Crew.Filled,
			&run.MeetupSpot, &meetupRapidID, &meetupAccessID,
			&run.MyRSVP, &run.MyMemberID,
		); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		if paddledAtRaw != nil {
			s := paddledAtRaw.Format(time.RFC3339)
			run.PaddledAt = &s
		}
		run.CreatedAt = createdAtRaw.Format(time.RFC3339)
		run.MeetupFeatureType, run.MeetupFeatureID = meetupFeatureTypeID(meetupRapidID, meetupAccessID)

		if _, seen := runsByDate[run.RunDate]; !seen {
			dateOrder = append(dateOrder, run.RunDate)
		}
		runsByDate[run.RunDate] = append(runsByDate[run.RunDate], run)
	}

	itinerary := make([]itineraryDay, 0, len(dateOrder))
	for _, d := range dateOrder {
		itinerary = append(itinerary, itineraryDay{Date: d, Runs: runsByDate[d]})
	}

	viewerIsHost := callerOK && callerID == pd.HostOwnerID
	members := []planMember{}
	// #246 A7: members[] gains plan_run_id so the web can map each RSVP to
	// its run — nil for a plan-level row (runless plan, see the membership
	// rule package comment above).
	memberRows, err := h.db.Query(ctx, `
		SELECT COALESCE(up.handle, pm.invite_handle, '') AS handle, pm.status::text,
		       pm.plan_run_id::text, pm.invite_email
		FROM plan_members pm
		LEFT JOIN user_profiles up ON up.owner_id = pm.member_owner_id
		WHERE pm.plan_id = $1
		ORDER BY pm.created_at
	`, planID)
	if err == nil {
		defer memberRows.Close()
		for memberRows.Next() {
			var m planMember
			if memberRows.Scan(&m.Handle, &m.Status, &m.PlanRunID, &m.InviteEmail) == nil {
				if !viewerIsHost {
					m.InviteEmail = nil // invitee privacy — emails are host-only
				}
				members = append(members, m)
			}
		}
	}

	resp := map[string]any{
		"plan":      pd,
		"itinerary": itinerary,
		"members":   members,
	}
	if tokenGrant {
		// Lets the frontend drive InviteAcceptCard/POST /invites/{id}/accept
		// for a caller who holds a valid invite token but isn't bound to the
		// invite yet (member_owner_id still NULL — e.g. signed up with a
		// different email than the invite, so it's absent from /me/invites
		// too; review finding, #246 W4). #246 A7: the token spans one row per
		// invited run (inviteOne's fan-out) — invite_token_runs carries run
		// context so the token-landing page renders a per-run accept list;
		// invite_member_ids kept as the bare-id view for the W4-era client.
		resp["invite_token_runs"] = tokenRuns
		ids := make([]string, 0, len(tokenRuns))
		for _, t := range tokenRuns {
			ids = append(ids, t.MemberID)
		}
		resp["invite_member_ids"] = ids
	}
	jsonResponse(w, http.StatusOK, resp)
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
		ID         string  `json:"id"`
		Slug       string  `json:"slug"`
		Name       string  `json:"name"`
		Type       string  `json:"type"`
		Visibility string  `json:"visibility"`
		StartDate  string  `json:"start_date"`
		EndDate    string  `json:"end_date"`
		Location   *string `json:"location,omitempty"`
		RunCount   int     `json:"run_count"`
	}

	plans := []myPlan{}
	for rows.Next() {
		var p myPlan
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Type, &p.Visibility,
			&p.StartDate, &p.EndDate, &p.Location, &p.RunCount); err != nil {
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
		Name       *string `json:"name"`
		Type       *string `json:"type"`
		Visibility *string `json:"visibility"`
		StartDate  *string `json:"start_date"`
		EndDate    *string `json:"end_date"`
		Location   *string `json:"location"`
		// LookingForCrew/MaxCrew: #246 A7 moved crew to plan_runs — see the
		// same comment on Create's body struct above. Decoded, never applied.
		LookingForCrew *bool `json:"looking_for_crew"`
		MaxCrew        *int  `json:"max_crew"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	ctx := r.Context()

	var curType, curVisibility, curStart, curEnd string
	err := h.db.QueryRow(ctx, `
		SELECT type::text, visibility::text, start_date::text, end_date::text
		FROM plans WHERE id = $1::uuid AND owner_id = $2 AND deleted_at IS NULL
	`, id, ownerID).Scan(&curType, &curVisibility, &curStart, &curEnd)
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
	// #246 A4 carry-over fix: shrinking [start_date,end_date] must not stray
	// from a live child plan_run's run_date — orphaning a scheduled run
	// outside its own plan's dates silently breaks validateRunDateInRange's
	// invariant everywhere else. 422, not a silent clamp.
	if body.StartDate != nil || body.EndDate != nil {
		var outOfRange bool
		if err := h.db.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM plan_runs
				WHERE plan_id = $1::uuid AND deleted_at IS NULL
				  AND (run_date < $2::date OR run_date > $3::date)
			)
		`, id, newStart, newEnd).Scan(&outOfRange); err != nil {
			errorResponse(w, http.StatusInternalServerError, "date-range check failed")
			return
		}
		if outOfRange {
			errorResponse(w, http.StatusUnprocessableEntity, "cannot shrink plan dates: one or more scheduled runs fall outside the new range")
			return
		}
	}

	_, err = h.db.Exec(ctx, `
		UPDATE plans SET
			name       = COALESCE($1, name),
			type       = $2::plan_type,
			visibility = $3::plan_visibility,
			start_date = $4::date,
			end_date   = $5::date,
			location   = COALESCE($6, location),
			updated_at = NOW()
		WHERE id = $7::uuid AND owner_id = $8
	`, body.Name, newType, newVisibility, newStart, newEnd, body.Location, id, ownerID)
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
