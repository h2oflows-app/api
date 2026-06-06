package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	gauge "github.com/h2oflow/h2oflow/apps/api/internal/gaugecore"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GaugeExternalHandler serves gauge discovery and external-add endpoints.
type GaugeExternalHandler struct {
	db   *pgxpool.Pool
	usgs *gauge.USGSSource
	dwr  *gauge.DWRSource
}

func NewGaugeExternalHandler(db *pgxpool.Pool, usgs *gauge.USGSSource, dwr *gauge.DWRSource) *GaugeExternalHandler {
	return &GaugeExternalHandler{db: db, usgs: usgs, dwr: dwr}
}

type externalSiteResult struct {
	ExternalID string   `json:"external_id"`
	Source     string   `json:"source"`
	Name       string   `json:"name"`
	Lat        *float64 `json:"lat"`
	Lng        *float64 `json:"lng"`
}

// SearchExternal handles GET /api/v1/gauges/search-external?q=&state=
//
// Searches USGS NWIS (all states) and Colorado DWR (when state=CO) for gauges
// matching the query string. Returns up to 40 combined results.
func (h *GaugeExternalHandler) SearchExternal(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	state := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("state")))

	if q == "" {
		jsonResponse(w, http.StatusOK, []externalSiteResult{})
		return
	}

	type sourceResult struct {
		sites []*gauge.SiteMetadata
		src   string
	}
	results := make(chan sourceResult, 2)

	var wg sync.WaitGroup

	// USGS NWIS requires a geographic filter (stateCd, bBox, or sites).
	// Only call USGS when a state is selected.
	if state != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sites, err := h.usgs.DiscoverSites(r.Context(), gauge.DiscoverOptions{
				NameLike:   q,
				StateCodes: []string{state},
				ActiveOnly: true,
			})
			if err != nil {
				log.Printf("gauge search-external USGS state=%s q=%q: %v", state, q, err)
			}
			results <- sourceResult{sites: sites, src: string(gauge.SourceUSGS)}
		}()
	}

	// DWR is Colorado-only but always searchable (includes abbreviation matching).
	if state == "" || state == "CO" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sites, err := h.dwr.DiscoverSites(r.Context(), gauge.DiscoverOptions{
				NameLike:   q,
				ActiveOnly: true,
			})
			if err != nil {
				log.Printf("gauge search-external DWR q=%q: %v", q, err)
			}
			results <- sourceResult{sites: sites, src: string(gauge.SourceDWR)}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]externalSiteResult, 0, 40)
	for res := range results {
		for _, s := range res.sites {
			if len(out) >= 40 {
				break
			}
			r := externalSiteResult{
				ExternalID: s.ExternalID,
				Source:     res.src,
				Name:       s.Name,
			}
			if s.Location != nil {
				r.Lat = &s.Location.Lat
				r.Lng = &s.Location.Lng
			}
			out = append(out, r)
		}
	}

	jsonResponse(w, http.StatusOK, out)
}

// AddExternal handles POST /api/v1/me/gauges/add-external
//
// Upserts a gauge by external_id+source and adds it to the caller's watchlist.
// Used when adding a gauge discovered via SearchExternal that may not yet be in
// our DB. After upsert the gauge enters the demand polling tier automatically.
func (h *GaugeExternalHandler) AddExternal(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var body struct {
		ExternalID  string   `json:"external_id"`
		Source      string   `json:"source"`
		Name        string   `json:"name"`
		Lat         *float64 `json:"lat"`
		Lng         *float64 `json:"lng"`
		DashboardID *string  `json:"dashboard_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.ExternalID == "" || body.Source == "" || body.Name == "" {
		errorResponse(w, http.StatusBadRequest, "external_id, source, name required")
		return
	}

	extID := normalizeExternalID(strings.TrimSpace(body.ExternalID), body.Source)

	// Upsert gauge record.
	var gaugeID string
	var err error
	if body.Lat != nil && body.Lng != nil {
		err = h.db.QueryRow(r.Context(), `
			INSERT INTO gauges (external_id, source, name, location)
			VALUES ($1, $2, $3, ST_SetSRID(ST_MakePoint($5, $4), 4326)::geography)
			ON CONFLICT (external_id, source) DO UPDATE
			  SET name = EXCLUDED.name, location = EXCLUDED.location
			RETURNING id
		`, extID, body.Source, body.Name, body.Lat, body.Lng).Scan(&gaugeID)
	} else {
		err = h.db.QueryRow(r.Context(), `
			INSERT INTO gauges (external_id, source, name)
			VALUES ($1, $2, $3)
			ON CONFLICT (external_id, source) DO UPDATE
			  SET name = EXCLUDED.name
			RETURNING id
		`, extID, body.Source, body.Name).Scan(&gaugeID)
	}
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to upsert gauge")
		return
	}

	// Add to watchlist (upsert — idempotent if already added).
	_, err = h.db.Exec(r.Context(), `
		INSERT INTO watchlist (user_id, gauge_id, dashboard_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, gauge_id, dashboard_id) DO NOTHING
	`, ownerID, gaugeID, body.DashboardID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to add to watchlist")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]string{"gauge_id": gaugeID})
}

