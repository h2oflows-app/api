package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/h2oflow/h2oflow/apps/api/internal/ai"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/h2oflow/h2oflow/apps/api/internal/config"
	"github.com/h2oflow/h2oflow/apps/api/internal/db"
	gauge "github.com/h2oflow/h2oflow/apps/api/internal/gaugecore"
	"github.com/h2oflow/h2oflow/apps/api/internal/handlers"
	"github.com/h2oflow/h2oflow/apps/api/internal/poller"
)

func main() {
	cfg := config.Load()

	// Run migrations before accepting traffic.
	if err := runMigrations(cfg); err != nil {
		log.Fatalf("migrations: %v", err)
	}

	pool, err := db.Connect(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{"http://localhost:3000", "https://h2oflows.app", "https://*.h2oflows.app", "https://*.netlify.app"},
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Accept", "Authorization", "Content-Type", "X-API-Key"},
		MaxAge:         300,
	}))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	// AI search enrichment — optional, degrades gracefully if key is absent.
	var enricher *ai.SearchEnricher
	if cfg.AnthropicAPIKey != "" {
		enricher = ai.NewSearchEnricher(cfg.AnthropicAPIKey)
	} else {
		log.Println("ANTHROPIC_API_KEY not set — AI search enrichment disabled")
	}

	// River assistant — long-context prompt-stuffing of structured DB data +
	// community reports (runs-unify 5c dropped the Voyage/reach_embeddings path).
	var asker *ai.ReachAsker
	if cfg.AnthropicAPIKey != "" {
		asker = ai.NewReachAsker(pool, cfg.AnthropicAPIKey)
	} else {
		log.Println("ANTHROPIC_API_KEY not set — river assistant (/ask) disabled")
	}

	// Start gauge poller — runs in background, survives HTTP errors.
	pollInterval := cfg.ParsePollInterval()
	p := poller.New(pool)
	p.Register(gauge.NewUSGSSource(cfg.USGSAPIKey), pollInterval.USGS)
	p.Register(gauge.NewDWRSource(cfg.DWRAPIKeys), pollInterval.DWR)
	pollerCtx, stopPoller := context.WithCancel(context.Background())
	go p.Run(pollerCtx)

	var describer *ai.TripDescriber
	if cfg.AnthropicAPIKey != "" {
		describer = ai.NewTripDescriber(pool, cfg.AnthropicAPIKey)
	}

	// Supabase JWT verifier — optional. When unset, all requests stay anonymous
	// and the existing device_id flow continues to work unchanged.
	var verifier *auth.Verifier
	if cfg.SupabaseJWKSURL != "" {
		v, err := auth.NewVerifier(context.Background(), cfg.SupabaseJWKSURL)
		if err != nil {
			log.Fatalf("auth: %v", err)
		}
		verifier = v
		log.Printf("auth: Supabase JWT verification enabled (%s)", cfg.SupabaseJWKSURL)
	} else {
		log.Println("SUPABASE_JWKS_URL not set — auth middleware disabled, all requests anonymous")
	}

	// devFallbackID lets user-scoped endpoints work in local dev (no Supabase).
	devFallbackID := ""
	if verifier == nil {
		devFallbackID = "dev-user"
	}

	gauges := handlers.NewGaugeHandler(pool, enricher).WithPoller(p)
	gaugeExt := handlers.NewGaugeExternalHandler(pool, gauge.NewUSGSSource(cfg.USGSAPIKey))
	reaches := handlers.NewReachHandler(pool, asker).WithPoller(p)
	watchlist := handlers.NewWatchlistHandler(pool)
	admin := handlers.NewAdminHandler(pool).WithPoller(p)
	corrections := handlers.NewCorrectionsHandler(pool)
	userReaches := handlers.NewUserReachHandler(pool, devFallbackID)
	customGauges := handlers.NewCustomGaugeHandler(pool, devFallbackID)
	nldiH := handlers.NewNLDIHandler(pool).WithAnthropicKey(cfg.AnthropicAPIKey).WithCacheWarmer(func() { reaches.WarmCache(context.Background()) })
	// Warm the reach map cache immediately, then refresh every poll cycle.
	reaches.WarmCache(context.Background())
	reaches.StartCacheRefresh(pollerCtx, pollInterval.USGS)
	trips := handlers.NewTripHandler(pool, describer)
	reports := handlers.NewReportHandler(pool, devFallbackID)
	flowProposals := handlers.NewFlowProposalHandler(pool, devFallbackID)
	flowBandOverrides := handlers.NewFlowBandOverrideHandler(pool, devFallbackID)
	cluster := handlers.NewClusterHandler(pool, devFallbackID)
	upvote := handlers.NewUpvoteHandler(pool, devFallbackID)
	meGauges := handlers.NewUserGaugesHandler(pool, devFallbackID).WithPoller(p)
	userProfiles := handlers.NewUserProfileHandler(pool)
	moderation := handlers.NewModerationHandler(pool, devFallbackID)
	discover := handlers.NewDiscoverHandler(pool)
	preferences := handlers.NewPreferencesHandler(pool, devFallbackID)
	profile := handlers.NewProfileHandler(pool, devFallbackID)
	dashboards := handlers.NewDashboardHandler(pool, devFallbackID)
	basinOverrides := handlers.NewBasinOverrideHandler(pool, devFallbackID)
	ogh := handlers.NewOGHandler(pool)
	// LoadAppRoles queries user_roles for the authenticated user on each request.
	// Runs after Optional/Required so the user ID is already in context.
	loadAppRoles := auth.LoadAppRoles(func(r *http.Request, userID string) ([]string, error) {
		rows, err := pool.Query(r.Context(),
			`SELECT role FROM user_roles WHERE user_id = $1`, userID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var roles []string
		for rows.Next() {
			var role string
			if rows.Scan(&role) == nil {
				roles = append(roles, role)
			}
		}
		return roles, nil
	})

	r.Route("/api/v1", func(r chi.Router) {
		// Optional: attaches user claims when a valid Bearer token is present,
		// but anonymous (device_id) requests still flow through.
		r.Use(auth.Optional(verifier))
		// LoadAppRoles enriches context with DB roles for authenticated users.
		r.Use(loadAppRoles)
		r.Get("/gauges/search", gauges.Search)
		r.Get("/gauges/search-external", gaugeExt.SearchExternal)
		r.Get("/gauges/batch", gauges.BatchGet)
		r.Post("/gauges/batch", gauges.BatchPost)
		r.Get("/gauges/{id}/readings", gauges.GetReadings)
		r.Get("/gauges/{id}/flow-ranges", gauges.GetFlowRanges)
		r.Get("/gauges/{id}/seasonal", gauges.GetSeasonalStats)
		r.Post("/gauges/{id}/refresh", gauges.Refresh)

		r.Get("/reaches/map/all", reaches.MapAll)
		r.Get("/reaches/map", reaches.Map)
		r.Get("/reaches/basin/{slug}/map", reaches.BasinMap)
		r.Get("/reaches/basin/{slug}/network", reaches.BasinNetwork)
		r.Get("/reaches", reaches.List)
		r.Get("/reaches/{slug}/flow-ranges", reaches.GetFlowRanges)
		// NOTE: /reaches/{slug} detail, conditions, hazards, and per-reach ask were
		// removed in runs-unify 5a — curated detail is served by /users/h2oflows/runs/{slug}.
		r.Post("/ask", reaches.GlobalAsk)
		r.Get("/stats", reaches.Stats)
		r.Get("/discover/runs", discover.ListRuns)
		r.Get("/admin/slug-check", admin.SlugCheck)
		// NLDI upstream-tributaries is public — run detail page needs it without auth.
		r.Get("/nldi/upstream-tributaries", nldiH.UpstreamTributaries)

		// Authenticated user routes — require a valid Supabase JWT.
		r.Group(func(r chi.Router) {
			r.Use(auth.Required(verifier))
			r.Get("/watchlist", watchlist.List)
			r.Post("/watchlist", watchlist.Add)
			r.Delete("/watchlist/{gaugeId}", watchlist.Remove)
			r.Get("/nldi/downstream", nldiH.DownstreamMainstem)
			r.Get("/nldi/river-name", nldiH.RiverName)
			r.Get("/nldi/preview-centerline", nldiH.PreviewCenterline)
			r.Get("/nldi/nearby-gauges", nldiH.NearbyGauges)
		})

		// User-defined reaches — auth optional here; handler gates by ownerID.
		r.Get("/me/reaches", userReaches.List)
		r.Post("/me/reaches", userReaches.Create)
		r.Post("/me/reaches/import", userReaches.Import)
		r.Get("/me/reaches/map/all", userReaches.MapAll)
		r.Get("/me/referenced-runs", userReaches.ReferencedRuns)
		r.Get("/me/reaches/{slug}", userReaches.Get)
		r.Patch("/me/reaches/{slug}", userReaches.Update)
		r.Delete("/me/reaches/{slug}", userReaches.Delete)
		r.Get("/me/reaches/{slug}/flow-ranges", userReaches.GetFlowRanges)
		r.Put("/me/reaches/{slug}/flow-ranges", userReaches.SetFlowRanges)
		r.Put("/me/reaches/{slug}/rapids", userReaches.SetRapids)
		r.Put("/me/reaches/{slug}/access", userReaches.SetAccessPoints)
		r.Post("/me/reaches/{slug}/centerline", userReaches.SetCenterline)
		r.Delete("/me/reaches/{slug}/centerline", userReaches.ClearCenterline)
		r.Put("/me/reaches/{slug}/gauge", userReaches.SetGauge)
		r.Delete("/me/reaches/{slug}/gauge", userReaches.ClearGauge)
		r.Post("/me/reaches/{slug}/kml", userReaches.ImportKML)
		r.Post("/me/reaches/{slug}/publish", userReaches.Publish)
		r.Post("/me/reaches/fork-reach/{slug}", userReaches.ForkReach)
		r.Post("/me/reaches/{slug}/reports", reports.CreateForUserReach)
		r.Get("/me/reaches/{slug}/reports", reports.ListByUserReach)
		r.Get("/me/reaches/{slug}/flow-proposals", flowProposals.ListForOwnRun)
		r.Post("/me/reaches/{slug}/flow-proposals", flowProposals.UpsertForOwnRun)
		r.Get("/me/reaches/{slug}/flow-band-override", flowBandOverrides.GetOwn)
		r.Put("/me/reaches/{slug}/flow-band-override", flowBandOverrides.UpsertOwn)
		r.Delete("/me/reaches/{slug}/flow-band-override", flowBandOverrides.DeleteOwn)

		r.Get("/me/gauges", meGauges.List)
		r.Post("/me/gauges/{id}/refresh", meGauges.Refresh)
		r.Post("/me/gauges/add-external", gaugeExt.AddExternal)

		// /me/runs/{slug} — user-facing aliases for /me/reaches/{slug}
		r.Get("/me/runs", userReaches.List)
		r.Post("/me/runs", userReaches.Create)
		r.Post("/me/runs/import", userReaches.Import)
		r.Get("/me/runs/map/all", userReaches.MapAll)
		r.Get("/me/runs/slug-check", userReaches.SlugCheck)
		r.Post("/me/runs/fork-reach/{slug}", userReaches.ForkReach)
		r.Get("/me/runs/{slug}", userReaches.Get)
		r.Patch("/me/runs/{slug}", userReaches.Update)
		r.Delete("/me/runs/{slug}", userReaches.Delete)
		r.Get("/me/runs/{slug}/flow-ranges", userReaches.GetFlowRanges)
		r.Put("/me/runs/{slug}/flow-ranges", userReaches.SetFlowRanges)
		r.Put("/me/runs/{slug}/rapids", userReaches.SetRapids)
		r.Put("/me/runs/{slug}/access", userReaches.SetAccessPoints)
		r.Post("/me/runs/{slug}/centerline", userReaches.SetCenterline)
		r.Delete("/me/runs/{slug}/centerline", userReaches.ClearCenterline)
		r.Put("/me/runs/{slug}/gauge", userReaches.SetGauge)
		r.Delete("/me/runs/{slug}/gauge", userReaches.ClearGauge)
		r.Post("/me/runs/{slug}/kml", userReaches.ImportKML)
		r.Post("/me/runs/{slug}/publish", userReaches.Publish)
		r.Post("/me/runs/{slug}/reports", reports.CreateForUserReach)
		r.Get("/me/runs/{slug}/reports", reports.ListByUserReach)
		r.Get("/me/runs/{slug}/flow-proposals", flowProposals.ListForOwnRun)
		r.Post("/me/runs/{slug}/flow-proposals", flowProposals.UpsertForOwnRun)

		// #155: authenticated public run-upload — API-key auth (X-API-Key), not JWT.
		r.Group(func(r chi.Router) {
			r.Use(auth.APIKey(pool))
			r.Post("/upload/runs", userReaches.UploadRuns)
			r.Patch("/upload/runs/{slug}", userReaches.UploadUpdate)
		})

		r.Post("/flow-proposals/{proposalId}/vote", flowProposals.ToggleVote)
		r.Get("/user-runs/{runId}/flow-proposals", flowProposals.ListByRunID)
		r.Post("/user-runs/{runId}/flow-proposals", flowProposals.UpsertByRunID)
		r.Get("/user-runs/{runId}/reports", reports.ListByRunID)
		r.Get("/users/search", userProfiles.Search)
		r.Get("/users/{handle}", userProfiles.GetProfile)
		r.Get("/users/{handle}/runs/map/all", userProfiles.MapAllByHandle)
		r.Get("/users/{handle}/runs/{slug}", userReaches.GetPublicByHandle)
		r.Get("/users/{handle}/runs/{slug}/flow-ranges", userReaches.GetPublicFlowRangesByHandle)

		r.Get("/user-runs/community", userReaches.ListCommunity)
		r.Get("/user-runs/map/community", userReaches.MapCommunity)
		r.Post("/user-runs/{runId}/fork", userReaches.ForkUserRun)
		r.Post("/user-runs/{runId}/add-to-dashboard", watchlist.AddReference)
		r.Post("/user-runs/{runId}/adopt", userReaches.Adopt)
		r.Get("/user-runs/{runId}", userReaches.GetPublic)
		r.Post("/user-runs/dupe-check", cluster.DupeCheck)
		r.Get("/user-runs/{runId}/cluster", cluster.ClusterForRun)
		r.Post("/user-runs/{runId}/upvote", upvote.Toggle)
		r.Post("/user-runs/{runId}/flag", moderation.FlagRun)
		r.Post("/reports/{reportId}/flag", moderation.FlagReport)
		r.Post("/me/river-corrections", corrections.CreateRiverCorrection)

		// Custom gauges — private to owner, auth gated via ownerID() + devFallbackID.
		r.Get("/me/custom-gauges", customGauges.List)
		r.Post("/me/custom-gauges", customGauges.Create)
		r.Post("/me/custom-gauges/import", customGauges.Import)
		r.Get("/me/custom-gauges/{slug}", customGauges.Get)
		r.Patch("/me/custom-gauges/{slug}", customGauges.Update)
		r.Delete("/me/custom-gauges/{slug}", customGauges.Delete)
		r.Get("/me/custom-gauges/{slug}/share", customGauges.Share)
		r.Get("/me/custom-gauges/{slug}/readings", customGauges.Readings)

		// Admin data/moderation routes — require the site_admin role.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireDataAdmin)
			// NOTE: curated-reach authoring/editing surface removed in runs-unify 3.3
			// (h2oflows now authors via the /me run flow + ?as=h2oflows override).
			// Rivers-management reads/writes (unassigned, grouped, auto-river, river
			// assign, /admin/rivers CRUD, reorder, gnis-lookup) are intentionally
			// retained here and flip as one unit in Phase 4.
			r.Get("/admin/rivers", admin.ListRivers)
			// NOTE: rivers-mgmt writes (CreateRiver/DeleteRiver/ReorderReachesForRiver)
			// retired in runs-unify 5b — they wrote the legacy reaches table and had
			// no live UI caller (admin UI only reads GET /admin/rivers).
			// NOTE: /admin/river-corrections GET+PATCH removed (#315) — the review
			// queue had no live caller; POST /me/river-corrections (user-facing
			// submission) is retained in corrections.go.
			r.Get("/admin/moderation/queue", moderation.Queue)
			r.Post("/admin/moderation/flags/{flagId}/dismiss", moderation.Dismiss)
			r.Post("/admin/moderation/flags/{flagId}/action", moderation.Action)
			r.Get("/admin/gauges", admin.ListAdminGauges)
			r.Post("/admin/gauges/{id}/poll", admin.PollAdminGauge)
			r.Post("/admin/gauges/{id}/retire", admin.RetireAdminGauge)
			r.Post("/admin/gauges/{id}/reactivate", admin.ReactivateAdminGauge)
			r.Patch("/admin/gauges/{id}/seasonal", admin.SetAdminGaugeSeasonal)
			r.Get("/admin/rivers/gnis-lookup", admin.GNISLookup)
			r.Get("/admin/nldi/watershed", nldiH.WatershedExplorer)
			r.Get("/admin/nldi/upstream-tributaries", nldiH.UpstreamTributaries)
			r.Get("/admin/nldi/downstream", nldiH.DownstreamMainstem)
			r.Get("/admin/nldi/river-name", nldiH.RiverName)
			r.Get("/admin/nldi/preview-centerline", nldiH.PreviewCenterline)
			r.Get("/admin/nldi/nearby-gauges", nldiH.NearbyGauges)
			// NOTE: /admin/reaches/flow-band-override-queue and
			// /admin/reaches/{slug}/apply-override-median removed (#315) — no live
			// caller. /admin/reaches/{slug}/flow-band-overrides (read) is retained.
			r.Get("/admin/reaches/{slug}/flow-band-overrides", flowBandOverrides.AdminListForReach)
		})

		// Site admin only — role management.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAdmin)
			r.Get("/admin/users/roles", admin.ListUserRoles)
			r.Post("/admin/users/roles", admin.AssignRole)
			r.Delete("/admin/users/roles/{roleId}", admin.RevokeRole)

			// Special users (#314): opt-in "official" accounts (h2oflows curator +
			// future partner orgs) with authoring rights granted via user_roles.
			r.Get("/admin/special-users", admin.ListSpecialUsers)
			r.Post("/admin/special-users", admin.CreateSpecialUser)
			r.Patch("/admin/special-users/{ownerId}", admin.UpdateSpecialUser)
			r.Delete("/admin/special-users/{ownerId}", admin.DeleteSpecialUser)
			r.Post("/admin/special-users/{ownerId}/rotate-key", admin.RotateAPIKey)
			r.Post("/admin/users/{ownerId}/rotate-key", admin.RotateAPIKey)

			r.Get("/admin/roles", admin.ListRoles)
			r.Post("/admin/roles/{role}/members", admin.AddRoleMember)
			r.Delete("/admin/roles/{role}/members/{userId}", admin.RemoveRoleMember)

			r.Get("/admin/users", admin.ListDirectoryUsers)
			r.Patch("/admin/users/{ownerId}/rate-limit", admin.UpdateRateLimit)
		})

		// Authenticated user — own role info.
		r.Group(func(r chi.Router) {
			r.Use(auth.Required(verifier))
			r.Get("/admin/me/roles", admin.GetMyRoles)
			r.Get("/me/authorable-accounts", admin.AuthorableAccounts)
		})

		// NOTE: legacy /reaches/{slug}/{contributions,trip-reports} + /proximity-events
		// retired in runs-unify 5c — superseded by the unified `reports` table below.
		// Reports (unified: trip reports, hazard warnings, conditions).
		r.Post("/reaches/{slug}/reports", reports.Create)
		r.Get("/reaches/{slug}/reports", reports.ListByReach)
		r.Get("/reports/{id}", reports.Get)
		r.Get("/me/reports", reports.ListMine)
		r.Patch("/me/reports/{slug}", reports.Update)
		r.Delete("/me/reports/{slug}", reports.Delete)
		r.Post("/me/reports/{slug}/aw-sync", reports.AWSync)
		r.Get("/me/preferences", preferences.Get)
		r.Patch("/me/preferences", preferences.Update)
		r.Get("/me/river-basin-overrides", basinOverrides.List)
		r.Put("/me/rivers/{riverID}/basin-override", basinOverrides.Upsert)
		r.Delete("/me/rivers/{riverID}/basin-override", basinOverrides.Delete)
		r.Get("/me/profile", profile.Get)
		r.Patch("/me/profile", profile.Update)
		r.Post("/me/profile/suggest", profile.Suggest)
		r.Get("/me/dashboards", dashboards.List)
		r.Post("/me/dashboards", dashboards.Create)
		r.Get("/me/dashboards/{slug}", dashboards.Get)
		r.Patch("/me/dashboards/{slug}", dashboards.Update)
		r.Delete("/me/dashboards/{slug}", dashboards.Delete)
		r.Patch("/me/dashboards-reorder", dashboards.Reorder)

		r.Post("/trips", trips.Create)
		r.Get("/trips", trips.List)
		r.Get("/trips/{id}", trips.Get)
		r.Patch("/trips/{id}", trips.Patch)
		r.Post("/trips/{id}/describe", trips.Describe)
	})

	// OpenGraph share-card PNGs — no auth, top-level path so social scrapers
	// can fetch directly without prefix.
	r.Get("/og/reaches/{slug}", ogh.Reach)
	r.Get("/og/reports/{id}", ogh.Report)
	r.Get("/og/gauges/{id}", ogh.Gauge)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", cfg.Port),
		Handler: r,
	}

	// Start server in background, shut down gracefully on SIGINT/SIGTERM.
	go func() {
		log.Printf("starting %s on :%s", cfg.AppName, cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")

	stopPoller() // stop poller before draining HTTP so in-flight DB writes finish

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("forced shutdown: %v", err)
	}
}

func runMigrations(cfg config.Config) error {
	// golang-migrate expects a file:// URL for the source and a pgx5:// URL for the DB.
	src := "file://" + cfg.MigrationsPath

	// golang-migrate's pgx/v5 driver uses "pgx5://" as the scheme.
	dbURL := "pgx5://" + stripScheme(cfg.DatabaseURL)

	m, err := migrate.New(src, dbURL)
	if err != nil {
		return fmt.Errorf("new: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("up: %w", err)
	}
	return nil
}

// stripScheme removes a leading "postgres://" or "postgresql://" so we can
// replace it with "pgx5://" for golang-migrate's pgx/v5 driver.
func stripScheme(url string) string {
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if len(url) > len(prefix) && url[:len(prefix)] == prefix {
			return url[len(prefix):]
		}
	}
	return url
}
