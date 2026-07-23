package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/h2oflow/h2oflow/apps/api/internal/flow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NudgeHandler handles /me/nudge/*, /me/season, and /me/calendar-prefs
// (#246 A5 — Trip Calendar nudge/season/prefs). Split into its own file/
// handler type from PlanHandler for the same reason InviteHandler is
// (invites.go comment): a distinct concern, sharing only the package-level
// helpers (dbQueryer, userToday, localNoonUTC, parseDate, ensureHandle,
// findOrCreatePaddledLog) defined in plans.go/plan_runs.go.
type NudgeHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
	sc            *seasonCache
}

func NewNudgeHandler(db *pgxpool.Pool, devFallbackID string) *NudgeHandler {
	return &NudgeHandler{db: db, devFallbackID: devFallbackID, sc: newSeasonCache()}
}

func (h *NudgeHandler) ownerID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

// ── Tier-A/Tier-B candidate scan (shared by Candidate, calendar.go's
//    needs_confirm, and calendar.go's nudge_dot_dates) ──────────────────────

// tierCandidate is one unpaddled, live plan_run in the scan window, with
// just enough joined to resolve its flow band.
type tierCandidate struct {
	PlanRunID   string
	UserReachID string
	ReachName   string
	BaseLabel   string
	RunDate     string // YYYY-MM-DD
}

// scanCandidates returns ownerID's unpaddled, live plan_runs with
// run_date ∈ [from,to], excluding (owner,user_reach_id,run_date) rows in
// nudge_dismissals (never re-ask) and days that already have a *separate*
// live paddled plan_run logged for that same reach (contract §5: "Exclude
// dismissed ... and any date already logged"). Both exclusions are pushed
// into the query so this stays one set-based read regardless of how wide
// [from,to] is — the per-row flow lookup is the only unavoidable per-row
// work (band derivation depends on internal/flow, not SQL) and callers keep
// that loop bounded by only ever asking for a narrow window.
//
// requirePriorPaddle, when true, restricts to reaches the caller has paddled
// at least once (any date) — the Tier-B "quiet dot" rule (contract §5:
// "in-band + previously-paddled").
func scanCandidates(ctx context.Context, db dbQueryer, ownerID, from, to string, requirePriorPaddle bool) ([]tierCandidate, error) {
	priorPaddleClause := ""
	if requirePriorPaddle {
		priorPaddleClause = `
		  AND EXISTS (
		    SELECT 1 FROM plan_runs pr3
		    JOIN plans p3 ON p3.id = pr3.plan_id AND p3.deleted_at IS NULL
		    WHERE pr3.owner_id = pr.owner_id AND pr3.user_reach_id = pr.user_reach_id
		      AND pr3.paddled AND pr3.deleted_at IS NULL
		  )`
	}

	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT pr.id, pr.user_reach_id::text, COALESCE(ur.long_name, ur.name), ur.base_label, pr.run_date::text
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		JOIN user_reaches ur ON ur.id = pr.user_reach_id AND ur.deleted_at IS NULL
		WHERE pr.owner_id = $1 AND pr.paddled = false AND pr.deleted_at IS NULL
		  AND pr.run_date BETWEEN $2::date AND $3::date
		  AND NOT EXISTS (
		    SELECT 1 FROM nudge_dismissals nd
		    WHERE nd.owner_id = pr.owner_id AND nd.user_reach_id = pr.user_reach_id AND nd.nudge_date = pr.run_date
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM plan_runs pr2
		    JOIN plans p2 ON p2.id = pr2.plan_id AND p2.deleted_at IS NULL
		    WHERE pr2.owner_id = pr.owner_id AND pr2.user_reach_id = pr.user_reach_id
		      AND pr2.run_date = pr.run_date AND pr2.paddled AND pr2.deleted_at IS NULL
		  )%s
		ORDER BY pr.run_date DESC
		LIMIT 90
	`, priorPaddleClause), ownerID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []tierCandidate
	for rows.Next() {
		var c tierCandidate
		if err := rows.Scan(&c.PlanRunID, &c.UserReachID, &c.ReachName, &c.BaseLabel, &c.RunDate); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// resolveOwnerLocation resolves ownerID's IANA calendar tz (user_calendar_prefs.tz,
// default America/Denver — mirrors userToday/localNoonUTC's COALESCE) ONCE as
// a *time.Location, so a caller looping resolveInBand over many candidates
// computes each candidate's local noon in Go instead of re-querying
// user_calendar_prefs.tz per candidate (that per-candidate round trip was the
// N+1 flagged in the #246 review — every candidate re-read the SAME owner's
// tz row). Callers resolve this once per request and pass it through.
func resolveOwnerLocation(ctx context.Context, db dbQueryer, ownerID string) (*time.Location, error) {
	var tz *string
	if err := db.QueryRow(ctx,
		`SELECT tz FROM user_calendar_prefs WHERE owner_id = $1`, ownerID,
	).Scan(&tz); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	zone := "America/Denver"
	if tz != nil && *tz != "" {
		zone = *tz
	}
	return time.LoadLocation(zone)
}

// resolveInBand stamps cand's flow reading nearest local noon of its
// run_date (in loc, the owner's tz resolved once via resolveOwnerLocation —
// never re-queried per candidate) and reports whether it's "in band".
// In-band rule (documented once, used everywhere): internal/flow.BandAndColorForCFS
// ("V9 logic") returns the reach's base_label as a fallback whenever cfs
// never clears the lowest configured threshold — that fallback is exactly
// the "no threshold range matched" case, so in-band means the resolved
// label is non-nil AND differs from base_label (a real threshold range
// matched).
func resolveInBand(ctx context.Context, db dbQueryer, loc *time.Location, cand tierCandidate) (flow.StampResult, bool) {
	rd, err := parseDate(cand.RunDate)
	if err != nil {
		return flow.StampResult{}, false
	}
	// Mirrors localNoonUTC's SQL: date + TIME '12:00' interpreted in the
	// owner's tz, i.e. local noon of run_date.
	noon := time.Date(rd.Year(), rd.Month(), rd.Day(), 12, 0, 0, 0, loc)
	stamp := flow.StampFull(ctx, db, cand.UserReachID, noon)
	inBand := stamp.Band != nil && *stamp.Band != cand.BaseLabel
	return stamp, inBand
}

// ── GET /me/nudge/candidate ───────────────────────────────────────────────

type nudgeMember struct {
	UserReachID string   `json:"user_reach_id"`
	RunName     string   `json:"run_name"`
	RunDate     string   `json:"run_date"`
	FlowBand    string   `json:"flow_band"`
	GaugeCFS    *float64 `json:"gauge_cfs,omitempty"`
	Reason      string   `json:"reason"`
}

func (h *NudgeHandler) Candidate(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	today, terr := userToday(ctx, h.db, ownerID)
	if terr != nil {
		errorResponse(w, http.StatusInternalServerError, "could not resolve local date")
		return
	}
	todayStr := today.Format("2006-01-02")

	var lastPrompt *time.Time
	if err := h.db.QueryRow(ctx,
		`SELECT last_nudge_prompt_date FROM user_calendar_prefs WHERE owner_id = $1`, ownerID,
	).Scan(&lastPrompt); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		errorResponse(w, http.StatusInternalServerError, "could not resolve nudge cap")
		return
	}
	promptAllowed := lastPrompt == nil || lastPrompt.Format("2006-01-02") != todayStr

	// Tier A arm-1 only (arm-2/"often-paddled" is descoped from MVP — no
	// run_interactions table exists, per contract §1). Bounded 14-day
	// lookback stays inside the 30-day gauge_readings retention window so
	// every candidate date still has live readings to stamp against.
	from := today.AddDate(0, 0, -14).Format("2006-01-02")
	to := today.AddDate(0, 0, -1).Format("2006-01-02")

	cands, err := scanCandidates(ctx, h.db, ownerID, from, to, false)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "candidate scan failed")
		return
	}

	loc, lerr := resolveOwnerLocation(ctx, h.db, ownerID)
	if lerr != nil {
		errorResponse(w, http.StatusInternalServerError, "could not resolve local timezone")
		return
	}

	var member *nudgeMember
	for _, c := range cands {
		stamp, inBand := resolveInBand(ctx, h.db, loc, c)
		if !inBand {
			continue
		}
		member = &nudgeMember{
			UserReachID: c.UserReachID,
			RunName:     c.ReachName,
			RunDate:     c.RunDate,
			FlowBand:    *stamp.Band,
			GaugeCFS:    stamp.CFS,
			Reason:      "planned",
		}
		break // cands is already ORDER BY run_date DESC -> first hit is "top 1"
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"member":         member,
		"prompt_allowed": promptAllowed,
	})
}

// ── POST /me/nudge/shown ──────────────────────────────────────────────────
// Stamps last_nudge_prompt_date — fired only when the client actually
// renders the popup, not on every candidate GET (contract §5).

func (h *NudgeHandler) Shown(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	today, terr := userToday(ctx, h.db, ownerID)
	if terr != nil {
		errorResponse(w, http.StatusInternalServerError, "could not resolve local date")
		return
	}

	if _, err := h.db.Exec(ctx, `
		INSERT INTO user_calendar_prefs (owner_id, last_nudge_prompt_date, updated_at)
		VALUES ($1, $2::date, NOW())
		ON CONFLICT (owner_id) DO UPDATE SET last_nudge_prompt_date = $2::date, updated_at = NOW()
	`, ownerID, today.Format("2006-01-02")); err != nil {
		errorResponse(w, http.StatusInternalServerError, "could not stamp nudge cap")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── POST /me/nudge/dismiss ────────────────────────────────────────────────

func (h *NudgeHandler) Dismiss(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		UserReachID string `json:"user_reach_id"`
		RunDate     string `json:"run_date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.UserReachID == "" || body.RunDate == "" {
		errorResponse(w, http.StatusBadRequest, "user_reach_id and run_date are required")
		return
	}
	if _, err := parseDate(body.RunDate); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid run_date")
		return
	}
	ctx := r.Context()

	if _, err := h.db.Exec(ctx, `
		INSERT INTO nudge_dismissals (owner_id, user_reach_id, nudge_date)
		VALUES ($1, $2::uuid, $3::date)
		ON CONFLICT DO NOTHING
	`, ownerID, body.UserReachID, body.RunDate); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			errorResponse(w, http.StatusNotFound, "river run not found")
			return
		}
		errorResponse(w, http.StatusInternalServerError, "dismiss failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── POST /me/nudge/confirm ────────────────────────────────────────────────

func (h *NudgeHandler) Confirm(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		UserReachID string  `json:"user_reach_id"`
		RunDate     string  `json:"run_date"`
		Notes       *string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.UserReachID == "" || body.RunDate == "" {
		errorResponse(w, http.StatusBadRequest, "user_reach_id and run_date are required")
		return
	}
	ctx := r.Context()

	today, terr := userToday(ctx, h.db, ownerID)
	if terr != nil {
		errorResponse(w, http.StatusInternalServerError, "could not resolve local date")
		return
	}
	rd, perr := parseDate(body.RunDate)
	if perr != nil {
		errorResponse(w, http.StatusBadRequest, "invalid run_date")
		return
	}
	if rd.After(today) {
		errorResponse(w, http.StatusUnprocessableEntity, "cannot confirm a future run")
		return
	}

	// Confirm is allowed even for a previously dismissed day — unlike the
	// silent Candidate scan (which permanently excludes dismissed days so
	// the unsolicited popup never re-asks), a user explicitly tapping/typing
	// "I paddled this" is a deliberate, user-initiated action and should
	// still log the paddle. Dismissal only ever suppresses the *prompt*,
	// never a caller's own confirm.
	var reachSlug string
	if err := h.db.QueryRow(ctx,
		`SELECT slug FROM user_reaches WHERE id = $1::uuid AND deleted_at IS NULL`,
		body.UserReachID,
	).Scan(&reachSlug); err != nil {
		errorResponse(w, http.StatusNotFound, "river run not found")
		return
	}

	planID, runID, existed, ferr := findOrCreatePaddledLog(ctx, h.db, ownerID, body.UserReachID, reachSlug, body.RunDate, body.Notes)
	if ferr != nil {
		errorResponse(w, http.StatusInternalServerError, ferr.Error())
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	jsonResponse(w, status, map[string]string{"plan_id": planID, "plan_run_id": runID})
}

// ── GET /me/calendar-prefs · PATCH /me/calendar-prefs ────────────────────

func (h *NudgeHandler) GetPrefs(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	var goalDays *int
	var tz *string
	var lastPrompt *string
	err := h.db.QueryRow(ctx, `
		SELECT season_goal_days, tz, last_nudge_prompt_date::text
		FROM user_calendar_prefs WHERE owner_id = $1
	`, ownerID).Scan(&goalDays, &tz, &lastPrompt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}

	resolvedTZ := "America/Denver"
	if tz != nil && *tz != "" {
		resolvedTZ = *tz
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"season_goal_days":       goalDays,
		"tz":                     resolvedTZ,
		"last_nudge_prompt_date": lastPrompt,
	})
}

func (h *NudgeHandler) UpdatePrefs(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		SeasonGoalDays *int    `json:"season_goal_days"`
		TZ             *string `json:"tz"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.SeasonGoalDays != nil && (*body.SeasonGoalDays < 1 || *body.SeasonGoalDays > 366) {
		errorResponse(w, http.StatusUnprocessableEntity, "season_goal_days must be between 1 and 366")
		return
	}
	if body.TZ != nil {
		if *body.TZ == "" {
			errorResponse(w, http.StatusUnprocessableEntity, "tz cannot be empty")
			return
		}
		if _, err := time.LoadLocation(*body.TZ); err != nil {
			errorResponse(w, http.StatusUnprocessableEntity, "invalid tz")
			return
		}
	}
	ctx := r.Context()

	if _, err := h.db.Exec(ctx, `
		INSERT INTO user_calendar_prefs (owner_id, season_goal_days, tz, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (owner_id) DO UPDATE SET
			season_goal_days = COALESCE($2, user_calendar_prefs.season_goal_days),
			tz               = COALESCE($3, user_calendar_prefs.tz),
			updated_at       = NOW()
	`, ownerID, body.SeasonGoalDays, body.TZ); err != nil {
		errorResponse(w, http.StatusInternalServerError, "update failed")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── GET /me/season?year= ──────────────────────────────────────────────────

// seasonCache mirrors reaches.go's statsCache shape (mutex + cachedAt TTL)
// but keyed per owner+year, since /me/season is a per-caller endpoint unlike
// the global reach/river/report counts reaches.go caches for everyone.
type seasonCache struct {
	mu      sync.Mutex
	entries map[string]seasonCacheEntry
}

type seasonCacheEntry struct {
	cachedAt time.Time
	data     seasonResponse
}

func newSeasonCache() *seasonCache { return &seasonCache{entries: map[string]seasonCacheEntry{}} }

func (c *seasonCache) get(key string, ttl time.Duration) (seasonResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.cachedAt) >= ttl {
		return seasonResponse{}, false
	}
	return e.data, true
}

func (c *seasonCache) set(key string, data seasonResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = seasonCacheEntry{cachedAt: time.Now(), data: data}
}

type seasonHighestFlow struct {
	CFS       float64 `json:"cfs"`
	PlanRunID string  `json:"plan_run_id"`
	Slug      string  `json:"slug"`
	RunName   string  `json:"run_name"`
	Date      string  `json:"date"`
}

type seasonNewRun struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Date string `json:"date"`
}

type seasonRecentRun struct {
	ID        string   `json:"id"`
	Slug      string   `json:"slug"`
	Name      *string  `json:"name,omitempty"`
	RunDate   string   `json:"run_date"`
	FlowBand  *string  `json:"flow_band,omitempty"`
	FlowColor *string  `json:"flow_color,omitempty"`
	GaugeCFS  *float64 `json:"gauge_cfs,omitempty"`
	Notes     *string  `json:"notes,omitempty"`
	PlanID    string   `json:"plan_id"`
}

type seasonResponse struct {
	DaysOnWater int                `json:"days_on_water"`
	Rivers      int                `json:"rivers"`
	Runs        int                `json:"runs"`
	Streak      int                `json:"streak"`
	HighestFlow *seasonHighestFlow `json:"highest_flow,omitempty"`
	NewRuns     []seasonNewRun     `json:"new_runs"`
	GoalDays    *int               `json:"goal_days,omitempty"`
	Recent      []seasonRecentRun  `json:"recent"`
}

// currentStreakDays computes the "consecutive days ending at today or
// yesterday" streak (contract decision #3: "Streak = consecutive DAYS with
// >=1 paddled run", decided over consecutive-weeks) from a
// descending-by-date list of a caller's distinct paddled dates (dates[0] is
// the most recent). A gap of more than one calendar day ends the count. If
// the most recent paddled date is older than yesterday, the streak is
// currently broken -> 0, regardless of any earlier unbroken run.
func currentStreakDays(dates []time.Time, today time.Time) int {
	if len(dates) == 0 {
		return 0
	}
	yesterday := today.AddDate(0, 0, -1)
	mostRecent := dates[0]
	if !mostRecent.Equal(today) && !mostRecent.Equal(yesterday) {
		return 0
	}
	streak := 1
	expected := mostRecent
	for i := 1; i < len(dates); i++ {
		expected = expected.AddDate(0, 0, -1)
		if dates[i].Equal(expected) {
			streak++
		} else {
			break
		}
	}
	return streak
}

func (h *NudgeHandler) Season(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	today, terr := userToday(ctx, h.db, ownerID)
	if terr != nil {
		errorResponse(w, http.StatusInternalServerError, "could not resolve local date")
		return
	}

	year := today.Year()
	if ys := r.URL.Query().Get("year"); ys != "" {
		y, err := strconv.Atoi(ys)
		if err != nil || y < 2000 || y > today.Year()+1 {
			errorResponse(w, http.StatusBadRequest, "invalid year")
			return
		}
		year = y
	}

	cacheKey := ownerID + ":" + strconv.Itoa(year)
	if cached, ok := h.sc.get(cacheKey, 60*time.Second); ok {
		jsonResponse(w, http.StatusOK, cached)
		return
	}

	yearStart := fmt.Sprintf("%04d-01-01", year)
	yearEnd := fmt.Sprintf("%04d-12-31", year)

	var resp seasonResponse
	resp.NewRuns = []seasonNewRun{}
	resp.Recent = []seasonRecentRun{}

	if err := h.db.QueryRow(ctx, `
		SELECT COUNT(DISTINCT pr.run_date), COUNT(*),
		       COUNT(DISTINCT COALESCE(ur.river_name, ur.id::text))
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
		WHERE pr.owner_id = $1 AND pr.paddled AND pr.deleted_at IS NULL
		  AND pr.run_date BETWEEN $2::date AND $3::date
	`, ownerID, yearStart, yearEnd).Scan(&resp.DaysOnWater, &resp.Runs, &resp.Rivers); err != nil {
		errorResponse(w, http.StatusInternalServerError, "season stats query failed")
		return
	}

	// highest_flow must carry the specific paddled plan_run's own id (routable
	// via GET /plan-runs/{param}, id-first resolution) plus the reach's slug
	// (routable via GET /me/reaches/{slug}) — NOT the user_reach UUID, which
	// resolves via neither route. pr.id/pr.slug are NOT NULL; ur.slug can be
	// NULL when user_reach_id was cleared (ON DELETE SET NULL).
	var hfCFS *float64
	var hfRunID string
	var hfSlug, hfName, hfDate *string
	if err := h.db.QueryRow(ctx, `
		SELECT pr.gauge_cfs, pr.id::text, ur.slug, ur.name, pr.run_date::text
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
		WHERE pr.owner_id = $1 AND pr.paddled AND pr.deleted_at IS NULL AND pr.gauge_cfs IS NOT NULL
		  AND pr.run_date BETWEEN $2::date AND $3::date
		ORDER BY pr.gauge_cfs DESC
		LIMIT 1
	`, ownerID, yearStart, yearEnd).Scan(&hfCFS, &hfRunID, &hfSlug, &hfName, &hfDate); err == nil && hfCFS != nil {
		resp.HighestFlow = &seasonHighestFlow{CFS: *hfCFS, PlanRunID: hfRunID}
		if hfSlug != nil {
			resp.HighestFlow.Slug = *hfSlug
		}
		if hfName != nil {
			resp.HighestFlow.RunName = *hfName
		}
		if hfDate != nil {
			resp.HighestFlow.Date = *hfDate
		}
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		errorResponse(w, http.StatusInternalServerError, "highest-flow query failed")
		return
	}

	// new_runs: reaches whose FIRST-ever paddled date (all-time, not just
	// this year) falls within the selected year — scans all paddled history
	// to find the true first date, filters the group on the year via HAVING.
	// Grouped/keyed by the user_reach itself (a reach, not a single dated
	// plan_run), so the routable identifier is ur.slug — reach pages resolve
	// on GET /me/reaches/{slug}, never by user_reach id.
	newRows, err := h.db.Query(ctx, `
		SELECT ur.slug, ur.name, MIN(pr.run_date)::text AS first_date
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		JOIN user_reaches ur ON ur.id = pr.user_reach_id
		WHERE pr.owner_id = $1 AND pr.paddled AND pr.deleted_at IS NULL
		GROUP BY ur.id, ur.slug, ur.name
		HAVING MIN(pr.run_date) BETWEEN $2::date AND $3::date
		ORDER BY MIN(pr.run_date)
	`, ownerID, yearStart, yearEnd)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "new-runs query failed")
		return
	}
	for newRows.Next() {
		var nr seasonNewRun
		if err := newRows.Scan(&nr.Slug, &nr.Name, &nr.Date); err != nil {
			newRows.Close()
			errorResponse(w, http.StatusInternalServerError, "new-runs scan failed")
			return
		}
		resp.NewRuns = append(resp.NewRuns, nr)
	}
	newRows.Close()

	// streak: current consecutive-days streak, computed over ALL-TIME
	// paddled dates (not clipped to the selected year) — a streak spanning
	// Dec 31 -> Jan 1 must not reset just because /me/season?year= is
	// viewing one calendar year; it's a "right now" number regardless of
	// which year's stats page is open.
	dateRows, err := h.db.Query(ctx, `
		SELECT DISTINCT pr.run_date::text
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		WHERE pr.owner_id = $1 AND pr.paddled AND pr.deleted_at IS NULL AND pr.run_date <= $2::date
		ORDER BY pr.run_date DESC
	`, ownerID, today.Format("2006-01-02"))
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "streak query failed")
		return
	}
	var paddledDates []time.Time
	for dateRows.Next() {
		var s string
		if err := dateRows.Scan(&s); err != nil {
			dateRows.Close()
			errorResponse(w, http.StatusInternalServerError, "streak scan failed")
			return
		}
		d, perr := parseDate(s)
		if perr == nil {
			paddledDates = append(paddledDates, d)
		}
	}
	dateRows.Close()
	resp.Streak = currentStreakDays(paddledDates, today)

	var goalDays *int
	if err := h.db.QueryRow(ctx,
		`SELECT season_goal_days FROM user_calendar_prefs WHERE owner_id = $1`, ownerID,
	).Scan(&goalDays); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		errorResponse(w, http.StatusInternalServerError, "goal query failed")
		return
	}
	resp.GoalDays = goalDays

	// recent: latest 10 paddled plan_runs WITHIN the selected year — a
	// /me/season?year=2024 browsing a past season should show that season's
	// recent activity, not literally-today's runs.
	recentRows, err := h.db.Query(ctx, `
		SELECT pr.id, pr.slug, ur.name, pr.run_date::text, pr.flow_band, pr.flow_color,
		       pr.gauge_cfs, pr.notes, pr.plan_id::text
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
		WHERE pr.owner_id = $1 AND pr.paddled AND pr.deleted_at IS NULL
		  AND pr.run_date BETWEEN $2::date AND $3::date
		ORDER BY pr.run_date DESC, pr.paddled_at DESC NULLS LAST
		LIMIT 10
	`, ownerID, yearStart, yearEnd)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "recent query failed")
		return
	}
	for recentRows.Next() {
		var rr seasonRecentRun
		if err := recentRows.Scan(&rr.ID, &rr.Slug, &rr.Name, &rr.RunDate, &rr.FlowBand, &rr.FlowColor,
			&rr.GaugeCFS, &rr.Notes, &rr.PlanID); err != nil {
			recentRows.Close()
			errorResponse(w, http.StatusInternalServerError, "recent scan failed")
			return
		}
		resp.Recent = append(resp.Recent, rr)
	}
	recentRows.Close()

	h.sc.set(cacheKey, resp)
	jsonResponse(w, http.StatusOK, resp)
}
