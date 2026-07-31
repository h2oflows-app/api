package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/h2oflow/h2oflow/apps/api/internal/flow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxMeetupSpotLen caps meetup_spot (free text or a feature-name snapshot) —
// generous enough for "the 'T' parking lot on Foxton, past the gate" style
// directions without letting it balloon into a second notes field.
const maxMeetupSpotLen = 200

// meetupFeatureBody is the optional "pick one of this run's own features"
// half of Meet-up-at: a rapid or an access point (#312) belonging to the
// SAME user_reach as the calendar_run. Type discriminates which table ID
// resolves against.
type meetupFeatureBody struct {
	Type string `json:"type"` // "rapid" | "access"
	ID   string `json:"id"`
}

// meetupEligibleAccessTypes mirrors the editable access-type set
// (feedback_editable_access_types.md) MINUS put_in/take_out — those are
// already implicit meeting points (you meet at the put-in by definition),
// so they aren't offered as a separate "meet up at" pick.
var meetupEligibleAccessTypes = map[string]bool{
	"camp": true, "parking": true, "boat_ramp": true,
	"intermediate": true, "shuttle_drop": true,
}

// accessTypeMeetupLabel is the readable fallback SUMMARY used to snapshot
// meetup_spot when a picked reach_access row has no name (reach_access.name
// is nullable, unlike rapids.name).
func accessTypeMeetupLabel(accessType string) string {
	switch accessType {
	case "camp":
		return "Campsite"
	case "parking":
		return "Parking area"
	case "boat_ramp":
		return "Boat ramp"
	case "intermediate":
		return "Intermediate access"
	case "shuttle_drop":
		return "Shuttle drop"
	default:
		return "Access point"
	}
}

// validateRunName trims a caller-supplied calendar-run name and rejects an
// empty/whitespace-only result. Unlike validateMeetupSpot's "empty string
// clears the field" convention, name has no clear state — it's REQUIRED
// (calendar_runs.name is NOT NULL, mig 000147), so an explicit empty string
// is always a 422, never a silent no-op. nil (key omitted) passes through
// untouched — PATCH's ordinary "didn't touch this field" case; POST
// (creation) doesn't call this at all, since there Name is a plain required
// string, not a *string.
func validateRunName(name *string) (*string, *apiError) {
	if name == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*name)
	if trimmed == "" {
		return nil, &apiError{http.StatusUnprocessableEntity, "name cannot be empty"}
	}
	return &trimmed, nil
}

// validateMeetupSpot trims and length-caps a caller-supplied meetup_spot.
// nil (key omitted) passes through untouched — callers use nil to mean
// "don't touch this field" per this package's COALESCE-patch convention.
func validateMeetupSpot(spot *string) (*string, *apiError) {
	if spot == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*spot)
	if len(trimmed) > maxMeetupSpotLen {
		return nil, &apiError{http.StatusUnprocessableEntity, fmt.Sprintf("meetup_spot must be %d characters or fewer", maxMeetupSpotLen)}
	}
	return &trimmed, nil
}

// resolveMeetupFeature validates a picked meetup feature against
// userReachID — the feature must belong to THIS run's own user_reach (#312
// rapids/reach_access are keyed to user_reaches), otherwise 422 rather than
// silently attaching someone else's pin. Returns the resolved (rapidID,
// accessID) pair (exactly one set) plus a snapshot display name to fall back
// into meetup_spot when the caller didn't supply their own text.
func resolveMeetupFeature(ctx context.Context, q dbQueryer, userReachID string, mf *meetupFeatureBody) (rapidID, accessID *string, snapshot string, apiErr *apiError) {
	if mf == nil {
		return nil, nil, "", nil
	}
	if userReachID == "" {
		return nil, nil, "", &apiError{http.StatusUnprocessableEntity, "cannot set a meetup feature: no river run attached"}
	}
	switch mf.Type {
	case "rapid":
		var id, name string
		if qerr := q.QueryRow(ctx,
			`SELECT id, name FROM rapids WHERE id = $1::uuid AND user_reach_id = $2::uuid`,
			mf.ID, userReachID,
		).Scan(&id, &name); qerr != nil {
			return nil, nil, "", &apiError{http.StatusUnprocessableEntity, "meetup rapid does not belong to this river run"}
		}
		return &id, nil, name, nil
	case "access":
		var id, accessType string
		var name *string
		if qerr := q.QueryRow(ctx,
			`SELECT id, access_type, name FROM reach_access
			 WHERE id = $1::uuid AND user_reach_id = $2::uuid`,
			mf.ID, userReachID,
		).Scan(&id, &accessType, &name); qerr != nil {
			return nil, nil, "", &apiError{http.StatusUnprocessableEntity, "meetup access point does not belong to this river run"}
		}
		if !meetupEligibleAccessTypes[accessType] {
			return nil, nil, "", &apiError{http.StatusUnprocessableEntity, "this access point's type isn't offered as a meetup spot"}
		}
		display := accessTypeMeetupLabel(accessType)
		if name != nil && strings.TrimSpace(*name) != "" {
			display = *name
		}
		return nil, &id, display, nil
	default:
		return nil, nil, "", &apiError{http.StatusUnprocessableEntity, `meetup_feature.type must be "rapid" or "access"`}
	}
}

// meetupFeatureTypeID turns a scanned (rapidID, accessID) pair — at most one
// set, per the DB's XOR check — into the (type, id) shape response payloads
// expose for the edit sheet to re-resolve the picked feature.
func meetupFeatureTypeID(rapidID, accessID *string) (featureType, featureID *string) {
	switch {
	case rapidID != nil:
		t := "rapid"
		return &t, rapidID
	case accessID != nil:
		t := "access"
		return &t, accessID
	default:
		return nil, nil
	}
}

