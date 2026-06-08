package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UpvoteHandler handles run upvotes.
// Toggle semantics: POST inserts if not present, deletes if already voted.
// Self-upvote rejected with 403.
type UpvoteHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
}

func NewUpvoteHandler(db *pgxpool.Pool, devFallbackID string) *UpvoteHandler {
	return &UpvoteHandler{db: db, devFallbackID: devFallbackID}
}

func (h *UpvoteHandler) callerID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

// Toggle — POST /api/v1/user-runs/{runId}/upvote
// Public run required. Returns {upvoted, upvote_count}.
func (h *UpvoteHandler) Toggle(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.callerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID := chi.URLParam(r, "runId")
	ctx := r.Context()

	var visibility, ownerID string
	if err := h.db.QueryRow(ctx,
		`SELECT visibility, owner_id FROM user_reaches WHERE id = $1`, runID,
	).Scan(&visibility, &ownerID); err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}
	if visibility != "public" {
		errorResponse(w, http.StatusForbidden, "run is not public")
		return
	}
	if ownerID == callerID {
		errorResponse(w, http.StatusForbidden, "cannot upvote your own run")
		return
	}

	// Toggle: attempt insert; unique constraint violation = already voted → delete.
	_, err := h.db.Exec(ctx, `INSERT INTO run_upvotes (user_reach_id, user_id) VALUES ($1, $2)`, runID, callerID)
	upvoted := true
	if err != nil {
		_, _ = h.db.Exec(ctx, `DELETE FROM run_upvotes WHERE user_reach_id = $1 AND user_id = $2`, runID, callerID)
		upvoted = false
	}

	var count int
	_ = h.db.QueryRow(ctx, `SELECT COUNT(*) FROM run_upvotes WHERE user_reach_id = $1`, runID).Scan(&count)

	jsonResponse(w, http.StatusOK, map[string]any{"upvoted": upvoted, "upvote_count": count})
}
