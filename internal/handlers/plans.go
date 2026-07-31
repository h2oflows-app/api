package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

// PlanHandler handles /plans (Events, web#354), /plan-runs (calendar Runs),
// and /me/calendar* routes. Split across plans.go (event CRUD + shared
// helpers), plan_runs.go (calendar-run CRUD), calendar.go (/me/calendar*).
//
// # web#354 A1 model (binding)
//
// Events (was "plans") and Runs (was "plan_runs", table `calendar_runs`) are
// coupled ONLY by date — an event has no FK to its runs, and a run has no
// plan_id at all (dropped, mig 000145). "Runs during this Event" (renderPlan
// below) is a pure date-containment query: the event owner's own
// calendar_runs whose run_date falls within [event.start_date,
// event.end_date]. Events are owner-only (no visibility/type concept
// anymore — both dropped). Deleting an event tombstones the event alone;
// its runs are never touched.
//
// run_invites (crew/RSVP + invites, table `run_invites`, was `plan_members`
// through the A1 bridge window) is web#354 A2's rework — migration 000146
// drops plan_members and replaces it with a fresh, Run-scoped-only table
// (no plan_id/event_id at all). Its run_id column keys the per-run crew/RSVP
// embeds below.
type PlanHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
	mailer        mail.Mailer
	rl            *reportRateLimiter // reused: generic (owner,limit,window) limiter — see reports.go
}

