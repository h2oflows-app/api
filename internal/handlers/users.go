package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserProfileHandler serves public user profile endpoints.
type UserProfileHandler struct {
	db *pgxpool.Pool
}

func NewUserProfileHandler(db *pgxpool.Pool) *UserProfileHandler {
	return &UserProfileHandler{db: db}
}

type userProfileRun struct {
	ID         string     `json:"id"`
	Slug       string     `json:"slug"`
	Name       string     `json:"name"`
	RiverName  *string    `json:"river_name"`
	ClassMin   *float64   `json:"class_min"`
	ClassMax   *float64   `json:"class_max"`
	CurrentCFS *float64   `json:"current_cfs"`
	FlowStatus string     `json:"flow_status"`
	CreatedAt  time.Time  `json:"created_at"`
}

type userProfileResponse struct {
	Handle string           `json:"handle"`
	Runs   []userProfileRun `json:"runs"`
}

// GET /api/v1/users/{handle}
func (h *UserProfileHandler) GetProfile(w http.ResponseWriter, r *http.Request) {
	handle := chi.URLParam(r, "handle")

	// Verify handle exists.
	var ownerID string
	err := h.db.QueryRow(r.Context(),
		`SELECT owner_id FROM user_profiles WHERE LOWER(handle) = LOWER($1)`,
		handle,
	).Scan(&ownerID)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "user not found")
		return
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT
			ur.id, ur.slug, ur.name, ur.river_name,
			ur.class_min, ur.class_max,
			COALESCE(lr.value, cg.last_value_cfs) AS current_cfs,
			CASE
				WHEN COALESCE(lr.value, cg.last_value_cfs) IS NULL OR fr.label IS NULL THEN 'unknown'
				WHEN fr.label = 'running' THEN 'runnable'
				WHEN fr.label = 'low'     THEN 'caution'
				WHEN fr.label = 'high'    THEN 'flood'
				ELSE 'unknown'
			END AS flow_status,
			ur.created_at
		FROM user_reaches ur
		LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
		LEFT JOIN LATERAL (
			SELECT value FROM gauge_readings
			WHERE gauge_id = ur.primary_gauge_id
			  AND timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY timestamp DESC LIMIT 1
		) lr ON TRUE
		LEFT JOIN LATERAL (
			SELECT label FROM user_reach_flow_ranges
			WHERE user_reach_id = ur.id
			  AND (min_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) >= min_value)
			  AND (max_value IS NULL OR COALESCE(lr.value, cg.last_value_cfs) <  max_value)
			ORDER BY min_value ASC NULLS FIRST
			LIMIT 1
		) fr ON TRUE
		WHERE ur.owner_id = $1 AND ur.is_private = FALSE
		ORDER BY ur.created_at DESC
	`, ownerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	runs := make([]userProfileRun, 0)
	for rows.Next() {
		var run userProfileRun
		if err := rows.Scan(
			&run.ID, &run.Slug, &run.Name, &run.RiverName,
			&run.ClassMin, &run.ClassMax,
			&run.CurrentCFS, &run.FlowStatus, &run.CreatedAt,
		); err == nil {
			runs = append(runs, run)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(userProfileResponse{Handle: handle, Runs: runs})
}
