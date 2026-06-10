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

echo "▶ Recreating staging database..."
$STG_DC exec -T postgres-stg psql -U "$DB" -d postgres -c "DROP DATABASE IF EXISTS $DB;"
$STG_DC exec -T postgres-stg psql -U "$DB" -d postgres -c "CREATE DATABASE $DB;"

echo "▶ Restoring into staging..."
cat "$DUMP" | $STG_DC exec -T postgres-stg pg_restore -U "$DB" -d "$DB" --no-owner --no-privileges 2>&1 \
  | grep "^pg_restore: error" || true

rm "$DUMP"
echo "✓ Staging DB seeded from prod. Restart api-stg to re-run migrations:"
echo "  $STG_DC up -d api-stg"