// createPlanRunBody is the request shape for a standalone calendar run
// (POST /plan-runs, web#354 A1 — runs are no longer created inline under an
// event; the old "add a run to this plan" endpoint is removed).
// LookingForCrew/MaxCrew/MeetupSpot/MeetupFeature are per-run (unchanged).
// Name (web#354 A4) is the calendar run's OWN name — REQUIRED, independent
// of the attached library run's name (user_reaches.name, exposed separately
// as reach_name in every response, see planRunSummary).
type createPlanRunBody struct {
	Name           string             `json:"name"`
	UserReachID    *string            `json:"user_reach_id"`
	ReachSlug      *string            `json:"reach_slug"`
	RunDate        string             `json:"run_date"`
	RunTime        *string            `json:"run_time"`
	Notes          *string            `json:"notes"`
	Companions     *string            `json:"companions"`
	Paddled        *bool              `json:"paddled"`
	LookingForCrew *bool              `json:"looking_for_crew"`
	MaxCrew        *int               `json:"max_crew"`
	MeetupSpot     *string            `json:"meetup_spot"`
	MeetupFeature  *meetupFeatureBody `json:"meetup_feature"`
}

// validateCrewFields enforces "looking_for_crew requires a positive
// max_crew".
func validateCrewFields(lookingForCrew *bool, maxCrew *int) error {
	if lookingForCrew != nil && *lookingForCrew && (maxCrew == nil || *maxCrew <= 0) {
		return fmt.Errorf("max_crew is required (and must be > 0) when looking_for_crew is true")
	}
	return nil
}

// insertPlanRun resolves the river run (user_reach_id or reach_slug),
// flow-stamps it, and inserts one standalone calendar_runs row via q — a
// pool or a tx. Returns an *apiError for anything that should surface as
// non-500 (400/404/422).
//
// web#354 A1: runs are decoupled from events entirely (no plan_id column,
// no parent date-range to validate against) — this used to take
// planID/planStart/planEnd and call validateRunDateInRange; both are gone.
func (h *PlanHandler) insertPlanRun(
	ctx context.Context, q dbQueryer,
	hostOwnerID string,
	body createPlanRunBody,
) (id, slug string, err error) {
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return "", "", &apiError{http.StatusUnprocessableEntity, "name required"}
	}
	if body.RunDate == "" {
		return "", "", &apiError{http.StatusBadRequest, "run_date is required"}
	}
	if verr := validateCrewFields(body.LookingForCrew, body.MaxCrew); verr != nil {
		return "", "", &apiError{http.StatusBadRequest, verr.Error()}
	}
	meetupSpot, mverr := validateMeetupSpot(body.MeetupSpot)
	if mverr != nil {
		return "", "", mverr
	}

	var userReachID, reachName, reachSlug string
	switch {
	case body.UserReachID != nil && *body.UserReachID != "":
		if err := q.QueryRow(ctx,
			`SELECT id, name, slug FROM user_reaches WHERE id = $1::uuid AND deleted_at IS NULL`,
			*body.UserReachID,
		).Scan(&userReachID, &reachName, &reachSlug); err != nil {
			return "", "", &apiError{http.StatusNotFound, "river run not found"}
		}
	case body.ReachSlug != nil && *body.ReachSlug != "":
		// Slug is UNIQUE(owner_id,slug), not globally unique — prefer a
		// match owned by the host (mirrors user_reaches.go Delete's
		// ORDER BY (owner_id = $N) DESC disambiguation).
		if err := q.QueryRow(ctx,
			`SELECT id, name, slug FROM user_reaches
			 WHERE slug = $1 AND deleted_at IS NULL
			 ORDER BY (owner_id = $2) DESC LIMIT 1`,
			*body.ReachSlug, hostOwnerID,
		).Scan(&userReachID, &reachName, &reachSlug); err != nil {
			return "", "", &apiError{http.StatusNotFound, "river run not found"}
		}
	default:
		return "", "", &apiError{http.StatusBadRequest, "user_reach_id or reach_slug is required"}
	}
	_ = reachName

	rapidID, accessID, featureSnapshot, ferr := resolveMeetupFeature(ctx, q, userReachID, body.MeetupFeature)
	if ferr != nil {
		return "", "", ferr
	}
	if featureSnapshot != "" && (meetupSpot == nil || *meetupSpot == "") {
		meetupSpot = &featureSnapshot
	}

	var runTimeVal *string
	if body.RunTime != nil && *body.RunTime != "" {
		runTimeVal = body.RunTime
	}

	paddled := body.Paddled != nil && *body.Paddled
	var stamp flow.StampResult
	var paddledAt *time.Time

	if paddled {
		today, terr := userToday(ctx, q, hostOwnerID)
		if terr != nil {
			return "", "", &apiError{http.StatusInternalServerError, "could not resolve local date"}
		}
		rd, perr := parseDate(body.RunDate)
		if perr != nil {
			return "", "", &apiError{http.StatusBadRequest, "invalid run_date"}
		}
		if rd.After(today) {
			return "", "", &apiError{http.StatusUnprocessableEntity, "cannot mark a future run paddled"}
		}
		noon, nerr := localNoonUTC(ctx, q, hostOwnerID, body.RunDate)
		if nerr != nil {
			return "", "", &apiError{http.StatusInternalServerError, "could not resolve local time"}
		}
		stamp = flow.StampFull(ctx, q, userReachID, noon)
		now := time.Now().UTC()
		paddledAt = &now
	} else {
		runTimeStr := ""
		if runTimeVal != nil {
			runTimeStr = *runTimeVal
		}
		stamp = flow.StampFull(ctx, q, userReachID, flow.ObservedAt(body.RunDate, runTimeStr))
	}
	stampedAt := time.Now().UTC()

	slugBase := reachSlug
	if slugBase == "" {
		slugBase = "run"
	}
	slug = uniqueRunSlug(ctx, q, hostOwnerID, slugBase+"-"+body.RunDate)

	lookingForCrew := body.LookingForCrew != nil && *body.LookingForCrew

	err = q.QueryRow(ctx, `
		INSERT INTO calendar_runs
			(owner_id, name, user_reach_id, slug, run_date, run_time,
			 gauge_cfs, flow_band, flow_color, gauge_id, stamped_at,
			 paddled, paddled_at, notes, companions, looking_for_crew, max_crew,
			 meetup_spot, meetup_rapid_id, meetup_access_id)
		VALUES ($1,$2,$3::uuid,$4,$5::date,$6,$7,$8,$9,$10::uuid,$11,$12,$13,$14,$15,$16,$17,$18,$19::uuid,$20::uuid)
		RETURNING id
	`,
		hostOwnerID, name, userReachID, slug, body.RunDate, runTimeVal,
		stamp.CFS, stamp.Band, stamp.Color, stamp.GaugeID, stampedAt,
		paddled, paddledAt, body.Notes, body.Companions, lookingForCrew, body.MaxCrew,
		meetupSpot, rapidID, accessID,
	).Scan(&id)
	if err != nil {
		return "", "", &apiError{http.StatusInternalServerError, fmt.Sprintf("create run failed: %v", err)}
	}
	return id, slug, nil
}

