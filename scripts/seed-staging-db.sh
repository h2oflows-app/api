#!/usr/bin/env bash
# seed-staging-db.sh — Run ON the EC2 host. Copies the prod database into the
# staging Postgres container. Re-run whenever you want fresh prod data in staging.
#
#   cd /home/ubuntu/h2oflows && ./scripts/seed-staging-db.sh
set -euo pipefail

PROD_DC="docker compose -f docker-compose.prod.yml"
# --env-file .env.staging is required: docker compose otherwise interpolates
# ${...} from the default .env (the PROD file), pulling prod creds/JWKS into staging.
STG_DC="docker compose -f docker-compose.staging.yml --env-file .env.staging"
DB=h2oflows
DUMP="/tmp/h2oflows-stg-seed-$(date +%Y%m%d-%H%M%S).dump"

echo "▶ Ensuring staging Postgres is up..."
$STG_DC up -d postgres-stg
# wait for healthy
for i in $(seq 1 30); do
  if $STG_DC exec -T postgres-stg pg_isready -U "$DB" >/dev/null 2>&1; then break; fi
  sleep 2
done

echo "▶ Dumping prod database..."
$PROD_DC exec -T postgres pg_dump -U "$DB" -Fc "$DB" > "$DUMP"
echo "  dump: $DUMP ($(du -sh "$DUMP" | cut -f1))"

# api-stg holds an open connection to the staging DB — stop it so DROP DATABASE
# isn't blocked ("database is being accessed by other users").
echo "▶ Stopping api-stg (releases DB connections)..."
$STG_DC stop api-stg 2>/dev/null || true

echo "▶ Recreating staging database..."
# WITH (FORCE) terminates any stray backend (Postgres 13+) as a backstop.
$STG_DC exec -T postgres-stg psql -U "$DB" -d postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE);"
$STG_DC exec -T postgres-stg psql -U "$DB" -d postgres -c "CREATE DATABASE $DB;"

echo "▶ Restoring into staging..."
cat "$DUMP" | $STG_DC exec -T postgres-stg pg_restore -U "$DB" -d "$DB" --no-owner --no-privileges 2>&1 \
  | grep "^pg_restore: error" || true

rm "$DUMP"

# Free up seeded handles for QA signups. Seeded user_profiles are orphans in
# staging — keyed by PROD Supabase UUIDs that don't exist in the staging
# Supabase project (separate UUID space). Suffixing their handles means a QA
# user can claim any name without colliding with seed data. left(handle,26)
# keeps the result under the 30-char handle CHECK constraint.
echo "▶ Freeing seeded handles (append -stg)..."
$STG_DC exec -T postgres-stg psql -U "$DB" -d "$DB" -c \
  "UPDATE user_profiles SET handle = left(handle, 26) || '-stg' WHERE handle !~ '-stg\$';"

echo "▶ Restarting api-stg (re-runs migrations on seeded DB)..."
$STG_DC up -d api-stg

echo "✓ Staging DB seeded from prod and api-stg restarted."
