#!/usr/bin/env bash
# reattach-staging-identity.sh — Run ON the EC2 host. Re-points a seeded prod
# identity at the staging Supabase user for the same person, so their handle,
# dashboards, watchlists, runs and everything else survive a re-seed.
#
#   cd /home/ubuntu/h2oflows && ./scripts/reattach-staging-identity.sh <email> <staging-uuid>
#
# WHY THIS EXISTS
#
# Staging runs its own Supabase project, so it has a completely separate UUID
# space from prod. seed-staging-db.sh restores the prod dump, which brings
# user_profiles rows keyed by PROD Supabase UUIDs — accounts that cannot exist
# in staging's auth. They are orphans: nobody can log in as them.
#
# Signing into staging therefore mints a BRAND NEW uuid with no profile, which
# is why you are asked to claim a handle, and why the seeded work (51 watchlist
# entries, four dashboards, calendar runs) appears to belong to someone else.
# The `-stg` handle suffix was only ever a workaround so your preferred name
# was not already taken by the orphan — it treated the symptom.
#
# This script treats the cause: it rewrites every column carrying an auth uuid
# from the prod value to your staging value, then drops the suffix. One
# identity, one handle, everything attached.
#
# Find your staging uuid in the staging Supabase dashboard (Authentication →
# Users), or read it out of the profile you were forced to create:
#   SELECT owner_id, handle FROM user_profiles ORDER BY created_at DESC LIMIT 5;
set -euo pipefail

EMAIL="${1:-}"
NEW_UUID="${2:-}"
if [[ -z "$EMAIL" || -z "$NEW_UUID" ]]; then
  echo "usage: $0 <email> <staging-supabase-uuid>" >&2
  exit 2
fi

DB=h2oflows
STG_DC="docker compose -f docker-compose.staging.yml --env-file .env.staging"

# Guard: this rewrites ownership across ~24 tables. Running it against prod
# would reassign real users' data, so refuse unless the target really is the
# staging container.
if ! $STG_DC ps --status running postgres-stg 2>/dev/null | grep -q postgres-stg; then
  echo "staging postgres is not running — refusing (is this the right host?)" >&2
  exit 1
fi

echo "▶ Re-attaching $EMAIL → $NEW_UUID"

$STG_DC exec -T postgres-stg psql -U "$DB" -d "$DB" \
  -v ON_ERROR_STOP=1 -v email="$EMAIL" -v new_uuid="$NEW_UUID" <<'SQL'
BEGIN;

-- psql variables cannot be interpolated inside dollar-quoted bodies, so the
-- arguments are staged in a temp table the DO blocks below can read.
CREATE TEMP TABLE _p ON COMMIT DROP AS
  SELECT :'email'::text AS email, :'new_uuid'::text AS new_uuid;

-- Resolve the seeded (prod) uuid from the dump's own email mapping. Keying on
-- email rather than uuid is what makes this survive re-seeds: prod uuids are
-- stable, so the same map keeps working after every refresh.
DO $$
DECLARE old_uuid text; new_uuid text; n int;
BEGIN
  SELECT p.new_uuid INTO new_uuid FROM _p p;

  SELECT ue.owner_id INTO old_uuid
    FROM user_emails ue JOIN _p p ON lower(ue.email) = lower(p.email);

  IF old_uuid IS NULL THEN
    RAISE EXCEPTION 'no seeded identity for % — known emails: %',
      (SELECT email FROM _p),
      (SELECT string_agg(email, ', ') FROM user_emails);
  END IF;
  IF old_uuid = new_uuid THEN
    RAISE EXCEPTION 'already attached to % — nothing to do', new_uuid;
  END IF;

  SELECT count(*) INTO n FROM user_profiles WHERE owner_id = old_uuid;
  IF n <> 1 THEN
    RAISE EXCEPTION 'expected exactly one seeded profile for %, found %', old_uuid, n;
  END IF;

  CREATE TEMP TABLE _ids ON COMMIT DROP AS SELECT old_uuid AS old, new_uuid AS new;
  RAISE NOTICE 'seeded % -> staging %', old_uuid, new_uuid;
END $$;