// ── POST /plan-runs ───────────────────────────────────────────────────────
// Standalone run create (web#354 A1 — replaces the removed
// POST /plans/{id}/runs; runs are no longer children of an event).

func (h *PlanHandler) CreateRun(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var body createPlanRunBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	ctx := r.Context()

	id, slug, err := h.insertPlanRun(ctx, h.db, ownerID, body)
	if err != nil {
		h.respondAPIError(w, err)
		return
	}

	// API-1 Invite Sync: best-effort capture the caller's current login
	// email (user_emails, migration 000148) so organizer-facing
	// notifications (notifyOrganizerAccepted etc., notifications.go) have
	// an address to resolve later — see upsertUserEmail's doc comment
	// (plans.go).
	if email, ok := auth.EmailFromContext(ctx); ok {
		upsertUserEmail(ctx, h.db, ownerID, email)
	}

	jsonResponse(w, http.StatusCreated, map[string]string{"id": id, "slug": slug})
}

// ── GET /plan-runs/{param} ───────────────────────────────────────────────
// Resolution order (binding, permalink-critical — unchanged by web#354):
// (1) id, (2) source_report_id (preserves old /reports/{uuid} links),
// (3) slug — only if globally unique across owners.

func (h *PlanHandler) GetRun(w http.ResponseWriter, r *http.Request) {
	param := chi.URLParam(r, "param")
	ctx := r.Context()
	_, callerOK := h.ownerID(r)

	var runID string
	if err := h.db.QueryRow(ctx,
		`SELECT id FROM calendar_runs WHERE id = $1::uuid AND deleted_at IS NULL`, param,
	).Scan(&runID); err != nil {
		if err2 := h.db.QueryRow(ctx,
			`SELECT id FROM calendar_runs WHERE source_report_id = $1::uuid AND deleted_at IS NULL`, param,
		).Scan(&runID); err2 != nil {
			if err3 := h.db.QueryRow(ctx, `
				SELECT id FROM calendar_runs
				WHERE slug = $1 AND deleted_at IS NULL
				  AND (SELECT COUNT(*) FROM calendar_runs WHERE slug = $1 AND deleted_at IS NULL) = 1
			`, param).Scan(&runID); err3 != nil {
				// web#354 A1 minor fix (findings[3]): an anonymous caller must
				// get the SAME 401 here as an existing-but-unauthorized run
				// gets below in renderPlanRun — a 404-here/401-there split let
				// anon distinguish "no such run" from "real run I can't see"
				// purely by status code, an existence oracle the renderPlanRun
				// gate below claims (in comment) not to allow. Authenticated
				// callers are unaffected and keep the ordinary 404. A valid
				// ?invite= token only ever matters once a run actually
				// resolves, so it plays no part in this branch.
				if !callerOK {
					errorResponse(w, http.StatusUnauthorized, "authentication required")
					return
				}
				errorResponse(w, http.StatusNotFound, "run not found")
				return
			}
		}
	}

	h.renderPlanRun(w, r, runID)
}

