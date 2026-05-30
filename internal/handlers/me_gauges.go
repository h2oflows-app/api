package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/h2oflow/h2oflow/apps/api/internal/poller"
	"github.com/jackc/pgx/v5/pgxpool"
)

const meGaugeRefreshLimit = 5

type meGaugePoller interface {
	FetchNowIfStale(ctx context.Context, gaugeID string, maxAge time.Duration) bool
}

// UserGaugesHandler handles /me/gauges endpoints.
type UserGaugesHandler struct {
	db            *pgxpool.Pool
	poller        meGaugePoller
	devFallbackID string
}

func NewUserGaugesHandler(db *pgxpool.Pool, devFallbackID string) *UserGaugesHandler {
	return &UserGaugesHandler{db: db, devFallbackID: devFallbackID}
}

func (h *UserGaugesHandler) WithPoller(p *poller.Poller) *UserGaugesHandler {
	h.poller = p
	return h
}

func (h *UserGaugesHandler) callerID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

// List handles GET /api/v1/me/gauges.
// Returns all standard gauges linked to the caller's runs or watchlist entries.
func (h *UserGaugesHandler) List(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.callerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	type gaugeItem struct {
		ID                   string     `json:"id"`
		ExternalID           string     `json:"external_id"`
		Source               string     `json:"source"`
		Name                 string     `json:"name"`
		Lat                  *float64   `json:"lat"`
		Lng                  *float64   `json:"lng"`
		Status               string     `json:"status"`
		LastReadingCfs       *float64   `json:"last_reading_cfs"`
		LastReadingTs        *time.Time `json:"last_reading_ts"`
		RecentFailCount      int        `json:"recent_fail_count"`
		SourceURL            *string    `json:"source_url"`
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT DISTINCT
			g.id, g.external_id, g.source, g.name,
			ST_Y(g.location::geometry) AS lat,
			ST_X(g.location::geometry) AS lng,
			g.status,
			g.current_cfs,
			g.last_reading_at,
			g.consecutive_poll_failures
		FROM gauges g
		WHERE g.id IN (
			SELECT ur.primary_gauge_id
			FROM user_reaches ur
			WHERE ur.owner_id = $1 AND ur.primary_gauge_id IS NOT NULL
			UNION
			SELECT uw.gauge_id
			FROM user_watchlists uw
			WHERE uw.user_id = $1 AND uw.gauge_id IS NOT NULL
		)
		ORDER BY g.name
	`, callerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	items := make([]gaugeItem, 0)
	for rows.Next() {
		var g gaugeItem
		if err := rows.Scan(
			&g.ID, &g.ExternalID, &g.Source, &g.Name,
			&g.Lat, &g.Lng,
			&g.Status,
			&g.LastReadingCfs,
			&g.LastReadingTs,
			&g.RecentFailCount,
		); err != nil {
			continue
		}
		if u := sourceURL(g.Source, g.ExternalID); u != "" {
			g.SourceURL = &u
		}
		items = append(items, g)
	}

	jsonResponse(w, http.StatusOK, map[string]any{"gauges": items})
}

// Refresh handles POST /api/v1/me/gauges/{id}/refresh.
// Force-polls the gauge. Rate-limited to 5 attempts per user per gauge per hour.
func (h *UserGaugesHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.callerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	gaugeID := chi.URLParam(r, "id")
	ctx := r.Context()

	// Verify gauge exists and caller has access (owns a run or watchlist entry).
	var exists bool
	_ = h.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM gauges g
			WHERE g.id = $1 AND g.id IN (
				SELECT primary_gauge_id FROM user_reaches WHERE owner_id = $2 AND primary_gauge_id IS NOT NULL
				UNION
				SELECT gauge_id FROM user_watchlists WHERE user_id = $2 AND gauge_id IS NOT NULL
			)
		)
	`, gaugeID, callerID).Scan(&exists)
	if !exists {
		errorResponse(w, http.StatusNotFound, "gauge not found")
		return
	}

	// Check rate limit: 5 attempts per hour.
	var recentCount int
	_ = h.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM gauge_refresh_attempts
		WHERE user_id = $1 AND gauge_id = $2 AND attempted_at > NOW() - INTERVAL '1 hour'
	`, callerID, gaugeID).Scan(&recentCount)

	if recentCount >= meGaugeRefreshLimit {
		// Find when the oldest of the 5 recent attempts falls out of the window.
		var oldest time.Time
		_ = h.db.QueryRow(ctx, `
			SELECT attempted_at FROM gauge_refresh_attempts
			WHERE user_id = $1 AND gauge_id = $2 AND attempted_at > NOW() - INTERVAL '1 hour'
			ORDER BY attempted_at ASC
			LIMIT 1
		`, callerID, gaugeID).Scan(&oldest)
		nextAttempt := oldest.Add(time.Hour)
		jsonResponse(w, http.StatusTooManyRequests, map[string]any{
			"error":           "rate limit exceeded",
			"next_attempt_ts": nextAttempt,
		})
		return
	}

	// Record the attempt.
	_, _ = h.db.Exec(ctx,
		`INSERT INTO gauge_refresh_attempts (user_id, gauge_id) VALUES ($1, $2)`,
		callerID, gaugeID,
	)

	if h.poller == nil {
		errorResponse(w, http.StatusServiceUnavailable, "poller unavailable")
		return
	}
	fetched := h.poller.FetchNowIfStale(ctx, gaugeID, 0)
	jsonResponse(w, http.StatusOK, map[string]any{"fetched": fetched})
}

// sourceURL returns the upstream monitoring URL for a gauge, or empty string.
func sourceURL(source, externalID string) string {
	switch source {
	case "usgs":
		return "https://waterdata.usgs.gov/monitoring-location/" + externalID + "/"
	case "dwr":
		return "https://dwr.state.co.us/Tools/Stations/" + externalID
	default:
		return ""
	}
}
