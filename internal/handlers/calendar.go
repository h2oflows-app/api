package handlers

import (
	"net/http"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
)

// ── GET /me/calendar?from=&to=&tz= ────────────────────────────────────────
// One range query -> {days, events, nudge_dot_dates}. Upserts tz when given
// (contract "Timezone rule": client sends Intl.DateTimeFormat().resolvedOptions().timeZone
// on every call).
//
// web#354 A1: days[].runs come straight from calendar_runs by owner+range —
// no JOIN, no plan_id (runs are decoupled from events). events[] (renamed
// from plans[]) is the owner's OWN events overlapping the range only — the
// old member/role/visibility UNION ALL branch is gone (events are owner-only
// now).
//
// web#354 A2 (decision #8): days[].runs ALSO includes runs the caller has an
// ACCEPTED run_invites row on (crew, not owner) — each run item now carries
// a `role` field ('owner'|'crew') so "trips I joined" surface on the
// calendar too, not just the host's own. events[] stays owner-only,
// unchanged.

func (h *PlanHandler) Calendar(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	from := q.Get("from")
	to := q.Get("to")
	tz := q.Get("tz")

	if from == "" || to == "" {
		errorResponse(w, http.StatusBadRequest, "from and to are required")
		return
	}
	if _, err := parseDate(from); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid from")
		return
	}
	if _, err := parseDate(to); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid to")
		return
	}

	// Broader email capture (API-1.x follow-up): this endpoint already
	// upserts the caller's tz on every visit below — piggyback a best-effort
	// upsertUserEmail (plans.go) the same way, so an active user's address
	// gets captured on their next calendar load instead of waiting for one
	// of the three narrower write paths (CreateRun/findOrCreatePaddledLog/
	// AcceptInvite) to fire. upsertUserEmail already no-ops silently on an
	// empty email (dev-fallback/API-key auth) and only log.Printf's a DB
	// failure — never blocks or fails this request.
	if email, ok := auth.EmailFromContext(ctx); ok {
		upsertUserEmail(ctx, h.db, ownerID, email)
	}

	if tz != "" {
		// Validate as an IANA zone before persisting — user_calendar_prefs.tz
		// is a plain TEXT column with no DB-side check, and a bogus value
		// would 500 every later `AT TIME ZONE tz` in userToday/localNoonUTC
		// (postgres "time zone not recognized") until overwritten. An
		// invalid zone is simply ignored rather than upserted.
		if _, tzErr := time.LoadLocation(tz); tzErr == nil {
			// Best-effort — an upsert failure shouldn't block rendering the
			// calendar; the guard below just keeps using whatever tz (or the
			// America/Denver default) is already on file.
			_, _ = h.db.Exec(ctx, `
				INSERT INTO user_calendar_prefs (owner_id, tz, updated_at)
				VALUES ($1, $2, NOW())
				ON CONFLICT (owner_id) DO UPDATE SET tz = EXCLUDED.tz, updated_at = NOW()
			`, ownerID, tz)
		}
	}

	// Name (web#354 A4) is the calendar run's OWN name (calendar_runs.name,
	// NOT NULL) — always populated, unlike ReachName (the attached library
	// run's own name, user_reaches.name, nil for an orphaned run).
	type calRun struct {
		ID          string   `json:"id"`
		UserReachID *string  `json:"user_reach_id,omitempty"`
		Name        string   `json:"name"`
		ReachName   *string  `json:"reach_name,omitempty"`
		FlowBand    *string  `json:"flow_band,omitempty"`
		FlowColor   *string  `json:"flow_color,omitempty"`
		GaugeCFS    *float64 `json:"gauge_cfs,omitempty"`
		Paddled     bool     `json:"paddled"`
		RunTime     *string  `json:"run_time,omitempty"`
		Role        string   `json:"role"` // "owner" | "crew" (web#354 A2 decision #8)
	}
	type calDay struct {
		Date         string   `json:"date"`
		Runs         []calRun `json:"runs"`
		NeedsConfirm bool     `json:"needs_confirm"`
	}

	// web#354 A2 decision #8: union the owner's own runs with runs they have
	// an ACCEPTED run_invites row on (crew) — SELECT DISTINCT in the inner
	// query is defensive (a host is NOT supposed to also hold an accepted
	// invite on their own run — JoinRun rejects host-self, and InviteToRun's
	// handle branch does too, but this is a should-not-happen invariant, not
	// a guaranteed one: DISTINCT keeps this query correct even if it's ever
	// violated).
	rows, err := h.db.Query(ctx, `
		SELECT id, user_reach_id, name, reach_name, run_date, flow_band, flow_color, gauge_cfs, paddled, run_time, role
		FROM (
			SELECT DISTINCT cr.id, cr.user_reach_id::text AS user_reach_id, cr.name, ur.name AS reach_name,
			       cr.run_date::text AS run_date, cr.flow_band, cr.flow_color, cr.gauge_cfs, cr.paddled,
			       cr.run_time::text AS run_time, cr.sort_order,
			       CASE WHEN cr.owner_id = $1 THEN 'owner' ELSE 'crew' END AS role
			FROM calendar_runs cr
			LEFT JOIN user_reaches ur ON ur.id = cr.user_reach_id
			WHERE cr.deleted_at IS NULL AND cr.run_date BETWEEN $2::date AND $3::date
			  AND (
			    cr.owner_id = $1
			    OR EXISTS(SELECT 1 FROM run_invites ri WHERE ri.run_id = cr.id AND ri.member_owner_id = $1 AND ri.status = 'accepted')
			  )
		) sub
		ORDER BY run_date, sort_order
	`, ownerID, from, to)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	var dateOrder []string
	runsByDate := map[string][]calRun{}
	for rows.Next() {
		var cr calRun
		var runDate string
		if err := rows.Scan(&cr.ID, &cr.UserReachID, &cr.Name, &cr.ReachName, &runDate,
			&cr.FlowBand, &cr.FlowColor, &cr.GaugeCFS, &cr.Paddled, &cr.RunTime, &cr.Role); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		if _, seen := runsByDate[runDate]; !seen {
			dateOrder = append(dateOrder, runDate)
		}
		runsByDate[runDate] = append(runsByDate[runDate], cr)
	}
	// needs_confirm (#246 A5): true on days with a Tier-A nudge candidate —
	// reuses the same scanCandidates/resolveInBand machinery as
	// GET /me/nudge/candidate (nudges.go), bounded to the 14-day nudge
	// lookback intersected with [from,to] so this never balloons into a
	// per-calendar-day scan even when the caller requests a full year.
	//
	// loc (the owner's calendar tz) is resolved ONCE here and threaded
	// through both this loop and the nudge_dot_dates loop below — resolveInBand
	// used to re-query user_calendar_prefs.tz for the SAME owner on every
	// single candidate, which on a year view (~90 candidates x this endpoint
	// being polled) turned into a real N+1 (#246 review).
	needsConfirmDates := map[string]bool{}
	var loc *time.Location
	{
		today, terr := userToday(ctx, h.db, ownerID)
		if terr == nil {
			if l, lerr := resolveOwnerLocation(ctx, h.db, ownerID); lerr == nil {
				loc = l
			}
			tierFrom := maxDateStr(from, today.AddDate(0, 0, -14).Format("2006-01-02"))
			tierTo := minDateStr(to, today.AddDate(0, 0, -1).Format("2006-01-02"))
			if loc != nil && tierFrom <= tierTo {
				if cands, cerr := scanCandidates(ctx, h.db, ownerID, tierFrom, tierTo, false); cerr == nil {
					for _, c := range cands {
						if _, inBand := resolveInBand(ctx, h.db, loc, c); inBand {
							needsConfirmDates[c.RunDate] = true
						}
					}
				}
			}
		}
	}

	days := make([]calDay, 0, len(dateOrder))
	for _, d := range dateOrder {
		days = append(days, calDay{Date: d, Runs: runsByDate[d], NeedsConfirm: needsConfirmDates[d]})
	}

	// events[] = the owner's own events overlapping [from,to] — owner-only,
	// no member/role/visibility branch (web#354 A1).
	type calEvent struct {
		ID         string  `json:"id"`
		Slug       string  `json:"slug"`
		Name       string  `json:"name"`
		StartDate  string  `json:"start_date"`
		EndDate    string  `json:"end_date"`
		Location   *string `json:"location,omitempty"`
		HostHandle string  `json:"host_handle"`
	}

	eventRows, err := h.db.Query(ctx, `
		SELECT e.id, e.slug, e.name, e.start_date::text, e.end_date::text, e.location,
		       COALESCE(up.handle, '') AS host_handle
		FROM events e
		LEFT JOIN user_profiles up ON up.owner_id = e.owner_id
		WHERE e.owner_id = $1 AND e.deleted_at IS NULL
		  AND e.start_date <= $3::date AND e.end_date >= $2::date
		ORDER BY e.start_date
	`, ownerID, from, to)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer eventRows.Close()

	events := []calEvent{}
	for eventRows.Next() {
		var e calEvent
		if err := eventRows.Scan(&e.ID, &e.Slug, &e.Name, &e.StartDate, &e.EndDate, &e.Location, &e.HostHandle); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		events = append(events, e)
	}

	// nudge_dot_dates (#246 A5): Tier-B "quiet dot" — in-band + previously
	// paddled, no popup — over the FULL requested [from,to] (not just the
	// 14-day Tier-A window; a quiet dot is a passive calendar hint, not a
	// prompt, so it isn't bounded by the nudge lookback). scanCandidates'
	// LIMIT 90 and the requirePriorPaddle EXISTS filter keep this a bounded
	// set-based read even for a wide range.
	//
	// Dates already in needsConfirmDates are skipped here rather than
	// re-resolved: a date that already earned a Tier-A "needs confirm" flag
	// must never ALSO carry a Tier-B quiet dot (one calendar cell can't
	// render both a '?' and a quiet dot), and skipping avoids paying
	// resolveInBand's flow.StampFull cost twice for the same candidate when
	// the two windows overlap (last 14 days is in both).
	nudgeDotDates := []string{}
	if loc == nil {
		if l, lerr := resolveOwnerLocation(ctx, h.db, ownerID); lerr == nil {
			loc = l
		}
	}
	if loc != nil {
		if cands, cerr := scanCandidates(ctx, h.db, ownerID, from, to, true); cerr == nil {
			seen := map[string]bool{}
			for _, c := range cands {
				if seen[c.RunDate] || needsConfirmDates[c.RunDate] {
					continue
				}
				if _, inBand := resolveInBand(ctx, h.db, loc, c); inBand {
					seen[c.RunDate] = true
					nudgeDotDates = append(nudgeDotDates, c.RunDate)
				}
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"days":            days,
		"events":          events,
		"nudge_dot_dates": nudgeDotDates,
	})
}

