package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/flow"
)

// createPlanRunBody is the shared request shape for a calendar run, used
// both by POST /plans (embedded `runs`) and POST /plans/{id}/runs.
type createPlanRunBody struct {
	UserReachID *string `json:"user_reach_id"`
	ReachSlug   *string `json:"reach_slug"`
	RunDate     string  `json:"run_date"`
	RunTime     *string `json:"run_time"`
	Notes       *string `json:"notes"`
	Companions  *string `json:"companions"`
	Paddled     *bool   `json:"paddled"`
}

// insertPlanRun resolves the river run (user_reach_id or reach_slug),
// flow-stamps it, and inserts one plan_runs row via q — a pool or a tx, so
// this is shared verbatim by POST /plans' single-tx create-with-runs and the
// standalone POST /plans/{id}/runs. Returns an *apiError for anything that
// should surface as non-500 (400/404/422).
func (h *PlanHandler) insertPlanRun(
	ctx context.Context, q dbQueryer,
	planID, hostOwnerID, planStart, planEnd string,
	body createPlanRunBody,
) (id, slug string, err error) {
	if body.RunDate == "" {
		return "", "", &apiError{http.StatusBadRequest, "run_date is required"}
	}
	if verr := validateRunDateInRange(body.RunDate, planStart, planEnd); verr != nil {
		return "", "", &apiError{http.StatusBadRequest, verr.Error()}
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
	slug = h.uniqueRunSlug(ctx, q, hostOwnerID, slugBase+"-"+body.RunDate)

	err = q.QueryRow(ctx, `
		INSERT INTO plan_runs
			(plan_id, owner_id, user_reach_id, slug, run_date, run_time,
			 gauge_cfs, flow_band, flow_color, gauge_id, stamped_at,
			 paddled, paddled_at, notes, companions)
		VALUES ($1,$2,$3::uuid,$4,$5::date,$6,$7,$8,$9,$10::uuid,$11,$12,$13,$14,$15)
		RETURNING id
	`,
		planID, hostOwnerID, userReachID, slug, body.RunDate, runTimeVal,
		stamp.CFS, stamp.Band, stamp.Color, stamp.GaugeID, stampedAt,
		paddled, paddledAt, body.Notes, body.Companions,
	).Scan(&id)
	if err != nil {
		return "", "", &apiError{http.StatusInternalServerError, fmt.Sprintf("create run failed: %v", err)}
	}
	return id, slug, nil
}

// ── POST /plans/{id}/runs ─────────────────────────────────────────────────

func (h *PlanHandler) CreateRun(w http.ResponseWriter, r *http.Request) {
	planID := chi.URLParam(r, "id")
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

	var startDate, endDate string
	if err := h.db.QueryRow(ctx,
		`SELECT start_date::text, end_date::text FROM plans WHERE id = $1::uuid AND owner_id = $2 AND deleted_at IS NULL`,
		planID, ownerID,
	).Scan(&startDate, &endDate); err != nil {
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}

	id, slug, err := h.insertPlanRun(ctx, h.db, planID, ownerID, startDate, endDate, body)
	if err != nil {
		h.respondAPIError(w, err)
		return
	}

	jsonResponse(w, http.StatusCreated, map[string]string{"id": id, "slug": slug})
}

// ── GET /plan-runs/{param} ───────────────────────────────────────────────
// Resolution order (binding): (1) id, (2) source_report_id (preserves old
// /reports/{uuid} links), (3) slug — only if globally unique across owners.

func (h *PlanHandler) GetRun(w http.ResponseWriter, r *http.Request) {
	param := chi.URLParam(r, "param")
	ctx := r.Context()

	var runID string
	if err := h.db.QueryRow(ctx,
		`SELECT id FROM plan_runs WHERE id = $1::uuid AND deleted_at IS NULL`, param,
	).Scan(&runID); err != nil {
		if err2 := h.db.QueryRow(ctx,
			`SELECT id FROM plan_runs WHERE source_report_id = $1::uuid AND deleted_at IS NULL`, param,
		).Scan(&runID); err2 != nil {
			if err3 := h.db.QueryRow(ctx, `
				SELECT id FROM plan_runs
				WHERE slug = $1 AND deleted_at IS NULL
				  AND (SELECT COUNT(*) FROM plan_runs WHERE slug = $1 AND deleted_at IS NULL) = 1
			`, param).Scan(&runID); err3 != nil {
				errorResponse(w, http.StatusNotFound, "plan run not found")
				return
			}
		}
	}

	h.renderPlanRun(w, r, runID)
}

func (h *PlanHandler) renderPlanRun(w http.ResponseWriter, r *http.Request, runID string) {
	ctx := r.Context()

	var run planRunSummary
	var runTime *string
	var paddledAtRaw *time.Time
	var createdAtRaw time.Time
	var planID, planSlug, planName, hostOwnerID, hostHandle, visibility string
	var planStart, planEnd string

	err := h.db.QueryRow(ctx, `
		SELECT pr.id, pr.slug, pr.user_reach_id::text, ur.name, pr.run_date::text, pr.run_time::text,
		       pr.sort_order, pr.gauge_cfs, pr.flow_band, pr.flow_color, pr.paddled, pr.paddled_at,
		       pr.notes, pr.companions, pr.created_at,
		       p.id, p.slug, p.name, p.owner_id, COALESCE(up.handle, ''), p.visibility::text,
		       p.start_date::text, p.end_date::text
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
		LEFT JOIN user_profiles up ON up.owner_id = p.owner_id
		WHERE pr.id = $1 AND pr.deleted_at IS NULL
	`, runID).Scan(
		&run.ID, &run.Slug, &run.UserReachID, &run.Name, &run.RunDate, &runTime,
		&run.SortOrder, &run.GaugeCFS, &run.FlowBand, &run.FlowColor, &run.Paddled, &paddledAtRaw,
		&run.Notes, &run.Companions, &createdAtRaw,
		&planID, &planSlug, &planName, &hostOwnerID, &hostHandle, &visibility,
		&planStart, &planEnd,
	)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "plan run not found")
		return
	}
	run.RunTime = runTime
	if paddledAtRaw != nil {
		s := paddledAtRaw.Format(time.RFC3339)
		run.PaddledAt = &s
	}
	run.CreatedAt = createdAtRaw.Format(time.RFC3339)

	if visibility != "public" {
		callerID, callerOK := h.ownerID(r)
		allowed := callerOK && callerID == hostOwnerID
		if !allowed && callerOK {
			h.db.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM plan_members
					WHERE plan_id = $1 AND member_owner_id = $2 AND status IN ('invited','accepted'))
			`, planID, callerID).Scan(&allowed)
		}
		if !allowed {
			errorResponse(w, http.StatusNotFound, "plan run not found")
			return
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"run": run,
		"plan": map[string]any{
			"id":          planID,
			"slug":        planSlug,
			"name":        planName,
			"host_handle": hostHandle,
			"visibility":  visibility,
			"start_date":  planStart,
			"end_date":    planEnd,
		},
	})
}

// ── PATCH /plan-runs/{id} ────────────────────────────────────────────────

type updatePlanRunBody struct {
	RunDate     *string  `json:"run_date"`
	RunTime     *string  `json:"run_time"`
	Notes       *string  `json:"notes"`
	Companions  *string  `json:"companions"`
	SortOrder   *int16   `json:"sort_order"`
	Paddled     *bool    `json:"paddled"`
	GaugeCFS    *float64 `json:"gauge_cfs"`     // never client-settable — 400 on attempts
	FlowBand    *string  `json:"flow_band"`     // never client-settable — 400 on attempts
	UserReachID *string  `json:"user_reach_id"` // never client-settable — 400 on attempts
}

func (h *PlanHandler) UpdateRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var body updatePlanRunBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.GaugeCFS != nil || body.FlowBand != nil || body.UserReachID != nil {
		errorResponse(w, http.StatusBadRequest, "gauge_cfs, flow_band, and user_reach_id are system-managed and cannot be edited")
		return
	}

	ctx := r.Context()

	var planID, curUserReachID, curRunDate, planStart, planEnd string
	var curPaddled bool
	var curPaddledAt *time.Time
	err := h.db.QueryRow(ctx, `
		SELECT pr.plan_id, COALESCE(pr.user_reach_id::text, ''), pr.run_date::text,
		       pr.paddled, pr.paddled_at, p.start_date::text, p.end_date::text
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		WHERE pr.id = $1 AND pr.owner_id = $2 AND pr.deleted_at IS NULL
	`, runID, ownerID).Scan(&planID, &curUserReachID, &curRunDate, &curPaddled, &curPaddledAt, &planStart, &planEnd)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "plan run not found")
		return
	}

	if curPaddled {
		// Locked state (contract): ONLY notes editable, and only within 24h
		// of paddled_at — mirrors reports.go's 24h edit-lock message.
		if body.RunDate != nil || body.RunTime != nil || body.Companions != nil ||
			body.SortOrder != nil || body.Paddled != nil {
			errorResponse(w, http.StatusBadRequest, "plan run is locked after paddling — only notes can be edited")
			return
		}
		if body.Notes == nil {
			jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		if curPaddledAt == nil || time.Since(*curPaddledAt) > 24*time.Hour {
			errorResponse(w, http.StatusForbidden, "plan runs are locked for editing 24 hours after being marked paddled")
			return
		}
		if _, err := h.db.Exec(ctx,
			`UPDATE plan_runs SET notes = $1, updated_at = NOW() WHERE id = $2 AND owner_id = $3`,
			body.Notes, runID, ownerID,
		); err != nil {
			errorResponse(w, http.StatusInternalServerError, "update failed")
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// Planned (unpaddled): run_date/run_time/notes/companions/sort_order are
	// freely editable. paddled:true is the mark-paddled transition.
	newRunDate := curRunDate
	if body.RunDate != nil {
		newRunDate = *body.RunDate
		if verr := validateRunDateInRange(newRunDate, planStart, planEnd); verr != nil {
			errorResponse(w, http.StatusBadRequest, verr.Error())
			return
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

	var stampedAt *time.Time
	if restamp {
		now := time.Now().UTC()
		stampedAt = &now
	}

	_, err = h.db.Exec(ctx, `
		UPDATE plan_runs SET
			run_date    = COALESCE($1::date, run_date),
			run_time    = COALESCE($2, run_time),
			notes       = COALESCE($3, notes),
			companions  = COALESCE($4, companions),
			sort_order  = COALESCE($5, sort_order),
			gauge_cfs   = CASE WHEN $6 THEN $7 ELSE gauge_cfs END,
			flow_band   = CASE WHEN $6 THEN $8 ELSE flow_band END,
			flow_color  = CASE WHEN $6 THEN $9 ELSE flow_color END,
			gauge_id    = CASE WHEN $6 THEN $10::uuid ELSE gauge_id END,
			stamped_at  = COALESCE($11, stamped_at),
			paddled     = CASE WHEN $12 THEN TRUE ELSE paddled END,
			paddled_at  = COALESCE($13, paddled_at),
			updated_at  = NOW()
		WHERE id = $14 AND owner_id = $15
	`,
		body.RunDate, body.RunTime, body.Notes, body.Companions, body.SortOrder,
		restamp, stamp.CFS, stamp.Band, stamp.Color, stamp.GaugeID, stampedAt,
		markPaddled, paddledAt,
		runID, ownerID,
	)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "update failed")
		return
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

	tag, err := h.db.Exec(r.Context(), `
		UPDATE plan_runs SET deleted_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL
		  AND EXISTS (SELECT 1 FROM plans p WHERE p.id = plan_runs.plan_id AND p.deleted_at IS NULL)
	`, runID, ownerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "plan run not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