// runInviteTokenGrant reports whether the raw ?invite= token (hashed) grants
// access to THIS specific run — web#354 A1: the token carve-out moves from
// the event page to the run page, and now grants exactly the one run it's
// checked against, not a whole event's fan-out. web#354 A2: re-keyed to
// run_invites (run_id, was plan_members.plan_run_id) — a token stops
// granting access the moment AcceptInvite consumes it (nulls
// invite_token_hash on accept), so this is single-use by construction, not
// by any extra bookkeeping here.
func runInviteTokenGrant(ctx context.Context, db *pgxpool.Pool, runID, token string) bool {
	if token == "" || runID == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	var exists bool
	_ = db.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM run_invites WHERE run_id = $1::uuid AND invite_token_hash = $2)
	`, runID, hash).Scan(&exists)
	return exists
}

func (h *PlanHandler) renderPlanRun(w http.ResponseWriter, r *http.Request, runID string) {
	ctx := r.Context()

	callerID, callerOK := h.ownerID(r)
	// A pending email invite carries member_owner_id=NULL until accept (see
	// run_invites DDL) — matching only member_owner_id=$2 below would 404 a
	// logged-in invitee whose JWT email matches a still-pending invite_email,
	// same class of bug MyInvites/AcceptInvite/DismissInvite already guard
	// against with this same email fallback (invites.go).
	callerEmail, _ := auth.EmailFromContext(ctx)

	var run planRunSummary
	run.CrewMembers = []crewMemberHandle{} // never null — see planRunSummary.CrewMembers doc comment
	var runTime *string
	var paddledAtRaw *time.Time
	var createdAtRaw time.Time
	var hostOwnerID string
	var meetupRapidID, meetupAccessID *string
	var userReachOwnerID, userReachOwnerHandle *string

	err := h.db.QueryRow(ctx, `
		SELECT cr.id, cr.slug, cr.name, cr.user_reach_id::text, ur.name, cr.run_date::text, cr.run_time::text,
		       cr.sort_order, cr.gauge_cfs, cr.flow_band, cr.flow_color, cr.paddled, cr.paddled_at,
		       cr.notes, cr.companions, cr.created_at,
		       cr.looking_for_crew, cr.max_crew, COALESCE(cm.filled, 0),
		       cr.meetup_spot, cr.meetup_rapid_id::text, cr.meetup_access_id::text,
		       cr.owner_id,
		       ur.slug, ur.owner_id::text, urp.handle,
		       me.status, me.id, COALESCE(hostp.handle, ''), hostp.display_name
		FROM calendar_runs cr
		LEFT JOIN user_reaches ur ON ur.id = cr.user_reach_id
		LEFT JOIN user_profiles urp ON urp.owner_id = ur.owner_id
		LEFT JOIN user_profiles hostp ON hostp.owner_id = cr.owner_id
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS filled FROM run_invites ri
			WHERE ri.run_id = cr.id AND ri.status = 'accepted'
		) cm ON true
		LEFT JOIN LATERAL (
			SELECT ri.id::text, ri.status::text FROM run_invites ri
			WHERE ri.run_id = cr.id
			  AND (ri.member_owner_id = $2 OR (ri.member_owner_id IS NULL AND LOWER(ri.invite_email) = LOWER($3)))
		) me ON true
		WHERE cr.id = $1 AND cr.deleted_at IS NULL
	`, runID, callerID, callerEmail).Scan(
		&run.ID, &run.Slug, &run.Name, &run.UserReachID, &run.ReachName, &run.RunDate, &runTime,
		&run.SortOrder, &run.GaugeCFS, &run.FlowBand, &run.FlowColor, &run.Paddled, &paddledAtRaw,
		&run.Notes, &run.Companions, &createdAtRaw,
		&run.Crew.LookingForCrew, &run.Crew.Max, &run.Crew.Filled,
		&run.MeetupSpot, &meetupRapidID, &meetupAccessID,
		&hostOwnerID,
		&run.UserReachSlug, &userReachOwnerID, &userReachOwnerHandle,
		&run.MyRSVP, &run.MyMemberID, &run.HostHandle, &run.HostDisplayName,
	)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}
	run.RunTime = runTime
	// is_owner (web#354 A2 — added on top of A1/W1 review: W1 hid owner
	// affordances for lack of a signal). callerID is "" for an anonymous
	// caller, which never equals a real hostOwnerID.
	run.IsOwner = callerOK && callerID == hostOwnerID
	if paddledAtRaw != nil {
		s := paddledAtRaw.Format(time.RFC3339)
		run.PaddledAt = &s
	}
	run.CreatedAt = createdAtRaw.Format(time.RFC3339)
	run.MeetupFeatureType, run.MeetupFeatureID = meetupFeatureTypeID(meetupRapidID, meetupAccessID)
	// UserReachOwnerHandle stays nil when the run belongs to the CALLER
	// (their own river run) — the web's contract (planRun.ts) is "nil means
	// fetch via /me/runs/{slug}", so don't send the handle back in that case
	// even though we resolved it above.
	if userReachOwnerID != nil && *userReachOwnerID != callerID {
		run.UserReachOwnerHandle = userReachOwnerHandle
	}

	// web#354 A1 visibility gate (§1, binding), replacing the old
	// "parent event public/private" check: owner, OR paddled=true (any
	// authenticated user — "only trips that have logs may be seen by other
	// h2oflows users"), OR invited/crew (a run_invites row for THIS run,
	// re-keyed web#354 A2), OR looking_for_crew AND run_date >= the caller's
	// local today, OR a valid ?invite= token scoped to this run (the sole
	// anon carve-out — moved here from the event page). Anon without a
	// grant gets 401; authed without a grant gets 404. This is NOT an
	// existence oracle on its own ONLY because GetRun's not-found path
	// (above) also 401s an anonymous caller before ever reaching here
	// (web#354 A1 minor fix, findings[3]) — the two must be kept in sync.
	tokenGrant := runInviteTokenGrant(ctx, h.db, run.ID, r.URL.Query().Get("invite"))
	if !callerOK && !tokenGrant {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	allowed := tokenGrant || run.Paddled || run.IsOwner
	if !allowed && callerOK {
		if run.Crew.LookingForCrew {
			if today, terr := userToday(ctx, h.db, callerID); terr == nil {
				allowed = run.RunDate >= today.Format("2006-01-02")
			}
		}
		if !allowed {
			h.db.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM run_invites
					WHERE run_id = $1 AND status IN ('invited','accepted')
					  AND (member_owner_id = $2 OR (member_owner_id IS NULL AND LOWER(invite_email) = LOWER($3))))
			`, run.ID, callerID, callerEmail).Scan(&allowed)
		}
	}
	if !allowed {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	// API-2 crew_members (INVITE_SYNC_PLAN.md Amendments): ACCEPTED crew
	// handles only, run AFTER the visibility gate above so a caller who
	// can't see the run at all never triggers this query. INNER JOIN
	// user_profiles is deliberate, not defensive: an accepted row always has
	// a real member_owner_id (accept requires an account, AcceptInvite binds
	// it), so this can never silently drop a real crew member — it just
	// keeps the query from needing a COALESCE fallback a LEFT JOIN would
	// otherwise require.
	if crewRows, cerr := h.db.Query(ctx, `
		SELECT up.handle, up.display_name FROM run_invites ri
		JOIN user_profiles up ON up.owner_id = ri.member_owner_id
		WHERE ri.run_id = $1::uuid AND ri.status = 'accepted'
		ORDER BY up.handle
	`, run.ID); cerr == nil {
		for crewRows.Next() {
			var cm crewMemberHandle
			if crewRows.Scan(&cm.Handle, &cm.DisplayName) == nil {
				run.CrewMembers = append(run.CrewMembers, cm)
			}
		}
		crewRows.Close()
	}

	// web#354 A1: drop the `plan` wrapper — the run is standalone now, its
	// fields stay flat under `run`.
	jsonResponse(w, http.StatusOK, map[string]any{"run": run})
}

// ── PATCH /plan-runs/{id} ────────────────────────────────────────────────

// updatePlanRunBody's MeetupSpot/MeetupFeature follow the same COALESCE-patch
// convention as the rest of this body (key omitted = don't touch) with TWO
// documented, binding exceptions:
//  1. An explicit empty-string meetup_spot ("") clears BOTH meetup_spot and
//     any feature ref together. Sending a non-nil meetup_feature alongside an
//     explicit empty meetup_spot is rejected (422): "clear = empty text + no
//     feature", not a valid state to build a feature ref on top of.
//  2. An explicitly-present `"meetup_feature": null` (distinguished from an
//     OMITTED key via the raw-JSON presence check in UpdateRun below) clears
//     just the feature ref while leaving meetup_spot's text alone — this is
//     the "typed over a picked spot" case: the ref is now stale (doesn't
//     match the edited text) but the user's typing shouldn't be discarded.
//     An omitted meetup_feature key (the ordinary "didn't touch this field"
//     case) leaves any existing ref untouched, same as every other field
//     here.
//
// updatePlanRunBody.Name (web#354 A4) follows the same "key omitted = don't
// touch" convention as every other field here, with ONE difference from
// MeetupSpot: name has no "clear" state (calendar_runs.name is NOT NULL,
// mig 000147), so an explicit empty/whitespace-only string is always a 422
// (validateRunName), never a valid patch.
type updatePlanRunBody struct {
	Name           *string            `json:"name"`
	RunDate        *string            `json:"run_date"`
	RunTime        *string            `json:"run_time"`
	Notes          *string            `json:"notes"`
	Companions     *string            `json:"companions"`
	SortOrder      *int16             `json:"sort_order"`
	Paddled        *bool              `json:"paddled"`
	LookingForCrew *bool              `json:"looking_for_crew"`
	MaxCrew        *int               `json:"max_crew"`
	MeetupSpot     *string            `json:"meetup_spot"`
	MeetupFeature  *meetupFeatureBody `json:"meetup_feature"`
	GaugeCFS       *float64           `json:"gauge_cfs"`     // never client-settable — 400 on attempts
	FlowBand       *string            `json:"flow_band"`     // never client-settable — 400 on attempts
	UserReachID    *string            `json:"user_reach_id"` // never client-settable — 400 on attempts
}