-- The four FKs onto user_profiles(owner_id) are ON UPDATE NO ACTION, so moving
-- the parent key is rejected while children still reference the old value, and
-- moving the children first is rejected because the new value does not exist
-- yet. Drop them for the duration and restore the EXACT definitions afterwards
-- (read back via pg_get_constraintdef rather than hardcoded, so this keeps
-- working if a migration adds another).
CREATE TEMP TABLE _fks ON COMMIT DROP AS
  SELECT conrelid::regclass::text AS tbl, conname::text AS name,
         pg_get_constraintdef(oid) AS def
  FROM pg_constraint
  WHERE contype = 'f' AND confrelid = 'user_profiles'::regclass;

DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT * FROM _fks LOOP
    EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', r.tbl, r.name);
  END LOOP;
END $$;

-- Discard the throwaway account created by the forced handle claim, then move
-- the seeded identity onto its uuid.
--
-- Both loops enumerate ownership columns from information_schema rather than a
-- hardcoded list. A fixed list is exactly the kind of thing that silently goes
-- stale when a migration adds a table — the same drift class as api#189 — and
-- here going stale would mean rows quietly left behind on a dead uuid.
DO $$
DECLARE r record; ids record; n int; total int := 0;
BEGIN
  SELECT * INTO ids FROM _ids;

  FOR r IN
    SELECT c.table_name AS tbl, c.column_name AS col
    FROM information_schema.columns c
    JOIN information_schema.tables t
      ON t.table_schema = c.table_schema AND t.table_name = c.table_name
     AND t.table_type = 'BASE TABLE'
    WHERE c.table_schema = 'public'
      AND (c.column_name LIKE '%owner_id%' OR c.column_name LIKE '%user_id%')
      -- handle, not a uuid
      AND c.column_name <> 'original_author_handle'
    ORDER BY (c.table_name = 'user_profiles')   -- profile row last
  LOOP
    EXECUTE format('DELETE FROM public.%I WHERE %I::text = $1', r.tbl, r.col)
      USING ids.new;
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n > 0 THEN
      total := total + n;
      RAISE NOTICE 'discarded % row(s) from %.%', n, r.tbl, r.col;
    END IF;
  END LOOP;
  RAISE NOTICE 'discarded % row(s) belonging to the throwaway account', total;

  FOR r IN
    SELECT c.table_name AS tbl, c.column_name AS col, c.data_type AS typ
    FROM information_schema.columns c
    JOIN information_schema.tables t
      ON t.table_schema = c.table_schema AND t.table_name = c.table_name
     AND t.table_type = 'BASE TABLE'
    WHERE c.table_schema = 'public'
      AND (c.column_name LIKE '%owner_id%' OR c.column_name LIKE '%user_id%')
      AND c.column_name <> 'original_author_handle'
    ORDER BY (c.table_name <> 'user_profiles')  -- profile row first
  LOOP
    -- Three of these columns are uuid rather than text; cast on the way in so
    -- one loop covers both.
    EXECUTE format('UPDATE public.%I SET %I = $1::%s WHERE %I::text = $2',
                   r.tbl, r.col, CASE WHEN r.typ = 'uuid' THEN 'uuid' ELSE 'text' END, r.col)
      USING ids.new, ids.old;
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n > 0 THEN
      RAISE NOTICE 'moved % row(s) in %.%', n, r.tbl, r.col;
    END IF;
  END LOOP;
END $$;

DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT * FROM _fks LOOP
    EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s', r.tbl, r.name, r.def);
  END LOOP;
END $$;

-- Drop the suffix seed-staging-db.sh applied. Only meaningful when reattaching
-- an already-seeded database; when called from the seed script this runs BEFORE
-- suffixing, so it is a no-op. Note the suffixing truncates to 26 chars, so a
-- handle longer than that cannot be restored exactly — it never has been here.
UPDATE user_profiles up
   SET handle = regexp_replace(up.handle, '-stg$', '')
  FROM _ids i
 WHERE up.owner_id = i.new AND up.handle ~ '-stg$';

DO $$
DECLARE ids record; h text;
BEGIN
  SELECT * INTO ids FROM _ids;
  SELECT handle INTO h FROM user_profiles WHERE owner_id = ids.new;
  IF h IS NULL THEN
    RAISE EXCEPTION 'no profile on % after remap — refusing to commit', ids.new;
  END IF;
  IF EXISTS (SELECT 1 FROM user_profiles WHERE owner_id = ids.old) THEN
    RAISE EXCEPTION 'seeded profile % still present after remap — refusing', ids.old;
  END IF;
  RAISE NOTICE 'done: % now owns the seeded data', h;
END $$;

COMMIT;
SQL

echo "✓ Re-attached. Log into staging as $EMAIL — no handle claim, data intact."