// NewPlanHandler's mailer param is API-2 Invite Sync: UpdateRun/DeleteRun
// (plan_runs.go) fire notifyRunMaterialChange/notifyRunCancelled
// (notifications.go), which need a mail.Mailer the same way InviteHandler
// already does — nil degrades to mail.NoopMailer{}, same shape as
// NewInviteHandler (invites.go).
func NewPlanHandler(db *pgxpool.Pool, devFallbackID string, mailer mail.Mailer) *PlanHandler {
	if mailer == nil {
		mailer = mail.NoopMailer{}
	}
	return &PlanHandler{db: db, devFallbackID: devFallbackID, mailer: mailer, rl: newReportRateLimiter()}
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
// list queries in this file). Lets shared helpers run identically inside a
// tx or standalone against the pool.
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

// upsertUserEmail is API-1 Invite Sync's email-capture helper
// (INVITE_SYNC_PLAN.md §API): user_profiles has no email column (emails
// otherwise ride the JWT per request, auth.EmailFromContext) — user_emails
// (migration 000148) is the ONE stored-address table, and this is the ONLY
// writer of it. Called from every authed write path that already has a
// fresh email in hand: PlanHandler.CreateRun (plan_runs.go), the shared
// findOrCreatePaddledLog (plan_runs.go, same call site as ensureHandle
// above), and InviteHandler.AcceptInvite (invites.go, post-commit) — so
// organizer/attendee notifications (notifications.go) have an address to
// resolve later. Package-level (not a method) for the same reason
// ensureHandle is: reusable across handler types. Takes dbQueryer (not
// *pgxpool.Pool) so a caller inside a tx can pass it through too, though no
// current caller does.
//
// Best-effort by design: skips silently on an empty email (dev-fallback/
// API-key auth carries none — h.ownerID's devFallbackID path, or a caller
// that simply has no auth.EmailFromContext hit) and only LOGS a DB error
// rather than returning one — capturing an address must never fail the
// write it's piggybacking on.
func upsertUserEmail(ctx context.Context, q dbQueryer, ownerID, email string) {
	email = strings.TrimSpace(email)
	if email == "" {
		return
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO user_emails (owner_id, email, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (owner_id) DO UPDATE SET email = EXCLUDED.email, updated_at = NOW()
	`, ownerID, email); err != nil {
		log.Printf("upsertUserEmail: best-effort capture failed (owner=%s): %v", ownerID, err)
	}
}

// ensureHandle mirrors ReportHandler.ensureProfile (reports.go) — an event's
// share URL embeds the host's handle, lazily creating a user_profiles row
// from their email on first use, same as the first report/dashboard/etc.
// Package-level (not a *PlanHandler method) so it's reusable from
// findOrCreatePaddledLog (plan_runs.go), which needs it from both
// PlanHandler.LogMine and NudgeHandler.Confirm.
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
			`SELECT EXISTS(SELECT 1 FROM events WHERE owner_id=$1 AND slug=$2)`,
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
			`SELECT EXISTS(SELECT 1 FROM calendar_runs WHERE owner_id=$1 AND slug=$2)`,
			ownerID, candidate,
		).Scan(&exists)
		if !exists {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return fmt.Sprintf("%s-%d", base, time.Now().UnixMilli())
}

// runFilled counts run_invites rows with status='accepted' for runID — the
// shared crew-meter read used by renderPlan/renderPlanRun (per itinerary
// run) and RunCrewList/RunCrewAccept/JoinRun/AcceptInvite (invites.go).
// web#354 A2: re-keyed from plan_members (plan_run_id) to run_invites
// (run_id) — table dropped/replaced by migration 000146. Host is never
// counted (no membership row for the host). Scan errors propagate as a real
// error rather than silently failing open to filled=0, which would let an
// over-cap join/accept through on a transient DB error at any of this
// function's gating call sites.
func runFilled(ctx context.Context, q dbQueryer, runID string) (int, error) {
	var filled int
	if err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM run_invites WHERE run_id = $1::uuid AND status = 'accepted'`, runID,
	).Scan(&filled); err != nil {
		return 0, fmt.Errorf("runFilled: %w", err)
	}
	return filled, nil
}

// ── Response shapes ──────────────────────────────────────────────────────

// eventDetail is the Event (was "plan") detail payload — visibility/type
// dropped entirely (web#354 A1): events are owner-only, and the title
// carries whatever semantics `type` used to (festival/race/cruise/etc).
type eventDetail struct {
	ID          string  `json:"id"`
	Slug        string  `json:"slug"`
	Name        string  `json:"name"`
	StartDate   string  `json:"start_date"`
	EndDate     string  `json:"end_date"`
	Location    *string `json:"location,omitempty"`
	HostOwnerID string  `json:"host_owner_id"`
	HostHandle  string  `json:"host_handle"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// runCrewMeter is the per-run crew embed: {filled,max,looking_for_crew},
// always present on a planRunSummary (not conditional on LookingForCrew) so
// the web can render "not looking for crew" vs "0 of N" without a second
// field to check.
type runCrewMeter struct {
	Filled         int  `json:"filled"`
	Max            *int `json:"max,omitempty"`
	LookingForCrew bool `json:"looking_for_crew"`
}

// planRunSummary is a Run (was "plan_run", table calendar_runs) as embedded
// in the event itinerary (renderPlan) and returned standalone by
// renderPlanRun. The JSON key name stays `plan_run_id`-flavored where it
// names the /plan-runs/{id} permalink (routes are unchanged, web#354 §1) —
// only the Event-wrapper keys (`plan`→`event`, `plans`→`events`) were
// renamed.
type planRunSummary struct {
	ID          string  `json:"id"`
	Slug        string  `json:"slug"`
	UserReachID *string `json:"user_reach_id,omitempty"`
	// Name (web#354 A4) is the calendar run's OWN name — calendar_runs.name,
	// REQUIRED (NOT NULL, mig 000147), so always populated (not omitempty)
	// unlike the *string fields around it. ReachName is the attached library
	// run's own name (user_reaches.name — separate from `river_name`
	// elsewhere in this package, which is the actual named river, e.g.
	// "Blue River", not the user's own saved-run name, e.g. "Lower Blue");
	// nil for an orphaned run (user_reach_id cleared, ON DELETE SET NULL).
	Name       string       `json:"name"`
	ReachName  *string      `json:"reach_name,omitempty"`
	RunDate    string       `json:"run_date"`
	RunTime    *string      `json:"run_time,omitempty"`
	SortOrder  int16        `json:"sort_order"`
	GaugeCFS   *float64     `json:"gauge_cfs,omitempty"`
	FlowBand   *string      `json:"flow_band,omitempty"`
	FlowColor  *string      `json:"flow_color,omitempty"`
	Paddled    bool         `json:"paddled"`
	PaddledAt  *string      `json:"paddled_at,omitempty"`
	Notes      *string      `json:"notes,omitempty"`
	Companions *string      `json:"companions,omitempty"`
	CreatedAt  string       `json:"created_at,omitempty"`
	Crew       runCrewMeter `json:"crew"`
	// MeetupSpot/MeetupFeatureType/MeetupFeatureID: "meet up at" — MeetupSpot
	// is the always-present display text (typed or feature-name snapshot);
	// the FeatureType/ID pair (present only when a #312 rapid/access feature
	// was picked, one or neither, per the DB's XOR) lets the edit sheet
	// re-resolve and re-highlight the pick.
	MeetupSpot        *string `json:"meetup_spot,omitempty"`
	MeetupFeatureType *string `json:"meetup_feature_type,omitempty"`
	MeetupFeatureID   *string `json:"meetup_feature_id,omitempty"`
	// IsOwner: true when the viewer is this run's own owner (web#354 A2,
	// added on top of A1/W1 review — W1 hid owner-only affordances in the UI
	// for lack of a signal on GET /plan-runs/{id}). Always populated (not
	// omitempty) so the web can trust its zero value.
	IsOwner bool `json:"is_owner"`
	// Per-viewer RSVP on THIS run (the caller's own run_invites row, if
	// any): status invited|requested|accepted|declined + the row id the
	// accept/dismiss endpoints take. Drives the itinerary's per-run accept
	// buttons + "You're in" state.
	MyRSVP     *string `json:"my_rsvp,omitempty"`
	MyMemberID *string `json:"my_member_id,omitempty"`
	// UserReachSlug/UserReachOwnerHandle: the underlying river run's OWN
	// slug/owner handle (distinct from this run's Slug above) — lets
	// PlanRunLogSheet's edit-mode meet-up-spot combobox fetch that run's
	// rapids/access features (#312). Only populated by GetRun/renderPlanRun.
	// OwnerHandle nil means the run is the CALLER's own (web fetches via
	// /me/runs/{slug} instead of the public /users/{handle}/runs/{slug}).
	UserReachSlug        *string `json:"user_reach_slug,omitempty"`
	UserReachOwnerHandle *string `json:"user_reach_owner_handle,omitempty"`
	// CrewMembers (API-2, INVITE_SYNC_PLAN.md Amendments — "Crew who ran it
	// belongs in the log"): ACCEPTED crew handles only (never emails — an
	// email-only accepted row can't exist, accept requires an account),
	// visible to ANYONE who can see the run at all (planned = who's coming;
	// logged = who ran it) — no extra gate beyond the run's own visibility
	// check. Always a non-nil, possibly-empty slice (never omitted/null) so
	// the web can render "Crew: …" without a nil check. Populated by
	// renderPlanRun (plan_runs.go); left as its zero value ([]crewMemberHandle{},
	// set at construction) everywhere else planRunSummary is built (e.g.
	// renderPlan's event itinerary) — those callers weren't in API-2's scope.
	CrewMembers []crewMemberHandle `json:"crew_members"`
	// HostHandle (prod-testing follow-up to API-1/2 Invite Sync): this run's
	// OWNER's user_profiles.handle, so crew/viewers can render "Organizer @x"
	// without a second lookup — naming reused from inviteRunSummary.HostHandle
	// (invites.go), the equivalent field on the /me/invites run projection.
	// Always present (not omitempty, '' fallback), same "always populated"
	// contract as IsOwner above. Populated by renderPlanRun (plan_runs.go)
	// only; left at its "" zero value everywhere else planRunSummary is built
	// (e.g. renderPlan's event itinerary) — those callers weren't in scope.
	HostHandle string `json:"host_handle"`
	// HostDisplayName (display-names feature, mig 000130): the host's
	// user_profiles.display_name, additive alongside HostHandle — never a
	// replacement, @handle stays the primary identifier. nil when unset
	// (no profile row, or a profile row with no display_name typed in) so
	// the web falls back to rendering "@" + HostHandle. Same
	// renderPlanRun-only population contract as HostHandle above.
	HostDisplayName *string `json:"host_display_name,omitempty"`
}

// crewMemberHandle is one row of planRunSummary.CrewMembers — handle plus an
// optional display_name (display-names feature, mig 000130), additive
// alongside Handle (never a replacement — see HostDisplayName's doc
// comment). Every crew member here has an accepted run_invites row bound to
// a real account (renderPlanRun's INNER JOIN — see that query's comment), so
// DisplayName is nil only when that member has no display_name typed in,
// never because the row is "unbound".
type crewMemberHandle struct {
	Handle      string  `json:"handle"`
	DisplayName *string `json:"display_name,omitempty"`
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
		errorResponse(w, http.StatusTooManyRequests, "rate limit: 20 events per hour")
		return
	}

	var body struct {
		Name      string  `json:"name"`
		StartDate string  `json:"start_date"`
		EndDate   string  `json:"end_date"`
		Location  *string `json:"location"`
		// Type/Visibility/Runs/Invites/LookingForCrew/MaxCrew: pre-web#354
		// event fields. Decoded and SILENTLY IGNORED (same skew-tolerance
		// pattern this endpoint has always followed for LookingForCrew/
		// MaxCrew below) — the event-type/visibility concepts and inline
		// run/invite creation are gone entirely as of web#354 A1 (runs are
		// standalone now, created via POST /plan-runs; invites are
		// Run-scoped only via InviteHandler's POST /plan-runs/{id}/invite,
		// web#354 A2), so a stale client posting any of these must not 400.
		// Invites decodes as raw JSON (not a typed []inviteBody — that type
		// no longer exists; InviteHandler's invite body shape is run-scoped
		// now, {handle}|{email}, and was never a fit for a plan-level array
		// anyway) since the value itself is never read past the decode.
		Type           *string             `json:"type"`
		Visibility     *string             `json:"visibility"`
		Runs           []createPlanRunBody `json:"runs"`
		Invites        json.RawMessage     `json:"invites"`
		LookingForCrew *bool               `json:"looking_for_crew"`
		MaxCrew        *int                `json:"max_crew"`
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

	ctx := r.Context()

	// user_profiles must exist before the events INSERT — events.owner_id has
	// a hard FK to user_profiles(owner_id) (mig 000138/000145), unlike
	// reports.owner_id which has no FK. Ensure the profile row up front
	// rather than after commit, mirroring reports.go:202, or a first-time
	// user's first event 500s on the FK and never reaches ensureHandle at all.
	email, _ := auth.EmailFromContext(ctx)
	handle, herr := ensureHandle(ctx, h.db, ownerID, email)
	if herr != nil {
		errorResponse(w, http.StatusInternalServerError, "could not assign user profile")
		return
	}

	slugBase := kmlimport.Slugify(name)
	if slugBase == "" {
		slugBase = "event"
	}
	slug := h.uniquePlanSlug(ctx, ownerID, slugBase)

	var eventID string
	err = h.db.QueryRow(ctx, `
		INSERT INTO events
			(owner_id, slug, name, start_date, end_date, location)
		VALUES ($1,$2,$3,$4::date,$5::date,$6)
		RETURNING id
	`, ownerID, slug, name, body.StartDate, body.EndDate, body.Location).Scan(&eventID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("create event failed: %v", err))
		return
	}

	jsonResponse(w, http.StatusCreated, map[string]string{
		"id":   eventID,
		"slug": slug,
		"url":  fmt.Sprintf("/plans/%s/%s", handle, slug),
	})
}

// ── GET /plans/{handle}/{slug} + GET /plans/{id} ──────────────────────────

func (h *PlanHandler) GetByHandleSlug(w http.ResponseWriter, r *http.Request) {
	handle := chi.URLParam(r, "handle")
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()

	var eventID string
	err := h.db.QueryRow(ctx, `
		SELECT e.id FROM events e
		JOIN user_profiles up ON up.owner_id = e.owner_id
		WHERE LOWER(up.handle) = LOWER($1) AND e.slug = $2 AND e.deleted_at IS NULL
	`, handle, slug).Scan(&eventID)
	if err != nil {
		// Anon gets a uniform 401 whether or not the event exists — a
		// 404-vs-401 split would let anon probe which handle/slug pairs exist.
		if _, ok := h.ownerID(r); !ok {
			errorResponse(w, http.StatusUnauthorized, "authentication required")
			return
		}
		errorResponse(w, http.StatusNotFound, "event not found")
		return
	}
	h.renderPlan(w, r, eventID)
}

func (h *PlanHandler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var eventID string
	if err := h.db.QueryRow(r.Context(),
		`SELECT id FROM events WHERE id = $1::uuid AND deleted_at IS NULL`, id,
	).Scan(&eventID); err != nil {
		// Uniform 401 for anon — see GetByHandleSlug.
		if _, ok := h.ownerID(r); !ok {
			errorResponse(w, http.StatusUnauthorized, "authentication required")
			return
		}
		errorResponse(w, http.StatusNotFound, "event not found")
		return
	}
	h.renderPlan(w, r, eventID)
}

func (h *PlanHandler) renderPlan(w http.ResponseWriter, r *http.Request, eventID string) {
	ctx := r.Context()

	// web#354 A1: Events are owner-only, full stop — no visibility concept,
	// no ?invite= token carve-out (that carve-out moved to the run page,
	// renderPlanRun in plan_runs.go), no members query. Anon gets a uniform
	// 401 (never an existence oracle); authed non-owner 404s below once the
	// row is loaded.
	callerID, callerOK := h.ownerID(r)
	if !callerOK {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var ed eventDetail
	var createdAt, updatedAt time.Time
	err := h.db.QueryRow(ctx, `
		SELECT e.id, e.slug, e.name,
		       e.start_date::text, e.end_date::text, e.location,
		       e.owner_id, COALESCE(up.handle, ''),
		       e.created_at, e.updated_at
		FROM events e
		LEFT JOIN user_profiles up ON up.owner_id = e.owner_id
		WHERE e.id = $1 AND e.deleted_at IS NULL
	`, eventID).Scan(
		&ed.ID, &ed.Slug, &ed.Name,
		&ed.StartDate, &ed.EndDate, &ed.Location,
		&ed.HostOwnerID, &ed.HostHandle,
		&createdAt, &updatedAt,
	)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "event not found")
		return
	}
	ed.CreatedAt = createdAt.Format(time.RFC3339)
	ed.UpdatedAt = updatedAt.Format(time.RFC3339)

	if callerID != ed.HostOwnerID {
		errorResponse(w, http.StatusNotFound, "event not found")
		return
	}

	// "Runs during this Event" (web#354 §1 decouple semantics, binding): the
	// event owner's own calendar_runs whose run_date falls within
	// [start_date,end_date] — pure date containment, no FK/membership. Since
	// the viewer here is always the host (asserted above), per-run crew/RSVP
	// LATERALs read run_invites (re-keyed web#354 A2, was plan_members)
	// keyed off the run's own id via run_invites.run_id.
	rows, err := h.db.Query(ctx, `
		SELECT cr.id, cr.slug, cr.name, cr.user_reach_id::text, ur.name, cr.run_date::text, cr.run_time::text,
		       cr.sort_order, cr.gauge_cfs, cr.flow_band, cr.flow_color, cr.paddled, cr.paddled_at,
		       cr.notes, cr.companions, cr.created_at,
		       cr.looking_for_crew, cr.max_crew, COALESCE(cm.filled, 0),
		       cr.meetup_spot, cr.meetup_rapid_id::text, cr.meetup_access_id::text,
		       me.status, me.id
		FROM calendar_runs cr
		LEFT JOIN user_reaches ur ON ur.id = cr.user_reach_id
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS filled FROM run_invites ri
			WHERE ri.run_id = cr.id AND ri.status = 'accepted'
		) cm ON true
		LEFT JOIN LATERAL (
			SELECT ri.id::text, ri.status::text FROM run_invites ri
			WHERE ri.run_id = cr.id AND ri.member_owner_id = $1
		) me ON true
		WHERE cr.owner_id = $1 AND cr.run_date BETWEEN $2::date AND $3::date AND cr.deleted_at IS NULL
		ORDER BY cr.run_date, cr.sort_order
	`, ed.HostOwnerID, ed.StartDate, ed.EndDate)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	var dateOrder []string
	runsByDate := map[string][]planRunSummary{}
	for rows.Next() {
		var run planRunSummary
		run.CrewMembers = []crewMemberHandle{} // never null — see field doc comment
		var paddledAtRaw *time.Time
		var createdAtRaw time.Time
		var meetupRapidID, meetupAccessID *string
		if err := rows.Scan(
			&run.ID, &run.Slug, &run.Name, &run.UserReachID, &run.ReachName, &run.RunDate, &run.RunTime,
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
		// The itinerary viewer is always the event's host (asserted above,
		// callerID == ed.HostOwnerID) — every run listed here is the
		// caller's own.
		run.IsOwner = true

		if _, seen := runsByDate[run.RunDate]; !seen {
			dateOrder = append(dateOrder, run.RunDate)
		}
		runsByDate[run.RunDate] = append(runsByDate[run.RunDate], run)
	}

	itinerary := make([]itineraryDay, 0, len(dateOrder))
	for _, d := range dateOrder {
		itinerary = append(itinerary, itineraryDay{Date: d, Runs: runsByDate[d]})
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"event":     ed,
		"itinerary": itinerary,
	})
}

// ── GET /me/plans?from=&to= ────────────────────────────────────────────────

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

	conds := []string{"e.owner_id = $1", "e.deleted_at IS NULL"}
	args := []any{ownerID}
	if from != "" {
		if _, err := parseDate(from); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid from")
			return
		}
		args = append(args, from)
		conds = append(conds, fmt.Sprintf("e.end_date >= $%d::date", len(args)))
	}
	if to != "" {
		if _, err := parseDate(to); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid to")
			return
		}
		args = append(args, to)
		conds = append(conds, fmt.Sprintf("e.start_date <= $%d::date", len(args)))
	}

	// run_count: date-containment (web#354 §1), same rule as renderPlan's
	// itinerary — calendar_runs has no plan_id to join on anymore.
	query := fmt.Sprintf(`
		SELECT e.id, e.slug, e.name,
		       e.start_date::text, e.end_date::text, e.location,
		       (SELECT COUNT(*) FROM calendar_runs cr
		        WHERE cr.owner_id = e.owner_id
		          AND cr.run_date BETWEEN e.start_date AND e.end_date
		          AND cr.deleted_at IS NULL) AS run_count
		FROM events e
		WHERE %s
		ORDER BY e.start_date
	`, strings.Join(conds, " AND "))

	rows, err := h.db.Query(ctx, query, args...)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type myEvent struct {
		ID        string  `json:"id"`
		Slug      string  `json:"slug"`
		Name      string  `json:"name"`
		StartDate string  `json:"start_date"`
		EndDate   string  `json:"end_date"`
		Location  *string `json:"location,omitempty"`
		RunCount  int     `json:"run_count"`
	}

	events := []myEvent{}
	for rows.Next() {
		var e myEvent
		if err := rows.Scan(&e.ID, &e.Slug, &e.Name,
			&e.StartDate, &e.EndDate, &e.Location, &e.RunCount); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		events = append(events, e)
	}

	jsonResponse(w, http.StatusOK, map[string]any{"events": events})
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
		Name      *string `json:"name"`
		StartDate *string `json:"start_date"`
		EndDate   *string `json:"end_date"`
		Location  *string `json:"location"`
		// Type/Visibility/LookingForCrew/MaxCrew: legacy fields — decoded,
		// never applied (see Create's identical convention above).
		Type           *string `json:"type"`
		Visibility     *string `json:"visibility"`
		LookingForCrew *bool   `json:"looking_for_crew"`
		MaxCrew        *int    `json:"max_crew"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	ctx := r.Context()

	var curStart, curEnd string
	err := h.db.QueryRow(ctx, `
		SELECT start_date::text, end_date::text
		FROM events WHERE id = $1::uuid AND owner_id = $2 AND deleted_at IS NULL
	`, id, ownerID).Scan(&curStart, &curEnd)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "event not found")
		return
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
	// web#354 A1: the old "shrinking dates can't orphan a child run outside
	// its range" guard is gone — runs are decoupled (date-containment only,
	// no parent FK), so shrinking an event's dates just drops it from that
	// event's itinerary view; the run itself is untouched.

	_, err = h.db.Exec(ctx, `
		UPDATE events SET
			name       = COALESCE($1, name),
			start_date = $2::date,
			end_date   = $3::date,
			location   = COALESCE($4, location),
			updated_at = NOW()
		WHERE id = $5::uuid AND owner_id = $6
	`, body.Name, newStart, newEnd, body.Location, id, ownerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("update failed: %v", err))
		return
	}

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── DELETE /plans/{id} ────────────────────────────────────────────────────
// Tombstones the event ONLY (web#354 A1 — decoupled, no child-run cascade).

func (h *PlanHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	tag, err := h.db.Exec(ctx,
		`UPDATE events SET deleted_at = NOW(), updated_at = NOW() WHERE id = $1::uuid AND owner_id = $2 AND deleted_at IS NULL`,
		id, ownerID,
	)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "event not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
