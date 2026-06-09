package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FlowProposalHandler handles community flow range proposals and votes.
type FlowProposalHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
}

func NewFlowProposalHandler(db *pgxpool.Pool, devFallbackID string) *FlowProposalHandler {
	return &FlowProposalHandler{db: db, devFallbackID: devFallbackID}
}

func (h *FlowProposalHandler) callerID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

// ── Types ─────────────────────────────────────────────────────────────────────

type flowRangeProposal struct {
	ID           string     `json:"id"`
	UserReachID  string     `json:"user_reach_id"`
	AuthorID     string     `json:"author_id"`
	AuthorHandle *string    `json:"author_handle"`
	LowMax       *float64   `json:"low_max"`
	RunningMin   float64    `json:"running_min"`
	RunningMax   float64    `json:"running_max"`
	HighMin      *float64   `json:"high_min"`
	Note         *string    `json:"note"`
	VoteCount    int        `json:"vote_count"`
	UserVoted    bool       `json:"user_voted"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type flowProposalListResponse struct {
	Proposals      []flowRangeProposal `json:"proposals"`
	MedianRunMin   *float64            `json:"median_running_min"`
	MedianRunMax   *float64            `json:"median_running_max"`
}

// listProposals queries proposals for a user_reach_id, annotating user_voted if callerID != "".
func (h *FlowProposalHandler) listProposals(r *http.Request, userReachID, callerID string) (*flowProposalListResponse, error) {
	ctx := r.Context()

	rows, err := h.db.Query(ctx, `
		SELECT
			fp.id,
			fp.user_reach_id::text,
			fp.author_id,
			up.handle,
			fp.low_max,
			fp.running_min,
			fp.running_max,
			fp.high_min,
			fp.note,
			fp.vote_count,
			CASE WHEN fv.user_id IS NOT NULL THEN TRUE ELSE FALSE END AS user_voted,
			fp.created_at,
			fp.updated_at
		FROM flow_range_proposals fp
		LEFT JOIN user_profiles up ON up.owner_id = fp.author_id
		LEFT JOIN flow_range_votes fv ON fv.proposal_id = fp.id AND fv.user_id = $2
		WHERE fp.user_reach_id = $1
		ORDER BY fp.vote_count DESC, fp.created_at ASC
	`, userReachID, callerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var proposals []flowRangeProposal
	for rows.Next() {
		var p flowRangeProposal
		if err := rows.Scan(
			&p.ID, &p.UserReachID, &p.AuthorID, &p.AuthorHandle,
			&p.LowMax, &p.RunningMin, &p.RunningMax, &p.HighMin,
			&p.Note, &p.VoteCount, &p.UserVoted,
			&p.CreatedAt, &p.UpdatedAt,
		); err == nil {
			proposals = append(proposals, p)
		}
	}
	if proposals == nil {
		proposals = []flowRangeProposal{}
	}

	resp := &flowProposalListResponse{Proposals: proposals}

	// Compute median running_min and running_max across all proposals.
	if len(proposals) > 0 {
		var rMin, rMax float64
		_ = h.db.QueryRow(ctx, `
			SELECT
				PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY running_min),
				PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY running_max)
			FROM flow_range_proposals WHERE user_reach_id = $1
		`, userReachID).Scan(&rMin, &rMax)
		resp.MedianRunMin = &rMin
		resp.MedianRunMax = &rMax
	}

	return resp, nil
}

// ── ListForOwnRun — GET /api/v1/me/reaches/{slug}/flow-proposals ──────────────

func (h *FlowProposalHandler) ListForOwnRun(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.callerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")

	var userReachID string
	if err := h.db.QueryRow(r.Context(),
		`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`,
		callerID, slug,
	).Scan(&userReachID); err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	resp, err := h.listProposals(r, userReachID, callerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	jsonResponse(w, http.StatusOK, resp)
}

// ── ListByRunID — GET /api/v1/user-runs/{runId}/flow-proposals ────────────────
// Public endpoint. Returns proposals for any non-private user run.

func (h *FlowProposalHandler) ListByRunID(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")
	callerID, _ := h.callerID(r) // optional auth for user_voted flag

	var visibility string
	if err := h.db.QueryRow(r.Context(),
		`SELECT visibility FROM user_reaches WHERE id = $1`,
		runID,
	).Scan(&visibility); err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}
	if visibility != "public" {
		errorResponse(w, http.StatusForbidden, "run is not public")
		return
	}

	resp, err := h.listProposals(r, runID, callerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	jsonResponse(w, http.StatusOK, resp)
}

// ── UpsertForOwnRun — POST /api/v1/me/reaches/{slug}/flow-proposals ──────────
// Owner submits or updates their proposal for their own run.

func (h *FlowProposalHandler) UpsertForOwnRun(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.callerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")

	var userReachID string
	if err := h.db.QueryRow(r.Context(),
		`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`,
		callerID, slug,
	).Scan(&userReachID); err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	h.upsertProposal(w, r, userReachID, callerID)
}

// ── UpsertByRunID — POST /api/v1/user-runs/{runId}/flow-proposals ─────────────
// Any authenticated user can propose flow ranges for any public run.

func (h *FlowProposalHandler) UpsertByRunID(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.callerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID := chi.URLParam(r, "runId")

	var visibility string
	if err := h.db.QueryRow(r.Context(),
		`SELECT visibility FROM user_reaches WHERE id = $1`,
		runID,
	).Scan(&visibility); err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}
	if visibility != "public" {
		errorResponse(w, http.StatusForbidden, "run is not public")
		return
	}

	h.upsertProposal(w, r, runID, callerID)
}

func (h *FlowProposalHandler) upsertProposal(w http.ResponseWriter, r *http.Request, userReachID, authorID string) {
	var body struct {
		LowMax     *float64 `json:"low_max"`
		RunningMin float64  `json:"running_min"`
		RunningMax float64  `json:"running_max"`
		HighMin    *float64 `json:"high_min"`
		Note       *string  `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.RunningMin <= 0 || body.RunningMax <= 0 || body.RunningMin >= body.RunningMax {
		errorResponse(w, http.StatusBadRequest, "running_min and running_max must be positive and min < max")
		return
	}
	note := body.Note
	if note != nil {
		trimmed := strings.TrimSpace(*note)
		note = &trimmed
		if *note == "" {
			note = nil
		}
	}

	var id string
	err := h.db.QueryRow(r.Context(), `
		INSERT INTO flow_range_proposals
			(user_reach_id, author_id, low_max, running_min, running_max, high_min, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_reach_id, author_id) DO UPDATE SET
			low_max     = EXCLUDED.low_max,
			running_min = EXCLUDED.running_min,
			running_max = EXCLUDED.running_max,
			high_min    = EXCLUDED.high_min,
			note        = EXCLUDED.note,
			updated_at  = now()
		RETURNING id
	`, userReachID, authorID, body.LowMax, body.RunningMin, body.RunningMax, body.HighMin, note).Scan(&id)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "save failed: "+err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"id": id})
}

