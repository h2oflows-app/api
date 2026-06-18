package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WatchlistHandler handles /api/v1/watchlist routes.
// All routes require an authenticated user (auth.Required middleware).
type WatchlistHandler struct {
	db *pgxpool.Pool
}

func NewWatchlistHandler(db *pgxpool.Pool) *WatchlistHandler {
	return &WatchlistHandler{db: db}
}

type watchlistItem struct {
	Kind                  string  `json:"kind"` // "gauge" | "custom_gauge" | "reference"
	GaugeID               *string `json:"gauge_id"`
	CustomGaugeID         *string `json:"custom_gauge_id"`
	ReachSlug             *string `json:"reach_slug"`
	DashboardID           *string `json:"dashboard_id"`
	ReferencedUserReachID *string `json:"referenced_user_reach_id"`
}

// List handles GET /api/v1/watchlist?dashboard_id=<uuid>
// Returns items for a specific dashboard (or all items if no dashboard_id given).
func (h *WatchlistHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	dashboardID := r.URL.Query().Get("dashboard_id")

	var (
		pgrows interface {
			Next() bool
			Scan(...any) error
			Close()
		}
		err error
	)

	if dashboardID != "" {
		pgrows, err = h.db.Query(r.Context(), `
			SELECT gauge_id::text, custom_gauge_id::text, reach_slug, dashboard_id::text,
			       referenced_user_reach_id::text
			FROM   user_watchlists
			WHERE  user_id = $1 AND dashboard_id = $2::uuid
			ORDER  BY created_at
		`, userID, dashboardID)
	} else {
		pgrows, err = h.db.Query(r.Context(), `
			SELECT gauge_id::text, custom_gauge_id::text, reach_slug, dashboard_id::text,
			       referenced_user_reach_id::text
			FROM   user_watchlists
			WHERE  user_id = $1
			ORDER  BY created_at
		`, userID)
	}
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer pgrows.Close()

	items := []watchlistItem{}
	for pgrows.Next() {
		var item watchlistItem
		if err := pgrows.Scan(&item.GaugeID, &item.CustomGaugeID, &item.ReachSlug, &item.DashboardID, &item.ReferencedUserReachID); err == nil {
			switch {
			case item.ReferencedUserReachID != nil:
				item.Kind = "reference"
			case item.CustomGaugeID != nil:
				item.Kind = "custom_gauge"
			default:
				item.Kind = "gauge"
			}
			items = append(items, item)
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{"items": items})
}

// Add handles POST /api/v1/watchlist
// Body: { "gauge_id": "<uuid>", "reach_slug": "<slug>", "dashboard_id": "<uuid>" }
//    or { "custom_gauge_id": "<uuid>", "reach_slug": "<slug>", "dashboard_id": "<uuid>" }
// If dashboard_id is omitted, defaults to the user's first dashboard (position=0).
// Idempotent — re-adding the same pair is a no-op.
func (h *WatchlistHandler) Add(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var body struct {
		GaugeID       *string `json:"gauge_id"`
		CustomGaugeID *string `json:"custom_gauge_id"`
		ReachSlug     *string `json:"reach_slug"`
		DashboardID   *string `json:"dashboard_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.GaugeID != nil && body.CustomGaugeID != nil {
		errorResponse(w, http.StatusBadRequest, "only one of gauge_id or custom_gauge_id allowed")
		return
	}
	if body.GaugeID == nil && body.CustomGaugeID == nil && (body.ReachSlug == nil || *body.ReachSlug == "") {
		errorResponse(w, http.StatusBadRequest, "gauge_id, custom_gauge_id, or reach_slug required")
		return
	}

	// Resolve dashboard_id: use provided or default to first dashboard.
	dashboardID := body.DashboardID
	if dashboardID == nil {
		var dbID string
		err := h.db.QueryRow(r.Context(), `
			SELECT id::text FROM user_dashboards
			WHERE owner_id = $1 ORDER BY position, created_at LIMIT 1
		`, userID).Scan(&dbID)
		if err != nil {
			// No dashboard yet — auto-create one.
			err = h.db.QueryRow(r.Context(), `
				INSERT INTO user_dashboards (owner_id, slug, name, position)
				VALUES ($1, 'default', 'My Dashboard', 0)
				ON CONFLICT (owner_id, slug) DO UPDATE SET name = EXCLUDED.name
				RETURNING id::text
			`, userID).Scan(&dbID)
			if err != nil {
				errorResponse(w, http.StatusInternalServerError, "dashboard resolution failed")
				return
			}
		}
		dashboardID = &dbID
	}

	// Auto-fork: if reach_slug points to a curated reaches row (not owned by this
	// user in user_reaches), fork it into the caller's namespace inside one tx
	// and rewrite reach_slug to the new fork slug before inserting the watchlist.
	finalReachSlug := body.ReachSlug
	if body.ReachSlug != nil && *body.ReachSlug != "" {
		tx, err := h.db.Begin(r.Context())
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, "tx begin failed")
			return
		}
		defer tx.Rollback(r.Context())

		var ownedID string
		ownErr := tx.QueryRow(r.Context(),
			`SELECT id FROM user_reaches WHERE owner_id = $1 AND slug = $2`,
			userID, *body.ReachSlug,
		).Scan(&ownedID)
		if ownErr != nil {
			// Not owned by caller — check if curated reach exists; if yes, fork.
			var curatedID string
			cErr := tx.QueryRow(r.Context(),
				`SELECT id FROM user_reaches WHERE slug = $1 AND owner_id = '00000000-0000-0000-0000-000000000001' AND deleted_at IS NULL`, *body.ReachSlug,
			).Scan(&curatedID)
			if cErr == nil {
				// Check if user already has a fork of this curated reach — reuse it
				// rather than creating a second fork (fork slug differs from source slug,
				// so the ownErr check above misses it and double-forks on re-add).
				var existingForkSlug string
				existingForkErr := tx.QueryRow(r.Context(),
					`SELECT slug FROM user_reaches WHERE owner_id = $1 AND forked_from_user_reach_id = $2::uuid LIMIT 1`,
					userID, curatedID,
				).Scan(&existingForkSlug)
				if existingForkErr == nil {
					finalReachSlug = &existingForkSlug
				} else {
					_, newSlug, fErr := forkCuratedReachTx(r.Context(), tx, userID, *body.ReachSlug)
					if fErr != nil {
						errorResponse(w, http.StatusInternalServerError, "fork failed: "+fErr.Error())
						return
					}
					finalReachSlug = &newSlug
				}
			}
			// else: slug is for another user's user_reaches — leave as-is; insert will
			// either fail FK or pin a reference. (No cross-user fork via watchlist yet.)
		}

		var insertErr error
		switch {
		case body.GaugeID != nil:
			_, insertErr = tx.Exec(r.Context(), `
				INSERT INTO user_watchlists (user_id, gauge_id, reach_slug, dashboard_id)
				VALUES ($1, $2::uuid, $3, $4::uuid)
				ON CONFLICT DO NOTHING
			`, userID, body.GaugeID, finalReachSlug, dashboardID)
		case body.CustomGaugeID != nil:
			_, insertErr = tx.Exec(r.Context(), `
				INSERT INTO user_watchlists (user_id, custom_gauge_id, reach_slug, dashboard_id)
				VALUES ($1, $2::uuid, $3, $4::uuid)
				ON CONFLICT DO NOTHING
			`, userID, body.CustomGaugeID, finalReachSlug, dashboardID)
		default:
			// Resolve primary_gauge_id from the user reach so the watchlist entry
			// includes the gauge — enables group-by-gauge on the dashboard and uses
			// the existing unique constraint (migration 082) to prevent duplicates.
			var primaryGaugeID *string
			_ = tx.QueryRow(r.Context(),
				`SELECT primary_gauge_id::text FROM user_reaches WHERE slug = $1 AND deleted_at IS NULL LIMIT 1`,
				finalReachSlug,
			).Scan(&primaryGaugeID)

			if primaryGaugeID != nil {
				_, insertErr = tx.Exec(r.Context(), `
					INSERT INTO user_watchlists (user_id, gauge_id, reach_slug, dashboard_id)
					VALUES ($1, $2::uuid, $3, $4::uuid)
					ON CONFLICT DO NOTHING
				`, userID, primaryGaugeID, finalReachSlug, dashboardID)
			} else {
				_, insertErr = tx.Exec(r.Context(), `
					INSERT INTO user_watchlists (user_id, reach_slug, dashboard_id)
					VALUES ($1, $2, $3::uuid)
					ON CONFLICT DO NOTHING
				`, userID, finalReachSlug, dashboardID)
			}
		}
		if insertErr != nil {
			errorResponse(w, http.StatusInternalServerError, "insert failed")
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			errorResponse(w, http.StatusInternalServerError, "commit failed")
			return
		}

		jsonResponse(w, http.StatusOK, map[string]any{
			"reach_slug":   finalReachSlug,
			"dashboard_id": dashboardID,
		})
		return
	}

	// No reach_slug: pure gauge/custom_gauge watchlist add — no fork path.
	var err error
	switch {
	case body.GaugeID != nil:
		_, err = h.db.Exec(r.Context(), `
			INSERT INTO user_watchlists (user_id, gauge_id, dashboard_id)
			VALUES ($1, $2::uuid, $3::uuid)
			ON CONFLICT DO NOTHING
		`, userID, body.GaugeID, dashboardID)
	case body.CustomGaugeID != nil:
		_, err = h.db.Exec(r.Context(), `
			INSERT INTO user_watchlists (user_id, custom_gauge_id, dashboard_id)
			VALUES ($1, $2::uuid, $3::uuid)
			ON CONFLICT DO NOTHING
		`, userID, body.CustomGaugeID, dashboardID)
	}
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "insert failed")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Remove handles DELETE /api/v1/watchlist/{id}?kind=gauge|custom_gauge&reach_slug=<slug>
// kind defaults to "gauge". reach_slug is optional.
func (h *WatchlistHandler) Remove(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	id := chi.URLParam(r, "gaugeId")
	if id == "" {
		errorResponse(w, http.StatusBadRequest, "id required")
		return
	}

	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "gauge"
	}

	reachSlug := r.URL.Query().Get("reach_slug")
	var reachSlugPtr *string
	if reachSlug != "" {
		reachSlugPtr = &reachSlug
	}

	dashboardID := r.URL.Query().Get("dashboard_id")
	var dashboardIDPtr *string
	if dashboardID != "" {
		dashboardIDPtr = &dashboardID
	}

	var err error
	switch kind {
	case "reference":
		// id is the referenced_user_reach_id UUID. After removal, GC tombstoned run if zero refs. (V13)
		_, err = h.db.Exec(r.Context(), `
			DELETE FROM user_watchlists
			WHERE user_id = $1 AND referenced_user_reach_id = $2::uuid
			  AND ($3::uuid IS NULL OR dashboard_id = $3::uuid)
		`, userID, id, dashboardIDPtr)
		if err == nil {
			go gcTombstonedRun(r.Context(), h.db, id)
		}
	case "reach":
		// id is the reach_slug (text); removes all entries for this reach (gauge-linked or not).
		_, err = h.db.Exec(r.Context(), `
			DELETE FROM user_watchlists
			WHERE user_id = $1 AND reach_slug = $2
			  AND ($3::uuid IS NULL OR dashboard_id = $3::uuid)
		`, userID, id, dashboardIDPtr)
	case "custom_gauge":
		_, err = h.db.Exec(r.Context(), `
			DELETE FROM user_watchlists
			WHERE user_id = $1 AND custom_gauge_id = $2::uuid
			  AND reach_slug IS NOT DISTINCT FROM $3
			  AND ($4::uuid IS NULL OR dashboard_id = $4::uuid)
		`, userID, id, reachSlugPtr, dashboardIDPtr)
	default:
		_, err = h.db.Exec(r.Context(), `
			DELETE FROM user_watchlists
			WHERE user_id = $1 AND gauge_id = $2::uuid
			  AND reach_slug IS NOT DISTINCT FROM $3
			  AND ($4::uuid IS NULL OR dashboard_id = $4::uuid)
		`, userID, id, reachSlugPtr, dashboardIDPtr)
	}
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// AddReference handles POST /api/v1/user-runs/{runId}/add-to-dashboard
// Adds a public run to the caller's dashboard by reference (no copy). (V6)
// Body: { "dashboard_id": "<uuid>" }  (optional; defaults to first dashboard)
func (h *WatchlistHandler) AddReference(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID := chi.URLParam(r, "runId")
	ctx := r.Context()

	// Verify run is public and not tombstoned (V6).
	var exists bool
	if err := h.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM user_reaches WHERE id = $1 AND visibility = 'public' AND deleted_at IS NULL)`,
		runID,
	).Scan(&exists); err != nil || !exists {
		errorResponse(w, http.StatusNotFound, "run not found or not public")
		return
	}

	var body struct {
		DashboardID *string `json:"dashboard_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	// Resolve dashboard_id: use provided or default to first dashboard.
	dashboardID := body.DashboardID
	if dashboardID == nil {
		var dbID string
		err := h.db.QueryRow(ctx, `
			SELECT id::text FROM user_dashboards
			WHERE owner_id = $1 ORDER BY position, created_at LIMIT 1
		`, userID).Scan(&dbID)
		if err != nil {
			// Auto-create default dashboard.
			err = h.db.QueryRow(ctx, `
				INSERT INTO user_dashboards (owner_id, slug, name, position)
				VALUES ($1, 'default', 'My Dashboard', 0)
				ON CONFLICT (owner_id, slug) DO UPDATE SET name = EXCLUDED.name
				RETURNING id::text
			`, userID).Scan(&dbID)
			if err != nil {
				errorResponse(w, http.StatusInternalServerError, "dashboard resolution failed")
				return
			}
		}
		dashboardID = &dbID
	}

	_, err := h.db.Exec(ctx, `
		INSERT INTO user_watchlists (user_id, referenced_user_reach_id, dashboard_id)
		VALUES ($1, $2::uuid, $3::uuid)
		ON CONFLICT DO NOTHING
	`, userID, runID, dashboardID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "insert failed")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"referenced_user_reach_id": runID,
		"dashboard_id":             dashboardID,
	})
}
