# h2oflows-app/api — Claude Code Guide

## Project docs

- [ARCHITECTURE.md](ARCHITECTURE.md) — tech stack, data model, guiding principles
- [ROADMAP.md](ROADMAP.md) — product phases and backlog

## Repo layout

```
cmd/
  server/           main entrypoint + Chi router + migrations + poller
  seed-flow-ranges/
  seed-usgs-states/
  backfill-river-gnis/
  test-ask/         local harness for the /ask river assistant
internal/
  ai/               Claude long-context river assistant + search enrichment
  alerts/           Flow threshold alert evaluation
  auth/             Supabase JWT middleware
  config/           Environment config
  db/               Database connection + helpers
  elevation/        Elevation profile lookups
  gaugecore/        Gauge adapter interface + USGS/DWR/HUC implementations
  handlers/         HTTP route handlers
  kmlimport/        KMZ/KML parser + NLDI centerline fetch (user run import)
  models/           Shared DB model types
  nldi/             USGS NLDI API client
  osm/              Overpass API client + reach centerline fetch
  poller/           Gauge polling scheduler (trusted/demand/cold tiers)
migrations/         golang-migrate SQL files (numbered, never edit old ones)
scripts/
  pull-prod-db.sh
infra/
  postgres/
  traefik/
  Caddyfile

.claude/memory/     persistent AI memory (gitignored, local only)
```

## Build

```sh
/usr/local/go/bin/go build ./...
/usr/local/go/bin/go run ./cmd/server
```

## Stack notes

- **pgx v5**: returns `text` columns as `[]byte` when scanned into `[]byte` — never add `::json` cast to `ST_AsGeoJSON()` output or pgx will try to base64-decode it
- **PostGIS**: use `ST_GeomFromGeoJSON($1)::geography` to store GeoJSON — `ST_GeogFromGeoJSON` does not exist in this version
- **gaugecore**: lives at `internal/gaugecore` (was `packages/gauge-core` in the monorepo); import path `github.com/h2oflow/h2oflow/apps/api/internal/gaugecore`

## Environment

```
DATABASE_URL=postgres://h2oflow:h2oflow@localhost:5432/h2oflow?sslmode=disable
APP_PORT=8080
```

## Conventions

- Migrations: sequential numbered files (`000083_*.up.sql` / `*.down.sql`), never edit existing ones
- Reach slugs are the canonical reach identifier across the codebase
- Flow difficulty stored as floats (`3.5`), rendered as Roman numerals (`III+`)
- Colorado rivers flow west→east, so `MIN(lng)` = most upstream, `MAX(lng)` = most downstream
- Go param type inference: use `COALESCE($N::numeric, col)` not `CASE WHEN` for nullable params