// ── ToggleVote — POST /api/v1/flow-proposals/{proposalId}/vote ────────────────
// Inserts a vote if not yet voted; deletes if already voted. Returns new count.

func (h *FlowProposalHandler) ToggleVote(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.callerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	proposalID := chi.URLParam(r, "proposalId")

	// Verify proposal exists and run is public.
	var visibility string
	if err := h.db.QueryRow(r.Context(), `
		SELECT ur.visibility
		FROM flow_range_proposals fp
		JOIN user_reaches ur ON ur.id = fp.user_reach_id
		WHERE fp.id = $1
	`, proposalID).Scan(&visibility); err != nil {
		errorResponse(w, http.StatusNotFound, "proposal not found")
		return
	}
	if visibility != "public" {
		errorResponse(w, http.StatusForbidden, "run is not public")
		return
	}

	// Toggle: attempt insert; if unique constraint fires, delete instead.
	ctx := r.Context()
	var voted bool
	_, err := h.db.Exec(ctx, `
		INSERT INTO flow_range_votes (proposal_id, user_id) VALUES ($1, $2)
	`, proposalID, callerID)
	if err != nil {
		// Already voted — remove vote.
		_, _ = h.db.Exec(ctx, `
			DELETE FROM flow_range_votes WHERE proposal_id = $1 AND user_id = $2
		`, proposalID, callerID)
		_, _ = h.db.Exec(ctx, `
			UPDATE flow_range_proposals SET vote_count = GREATEST(0, vote_count - 1) WHERE id = $1
		`, proposalID)
		voted = false
	} else {
		_, _ = h.db.Exec(ctx, `
			UPDATE flow_range_proposals SET vote_count = vote_count + 1 WHERE id = $1
		`, proposalID)
		voted = true
	}

	var count int
	_ = h.db.QueryRow(ctx, `SELECT vote_count FROM flow_range_proposals WHERE id = $1`, proposalID).Scan(&count)
	jsonResponse(w, http.StatusOK, map[string]any{"voted": voted, "vote_count": count})
}
