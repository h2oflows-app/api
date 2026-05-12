# h2oflows-app/api

Go backend for the H2OFlows streamflow platform. Chi router, PostgreSQL + PostGIS + pgvector, gauge polling, AI seeding, and the reach registry.

See [ARCHITECTURE.md](ARCHITECTURE.md) for full technical design and [ROADMAP.md](ROADMAP.md) for the product plan.

---

## Stack

| Layer | Choice |
|---|---|
| Language | Go 1.25 |
| Router | Chi |
| Database | PostgreSQL 16 + PostGIS + pgvector |
| Migrations | golang-migrate |
| AI | Anthropic Claude (anthropic-sdk-go) |
| Embeddings | Voyage AI `voyage-3` (pgvector) |
| Auth | Supabase (JWT verification) |

---

## Running locally

### Prerequisites

- Go 1.25+ (`/usr/local/go/bin/go version`)
- PostgreSQL 16 + PostGIS (`sudo apt install postgresql-16-postgis-3`)

### Setup

```sh
# 1. Create the database
sudo -u postgres createdb h2oflow

# 2. Copy and fill in env vars
cp .env.example .env
# Required: DATABASE_URL, ANTHROPIC_API_KEY, VOYAGE_API_KEY

# 3. Start the server (auto-runs migrations on startup)
set -a && source .env && set +a
go run ./cmd/server
```

API runs on `:8080`.

---

## Commands

```sh
# Seed Front Range CO reaches + AI-generated content
go run ./cmd/seed-reaches

# Seed broader CO + surrounding states
go run ./cmd/seed-state-reaches

# Embed reach content chunks into pgvector
go run ./cmd/embed-reaches

# Bulk import USGS gauges by state
go run ./cmd/seed-usgs-states

# Seed flow bands for gauge+reach pairs
go run ./cmd/seed-flow-ranges

# Import reach data from a KMZ file
go run ./cmd/import-kml -file /path/to/export.kmz

# Backfill NHD ComIDs on existing reaches
go run ./cmd/backfill-comids
```

---

## Repo layout

```
cmd/
  server/               Chi router, migrations on startup, poller
  seed-reaches/         Front Range CO reaches + AI content
  seed-state-reaches/   Broader reach inventory
  embed-reaches/        pgvector embedding for RAG
  seed-flow-ranges/     Flow bands for gauge+reach pairs
  seed-usgs-states/     Bulk USGS gauge import
  import-kml/           KMZ/KML reach importer
  backfill-comids/      NHD ComID backfill
internal/
  ai/                   Claude + Voyage AI (RAG, reach seeder, search enrichment)
  alerts/               Flow threshold alert evaluation
  auth/                 Supabase JWT middleware
  config/               Environment config
  db/                   Database connection + helpers
  elevation/            Elevation profile lookups
  gaugecore/            Gauge adapter interface + USGS/DWR/HUC implementations
  handlers/             HTTP route handlers
  kmlimport/            KMZ/KML importer + OSM/NLDI centerline sync
  models/               Shared DB model types
  nldi/                 USGS NLDI API client (snap, navigate, mainstem merge)
  osm/                  Overpass API client + reach centerline fetch
  poller/               Gauge polling scheduler (trusted/demand/cold tiers)
migrations/             golang-migrate SQL files (numbered, never edit old ones)
scripts/
  pull-prod-db.sh       Pull production DB to local
infra/
  postgres/             Postgres Docker config
  traefik/              Traefik reverse proxy config
  Caddyfile             Caddy config
```

---

## Gauge adapters

All gauge sources implement a common interface in `internal/gaugecore`:

```go
type GaugeReading struct {
    ExternalID string
    CFS        *float64
    FlowStatus string    // runnable | caution | low | flood | unknown
    ReadingAt  time.Time
}
```

Current adapters: `usgs.go` (USGS NWIS IV), `dwr.go` (Colorado DWR CDSS), `huc.go` (USGS HUC watershed lookup).

Adding a new gauge source = one file implementing the adapter interface.

---

## Migrations

Sequential numbered files in `migrations/`. Never edit old ones — always add a new file.

```sh
# Migrations run automatically on server startup.
# To run manually:
go run ./cmd/server  # or use golang-migrate CLI directly
```

---

## Environment variables

| Var | Required | Description |
|---|---|---|
| `DATABASE_URL` | yes | `postgres://user:pass@host/db?sslmode=disable` |
| `ANTHROPIC_API_KEY` | yes | Claude API key |
| `VOYAGE_API_KEY` | yes | Voyage AI embeddings key |
| `APP_PORT` | no | HTTP port (default `8080`) |
| `SUPABASE_JWT_SECRET` | yes | Supabase JWT secret for auth middleware |
