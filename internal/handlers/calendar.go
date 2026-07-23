package handlers

import (
	"net/http"
	"time"
)

// ── GET /me/calendar?from=&to=&tz= ────────────────────────────────────────
// One range query -> {days, plans, nudge_dot_dates}. Upserts tz when given
// (contract "Timezone rule": client sends Intl.DateTimeFormat().resolvedOptions().timeZone
// on every call). needs_confirm/nudge_dot_dates are hardcoded empty — A5 fills
// them; the fields stay in the shape so the web contract is stable.

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

	type calRun struct {
		ID          string   `json:"id"`
		UserReachID *string  `json:"user_reach_id,omitempty"`
		Name        *string  `json:"name,omitempty"`
		FlowBand    *string  `json:"flow_band,omitempty"`
		FlowColor   *string  `json:"flow_color,omitempty"`
		GaugeCFS    *float64 `json:"gauge_cfs,omitempty"`
		Paddled     bool     `json:"paddled"`
		RunTime     *string  `json:"run_time,omitempty"`
		PlanID      string   `json:"plan_id"`
	}
	type calDay struct {
		Date         string   `json:"date"`
		Runs         []calRun `json:"runs"`
		NeedsConfirm bool     `json:"needs_confirm"`
	}

	rows, err := h.db.Query(ctx, `
		SELECT pr.id, pr.user_reach_id::text, ur.name, pr.run_date::text,
		       pr.flow_band, pr.flow_color, pr.gauge_cfs, pr.paddled, pr.run_time::text, pr.plan_id::text
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
		WHERE pr.owner_id = $1 AND pr.deleted_at IS NULL
		  AND pr.run_date BETWEEN $2::date AND $3::date
		ORDER BY pr.run_date, pr.sort_order
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
		if err := rows.Scan(&cr.ID, &cr.UserReachID, &cr.Name, &runDate,
			&cr.FlowBand, &cr.FlowColor, &cr.GaugeCFS, &cr.Paddled, &cr.RunTime, &cr.PlanID); err != nil {
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
	needsConfirmDates := map[string]bool{}
	{
		today, terr := userToday(ctx, h.db, ownerID)
		if terr == nil {
			tierFrom := maxDateStr(from, today.AddDate(0, 0, -14).Format("2006-01-02"))
			tierTo := minDateStr(to, today.AddDate(0, 0, -1).Format("2006-01-02"))
			if tierFrom <= tierTo {
				if cands, cerr := scanCandidates(ctx, h.db, ownerID, tierFrom, tierTo, false); cerr == nil {
					for _, c := range cands {
						if _, inBand := resolveInBand(ctx, h.db, ownerID, c); inBand {
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

	// plans[] = own plans in range PLUS plans where caller is a plan_members
	// member, regardless of how membership was obtained: an invitee
	// (origin='invite', status IN invited/accepted) or an accepted crew
	// request (origin='request', status='accepted'). role: own | member
	// (accepted) | invited. Ordered by column position (2 UNION ALL
	// branches — ORDER BY name is ambiguous across a union).
	type calPlan struct {
		ID           string  `json:"id"`
		Slug         string  `json:"slug"`
		Name         string  `json:"name"`
		Type         string  `json:"type"`
		StartDate    string  `json:"start_date"`
		EndDate      string  `json:"end_date"`
		Visibility   string  `json:"visibility"`
		Role         string  `json:"role"`
		MemberStatus *string `json:"member_status,omitempty"`
	}

	planRows, err := h.db.Query(ctx, `
		SELECT p.id, p.slug, p.name, p.type::text, p.start_date::text, p.end_date::text,
		       p.visibility::text, 'own' AS role, NULL::text AS member_status
		FROM plans p
		WHERE p.owner_id = $1 AND p.deleted_at IS NULL
		  AND p.start_date <= $3::date AND p.end_date >= $2::date
		UNION ALL
		SELECT p.id, p.slug, p.name, p.type::text, p.start_date::text, p.end_date::text,
		       p.visibility::text,
		       CASE WHEN pm.status = 'accepted' THEN 'member' ELSE 'invited' END AS role,
		       pm.status::text AS member_status
		FROM plan_members pm
		JOIN plans p ON p.id = pm.plan_id AND p.deleted_at IS NULL
		WHERE pm.member_owner_id = $1
		  AND (
		    (pm.origin = 'invite' AND pm.status IN ('invited','accepted'))
		    OR (pm.origin = 'request' AND pm.status = 'accepted')
		  )
		  AND p.start_date <= $3::date AND p.end_date >= $2::date
		ORDER BY 5
	`, ownerID, from, to)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer planRows.Close()

	plans := []calPlan{}
	for planRows.Next() {
		var p calPlan
		if err := planRows.Scan(&p.ID, &p.Slug, &p.Name, &p.Type, &p.StartDate, &p.EndDate,
			&p.Visibility, &p.Role, &p.MemberStatus); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		plans = append(plans, p)
	}

	// nudge_dot_dates (#246 A5): Tier-B "quiet dot" — in-band + previously
	// paddled, no popup — over the FULL requested [from,to] (not just the
	// 14-day Tier-A window; a quiet dot is a passive calendar hint, not a
	// prompt, so it isn't bounded by the nudge lookback). scanCandidates'
	// LIMIT 90 and the requirePriorPaddle EXISTS filter keep this a bounded
	// set-based read even for a wide range.
	nudgeDotDates := []string{}
	if cands, cerr := scanCandidates(ctx, h.db, ownerID, from, to, true); cerr == nil {
		seen := map[string]bool{}
		for _, c := range cands {
			if seen[c.RunDate] {
				continue
			}
			if _, inBand := resolveInBand(ctx, h.db, ownerID, c); inBand {
				seen[c.RunDate] = true
				nudgeDotDates = append(nudgeDotDates, c.RunDate)
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"days":            days,
		"plans":           plans,
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

	rows, err := h.db.Query(ctx, `
		SELECT pr.id, pr.plan_id::text, pr.slug, pr.user_reach_id::text, ur.name,
		       pr.flow_band, pr.flow_color, pr.gauge_cfs, pr.paddled, pr.run_time::text, pr.notes
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
		WHERE pr.owner_id = $1 AND pr.run_date = $2::date AND pr.deleted_at IS NULL
		ORDER BY pr.sort_order
	`, ownerID, date)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type dayRun struct {
		ID             string   `json:"id"`
		PlanID         string   `json:"plan_id"`
		Slug           string   `json:"slug"`
		UserReachID    *string  `json:"user_reach_id,omitempty"`
		Name           *string  `json:"name,omitempty"`
		FlowBand       *string  `json:"flow_band,omitempty"`
		FlowColor      *string  `json:"flow_color,omitempty"`
		GaugeCFS       *float64 `json:"gauge_cfs,omitempty"`
		Paddled        bool     `json:"paddled"`
		RunTime        *string  `json:"run_time,omitempty"`
		Notes          *string  `json:"notes,omitempty"`
		CanMarkPaddled bool     `json:"can_mark_paddled"`
	}

	runs := []dayRun{}
	for rows.Next() {
		var dr dayRun
		if err := rows.Scan(&dr.ID, &dr.PlanID, &dr.Slug, &dr.UserReachID, &dr.Name,
			&dr.FlowBand, &dr.FlowColor, &dr.GaugeCFS, &dr.Paddled, &dr.RunTime, &dr.Notes); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		dr.CanMarkPaddled = !dr.Paddled && canMark
		runs = append(runs, dr)
	}

	jsonResponse(w, http.StatusOK, map[string]any{"date": date, "runs": runs})
}