// ── GET /me/calendar/day?date= ────────────────────────────────────────────

func (h *PlanHandler) CalendarDay(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	date := r.URL.Query().Get("date")
	if date == "" {
		errorResponse(w, http.StatusBadRequest, "date is required")
		return
	}
	d, err := parseDate(date)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid date")
		return
	}

	today, err := userToday(ctx, h.db, ownerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "could not resolve local date")
		return
	}
	canMark := !d.After(today)

	// web#354 A1: no plan_id / JOIN plans — calendar_runs by owner+date only.
	// web#354 A2 (decision #8, "same for CalendarDay"): also include runs the
	// caller has an ACCEPTED run_invites row on (crew) — same union + role
	// shape as Calendar above.
	rows, err := h.db.Query(ctx, `
		SELECT id, slug, user_reach_id, name, reach_name, flow_band, flow_color, gauge_cfs, paddled, run_time, notes,
		       meetup_spot, meetup_rapid_id, meetup_access_id, role
		FROM (
			SELECT DISTINCT cr.id, cr.slug, cr.user_reach_id::text AS user_reach_id, cr.name, ur.name AS reach_name,
			       cr.flow_band, cr.flow_color, cr.gauge_cfs, cr.paddled, cr.run_time::text AS run_time, cr.notes,
			       cr.meetup_spot, cr.meetup_rapid_id::text AS meetup_rapid_id, cr.meetup_access_id::text AS meetup_access_id,
			       cr.sort_order,
			       CASE WHEN cr.owner_id = $1 THEN 'owner' ELSE 'crew' END AS role
			FROM calendar_runs cr
			LEFT JOIN user_reaches ur ON ur.id = cr.user_reach_id
			WHERE cr.run_date = $2::date AND cr.deleted_at IS NULL
			  AND (
			    cr.owner_id = $1
			    OR EXISTS(SELECT 1 FROM run_invites ri WHERE ri.run_id = cr.id AND ri.member_owner_id = $1 AND ri.status = 'accepted')
			  )
		) sub
		ORDER BY sort_order
	`, ownerID, date)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	// Name (web#354 A4) is the calendar run's OWN name (calendar_runs.name,
	// NOT NULL) — always populated, unlike ReachName (the attached library
	// run's own name, nil for an orphaned run).
	type dayRun struct {
		ID                string   `json:"id"`
		Slug              string   `json:"slug"`
		UserReachID       *string  `json:"user_reach_id,omitempty"`
		Name              string   `json:"name"`
		ReachName         *string  `json:"reach_name,omitempty"`
		FlowBand          *string  `json:"flow_band,omitempty"`
		FlowColor         *string  `json:"flow_color,omitempty"`
		GaugeCFS          *float64 `json:"gauge_cfs,omitempty"`
		Paddled           bool     `json:"paddled"`
		RunTime           *string  `json:"run_time,omitempty"`
		Notes             *string  `json:"notes,omitempty"`
		CanMarkPaddled    bool     `json:"can_mark_paddled"`
		MeetupSpot        *string  `json:"meetup_spot,omitempty"`
		MeetupFeatureType *string  `json:"meetup_feature_type,omitempty"`
		MeetupFeatureID   *string  `json:"meetup_feature_id,omitempty"`
		Role              string   `json:"role"` // "owner" | "crew" (web#354 A2 decision #8)
	}

	runs := []dayRun{}
	for rows.Next() {
		var dr dayRun
		var meetupRapidID, meetupAccessID *string
		if err := rows.Scan(&dr.ID, &dr.Slug, &dr.UserReachID, &dr.Name, &dr.ReachName,
			&dr.FlowBand, &dr.FlowColor, &dr.GaugeCFS, &dr.Paddled, &dr.RunTime, &dr.Notes,
			&dr.MeetupSpot, &meetupRapidID, &meetupAccessID, &dr.Role); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		// CanMarkPaddled only applies to the caller's OWN run — PATCH
		// /plan-runs/{id} is owner_id-gated, so a crew member could never
		// exercise this action on the host's run.
		dr.CanMarkPaddled = dr.Role == "owner" && !dr.Paddled && canMark
		dr.MeetupFeatureType, dr.MeetupFeatureID = meetupFeatureTypeID(meetupRapidID, meetupAccessID)
		runs = append(runs, dr)
	}

	jsonResponse(w, http.StatusOK, map[string]any{"date": date, "runs": runs})
}