func (h *PlanHandler) UpdateRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	bodyBytes, rerr := io.ReadAll(r.Body)
	if rerr != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var body updatePlanRunBody
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// Raw key-presence check (distinguishes an explicit `"meetup_feature":
	// null` from an omitted key — json.Unmarshal alone maps both to a nil
	// *meetupFeatureBody, see updatePlanRunBody doc comment above).
	var rawFields map[string]json.RawMessage
	_ = json.Unmarshal(bodyBytes, &rawFields) // body already validated above; best-effort
	_, meetupFeatureKeyPresent := rawFields["meetup_feature"]
	if body.GaugeCFS != nil || body.FlowBand != nil || body.UserReachID != nil {
		errorResponse(w, http.StatusBadRequest, "gauge_cfs, flow_band, and user_reach_id are system-managed and cannot be edited")
		return
	}

	ctx := r.Context()

	// curRunTime/curMeetupSpot/curName (API-2, INVITE_SYNC_PLAN.md item 2):
	// the pre-update snapshot of the material-field set {run_date, run_time,
	// meetup_spot, name} — diffed against the post-update RETURNING values
	// below to decide whether this PATCH touched anything an
	// accepted/pending invitee's external calendar cares about (ics_sequence
	// itself is bumped via its own UPDATE below, then re-read fresh by
	// notifyRunMaterialChange — no need to carry it through here).
	// curRunTime/curMeetupSpot are normalized the SAME way the
	// post-update RETURNING clauses are (COALESCE(...::text, '')) so the
	// comparison is apples-to-apples — in particular, run_time is a TIME
	// column that Postgres reformats on write (e.g. a client-sent "14:30"
	// reads back as "14:30:00"), so comparing against the caller's raw JSON
	// input instead of a DB-normalized value would false-positive on every
	// save that merely resends the same time in a different but equivalent
	// format.
	var curUserReachID, curRunDate, curRunTime, curMeetupSpot, curName string
	var curPaddled, curLookingForCrew bool
	var curPaddledAt *time.Time
	var curMaxCrew *int
	err := h.db.QueryRow(ctx, `
		SELECT COALESCE(cr.user_reach_id::text, ''), cr.run_date::text,
		       COALESCE(cr.run_time::text, ''), COALESCE(cr.meetup_spot, ''), cr.name,
		       cr.paddled, cr.paddled_at,
		       cr.looking_for_crew, cr.max_crew
		FROM calendar_runs cr
		WHERE cr.id = $1 AND cr.owner_id = $2 AND cr.deleted_at IS NULL
	`, runID, ownerID).Scan(&curUserReachID, &curRunDate, &curRunTime, &curMeetupSpot, &curName,
		&curPaddled, &curPaddledAt, &curLookingForCrew, &curMaxCrew)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	if curPaddled {
		// Locked state: ONLY name+notes editable on a paddled run — the
		// structural fields (date/meetup/crew/…) describe a trip that already
		// happened. The reports-era 24h clock on name+notes was removed
		// (user, 2026-07-29): descriptive text stays editable indefinitely.
		// Name (web#354 A4) is grouped with Notes here, not with the
		// structural fields blocked below: it's the same kind of
		// user-descriptive text (what you called the trip), not logistics.
		if body.RunDate != nil || body.RunTime != nil || body.Companions != nil ||
			body.SortOrder != nil || body.Paddled != nil ||
			body.LookingForCrew != nil || body.MaxCrew != nil ||
			body.MeetupSpot != nil || body.MeetupFeature != nil || meetupFeatureKeyPresent {
			errorResponse(w, http.StatusBadRequest, "run is locked after paddling — only name and notes can be edited")
			return
		}
		name, nerr := validateRunName(body.Name)
		if nerr != nil {
			h.respondAPIError(w, nerr)
			return
		}
		if name == nil && body.Notes == nil {
			jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		// API-2 material-change fan-out (item 2): name is in the material set
		// {run_date, run_time, meetup_spot, name} even on a paddled/locked
		// run — the guard above already rejects every OTHER material field on
		// a locked run, so name is the only one that can have actually
		// changed here; no need for a post-update RETURNING round trip like
		// the planned branch below uses, since we already know pre-update
		// (curName) and post-update (name) values from Go state alone.
		//
		// nameChanged also gates the ics_sequence bump, folded directly into
		// this UPDATE (review fix, plan_runs.go bump-desync finding): a
		// separate follow-up "UPDATE ... SET ics_sequence = ics_sequence + 1"
		// statement can fail AFTER the name/notes change has already
		// committed, silently desyncing calendars with no way to retry short
		// of a future edit. One atomic statement removes that window.
		nameChanged := name != nil && *name != curName
		tag, err := h.db.Exec(ctx,
			`UPDATE calendar_runs SET
				name         = COALESCE($1, name),
				notes        = COALESCE($2, notes),
				ics_sequence = CASE WHEN $5 THEN ics_sequence + 1 ELSE ics_sequence END,
				updated_at   = NOW()
			WHERE id = $3 AND owner_id = $4 AND deleted_at IS NULL`,
			name, body.Notes, runID, ownerID, nameChanged,
		)
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, "update failed")
			return
		}
		// deleted_at IS NULL above closes the same DELETE-race window as the
		// planned branch below: a PATCH that loses to a concurrent DeleteRun
		// now matches nothing here instead of silently mutating the tombstone.
		if tag.RowsAffected() == 0 {
			errorResponse(w, http.StatusNotFound, "run not found")
			return
		}
		if nameChanged {
			go notifyRunMaterialChange(h.mailer, h.db, runID)
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// Planned (unpaddled): run_date/run_time/notes/companions/sort_order/name
	// are freely editable. paddled:true is the mark-paddled transition. No
	// parent event date-range to validate against (decoupled, web#354 A1).
	name, nerr := validateRunName(body.Name)
	if nerr != nil {
		h.respondAPIError(w, nerr)
		return
	}

	newRunDate := curRunDate
	if body.RunDate != nil {
		newRunDate = *body.RunDate
	}

	newLookingForCrew := curLookingForCrew
	if body.LookingForCrew != nil {
		newLookingForCrew = *body.LookingForCrew
	}
	newMaxCrew := curMaxCrew
	if body.MaxCrew != nil {
		newMaxCrew = body.MaxCrew
	}
	if verr := validateCrewFields(&newLookingForCrew, newMaxCrew); verr != nil {
		errorResponse(w, http.StatusBadRequest, verr.Error())
		return
	}

	meetupSpot, mverr := validateMeetupSpot(body.MeetupSpot)
	if mverr != nil {
		h.respondAPIError(w, mverr)
		return
	}
	meetupSpotProvided := body.MeetupSpot != nil
	clearingMeetup := meetupSpotProvided && *meetupSpot == ""
	if clearingMeetup && body.MeetupFeature != nil {
		errorResponse(w, http.StatusUnprocessableEntity, "meetup_spot cannot be cleared while meetup_feature is set — clear both or send neither")
		return
	}
	// explicitClearFeature: `"meetup_feature": null` sent explicitly (key
	// present, value null) — clears just the ref, keeps meetup_spot's text.
	// clearingMeetup already covers the "clear both together" case, so this
	// only fires for the "typed over a pick" path (doc comment above).
	explicitClearFeature := meetupFeatureKeyPresent && body.MeetupFeature == nil && !clearingMeetup
	featureTouched := clearingMeetup || body.MeetupFeature != nil || explicitClearFeature

	// materialFieldPresent (review fix, bump-desync finding): whether this
	// PATCH's *request body* touches any key of the material set
	// {run_date, run_time, meetup_spot, meetup_feature, name} — presence,
	// not "did the value actually change" (that diff happens below, AFTER
	// the update, against the RETURNING values). Gates the ics_sequence
	// bump folded into the main UPDATE right below: a request that resends
	// an unchanged material value still bumps the sequence (a harmless gap
	// — RFC5545 only requires SEQUENCE to be monotonic, and no email goes
	// out for it since the notify goroutine is separately gated on the
	// post-update diff), in exchange for not needing a second statement
	// after the fact to decide "should I have bumped."
	materialFieldPresent := body.RunDate != nil || body.RunTime != nil ||
		body.MeetupSpot != nil || meetupFeatureKeyPresent || body.Name != nil

	var meetupRapidID, meetupAccessID *string
	if body.MeetupFeature != nil {
		rid, aid, snap, ferr := resolveMeetupFeature(ctx, h.db, curUserReachID, body.MeetupFeature)
		if ferr != nil {
			h.respondAPIError(w, ferr)
			return
		}
		meetupRapidID, meetupAccessID = rid, aid
		if snap != "" && (!meetupSpotProvided || *meetupSpot == "") {
			meetupSpot = &snap
			meetupSpotProvided = true
		}
	}

	markPaddled := body.Paddled != nil && *body.Paddled
	restamp := body.RunDate != nil || body.RunTime != nil || markPaddled

	var stamp flow.StampResult
	var paddledAt *time.Time

	if markPaddled {
		if curUserReachID == "" {
			errorResponse(w, http.StatusUnprocessableEntity, "cannot mark paddled: no river run attached")
			return
		}
		today, terr := userToday(ctx, h.db, ownerID)
		if terr != nil {
			errorResponse(w, http.StatusInternalServerError, "could not resolve local date")
			return
		}
		rd, perr := parseDate(newRunDate)
		if perr != nil {
			errorResponse(w, http.StatusBadRequest, "invalid run_date")
			return
		}
		if rd.After(today) {
			errorResponse(w, http.StatusUnprocessableEntity, "cannot mark a future run paddled")
			return
		}
		noon, nerr := localNoonUTC(ctx, h.db, ownerID, newRunDate)
		if nerr != nil {
			errorResponse(w, http.StatusInternalServerError, "could not resolve local time")
			return
		}
		stamp = flow.StampFull(ctx, h.db, curUserReachID, noon)
		now := time.Now().UTC()
		paddledAt = &now
	} else if restamp && curUserReachID != "" {
		runTimeStr := ""
		if body.RunTime != nil {
			runTimeStr = *body.RunTime
		}
		stamp = flow.StampFull(ctx, h.db, curUserReachID, flow.ObservedAt(newRunDate, runTimeStr))
	}

	// StampFull returning no reading (nil CFS — no gauge/no data at the
	// target time) must never blank out a previously stamped snapshot.
	// restampFlow (not the bare restamp intent) gates the flow columns, so a
	// re-stamp attempt that finds nothing simply keeps whatever was already
	// on the row.
	restampFlow := restamp && stamp.CFS != nil

	var stampedAt *time.Time
	if restampFlow {
		now := time.Now().UTC()
		stampedAt = &now
	}

	// newRunDateOut/newRunTimeOut/newMeetupSpotOut/newNameOut (API-2, item 2):
	// RETURNING the material set's POST-update, DB-normalized values in the
	// same statement — rather than recomputing "what did COALESCE/CASE just
	// resolve to" in Go — is the only way to diff against curRunTime/
	// curMeetupSpot below without re-deriving Postgres's own TIME
	// normalization/NULL-coalescing by hand.
	//
	// deleted_at IS NULL in the WHERE (review fix — resurrected-tombstone
	// finding): the pre-update snapshot SELECT above already filters
	// deleted_at IS NULL, but this UPDATE previously didn't — a PATCH
	// racing a concurrent DeleteRun could match the just-tombstoned row,
	// mutate it, and fan out a REQUEST .ics that beats DeleteRun's CANCEL on
	// SEQUENCE, resurrecting the deleted run on recipients' calendars. With
	// the filter added, a PATCH that loses that race now matches zero rows
	// (ErrNoRows below) instead.
	//
	// ics_sequence is folded into this same UPDATE, gated on
	// materialFieldPresent (computed above from raw request-body presence,
	// not the post-update diff) — see that variable's doc comment. Previously
	// this was a second, separate `UPDATE ... SET ics_sequence =
	// ics_sequence + 1` statement fired only after the diff below found a
	// real change; if THAT statement failed (transient DB error, pool
	// exhaustion, client-disconnect ctx cancellation), the material change
	// had already committed but no REQUEST email went out and the sequence
	// never advanced — calendars silently went stale until some future
	// edit. Folding it into the one atomic statement removes that window
	// entirely.
	newIcsSequence := 0
	var newRunDateOut, newRunTimeOut, newMeetupSpotOut, newNameOut string
	err = h.db.QueryRow(ctx, `
		UPDATE calendar_runs SET
			run_date         = COALESCE($1::date, run_date),
			run_time         = COALESCE($2, run_time),
			notes            = COALESCE($3, notes),
			companions       = COALESCE($4, companions),
			sort_order       = COALESCE($5, sort_order),
			gauge_cfs        = CASE WHEN $6 THEN $7 ELSE gauge_cfs END,
			flow_band        = CASE WHEN $6 THEN $8 ELSE flow_band END,
			flow_color       = CASE WHEN $6 THEN $9 ELSE flow_color END,
			gauge_id         = CASE WHEN $6 THEN $10::uuid ELSE gauge_id END,
			stamped_at       = COALESCE($11, stamped_at),
			paddled          = CASE WHEN $12 THEN TRUE ELSE paddled END,
			paddled_at       = COALESCE($13, paddled_at),
			looking_for_crew = $16,
			max_crew         = $17,
			meetup_spot      = CASE WHEN $18 THEN $19 ELSE meetup_spot END,
			meetup_rapid_id  = CASE WHEN $20 THEN $21::uuid ELSE meetup_rapid_id END,
			meetup_access_id = CASE WHEN $20 THEN $22::uuid ELSE meetup_access_id END,
			name             = COALESCE($23, name),
			ics_sequence     = CASE WHEN $24 THEN ics_sequence + 1 ELSE ics_sequence END,
			updated_at       = NOW()
		WHERE id = $14 AND owner_id = $15 AND deleted_at IS NULL
		RETURNING run_date::text, COALESCE(run_time::text, ''), COALESCE(meetup_spot, ''), name, ics_sequence
	`,
		body.RunDate, body.RunTime, body.Notes, body.Companions, body.SortOrder,
		restampFlow, stamp.CFS, stamp.Band, stamp.Color, stamp.GaugeID, stampedAt,
		markPaddled, paddledAt,
		runID, ownerID,
		newLookingForCrew, newMaxCrew,
		meetupSpotProvided, meetupSpot,
		featureTouched, meetupRapidID, meetupAccessID,
		name,
		materialFieldPresent,
	).Scan(&newRunDateOut, &newRunTimeOut, &newMeetupSpotOut, &newNameOut, &newIcsSequence)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either no such run, or it lost a race to a concurrent
			// DeleteRun's tombstone between the pre-update snapshot SELECT
			// above and this UPDATE — either way, nothing to update, and
			// definitely no material-change fan-out to fire.
			errorResponse(w, http.StatusNotFound, "run not found")
			return
		}
		errorResponse(w, http.StatusInternalServerError, "update failed")
		return
	}

	// API-2 material-change fan-out (item 2, diff-gating D3): strict value
	// comparison against the pre-update snapshot — a PATCH touching only
	// notes/companions/sort_order/crew/paddled leaves all four of these
	// identical and sends nothing. This is independent of
	// materialFieldPresent above: a PATCH that resends an unchanged material
	// value still bumped ics_sequence (harmless gap, see that var's doc
	// comment) but does NOT reach here truthy, so no email goes out for it.
	if newRunDateOut != curRunDate || newRunTimeOut != curRunTime ||
		newMeetupSpotOut != curMeetupSpot || newNameOut != curName {
		// newIcsSequence (RETURNING above) confirms the bump landed in the
		// same statement as the material change — logged rather than
		// threaded into notifyRunMaterialChange, which re-reads it fresh via
		// loadRunForNotify anyway (matching every other sender in
		// notifications.go, which only ever take a run_id).
		log.Printf("UpdateRun: material change run=%s new_ics_sequence=%d", runID, newIcsSequence)
		go notifyRunMaterialChange(h.mailer, h.db, runID)
	}

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── DELETE /plan-runs/{id} ───────────────────────────────────────────────

