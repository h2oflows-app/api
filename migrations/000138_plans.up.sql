-- #246: Trip Calendar — enums + plans (personal/festival/race/cruise trip
-- containers; one calendar entity, dates + optional crew).

CREATE TYPE plan_type AS ENUM ('personal', 'festival', 'race', 'cruise');
CREATE TYPE plan_visibility AS ENUM ('public', 'private');
CREATE TYPE plan_member_origin AS ENUM ('invite', 'request');
CREATE TYPE plan_member_status AS ENUM ('invited', 'requested', 'accepted', 'declined');

CREATE TABLE plans (
  id                UUID            PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id          TEXT            NOT NULL REFERENCES user_profiles(owner_id) ON DELETE CASCADE,
  slug              TEXT            NOT NULL,
  name              TEXT            NOT NULL,
  type              plan_type       NOT NULL DEFAULT 'personal',
  visibility        plan_visibility NOT NULL DEFAULT 'public',
  start_date        DATE            NOT NULL,
  end_date          DATE            NOT NULL,
  location          TEXT,
  looking_for_crew  BOOLEAN         NOT NULL DEFAULT FALSE,
  max_crew          INT,
  cover_photo_id    UUID, -- stub, no FK — photos deferred
  created_at        TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
  updated_at        TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
  deleted_at        TIMESTAMPTZ, -- tombstone
  UNIQUE (owner_id, slug),
  CHECK (end_date >= start_date),
  CHECK (max_crew IS NULL OR max_crew > 0),
  CHECK (NOT looking_for_crew OR max_crew IS NOT NULL)
);

CREATE INDEX plans_owner_dates_idx ON plans (owner_id, start_date, end_date)
  WHERE deleted_at IS NULL;

CREATE INDEX plans_discover_idx ON plans (start_date)
  WHERE visibility = 'public' AND looking_for_crew AND deleted_at IS NULL;