func (h *PlanHandler) DeleteRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	// web#354 A1: no parent-event guard anymore (decoupled) — tombstone by
	// owner_id alone.
	//
	// API-2 CANCEL fan-out (item 3): the SEQUENCE bump is folded into the
	// SAME statement as the tombstone (review fix, same fold as UpdateRun's
	// material UPDATE above) — this used to be a second, separate `UPDATE
	// ... SET ics_sequence = ics_sequence + 1` fired only after the
	// tombstone had already committed, so a failure there (deliberately
	// left un-retried, matching the old bumpSequenceAndNotifyMaterialChange
	// convention) meant the run was gone but no CANCEL .ics ever went out —
	// recipients' calendars would keep showing a "deleted" trip
	// indefinitely. Both changes are always unconditional together here (no
	// separate diff-gating like UpdateRun's material set), so there's no
	// reason to keep them as two statements. loadRunForNotify inside
	// notifyRunCancelled has no deleted_at filter by design (runNotifyInfo's
	// doc comment, notifications.go), so it still resolves this run's data
	// after the tombstone commits.
	tag, err := h.db.Exec(r.Context(), `
		UPDATE calendar_runs SET deleted_at = NOW(), updated_at = NOW(), ics_sequence = ics_sequence + 1
		WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL
	`, runID, ownerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	go notifyRunCancelled(h.mailer, h.db, runID)

	w.WriteHeader(http.StatusNoContent)
}

// ── POST /plan-runs/{id}/log-mine ────────────────────────────────────────
// The caller — the host or an accepted crew member (run_invites, re-keyed
// web#354 A2) — gets their own paddled log of this same river run without
// touching the host's run: "your logged flows stay yours."

func (h *PlanHandler) LogMine(w http.ResponseWriter, r *http.Request) {
	sourceRunID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	var hostOwnerID, userReachID, runDate, reachSlug, reachName string
	err := h.db.QueryRow(ctx, `
		SELECT cr.owner_id, COALESCE(cr.user_reach_id::text, ''), cr.run_date::text, COALESCE(ur.slug, ''), COALESCE(ur.name, '')
		FROM calendar_runs cr
		LEFT JOIN user_reaches ur ON ur.id = cr.user_reach_id
		WHERE cr.id = $1::uuid AND cr.deleted_at IS NULL
	`, sourceRunID).Scan(&hostOwnerID, &userReachID, &runDate, &reachSlug, &reachName)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}
	if userReachID == "" {
		errorResponse(w, http.StatusUnprocessableEntity, "this run has no river run attached to log")
		return
	}

	allowed := ownerID == hostOwnerID
	if !allowed {
		h.db.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM run_invites WHERE run_id = $1::uuid AND member_owner_id = $2 AND status = 'accepted')`,
			sourceRunID, ownerID,
		).Scan(&allowed)
	}
	if !allowed {
		errorResponse(w, http.StatusForbidden, "must be the host or an accepted crew member to log this run")
		return
	}

	today, terr := userToday(ctx, h.db, ownerID)
	if terr != nil {
		errorResponse(w, http.StatusInternalServerError, "could not resolve local date")
		return
	}
	rd, perr := parseDate(runDate)
	if perr != nil {
		errorResponse(w, http.StatusInternalServerError, "invalid run_date on source run")
		return
	}
	if rd.After(today) {
		errorResponse(w, http.StatusUnprocessableEntity, "cannot log a future run")
		return
	}

	myRunID, existed, ferr := findOrCreatePaddledLog(ctx, h.db, ownerID, userReachID, reachSlug, reachName, runDate, nil)
	if ferr != nil {
		errorResponse(w, http.StatusInternalServerError, ferr.Error())
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	jsonResponse(w, status, map[string]string{"plan_run_id": myRunID})
}

// findOrCreatePaddledLog is the "log-mine" find-or-create machinery shared
// by PlanHandler.LogMine (POST /plan-runs/{id}/log-mine — the host or an
// accepted crew member logging their own paddle of a visible run) and
// NudgeHandler.Confirm (POST /me/nudge/confirm — a user confirming a Tier-A
// nudge candidate they paddled).
//
// web#354 A1 MAJOR SIMPLIFICATION: no more "log-{date} Personal event"
// find/create/un-tombstone dance — runs are standalone now, so this just
// finds-or-creates one paddled calendar_runs row for (owner, user_reach_id,
// run_date). No event is created or touched, so no tx is needed either (a
// single INSERT, no multi-table coordination to keep atomic).
//
// Idempotent: if the caller already has a live paddled calendar_run for this
// exact reach+date, returns that existing row instead of creating a
// duplicate (existed=true — callers use this to pick 200 vs 201).
//
// reachName (web#354 A4) seeds the new row's REQUIRED name column — these
// two callers have no user-supplied name to fall back on (log-mine/
// nudge-confirm are one-tap auto-create flows, unlike POST /plan-runs'
// explicit body.Name), so it defaults to the library run's own name
// (user_reaches.name), same field every other calendar-domain display
// already reads. "Paddle" is the last-resort fallback for the
// theoretically-empty case (user_reaches.name is NOT NULL, so this only
// guards a caller passing "" outright), matching the COALESCE(ur.name,
// 'Paddle') convention already used elsewhere in this package.
func findOrCreatePaddledLog(
	ctx context.Context, db *pgxpool.Pool,
	ownerID, userReachID, reachSlug, reachName, runDate string, notes *string,
) (runID string, existed bool, err error) {
	var existingRunID string
	if qerr := db.QueryRow(ctx, `
		SELECT id FROM calendar_runs
		WHERE owner_id = $1 AND user_reach_id = $2::uuid AND run_date = $3::date
		  AND paddled AND deleted_at IS NULL
		LIMIT 1
	`, ownerID, userReachID, runDate).Scan(&existingRunID); qerr == nil {
		return existingRunID, true, nil
	}

	// calendar_runs.owner_id has no hard FK to user_profiles, but ensure the
	// caller's profile exists anyway (same as every other write path in this
	// package) so a first-time paddler's handle/share surfaces are ready.
	email, _ := auth.EmailFromContext(ctx)
	if _, herr := ensureHandle(ctx, db, ownerID, email); herr != nil {
		return "", false, fmt.Errorf("could not assign user profile: %w", herr)
	}
	// API-1 Invite Sync: best-effort capture (see upsertUserEmail's doc
	// comment, plans.go) — this is one of the three call sites the plan
	// names explicitly (POST /plan-runs, findOrCreatePaddledLog,
	// AcceptInvite).
	upsertUserEmail(ctx, db, ownerID, email)

	noon, nerr := localNoonUTC(ctx, db, ownerID, runDate)
	if nerr != nil {
		return "", false, fmt.Errorf("could not resolve local time: %w", nerr)
	}
	stamp := flow.StampFull(ctx, db, userReachID, noon)
	stampedAt := time.Now().UTC()
	paddledAt := time.Now().UTC()

	slugBase := reachSlug
	if slugBase == "" {
		slugBase = "run"
	}
	mySlug := uniqueRunSlug(ctx, db, ownerID, slugBase+"-"+runDate)

	name := reachName
	if name == "" {
		name = "Paddle"
	}

	var myRunID string
	if ierr := db.QueryRow(ctx, `
		INSERT INTO calendar_runs
			(owner_id, name, user_reach_id, slug, run_date,
			 gauge_cfs, flow_band, flow_color, gauge_id, stamped_at,
			 paddled, paddled_at, notes)
		VALUES ($1,$2,$3::uuid,$4,$5::date,$6,$7,$8,$9::uuid,$10,TRUE,$11,$12)
		RETURNING id
	`, ownerID, name, userReachID, mySlug, runDate,
		stamp.CFS, stamp.Band, stamp.Color, stamp.GaugeID, stampedAt, paddledAt, notes,
	).Scan(&myRunID); ierr != nil {
		return "", false, fmt.Errorf("create logged run failed: %w", ierr)
	}

	return myRunID, false, nil
}
