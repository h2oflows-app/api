# H2OFlows Roadmap

Current state as of June 2026. Phase 1 (gauge dashboard + reach pages + AI assistant) is complete. Backend routes exist for trip reports, trips, contributions, and proximity events — the frontend for those is stub. Everything below is unbuilt or incomplete.

**Status snapshot (2026-06-03):**
- ✅ Phase 2 (2.1–2.6) — shipped 2026-05-06
- ✅ Phase 2b (2b.1–2b.7) — shipped 2026-05-07
- ✅ Repository restructure — completed 2026-05-13; live at `h2oflows-app/{api,web,docs}`
- ✅ Phase 2c — Pilot polish — shipped 2026-05-17 (v0.2.1–v0.2.17)
- ✅ Demo Pack (0.3.0) — shipped 2026-05-18, tagged v0.3.3
- ✅ Pre-pilot polish (PR #35) — merged 2026-05-18; all web GH issues closed except #16 (active, see 2c.4)
- ✅ **Phase 2d — Post-pre-pilot iteration (web#48 + api#11) — merged 2026-05-20.** Reach author parity (admin = user flow), per-dashboard prefs, gauge-map toggle, basin loading banner, user reach difficulty fields, reach-only watchlist path, theme-aware user reach line color.
- ✅ **Dashboard UX polish — web#67/#68 — merged 2026-05-21.** Full view mode, sparkline full-width in comfortable/full, 12h/24h selector top-left, card consistency, mobile hides full-mode button.
- ✅ **[web#16] 2c.4 PRs A/B/C — 30d retention, poll status UI, admin gauges page — shipped 2026-05-22** (api#14–19, web#69–71)
- ✅ **Umbrella A: UGC shift — merged 2026-05-23, tagged v0.4.0.** Runs rename, author model, fork, reports on user runs, features + KML import, community flow proposals + voting, clustering + dupe prevention, upvotes, anon read-only at `/runs/u/:id`, moderation primitives (abuse flags + admin queue).
- ✅ **Umbrella G: UX Polish batch — merged 2026-05-31, tagged v0.6.0.** Dashboard filter removal, explore sidebar cleanup, forked-from inline, self-upvote guard, run editor polish (DashboardMembershipPicker, ask/override removal, KML move, similar-runs @handle), /my/runs table, gaugeSourceUrl util, admin Last CFS column, /me/gauges + refresh endpoint, /my/gauges page, /users/{handle}/runs/map/all, explore Browse User mode, NLDI tributary overlay, adaptive poll intervals.
- ✅ **Umbrella I: All-user ownership + handle URLs — merged 2026-06-03.** Fork-on-add + handle claim, /runs/{handle}/{slug} routing, explore 3-tab redesign, dashboard card refactor, /my/runs retired, owner toolbar, curated concept dropped. Migration 106 (watchlist fork) + 107 (long_name). All runs user-owned; h2oflows curator handle for official content.
- ✅ **long_name on user_reaches — merged 2026-06-03** (api#68, web#159). Migration 107 adds nullable `long_name` column; backfills forked runs from curated reach name. Editor exposes Short name + Full name fields. Dashboard shows full name as subtitle.
- 🔨 **Umbrella N: Channels — IN PROGRESS (target v0.7.0).** Every user is a channel; `/explore/{handle}` becomes a public profile map. `visibility` collapses to binary public/private whose only meaning is "shows on your profile" (`unlisted` + direct-link sharing dropped). Forks hidden from public map. "My Runs" management page returns under the avatar menu with dashboard-membership + library toggles. Add Run / Report relocated off the AppHeader onto the dashboard + explore pages. Upvoting surfaced inline with counts. Custom-gauge channel layer + AW-as-user deferred to their own items. Full plan in the "Umbrella N — Channels" section below.
- ⏳ **LLM audit (PR 9 deferred)** — nightly Haiku scan of new UGC, auto-flags outliers into abuse queue. Add when UGC volume warrants it. See "Post-v0.4 backlog" below.
- ⏳ Pilot rollout (0.x) — on hold per user
- ⏳ Phase 3 — SEO infra (build behind noindex wall, flip robots.txt at 1.0 launch)
- ⏳ Phase 4 — Public API + PATs + data export (expanded 2026-05-18; depends on pilot signal)
- ⏳ Umbrella B — API v1 route split (`/public` + `/me` + `/internal`); target v0.5.0 post-UGC
- ⏳ Phase 5 — American Whitewater interop (`reach-ingest-v1` schema drafted 2026-05-18)
- ⏳ Phase 6+ — deferred

---

## Umbrella N — Channels (target v0.7.0) 🔨 IN PROGRESS

*Every user is a channel. `/explore/{handle}` becomes that user's public profile map; h2oflows and (later) American Whitewater are simply channels among many.*

**Premise.** Umbrella I already made every run user-owned, added handle URLs (`/runs/{handle}/{slug}`), and demoted h2oflows to a curator handle. PR #275 made `/explore/[[handle]].vue` route-driven — `/explore` = my runs, `/explore/{handle}` = browse a user. Channels completes that arc: a user's `/explore/{handle}` is their published profile, discovery is **per-channel** (you browse one profile at a time), and the global "all runs" map is intentionally absent.

**Locked decisions (2026-06-22):**

1. **Channel model (option a).** Discovery is browsing channels one at a time. No global cross-user browse surface. Logged-out already redirects to `/explore/h2oflows`, consistent with this.
2. **`visibility` collapses to binary public / private.** Its *only* meaning is "does this run appear on your `/explore/{handle}` profile." New runs default **public**. `unlisted` is dropped from the UX; direct-link-without-listing sharing is dropped (deemed pointless). Private = owner-only, off-profile.
3. **Forks hidden from the public channel map.** Originals (both `forked_from_*` columns null) appear on a profile; forks appear only in the owner's dashboard + My Runs management.
4. **"My Runs" management page returns** under the avatar menu (the list view was retired in Umbrella I; only `/my/runs/{slug}` detail/edit survived). Grouped like the explore sidebar, with **two toggles**: per-run dashboard membership on/off, and view-whole-authored-library vs dashboard-only.
5. **Custom-gauge channel layer is DEFERRED** to its own GH issue (see N.deferred). Not in channels-v1.
6. **AW-as-user / PATs stay Phase 4 + Phase 5.** Channels is the *prerequisite* (AW becomes a channel once PATs land), but token infrastructure is greenfield and not bundled here.

### Grounding (verified 2026-06-22)

- Explore is already channel-shaped: `web/app/pages/explore/[[handle]].vue`, optional `handle` param, my-runs vs browse-user modes.
- Public map endpoint exists and already filters privacy: `api/internal/handlers/users.go:115` `MapAllByHandle` → `WHERE ur.owner_id = $1 AND ur.visibility = 'public' AND ur.deleted_at IS NULL`. **It does not yet filter forks.**
- `user_reaches.visibility` is `ENUM('private','unlisted','public')`, DB default **`'private'`** (migration `000112`). Backfill set existing non-private rows to public. New-run creation must therefore set `public` **explicitly** — do not rely on the column default, and do not migrate-drop the column.
- Fork origin tracked: `forked_from_reach_id` / `forked_from_user_reach_id` (migration `000093`), mutually exclusive; attribution snapshot in `000101`.
- Reports survived runs-unify healthy: `reports` now FKs `user_reach_id` (migration `000094`), writes target `user_reach_id` only, reads filter `visibility='public'`. Relocating the Report button is safe.
- Upvotes shipped in Umbrella A with a self-upvote guard.

### Items

| # | Item | Repo | Size |
|---|------|------|------|
| N.1 | **Visibility simplification.** Creation defaults `public` (set explicitly in the create path; verify `WizardEntryModal` + create handler). Replace the visibility picker with a single "Make private" toggle, off by default. Stop writing `unlisted`. One-off: count existing `unlisted` rows in prod and migrate → `public` (verify count first; they were meant to be visible). Query layer stays as-is — `MapAllByHandle` + reports already filter `visibility='public'`, so private stays hidden. **Never auto-flip existing `private` rows.** | api + web | S |
| N.2 | **Hide forks from public channel map.** Add `AND forked_from_reach_id IS NULL AND forked_from_user_reach_id IS NULL` to `MapAllByHandle` (users.go:165). Audit any other public-channel endpoint for the same filter. Originals on profile; forks only in dashboard / My Runs. | api | S |
| N.3 | **Channel header chrome.** Browse mode of `/explore/[[handle]].vue` gains a profile header — handle, avatar, run count, river count — so it reads as a profile, not a bare map. Sort-by-upvotes on the channel sidebar. Own-`/explore` keeps current my-runs behavior. | web | S |
| N.4 | **"My Runs" management page.** New `/my/runs` list view + avatar-menu entry. Grouped like the explore sidebar. Two toggles: per-run dashboard membership, and authored-library vs dashboard-only. Reuse `DashboardMembershipPicker`. | web (+api if a list endpoint is missing) | M |
| N.5 | **Relocate Add Run + Report.** Remove from `AppHeader` (desktop `60-69` Add Run / `43-57` Report; mobile `209-218` / `246-260`). Add subtle buttons to the dashboard page and explore page. Report still routes `/reports/new`. | web | S |
| N.6 | **Easier upvoting.** Surface upvote in explore sidebar rows + map popup as a one-tap optimistic toggle. **Show the upvote count next to the thumbs-up icon** everywhere it renders. Sort-by-upvotes ties into N.3. Verify upvote endpoint at build time. | web (+verify api) | S |

### Deferred (own items)

- **N.deferred — Custom-gauge channel layer.** Showcase a channel's custom gauges on the profile map and let visitors fork them. Render each public custom gauge as its **own pin** (calc icon at the centroid of its input gauges), tap → popover with name, computed CFS, formula (`A + B − C`), faint lines to input gauges, and a "Fork to my gauges" button reusing the existing payload export/import (`POST /me/custom-gauges`). *Not* the color-matched-input-gauges rendering — a real gauge can feed many custom gauges (`custom_gauge_inputs`), so one-pin-one-color collapses. Requires a new `is_public`/visibility column on `custom_gauges` (currently private-by-design — roadmap 2.3) and a moderation thought for user-authored gauge names. Tracked as **web#276**; revisit after channels-v1.
- **AW-as-user / bulk upload.** Phase 4 (PATs) + Phase 5 (AW interop, `reach-ingest-v1`). Once PATs land, AW is a channel with a service-account token — zero rework on the channel model.

### PR sequencing

1. **api PR** — N.2 fork filter + N.1 server-side `public` default. Small, independent, lands first so profile correctness is right before the UX builds on it.
2. **web PR** — N.5 button relocation. No dependencies; declutters the header early.
3. **web PR** — N.1 wizard/editor visibility simplification (single Make-private toggle).
4. **web PR** — N.3 channel header + N.6 upvote surfacing/sort (cohesive; share the channel sidebar work).
5. **web PR** — N.4 My Runs management page (largest; lands last).
6. ~~**Ops task** — count + migrate prod `unlisted` rows → `public`~~ — **not needed**. Prod count 2026-06-22: 0 unlisted rows (169 public, 3 private).

No schema migration for v1 (the `visibility` and fork columns already exist) and no data backfill — prod has zero `unlisted` rows.

### Verification

- Logged-out `/explore` → redirects to `/explore/h2oflows`; that profile shows h2oflows runs only, no forks, no private rows.
- Create a run → defaults public, appears on own `/explore/{handle}`. Toggle private → drops off the public profile, still visible to owner.
- Fork another user's run → appears in own dashboard + My Runs, **not** on own public profile.
- My Runs page lists authored library; dashboard-membership toggle adds/removes from a dashboard; library/dashboard-only toggle filters the list.
- Add Run / Report absent from AppHeader; present (subtle) on dashboard + explore.
- Upvote a run from the explore sidebar → count increments next to the thumbs-up, optimistic, persists on reload; sort-by-upvotes reorders the channel.

---

## Phase 2 — Pilot polish + personal data layer ✅ shipped 2026-05-06

*Attractive interface + pilot onboarding. Private user reaches and custom gauges before any community/social features.*

### 2.1 — Flow band simplification

Three bands replace the existing five. Fixed names and colors — users only set CFS threshold values. `craft_type` column dropped from `flow_ranges` (kayak/raft/sup distinction not used in pilot).

| Band | Color | Stored values |
|---|---|---|
| `low` | red | `max_value` only |
| `running` | green | `min_value` + `max_value` |
| `high` | blue | `min_value` only |

**Migration from existing 5-tier schema** (`below_recommended` / `low_runnable` / `runnable` / `high_runnable` / `above_recommended`):

- `low.max` = `below_recommended.max`
- `running.min` = `COALESCE(low_runnable.min, runnable.min, high_runnable.min)` — lowest available bottom across the runnable tiers
- `running.max` = `COALESCE(above_recommended.min, high_runnable.max, runnable.max)` — highest available top
- `high.min` = same value as `running.max` (boundary mirrors)
- `low_runnable`, `runnable`, `high_runnable` collapse into the single `running` band — all three runnable tiers fold together

**Coloring rule (web):**
- reading ≤ `running.min` → red (low)
- reading ≥ `running.max` → blue (high)
- else → green (running)

`low.max` and `high.min` are persisted for future visual gradient or admin reference; primary classification uses `running.min` / `running.max`.

**Migration 000068 highlights:**
- Drop `craft_type` column from `flow_ranges`; replace `(reach_id, label, craft_type)` unique constraint with `(reach_id, label)`
- Aggregate per-reach into 3 rows; preserve `data_source` (default `manual`) and `verified` flag
- Replace CHECK constraint: `label IN ('low','running','high')`
- Temporary `legacy_band_data JSONB` column retained on modified rows during migration window for rollback. Dropped after Phase 2 ships.

**UI sweep:** admin reach form, gauge modal, flow badges, ReachMap pins, GaugeCard, Sparkline, graph thresholds.

---

### 2.2 — Admin reach workflow

**Rivers tab restructure:**
- Group: state → basin → river → reach
- Pagination 10 / 50 / 100, default 50
- "Needs review" sub-section at top: rivers with `verified = false` (auto-created from user reach saves in 2.4)

**New reach flow (progressive, admin mode):**

1. Click "New reach" → enter pick-anchor mode immediately, no toggle required
2. Helper: "Find the start point for your river or creek. Tap the river as close to the start point as possible."
3. Anchor selected → "Pick another point" and "Clear" buttons appear. Re-pick replaces anchor; clear resets map.
4. Helper updates: "Tap the river as close to the put-in (starting point) as possible. Try satellite view to find the boat ramp."
5. Take-out selected → auto-trim and preview centerline immediately. No "Save flowlines" button.
6. Auto GNIS lookup → display "Looks like Trout Creek"
7. Full admin form: slug, common name, class, description, multi-day, permit, flow band thresholds, gauge
8. Click "Save reach" → GNIS confirm prompt ("Trout Creek, basin: South Platte, state: CO?" with manual override) → river auto-created with `verified = false` if no GNIS match → redirect to reach detail page
9. If gauge is new to system → warn "This gauge was just added. Polling starts within ~15 minutes."

User reach flow (2.4) reuses map steps 1–6, then a slim form.

---

### 2.3 — Custom gauges

A custom gauge is a named sum or difference of real gauges. Produces a CFS reading. Private to owner. Stored in its own table — distinct from admin singular `gauges`, which remain a separate concept.

**Operations:** `+` and `-` only. No multiply, divide, parens, or constants — additive/subtractive watershed flow modeling only.

**Standalone:** can exist without a reach. Dashboard card shows computed CFS + custom-gauge icon (calc icon), no sparkline. Clicking opens a modal with a stacked graph of all contributing real gauges. Single-input custom gauges allowed (acts as a labeled passthrough; modal shows one trace).

**Colorization:** raw CFS only on standalone card — no band color without a reach. When a custom gauge backs a user reach, the reach's flow band thresholds determine card color.

**Data model (migration 000070):**

```sql
CREATE TABLE custom_gauges (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id        TEXT NOT NULL,
  slug            TEXT NOT NULL,
  name            TEXT NOT NULL,
  description     TEXT,
  note            TEXT,
  unit            TEXT NOT NULL DEFAULT 'cfs',
  last_value_cfs  NUMERIC,
  last_value_at   TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (owner_id, slug)
);
CREATE INDEX custom_gauges_owner_idx ON custom_gauges (owner_id);

CREATE TABLE custom_gauge_inputs (
  custom_gauge_id  UUID NOT NULL REFERENCES custom_gauges(id) ON DELETE CASCADE,
  position         SMALLINT NOT NULL,
  gauge_id         UUID NOT NULL REFERENCES gauges(id) ON DELETE RESTRICT,
  sign             SMALLINT NOT NULL CHECK (sign IN (-1, 1)),
  PRIMARY KEY (custom_gauge_id, position)
);
CREATE INDEX custom_gauge_inputs_gauge_idx ON custom_gauge_inputs (gauge_id);
```

`owner_id` is `TEXT` to match Supabase auth user IDs — same pattern as `user_roles` and `user_watchlists`. No `users` table in DB.

`gauge_id` uses `ON DELETE RESTRICT`: prevents accidental loss of a custom gauge's input when an admin deletes the underlying gauge — admin must explicitly migrate or break the formula first.

No `public` flag. No subscriber tracking. No flow ranges on custom gauges — bands belong to reaches.

**Slug uniqueness:** scoped per owner (`owner_id, slug`). Form checks availability before save and blocks on collision.

**Delete:** hard delete, cascades inputs. Blocked if a user reach currently uses this gauge — form warns "Reach `xyz` uses this gauge. Reassign reach gauge or delete the reach first."

**Polling integration:**

Current poller (mig 000065) polls only gauges subscribed via `gauge_reach_associations`. Custom gauge inputs and user-reach gauges must also drive polling.

Migration 000072 — replace polling source with a union view:

```sql
CREATE OR REPLACE VIEW polled_gauge_ids AS
  SELECT DISTINCT gauge_id FROM gauge_reach_associations
  UNION
  SELECT DISTINCT gauge_id FROM custom_gauge_inputs
  UNION
  SELECT DISTINCT primary_gauge_id AS gauge_id
    FROM user_reaches WHERE primary_gauge_id IS NOT NULL;
```

Poller selects from `polled_gauge_ids` instead of `gauge_reach_associations` directly. Adding a custom gauge auto-enrolls its inputs. Cascade delete on inputs auto-de-enrolls gauges no longer needed by anyone.

**Custom gauge value computation:**

After each poll cycle, a worker pass recomputes every custom gauge:

- `value = SUM(latest_reading × sign)` over inputs
- writes `last_value_cfs` and `last_value_at` on `custom_gauges`
- if any input gauge has `poll_health` worse than `healthy` (see 2.5), the worker still computes a value but flags it stale — UI shows "depends on stale gauge: [name]"

**Formula builder UI:**
- Searchable real gauge picker (by name, river, station ID)
- Add gauges row by row with +/- toggle
- Drag handles to reorder rows
- Live preview of computed current value
- Note field (owner-visible, editable — not RAG-indexed)
- Save → owner-only, no public toggle

**API:**

```
POST   /me/custom-gauges
GET    /me/custom-gauges
GET    /me/custom-gauges/{slug}
PATCH  /me/custom-gauges/{slug}
DELETE /me/custom-gauges/{slug}
GET    /me/custom-gauges/{slug}/readings
```

All routes require auth. Slug resolved against the authenticated session's user — no `{handle}` in URL needed. Owner check enforced on every path.

**Readings computation:** on-the-fly from latest polled values of contributing gauges. Historical graph: reconstructed by joining stored `gauge_readings` over a common timestamp window across inputs.

**Watchlists extended:** migration 000074 adds `custom_gauge_id` (nullable) to `user_watchlists` so users can pin custom gauges to dashboard the same way as real gauges. CHECK enforces that exactly one of `gauge_id` / `custom_gauge_id` is set.

**Export / share via payload (no DB sharing):**

Tapping "Share" on a custom gauge generates a portable payload — a snapshot of the formula only, not a DB record. Recipient imports it as their own independent copy. Pattern is similar to Grafana's dashboard JSON export/import.

Payload format (compact JSON, base64url-encoded for URL transport):

```json
{
  "v": 1,
  "n": "Cache la Poudre Confluence Estimate",
  "d": "Optional description",
  "i": [
    {"s": 1, "g": "USGS:09058000"},
    {"s": 1, "g": "USGS:09060500"},
    {"s": -1, "g": "USGS:09057500"}
  ]
}
```

`g` = gauge external ID prefixed by source (`USGS:`, `DWR:`) — resolves across any user's account. Import fails with a clear error if a gauge isn't in the system; offers to add it from USGS/DWR before retry.

Share modal options:
- "Copy as message" — human-readable text + import link
- "Copy import link" — raw URL (`/import/gauge?d=<base64>`)
- Social intents (Twitter, SMS, Discord) — text + link

Import flow: link opens formula builder pre-filled. Slug collision prompts user to rename before save. The payload has no reference back to the original — once imported, edits diverge.

QR code sharing deferred to a later phase.

---

### 2.4 — User-defined reaches

Private reaches any authenticated user can create. Stored in a separate table from curated `reaches` so curated and user spaces never cross-contaminate by query oversight.

**Rivers stay shared.** When a user saves a reach for a river not in `rivers`, the row is auto-created with `verified = false`. Admin Rivers tab surfaces unverified rows for review. No `owner_id` on rivers — rivers are physical entities, deduped by GNIS lookup across all users.

Migration 000069 adds `verified BOOLEAN NOT NULL DEFAULT FALSE` to `rivers`. Existing curated rivers backfilled to `true`.

**Schema (migration 000071):**

```sql
CREATE TABLE user_reaches (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id          TEXT NOT NULL,
  slug              TEXT NOT NULL,
  name              TEXT NOT NULL,
  river_id          UUID REFERENCES rivers(id) ON DELETE SET NULL,
  put_in            GEOGRAPHY(POINT, 4326) NOT NULL,
  take_out          GEOGRAPHY(POINT, 4326) NOT NULL,
  centerline        GEOGRAPHY(LINESTRING, 4326),
  primary_gauge_id  UUID REFERENCES gauges(id) ON DELETE SET NULL,
  custom_gauge_id   UUID REFERENCES custom_gauges(id) ON DELETE SET NULL,
  note              TEXT,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (owner_id, slug),
  CHECK (primary_gauge_id IS NULL OR custom_gauge_id IS NULL)
);
CREATE INDEX user_reaches_owner_idx ON user_reaches (owner_id);
CREATE INDEX user_reaches_river_idx ON user_reaches (river_id);

CREATE TABLE user_reach_flow_ranges (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_reach_id  UUID NOT NULL REFERENCES user_reaches(id) ON DELETE CASCADE,
  label          TEXT NOT NULL CHECK (label IN ('low','running','high')),
  min_value      NUMERIC,
  max_value      NUMERIC,
  UNIQUE (user_reach_id, label)
);
```

A user reach uses **either** a real gauge or a custom gauge — never both. CHECK enforces it.

**Slug rules:** auto-generated from name on first save, editable. Unique per owner, not globally — two users can each own a `clear-creek-section-1`. Server validates uniqueness against `(session.owner_id, slug)` before insert.

**Slim form (user mode, post-map flow):**
- Reach name (required)
- Optional note
- Gauge selection: real gauge picker OR "My Gauges" (custom gauges)
- 3 flow band threshold values (low max, running min, running max — high min auto-mirrors running max)
- Omits: slug input (auto-generated, optional override link), common name, class definition, description, multi-day, permit

**Save flow:**
- GNIS confirm prompt same as admin flow
- River auto-created with `verified = false` if no GNIS match
- New gauge warning same as admin flow ("Polling starts within ~15 minutes.")
- Redirect to user reach detail page after save

**Reach detail page (user reach):**
- Shows computed gauge reading with flow band color, reach map, note field (editable for owner)
- "Add to dashboard" button
- 404 for non-owner (no existence leak)
- `noindex, nofollow` meta

**URLs:**
- Curated: `/reaches/{slug}` — public, indexed
- User reach: `/my/reaches/{slug}` — owner auth required, slug resolved against session owner_id; not addressable by any other user

**Delete:** hard delete. Dashboard cards referencing the reach removed silently. Associated custom gauge (if any) survives in owner's library — only the reach link is broken.

**"My Reaches" page (avatar menu):**
- Not a top-level nav tab — lives under avatar menu
- Layout: state → basin → river → reach grouping, same as admin Rivers tab
- Pagination 10 / 50 / 100, default 50
- Per-row actions: edit, delete, add to dashboard

**"My Gauges" page (avatar menu):**
- Lists owner's custom gauges
- Per-row actions: edit, delete, share (payload), add to dashboard
- Same pagination

**Explore page change:** "+" button made prominent so users without admin access discover reach creation. Links to user reach creation flow (slim form path).

**Trip reports / hazards / conditions (Phase 2b):** writes blocked against `user_reaches`. Community data layer applies only to curated `reaches` so moderation surface stays bounded. User reaches remain personal-use only.

---

### 2.5 — Polling resilience

Gauges are not manually retired — sources (USGS, DWR via NLDI) decide when a gauge stops reporting. We surface poll health instead of curating gauge lifecycle.

**Schema (migration 000073):**

```sql
ALTER TABLE gauges
  ADD COLUMN consecutive_poll_failures INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN last_poll_failure_at      TIMESTAMPTZ,
  ADD COLUMN last_poll_success_at      TIMESTAMPTZ,
  ADD COLUMN poll_health               TEXT NOT NULL DEFAULT 'healthy'
    CHECK (poll_health IN ('healthy','degraded','stale','unreachable'));
CREATE INDEX gauges_poll_health_idx ON gauges (poll_health) WHERE poll_health <> 'healthy';
```

**Poller logic (15-minute cadence — pilot baseline):**

| Failures | Health | Action |
|---|---|---|
| 0 | `healthy` | normal cadence |
| 2 (~30 min) | `degraded` | normal cadence; UI badge appears |
| 4 (~1 hr) | `stale` | normal cadence; reach pages show "data stale" banner |
| 48 (~12 hr) | `unreachable` | back off to 1× per hour; admin alert in Rivers tab |
| 7 days unreachable | `unreachable` | log warning; flag on admin dashboard for swap |

Success at any state resets `consecutive_poll_failures = 0`, sets `last_poll_success_at`, returns to `healthy`.

**UI surfacing:**
- Gauge card: "stale" / "unreachable" badge with last successful timestamp when not healthy
- Reach detail: banner above gauge graph when reach's gauge is `stale` or worse
- Admin Rivers tab: per-river health summary; reaches needing gauge swap surfaced
- Custom gauge: if any input gauge unhealthy, the computed value flagged stale on the dashboard card

No automatic retirement — user / admin decides whether to swap a reach's gauge. The existing `gauges.status` enum (`active|seasonal|inactive|retired|maintenance`) remains untouched and continues to serve manual admin lifecycle decisions; `poll_health` is orthogonal.

---

### 2.6 — Discovery + dashboard distinctions

**Add gauge / add reach search:**
- Default tab: curated h2oflows reaches/gauges
- Second tab: "My Reaches & Gauges" — owner-only personal items
- Import button next to search bar: "Import from share code" → payload paste dialog
- No public/community tab — sharing is point-to-point via payload only

**Dashboard card icons:**
- Curated reach card: H2OFlows badge
- User reach card: just a regular "user" icon, like the blacked-out headshot default avatar
- Curated gauge card: H2OFlows logo
- Custom gauge card: calc icon + "calculated" label, no sparkline (single trace only on click-through modal)

---

### Migration sequence

```
000068_flow_bands_three_tier.up.sql        (2.1: 5→3, drop craft_type)
000069_rivers_verified_flag.up.sql         (rivers.verified for review queue)
000070_custom_gauges.up.sql                (custom_gauges + custom_gauge_inputs)
000071_user_reaches.up.sql                 (user_reaches + user_reach_flow_ranges)
000072_polled_gauge_ids_view.up.sql        (poll source = union view)
000073_gauges_poll_health.up.sql           (2.5 health columns)
000074_user_watchlists_custom_gauge.up.sql (watchlist custom_gauge_id column)
```

Each migration self-contained, reversible. Order matters: 68 first (band format change touches admin form before any new table references flow ranges); 70 + 71 must precede 72 (view depends on both); 74 depends on 70.

---

## Phase 2b — Reports + multi-dashboards + theming ✅ shipped 2026-05-07

*Contribution layer rebuilt around a unified Reports concept. Trip reports, hazard warnings, and conditions board collapse into one entity. Adds tabbed dashboards and a theme picker. Re-planned from `NewFeatures.md` 2026-05-06; supersedes the original 2b split.*

The existing `trip_reports`, `hazards`, and `reach_conditions` tables stay live during 2b development (frontend was stub anyway), then drop once routes are migrated. `proximity_events` keeps its FK to `trip_reports` and is deferred to its own phase.

---

### 2b.1 — Unified Reports model

A **Report** is any user-submitted observation about a reach. Drive-by, paddle, hazard sighting, conditions note — same record. Lower bar than AW trip reports: a user driving home from work who notices a strainer can submit one in 30 seconds.

**Required:** reach, report_date, name, content
**Optional:** report_time, hazard_warning text, photos, paddled flag (`true` = author was on the water; default `false`)

CFS at observation time is auto-stamped from the reach's primary gauge nearest to `report_date` + `report_time` (or noon if time omitted). Flow band at observation time is computed from the reach's bands and stored.

**Schema (migration 000076):**

```sql
CREATE TABLE reports (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id        TEXT NOT NULL,
  slug            TEXT NOT NULL,
  reach_id        UUID NOT NULL REFERENCES reaches(id) ON DELETE CASCADE,
  name            TEXT NOT NULL,
  report_date     DATE NOT NULL,
  report_time     TIME,
  content         TEXT NOT NULL,
  hazard_warning  TEXT,
  paddled         BOOLEAN NOT NULL DEFAULT FALSE,
  flow_cfs        NUMERIC,
  flow_band       TEXT CHECK (flow_band IN ('low','running','high')),
  aw_synced_at    TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (owner_id, slug)
);
CREATE INDEX reports_reach_idx       ON reports (reach_id, report_date DESC);
CREATE INDEX reports_owner_idx       ON reports (owner_id);
CREATE INDEX reports_hazard_idx      ON reports (reach_id) WHERE hazard_warning IS NOT NULL;

CREATE TABLE report_photos (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  report_id   UUID NOT NULL REFERENCES reports(id) ON DELETE CASCADE,
  position    SMALLINT NOT NULL,
  storage_key TEXT NOT NULL,
  caption     TEXT,
  taken_at    TIMESTAMPTZ,
  exif_lat    DOUBLE PRECISION,
  exif_lng    DOUBLE PRECISION
);
CREATE INDEX report_photos_report_idx ON report_photos (report_id, position);
```

`storage_key` points to R2; upload pre-signed URLs issued by the API.

**Reports against `user_reaches` are blocked at the API layer** — community moderation surface stays bounded. User reach detail page hides the Report CTA.

**Rate limit + abuse:**
- 5 reports per user per hour (sliding window)
- Content text passed through a lightweight profanity / link-spam check before insert; soft-flag for admin queue if heuristic trips
- Photo upload: max 8 per report, 10 MB each, MIME sniffed server-side
- Edits within 24 hours of creation; after that, locked to preserve attestation. Owner can always delete.

**API:**

```
POST   /reaches/{slug}/reports
GET    /reaches/{slug}/reports?cursor=&limit=
GET    /reports/{slug}                      (public — owner-attributed view)
PATCH  /me/reports/{slug}
DELETE /me/reports/{slug}
GET    /me/reports
```

Public read; auth required for write/edit/delete. Slug is global (not per-owner) so report URLs are shareable: `/reports/{slug}`.

---

### 2b.2 — Submission UX + nav integration

**Header nav:** "Report" tab added beside Dashboard / Explore. Auth-gated; click while logged-out routes through sign-in.

**Submit flow:**
1. Reach picker (defaults to last-viewed reach if recent)
2. Date picker (defaults to today), optional time
3. Name field (one-line)
4. Content textarea
5. Optional hazard warning textarea (separate, surfaces with red badge in lists)
6. Optional "I paddled this" toggle
7. Photo uploader (drag/drop)
8. Submit → confirmation toast + redirect to the report detail page

Submit also reachable from a reach detail page via "Add report" button — pre-fills the reach.

**Report detail page (`/reports/{slug}`):**
- Owner attribution + avatar
- Reach link with current band color
- CFS / flow band at observation time
- Content + hazard callout + photo gallery
- Share button (2b.4)

**Per-reach reports section:**
- Below the gauge card on the reach detail page
- Paginated 5/10/25, default 5, sorted recency
- Hazard reports float to the top within page, prefixed with the warning badge
- Empty state: "Be the first to file a report for this reach."

**My Reports (avatar menu):**
- `/me/reports`
- Card grid (desktop multi-column, mobile single column)
- Filters: hazard-only, by reach, by date range
- Per-card actions: view, edit (within 24 h), delete

---

### 2b.3 — Sharing + AW cross-post

Each report has a Share button. Two surfaces: native social and AW cross-post.

**Social share:**
- Twitter / X, Facebook, SMS, Discord, "Copy link"
- Pre-formatted text: `{report.name} — {reach.name} @ {flow_cfs} cfs ({flow_band}). h2oflows.app/reports/{slug}`
- OG image generation pulled forward from Phase 3 for reports specifically: `/og/reports/{slug}.png` showing reach name, CFS, band color, first photo if present. Curated reach pages still get OG in Phase 3; reports need it now.

**AW cross-post:**

AW trip-report fields: title, run date, gauge, flow band (5 buckets: too-low / low / medium / high / too-high), rich-text content, photos.

H2OFlows has 3 bands. **Mapping is per-user, set once,** then reused on every cross-post:

| H2OFlows band | AW band (user picks) |
|---|---|
| below `running.min` | too-low *or* low |
| `running.min`…`running.max` | low *or* medium *or* high |
| above `running.max` | high *or* too-high |

Mapping prompt shown the first time a user clicks "Share to AW". Stored as a JSON object on a new `user_preferences` row. Editable later from settings.

If AW has a usable submission API → POST directly with token-bound auth and stamp `aw_synced_at`.
If not (most likely path) → open a deep-link to AW's web form with all fields URL-encoded, including the mapped flow band. User reviews + submits manually; we still stamp `aw_synced_at` optimistically with an "I posted it" confirmation.

Photos cross-posted only if AW endpoint supports multipart; otherwise the share copy notes "photos available at h2oflows.app/reports/{slug}".

---

### 2b.4 — Long-context report grounding (no RAG)

The existing AI assistant (`internal/ai`) ingests reports at query time by **stuffing all reach-scoped reports into the prompt** instead of retrieving via embeddings. Reach-bounded queries are naturally narrow — pilot reaches will see tens of reports, 1.0-era reaches unlikely to exceed a few hundred. Long context (Claude 200K+) absorbs that comfortably and prompt caching makes repeat queries to the same reach near-free.

**Why not RAG:** embedding pipeline + pgvector + reindex worker + chunking + retrieval tuning all add infrastructure for a problem we don't have at this scale. Reaches are the natural shard. Skip the retrieval layer entirely.

**Loader:**
- For a reach query, fetch all reports for that reach (most recent first), capped at last 24 months and ~500 reports max as a defensive ceiling
- Stamp each with author handle, date, CFS at observation, flow band, hazard flag
- Format as a structured prompt section the model can cite from verbatim

**Prompt caching:**
- The reach-reports block is cached per reach using Claude's prompt cache (5-minute TTL, refreshed on each query)
- Cache key = reach_slug + last report `updated_at` — invalidates automatically on new report or edit
- Cold-cache cost paid once per reach per ~5-minute window; subsequent queries to the same reach are cheap

**Prompt scaffolding (mandatory):**

> "The following are user-submitted reports about this reach. They are unverified and may be inaccurate, stale, or contradicted by current conditions. Cite each report by author + date when referencing. If a hazard is mentioned, surface it with a 'paddler caution' note even if uncertain about current state."

Response format requires inline citations: `[Jane D., 2026-04-12]` linking back to `/reports/{slug}`. The assistant never paraphrases a report as authoritative h2oflows data. Because the model sees the full text of every cited report, citation accuracy is inherent — no retrieval-quality failure mode.

**Hazard short-circuit:** if any loaded report has a non-null `hazard_warning` within the last 30 days, the assistant leads with the hazard summary regardless of whether the user asked about hazards. Implemented as a deterministic check on the loaded set before prompting, not as a model behavior.

**Scale ceiling:** if a single reach ever crosses ~1000 reports (no current path to that — even an extremely active reach would take years), revisit. Options at that point: (1) trim to last N by date before stuffing, (2) reintroduce retrieval as a pre-filter while keeping long context for the final prompt. Not a 1.0 concern.

---

### 2b.5 — Multiple tabbed dashboards

Single dashboard becomes multi-dashboard with tabs.

**Schema (migration 000077):**

```sql
CREATE TABLE user_dashboards (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id    TEXT NOT NULL,
  slug        TEXT NOT NULL,
  name        TEXT NOT NULL,
  position    INTEGER NOT NULL DEFAULT 0,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (owner_id, slug)
);
CREATE INDEX user_dashboards_owner_idx ON user_dashboards (owner_id, position);

ALTER TABLE user_watchlists
  ADD COLUMN dashboard_id UUID REFERENCES user_dashboards(id) ON DELETE CASCADE;
```

**Backfill:** for each distinct `owner_id` in `user_watchlists`, insert one `user_dashboards` row (slug `default`, name "My Dashboard", position 0) and update existing watchlist rows to point at it. After backfill, set `dashboard_id NOT NULL` in a follow-up migration once all writers are updated.

**UX:**
- Tab bar above the dashboard grid
- "+" tab → modal: name, optional description, save
- Three-dot per tab: rename, delete, reorder via drag
- Delete prompts confirm; cascades watchlist rows
- Mobile: tabs become a horizontal-scroll strip; long-press for the three-dot menu
- Empty new dashboard shows the same empty state as the existing single-dashboard build

**Add-to-dashboard flow** (reach card, gauge card, custom gauge card) gains a dashboard picker — defaults to the most recently used dashboard.

**API:**

```
GET    /me/dashboards
POST   /me/dashboards
PATCH  /me/dashboards/{slug}
DELETE /me/dashboards/{slug}
PATCH  /me/dashboards/reorder
```

Watchlist routes gain a `dashboard_id` query param + body field.

---

### 2b.6 — Theme picker + dark mode

Avatar menu addition. Pattern lifted from Nuxt UI's docs theme picker (`docs/app/components/theme-picker/ThemePicker.vue`, `useTheme.ts`) and the local reference at `~/projects/iansrecipes.com/frontend/components/theme-picker.vue` + `app.config.ts` + `assets/css/main.css`.

**Color palettes (initial set):**
- `h2oflows` (default — existing brand)
- `ocean`
- `river`
- `indigo-pink`
- `forest`

Each palette defined as primary/neutral pairs in `app.config.ts`; CSS custom properties live in `assets/css/main.css` keyed off a body data attribute.

**Persistence:**
- Pinia `useTheme` composable; persisted to localStorage (per `feedback_pinia_hydration.md` — never cookies)
- `data-theme` and `data-color-mode` attributes set on `<html>` before paint to avoid FOUC (Nuxt color-mode integration)

**Dark mode toggle** sits next to the palette swatches in the same menu. Three-state: light / dark / system.

No DB column for now; cross-device sync deferred until pilot demands it.

---

### 2b.7 — Hero report count

Landing hero already shows reach + river counts. Add reports.

- Source: `SELECT COUNT(*) FROM reports`
- Cached 60 s in API memory; no need for materialized view at pilot scale
- New stat block to the right of rivers; same typography
- Hide block until > 0 reports exist (keeps hero clean before any user contributes)

---

### Migration sequence (2b)

```
000076_reports.up.sql                   (2b.1: unified reports + report_photos)
000077_user_dashboards.up.sql           (2b.5: dashboards + watchlist FK)
000078_user_preferences.up.sql          (2b.3: aw_band_mapping + future prefs)
000079_user_dashboards_required.up.sql  (after backfill: NOT NULL dashboard_id)
```

Old tables (`trip_reports`, `hazards`, `reach_conditions`) remain through 2b. A later cleanup migration drops them once frontend cuts over to `/reports`. `proximity_events.promoted_to → trip_reports` FK is repointed to `reports` *or* the column dropped, depending on whether proximity work resumes.

---

### Deferred from 2b

- **Proximity events + passive telemetry** — backend route exists; resume when mobile PWA work begins (Phase 10 territory).
- **QR sharing** for custom gauges, reaches, and reports — nice-to-have for parking-lot communication; revisit post-1.0 once link/JSON sharing has usage data.
- **Discord bot ingestion** of report posts — folded into Phase 7.
- **Google Earth picture layer** — moved to Future ideas at the end of this roadmap.

---

## Phase 2c — Pilot polish (0.2.x patches)

*Bug-and-polish pass driven by issues filed on `h2oflows-app/web` after 2b shipped. Gates pilot outreach — mobile acid-test scenarios and admin regressions must clear before any of the six contacts get a link. Sequenced as small PRs against `main`, each tagged as a `0.2.x` patch.*

### 2c.0 — P0 regression

**[web#11] NHD flowline unclickable in admin New Reach.** Regression in 2.2 admin reach flow — clicking a flowline does nothing on the map. Blocks all admin reach creation. Fix immediately as a standalone PR. Bisect recent map-related PRs; suspect MapLibre layer interactivity or pointer-event z-stacking.

### 2c.1 — Mobile pilot blockers

Pilot rollout (below) acid-tests on phones. Each of these breaks a core flow on mobile.

- **[web#10] Fonts too small on mobile.** Root utility pass — bump base font-size + utility icon sizes via Tailwind responsive variants. Pad/margin pass to let content consume full viewport width on mobile.
- **[web#13] Reach-detail buttons bleed off right edge on mobile.** "Add to dashboard" / "Edit" / "Share" stack or shrink to icons on narrow viewports. Resolved together with 2c.2 toolbar rework — same toolbar pattern.
- **[web#9] Gauge / reach modals near-full-screen on mobile.** Maximize vertical + horizontal space, explicit X close button.
- **[web#7] Dashboard toolbar margin gap.** Content shows through gap between AppHeader and sticky tab bar on scroll. Dashboard tab header must sit flush at bottom of navbar (no gap). Likely a `top-` offset mismatch — `feedback_appheader_height.md` notes AppHeader is `h-[50px]` and sticky bars use `top-12.75`.

Bundle as one PR per fix or one polish PR — fixer's call. SEO blocking (3.4) piggybacks on whichever lands first.

### 2c.2 — Reach detail + toolbar rework

Layout pass on the reach detail page, plus a standardized toolbar shared with the dashboard.

- **[web#15] Re-arrange reach detail page.**
  - Description underneath the reach title
  - Buttons minimized to icons in a toolbar at the top, right of title, above description
  - "Ask" section moved under the toolbar
  - Reports section moved to the bottom; paginated 5 / 10 / 50 max
- **[web#14] Replace "Share" button with "Create Report" button on reach detail.** Pre-fills reach + date + any context already on hand. Already promised in 2b.2 ("Add report button — pre-fills reach"); just not built. Share lives in the report itself (already shipped 2b.3).
- **[web#12] Standardize dashboard toolbar.** Common toolbar component shared with reach-detail toolbar from web#15. View-mode, group, expand-all, add-gauge in one row; consistent icon set, sizes, colors. "Expand all" gets an icon. "Add gauge" moves out of right-corner isolation into the utility cluster.

Ship as one PR — toolbar component lands once, both surfaces consume it.

### 2c.3 — Dashboard "My Reaches" merge

**[web#8] Merge "My Reaches" into the main curated dashboard list.** Reverses the 2.4 decision to keep user reaches under the avatar menu only. New behavior: all reaches grouped together by state → basin → river, with a user-outline icon (primary color, no avatar circle) marking user reaches inline. The "My Reaches" sidebar/section is removed from the dashboard surface; avatar menu entry stays for the management view.

*Caveat:* this is a deliberate reversal of a prior product call. Confirm intent with at least one pilot tester before shipping if pilot is imminent. If shipped, `feedback_user_content_private.md` still holds — these are private rows, just rendered inline.

### 2c.4 — Polling polish + gauge health (extends shipped 2.5) 🔧 IN PROGRESS

**[web#16] Gauge reliability, 30d history, admin tooling, and user-facing health indicators.**

Full architecture decision logged in memory (project_issue16_pr_plan, project_gauge_scale_analysis). PRs ship in order A → B → D → C → E.

#### PR A — 30d retention + sparkline window ✅ merged (api#14, web#69)

- `readingRetention` + `backfillWindow`: 7d → 30d
- Backfill gap-detection SQL window: 7d → 30d
- `GetReadings` limit ceiling: 500 → 5000
- Sparklines: `12h / 24h / 30d` button group; compact label shows `30d`; localStorage persists all three
- On first deploy: backfiller seeds 30d history for all polled gauges (~5-10 min background, non-blocking)

#### PR B — Gauge status UI (user-facing) ✅ merged (api#15, web#70)

- New `app/components/gauge/GaugePollStatus.vue` — shared badge + refresh button
- States: `Updated Xm ago` / `Stale 1h+` / `Offline` / `History loading…` / `Decommissioned` / `Seasonal — off-season`
- New `POST /api/v1/gauges/:id/refresh` → calls `FetchNowIfStale(ctx, gaugeID, 0)`; rate-limited ~1 req/30s per gauge
- Gauge endpoint exposes `poll_health`, `last_reading_at`, `last_poll_success_at`, `status` in payload
- Surfaces in: GaugeSparkline header, reach detail gauge card, dashboard gauge rows, gauge modal

#### PR C — Admin gauges page ✅ merged (api#16–19, web#71)

- `GET /admin/gauges` — paginated, filterable (status, poll_health, source, orphaned, q); includes `reach_count`
- `POST /admin/gauges/:id/poll` — force poll (bypasses maxAge)
- `POST /admin/gauges/:id/retire` → `status='retired'`
- `POST /admin/gauges/:id/reactivate` → `status='active'`, clear failures
- `PATCH /admin/gauges/:id/seasonal` → set `status='seasonal'` + `seasonal_start_mmdd` / `seasonal_end_mmdd`
- New page `app/pages/admin/gauges.vue`: sortable table, summary counts, "Show decommissioned" toggle (default off)

#### PR D — Seasonal heartbeat + retire flow

- `loadGauges` SQL: `seasonal` gauges poll 1×/day outside season window (heartbeat — detects gauge coming back before paddle season)
- Explore + reach lists default filter: `status != 'retired'`
- Admin-only "Show decommissioned" filter toggle on explore page

#### PR E — 1y daily-mean graph (deferred — ship after A-D, measure usage)

- `FetchDailyMeans(ctx, externalID, since)` on GaugeSource interface
- USGS adapter: hits `/nwis/dv` (daily values service)
- DWR adapter: daily endpoint
- `GET /api/v1/gauges/:id/history?window=1y` → ~365 daily-mean points; `Cache-Control: max-age=3600`; no DB write
- 1y option on reach detail graph (separate from sparkline 30d)
- Scale note: ~3,200 gauges × 30d × 96 readings ≈ 9M rows at steady state — well within single-instance PG. Revisit caching with Prometheus metrics post-deployment.

**Status/health model (already in schema, no new migration for A-D):**
- `poll_health`: `healthy` | `degraded` (~30m) | `stale` (~1h) | `unreachable` (~12h)
- `status`: `active` | `inactive` (auto after 14d no success) | `seasonal` | `maintenance` | `retired`
- `auto_managed`: auto-recovers on next success; manual `retired` = stops polling permanently
- Unreachable gauges back off to 1×/hr automatically (already in `loadGauges`)

### 2c.6 — River identity ownership

**Model.** Rivers are 1:1 with GNIS IDs. `name`, `state_abbr`, `huc8`, `gnis_id` are NLDI-derived and immutable. `basin` is NLDI-defaulted but admin-overridable. Users cannot mutate any river field — but they can *suggest* corrections on `basin` or `state_abbr` via a feedback flow that admins curate.

**Removed:**
- `rivers.verified` column (all rivers are GNIS-verified by definition)
- `rivers.basin_locked` column (admin-set basin is the source of truth; NLDI sync no longer touches basin once set)
- Old "needs review" amber banner (semantically replaced — see 2c.6c)
- Free-form "New river" modal (name + state + basin text inputs)
- River-name override field on UserReachAuthor
- "Auto-lookup basin & state" admin button (implicit on creation now)

**Concrete bug this resolves.** A `user_reach` created on an existing GNIS-less legacy river (e.g. Foxton's "North Fork South Platte" seeded pre-`000061`) silently linked without backfilling gnis_id, leaving `AutoFillRiverMeta` to 404 on later admin lookup. 2c.6a shipped the immediate fix; 2c.6b–e remove the architectural conditions that made the bug possible.

**2c.6a — bugfix (shipped v0.2.8):**
- `AutoFillRiverMeta` falls back to `user_reaches.put_in` when `reaches` has no coords for the river.
- New helper `resolveOrCreateRiver(ctx, db, name, gnisID)` in `user_reaches.go`: GNIS-match → backfill missing gnis_id on name-matched legacy rows → on INSERT, populate state_abbr/basin/huc8 via `riverMetaFromGNIS`. Replaces inline river-resolution in Create + Update handlers.

**2c.6b — schema lockdown + corrections table:**
- New `cmd/backfill-river-gnis`: walks `rivers WHERE gnis_id IS NULL`, NHD-by-name lookup, fills uniquely-matched rows, outputs ambiguous/unmatched lists for manual review. Run in prod before migrations 000084/085.
- Migration `000084`: `ALTER TABLE rivers ALTER COLUMN gnis_id SET NOT NULL`, `DROP COLUMN verified`, `DROP COLUMN basin_locked`. **Do not bundle with backfill cmd in same PR** — NOT NULL constraint fails on any straggler row.
- Migration `000085`: create `river_corrections` table:
  ```sql
  CREATE TABLE river_corrections (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    river_id        UUID        NOT NULL REFERENCES rivers(id) ON DELETE CASCADE,
    proposed_by     TEXT        NOT NULL,
    field           TEXT        NOT NULL CHECK (field IN ('basin', 'state_abbr')),
    proposed_value  TEXT        NOT NULL,
    note            TEXT,
    status          TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'accepted', 'rejected')),
    reviewed_by     TEXT,
    reviewed_at     TIMESTAMPTZ,
    review_note     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
  );
  CREATE INDEX river_corrections_status_idx ON river_corrections (status) WHERE status = 'pending';
  CREATE INDEX river_corrections_river_idx  ON river_corrections (river_id);
  ```
  `field` is `basin` or `state_abbr` only. River name is GNIS-canonical, not user-mutable. Rebinding the wrong river to a reach is out of scope — admin handles via reach edit.

**2c.6c — admin UI:**
- "New river" modal becomes single-field GNIS ID input → NLDI preview card (name/state/basin/huc8 fetched live) → "Add" button. No free-form name/state/basin.
- Edit-river modal: name/state/gnis_id/huc8 all read-only; only `basin` editable.
- Drop old "needs review" amber banner (verified flag is gone).
- **New "Needs review (N)" tab** at top of Rivers admin, listing pending `river_corrections` grouped by river:
  ```
  North Fork South Platte (CO · South Platte basin)
    ┌─ basin → "Cherry Creek"   (user note: "this reach is in Cherry Creek drainage")
    │  by user@x.com · 2d ago    [ Accept ]  [ Reject ]
    └─ state → "WY"  (no note)
       by user@y.com · 1d ago    [ Accept ]  [ Reject ]
  ```
  Accept → apply `proposed_value` to `rivers.{field}`, mark `accepted`. Reject → mark `rejected` with optional review_note.
- Remove "Auto-lookup basin & state" button on edit modal (basin is the only mutable field; lookup is implicit at create-time).

**2c.6d — user reach flow:**
- `UserReachAuthor.vue`: drop river-name override field. NHD-returned name is canonical.
- Reach Create payload simplifies — accepts `{gnis_id, river_name}` for server-side `resolveOrCreateRiver` resolution; returns `river_id`. No client-side resolve modal (nothing for user to override).
- Post-create banner on reach detail:
  ```
  ✓ Looks like Trout Creek (CO · Arkansas basin) — is this correct?
                                                    [ Yes ]  [ No, fix... ]
  ```
  - **Yes** → dismiss, set localStorage flag so banner doesn't reappear for this user+reach.
  - **No, fix...** → modal: radio `Basin` / `State`, single field input, optional note → `POST /me/river-corrections` → toast "Thanks — admin will review."
- Banner only renders once per (user, reach) — gated on localStorage + correction status. Reaches whose user already submitted a correction skip the banner.

**2c.6e — backend handler cleanup:**
- `UpdateRiver` accepts only `basin` (rejects name/state/gnis_id/huc8 changes).
- Drop `auto-fill` endpoint (no longer needed — basin is the only mutable field).
- Remove `verified`/`basin_locked` from all structs and SQL.
- New `RiverCorrectionsHandler`:
  - `POST /api/v1/me/river-corrections` (user) — creates pending row.
  - `GET  /api/v1/admin/river-corrections?status=pending` — admin list.
  - `PATCH /api/v1/admin/river-corrections/{id}` — accept/reject; on accept, also writes to `rivers.{field}`.

**Sequencing risk:** can't ship 000084 (NOT NULL) until backfill cmd runs prod-clean. Two PRs minimum:
1. cmd + migration 000085 (corrections table) — independent
2. migration 000084 — only after backfill verified

### 2c.7 — Explore page polish

**Problem.**
- H2OFlows / My Reaches mode toggle lives at top of sidebar — pushes search down, mixed in with list metadata, easy to miss.
- "+ New reach" button is a tiny ghost icon, admin-only, only triggers the curated-create flow. No path to import a shared reach from the explore page.

**Changes:**

**1. Floating mode toggle on map.**
- Pill segmented control `[ H2OFlows | My Reaches ]` floating in the **top-left** of the map area (top-right reserved for MapLibre zoom controls).
- Authenticated users only (same gating as today).
- Replaces inline block at `explore.vue:25-41`. Same `mode` ref drives sidebar list + `mapSourceUrl` — relocation only, no logic change.
- Mobile: pill at top-left of map; existing "N reaches" list-toggle button shifts to avoid collision.

**2. Prominent + button at top of sidebar.**
- Replace tiny ghost icon (`explore.vue:51-61`) with full-width button below search bar: `+  New reach`, primary-color icon, ~36px height.
- Visible to all authenticated users (not just admins). Admin gating moves into the picker.
- Click → popover positioned below the button:
  ```
  ┌──────────────────────┐
  │ ✏  Create New        │
  │ ↓  Import shared…    │
  └──────────────────────┘
  ```
  - Admin in curated mode → "Create New" opens existing ReachAuthor modal.
  - Non-admin or user mode → "Create New" routes to `/my/reaches/new`.
  - "Import shared…" → opens new `ReachImportModal.vue` for all authenticated users.

**3. Import flow.**
- Extract paste-JSON UX from `GaugeSearchModal.vue:450-475` into reusable `components/reach/ReachImportModal.vue`.
- Reuses existing `POST /api/v1/me/reaches/import` endpoint — no backend changes required.
- Modal accepts pasted JSON for MVP. Share-URL handling (`/share/reach/<token>`) deferred to a separate roadmap item.

**Effort:** ~4h. 1h toggle relocation, 1h + button + picker popover, 2h `ReachImportModal.vue` extraction + wiring.

**Verification:**
- Sidebar: search → big + button → list (no mode toggle inside sidebar).
- Map: pill toggle visible top-left, mode persists across reloads via existing ref.
- + picker: Create New routes correctly per admin/user; Import opens modal, paste valid JSON → reach lands in My Reaches mode.
- Mobile: pill doesn't collide with map controls or list-toggle button.

### 2c.8 — KML import for user reaches

**Problem.** Today only admins import KMZ/KML, via the global `/api/v1/import/kmz` endpoint that walks folders, slug-matches against `reaches.slug`, and writes pins (rapids, hazards, access points, parking, campsites, waves) into `rapids` + `reach_access`, both FK'd to `reaches.id`. Users who build their own reaches have no path to attach pins — their map renders only the centerline + put-in/take-out endpoints.

**Schema (migration `000086_pins_user_reach_id`):**
- Add `user_reach_id UUID NULL REFERENCES user_reaches(id) ON DELETE CASCADE` to `rapids` and `reach_access`.
- Relax `reach_id` from `NOT NULL` to `NULL`.
- Add CHECK: `(reach_id IS NOT NULL) <> (user_reach_id IS NOT NULL)` on both tables (XOR — exactly one parent).
- Partial indexes on `(user_reach_id)` where `user_reach_id IS NOT NULL`.
- Existing unique constraints `(reach_id, name)` / `(reach_id, access_type, name)` extend: add parallel `(user_reach_id, …)` partial uniques.

**Importer refactor (`internal/kmlimport`).**
- `Importer` gains optional `OwnerScope` config: when set, resolver skips the `reaches` slug lookup and instead matches against `user_reaches WHERE owner_id = ? AND slug = ?`. Single-reach mode — pins are direct children of the doc, no folder structure required (folders ignored if present).
- All `upsert*` helpers (`upsertRapidLocation`, `upsertAccess`, `upsertParking`) accept a target struct `{ ReachID, UserReachID *string }` and route to the right FK.
- Skip reach-metadata writes (description, flow bands, river-name lookup, centerline backfill) when in user-reach mode — user reach has its own NHD repin + flow-range UI; KML is pin-only for users.
- Admin path unchanged — folder-walk multi-reach import still calls the curated-reach code path.

**API.**
- `POST /api/v1/me/reaches/{slug}/kml` — multipart `file` field, scoped to authenticated owner.
- Owner check via existing `UserReachHandler.ownerID`.
- Returns same `ReachResult` shape (counts + log) so the UI can render an import log identical to the admin panel.

**User reach Get handler.**
- `userReachDetail` currently does not include rapids / access lists. Extend the Get query (or add separate sub-queries) to return:
  - `rapids[]` — `{ id, name, description, class_rating, is_surf_wave, is_permanent_hazard, hazard_type, lng, lat }`
  - `access_points[]` — `{ id, access_type, name, notes, lng, lat }`
- Reach detail map (`my/reaches/[slug].vue`) reads from these arrays and renders pin markers reusing the curated `ReachMap` pin rendering logic.

**Vue.**
- New `components/reach/UserKmlImportPanel.vue` — adapted from `admin/KmlImportPanel.vue`. Simpler help text (pins go directly in the doc, no slug placemark / folder structure). Drag-drop optional.
- Mount on `pages/my/reaches/[slug].vue` form panel, below the gauge / flow-line section.
- Show import-log toast on success; re-`load()` reach so new pins render immediately.

**Effort.** ~6h. 1h migration + Go schema, 3h importer refactor (most surface area — every upsert touched), 1h endpoint + handler + Get-query extension, 1h Vue panel + map wiring.

**Verification.**
- Upload KML containing `Rapid: Phone Boof (IV)`, `Hazard: Strainer`, `Parking: Lot A` against a user reach → 3 pins persist with `user_reach_id` set.
- Detail map renders new pins styled identically to curated reaches.
- Admin global import still imports curated multi-reach KMZ unchanged.

### 2c.9 — Theme palette overhaul

**Problem.** Current palette picker = 9 colors × 2 neutrals = 18 entries, presented as a two-row swatch grid (Slate row + Stone row) in the AppHeader appearance dropdown. Users must reason about two axes. We want a single curated list of 11 named themes, each pairing a primary with a tinted neutral, no slate/stone toggle.

**Prerequisite.**
- Bump `@nuxt/ui` to `^4.7.1` (currently `^4.6.0`). Smoke test that TW 4.2 tinted neutral scales (`mist`, `olive`, `mauve`, `taupe`) resolve through Nuxt UI color tokens. If they don't auto-resolve, register them via `@theme` block in `assets/css/main.css` (TW 4 customization path).
- Pin exact version after smoke — Nuxt UI minors have flipped API surface before.

**`app.config.ts` rewrite.**
- Replace `PALETTES` (18 entries) with `THEMES` (11 entries):
  ```ts
  { id: 'h2oflows', label: 'H2OFlows', primary: 'blue',    neutral: 'mist'    },
  { id: 'ocean',    label: 'Ocean',    primary: 'teal',    neutral: 'olive'   },
  { id: 'river',    label: 'River',    primary: 'sky',     neutral: 'mist'    },
  { id: 'forest',   label: 'Forest',   primary: 'emerald', neutral: 'olive'   },
  { id: 'dawn',     label: 'Dawn',     primary: 'amber',   neutral: 'mauve'   },
  { id: 'coral',    label: 'Coral',    primary: 'rose',    neutral: 'neutral' },
  { id: 'sunset',   label: 'Sunset',   primary: 'orange',  neutral: 'mauve'   },
  { id: 'moss',     label: 'Moss',     primary: 'lime',    neutral: 'stone'   },
  { id: 'cosmic',   label: 'Cosmic',   primary: 'pink',    neutral: 'mauve'   },
  { id: 'night',    label: 'Night',    primary: 'indigo',  neutral: 'slate'   },
  { id: 'sunrise',  label: 'Sunrise',  primary: 'yellow',  neutral: 'mauve'   },
  ```
- Each entry adds `primarySwatch` / `neutralSwatch` hex (TW 4.2 ramp level 500).
- Export `ThemeId = typeof THEMES[number]['id']`.

**`stores/theme.ts`.**
- Rename `paletteId` → `themeId`, type `ThemeId`, default `'h2oflows'`.
- Legacy ID migration in `plugins/theme.client.ts` (runs once on hydration):
  - Strip `-slate` / `-stone` suffix → look up base.
  - Map old → new where the visual mapping changed:
    - `h2oflows-*` → `h2oflows`
    - `ocean-*`    → `ocean`   (was sky+neutral, now teal+olive — visual change)
    - `river-*`    → `river`   (was teal+neutral, now sky+mist — visual change)
    - `forest-*`   → `forest`
    - `indigo-*`   → `night`
    - `sunset-*`   → `sunset`
    - `coral-*`    → `coral`   (was fuchsia, now rose — visual change)
    - `dawn-*`     → `dawn`    (was rose, now amber — visual change)
    - `moss-*`     → `moss`
  - Unknown legacy ID → fall back to `'h2oflows'`.

**`AppHeader.vue` UI rebuild (lines 142–186).**
- Drop two-row `Slate` / `Stone` grid + `Mode` row stays untouched.
- Replace with single vertical list inside the appearance dropdown:
  ```
  ●●  H2OFlows           ✓
  ●●  Ocean
  ●●  River
  ...
  ```
- Each row = full-width button, two-tone circular swatch (primary + neutral split) on left, label, checkmark on active. Click → `applyTheme(id)` → store + `appConfig.ui.colors.primary` + `appConfig.ui.colors.neutral`.
- Dropdown auto-sizes to list height (11 rows ≈ 280px); add internal scroll if needed.

**Effort.** ~4h. 1h npm bump + TW 4.2 neutral smoke test, 1h `app.config.ts` rewrite + theme store rename + migration, 2h AppHeader UI rebuild + visual QA across all 11 themes in light + dark.

**Verification.**
- All 11 themes render distinctly in light + dark mode.
- Existing users with `paletteId = 'forest-stone'` (etc.) land on the right new theme without console errors.
- Active theme swatch in the AppHeader chip preview shows correct two-tone.

**Risk note.** Coral and Dawn change visually (Coral fuchsia→rose, Dawn rose→amber). Visual diff is acceptable since pilot hasn't started.

### Sequencing

```
0.2.1  →  2c.0 (#11)         + 3.4 SEO blocking (trivial piggyback)
0.2.2  →  2c.1 (#10,#13,#9,#7) — mobile sprint
0.2.3  →  2c.2 (#15,#14,#12) — reach detail + toolbar
0.2.4  →  2c.3 (#8)           — my-reaches inline (confirm intent first)
0.2.5  →  2c.4 (#16)          — polling polish
0.2.8  →  2c.6a               — river identity bugfix (unsticks NFK South Platte)  ✅
0.2.9  →  2c.7                — explore page polish (toggle relocation + import picker)
0.2.10 →  2c.6b               — backfill cmd + corrections table migration
0.2.11 →  2c.6c–e             — admin curation UI + user feedback banner + backend cleanup
0.2.12 →  migration 000084    — gnis_id NOT NULL + drop verified/basin_locked (post-backfill)
0.2.13 →  2c.8                — KML import for user reaches (schema + importer refactor + UI)
0.2.14 →  2c.9                — theme palette overhaul (11 named themes + Nuxt UI 4.7.1 bump)

→ then Demo Pack (0.3.0)
```

Web issues stay open against `h2oflows-app/web` until their PR merges; close on merge with the PR ref.

---

## Demo Pack (0.3.0)

*Pre-pilot feature extraction + new build to make the app demo-ready for the six pilot contacts. Each item below is either a polish-and-surface pass on an existing feature or a small new build. Lands as `0.3.0` once all four ship.*

### 3.1 — Basin Maps for dashboards

Dendritic tree view of the real gauges behind a dashboard, accessed via a link/button that opens a modal or dedicated page. Not a dashboard mode toggle. A "neat" visualization that shows the user where their selected rivers sit in the watershed — nice to have for the demo, easy to build since `BasinTree.vue` already exists as the rendering pattern.

**What renders in the tree:**

- Real gauges from the dashboard watchlist (reach-associated USGS/DWR gauges, user reach primary gauges)
- Custom gauges are **not** shown as nodes — they are exploded into their input gauges. Each input gauge is labeled with the custom gauge name it contributes to (e.g. PLAGRACO and PLAWATCO nodes both tagged "Foxton Calculated"). This keeps the tree grounded in actual measurement points and avoids a derived value "throwing a wrench" in the topology.
- Upstream→downstream order resolved via reach lng/lat (Colorado convention: ascending lng = downstream)
- Tap a node → opens the gauge/reach detail modal

**Implementation notes:**

- `BasinTree.vue` exists and handles d3-hierarchy SVG rendering; this is an adapter job
- New component feeds `WatchedGauge[]` from `store.gauges` + custom gauge input expansion from the API
- Modal wrapper or `/dashboard/tree` page — no dashboard view-mode toggle needed

### 3.2 — AW trip-report HTTP preview window

Generate the AW trip-report submission form structure (gathered by inspecting AW's submit form as a logged-in member). On "submit", show the constructed HTTP request + body in a modal instead of posting. User can copy the body and paste it into AW manually.

- Unblocks demos to AW-side contacts (tech team + Stream Team) without needing prior AW board approval to actually POST
- Sets the table for Phase 5 outbound AW integration once approval is in hand — same payload, different submit handler
- Form schema lives in `/me/preferences` `aw_band_mapping` adjacent (already shipped 2b.3); add `aw_form_schema` JSONB if needed

### 3.3 — Share custom gauges + user reaches via link / JSON

Generate a portable share payload for a custom gauge or user reach so a recipient can clone it into their own dashboard.

- Two transports: signed link (`/share/{token}` → recipient logs in → "Add to dashboard?") and a copyable JSON snippet (recipient pastes into an Add → Import dialog)
- Recipient gets a clone, not a reference — payload includes formula (custom gauge) or geometry (user reach) + display metadata
- No DB-level sharing; per private-content rule (see `feedback_user_content_private.md`)
- QR generation pulled out — deferred (see "Deferred from 2b")

### 3.4 — SEO blocking until 1.0

Block search engines from indexing curated reach + report pages until 1.0 ships. Pilot is private validation, not a public launch — we don't want half-finished pages cached in Google.

- `robots.txt` disallow all
- `<meta name="robots" content="noindex, nofollow">` on layout default
- Pulled in the 1.0 release as part of the launch checklist
- Trivial; ships with whichever PR is first to merge after this section starts

### 3.5 — Dashboard share (deferred, post-0.3)

Three-dot menu on the dashboard gains "Share dashboard". Generates a portable JSON payload — same pattern as 3.3 — that bundles: the active tab's gauge watchlist, hidden-reach set, pinned custom gauges (full formula payload, not by reference), embedded user reaches (geometry + flow bands + gauge binding payload, including embedded `custom_gauge` blocks introduced in 3.3 fix).

Recipient imports via the existing import picker on `/explore` (or new dashboard-import entry). Pure clone semantics — no DB sharing, no subscriber tracking, no notifications. Same privacy boundary as 3.3: only fields included in the payload travel, owner's notes stay private.

- New top-level payload type `dashboard` whose body is `{ watchlist_entries[], custom_gauges[], user_reaches[] }`
- Each `user_reach` carries its own optional `custom_gauge` block (recursive 3.3 codec)
- Import order: custom gauges first (assign new IDs), then user reaches (link new IDs), then watchlist (resolve curated reaches by slug, user reaches by new ID)
- Defer until post-pilot; pilot users can share individual reaches/gauges via 3.3 today

### 3.6 — Rethink hazards (deferred)

Trip report `hazard_warning` field was removed in 0.3.0 polish (liability/bad-info concern). Reintroduce later with proper guardrails: stronger UX warnings on display, attribution to author (not "h2oflows says"), reach-author moderation, possibly admin-confirmed-only. Must reintegrate with RAG context generation (currently dropped from `report_context.go`). Open question: is hazard-as-trip-report the right model at all, or should it be a separate reach-attached object with author attribution and admin review?

---

## Phase 2d — Post-pre-pilot iteration ✅ shipped 2026-05-20

*Bundled polish + small features driven by hands-on testing after PR #35 merged. Ships as `h2oflows-app/web#48` + `h2oflows-app/api#11`. Closes 16 web issues (#49–#64) + 1 api issue (#12).*

### 2d.1 — Reach author parity

Admin `ReachAuthor` now mirrors `UserReachAuthor`:
- Anchor click sets put-in in one step (was two clicks)
- No zoom-out on anchor pick — user controls map viewport while picking flowlines
- "Upstream/Downstream" labels renamed to "Put-in/Take-out"
- ComID `pair-lock` auto-engages after take-out is set, so stray flowline clicks while hunting for a gauge don't replace take-out

Same `comIDPairLocked` fix applied to `UserReachAuthor`. Closes web#49, #50, #60.

### 2d.2 — User reach difficulty

Migration `000090_user_reaches_class` adds `class_min NUMERIC(3,1)` + `class_max NUMERIC(3,1)` columns on `user_reaches`. List/Get/Create/Update/MapAll all accept and emit the fields. PATCH semantics: nil pointer = keep existing (matches `name` field).

UI: Class min/max inputs in user reach editor + new-reach form. Closes web#57, api#12.

### 2d.3 — Per-dashboard preferences

View mode, grouping (state/basin/gauge), filter (curated/myReaches/gauges), collapsed sections, and gauge-map visibility now persist per-dashboard. Switching tabs rehydrates from a single JSON blob keyed by `activeDashboardId`. Hydration flag suppresses the save watcher during rehydrate so dashboard A's prefs don't get written under dashboard B's key. Closes web#52, #61.

### 2d.4 — Dashboard gauge-map toggle

Toolbar adds map-icon button to show/hide the gauge map. Section header + divider hide entirely when the toggle is off. Closes web#53, #58.

### 2d.5 — Phantom curated reach filter

`byStateTree` filters watchlist gauges whose `contextReachSlug` matches a user reach slug — eliminates the phantom curated-reach card rendered under "Unknown River" when a user reach shared its primary gauge with a curated reach (caused by legacy gauge-bound watchlist entries from before the reach-only watchlist path).

Pairs with explore-page change: user reach add-to-dashboard now goes through `addReachToWatchlist(slug, dashboardId)` (no `gauge_id`) and DELETE via `kind=reach`. Closes web#51, #59.

### 2d.6 — Basin map loading banner

Centered "Loading tributaries…" banner with spinner renders on `BasinMap` while NLDI fetch in flight on larger basins (`/basin/colorado`). Closes web#54.

### 2d.7 — Misc UX polish

- New Reach button on `/explore`: admin-only in curated mode (opens admin author directly); user mode keeps the picker (Create new + Import shared). Closes web#55.
- `/my/reaches` index gets the same picker pattern. Closes web#62.
- `/my/reaches/new` page gains an X close button in the header bar. Closes web#63.
- User reach line color follows the active theme's `primarySwatch` hex (read from `THEMES`, not the Tailwind v4 `--color-primary-500` CSS var — oklch is rejected by MapLibre paint). Closes web#64.
- Regression fix: explore map lines blanked out during the user-reach color expression iteration when nested `case` + null-typed comparator was used. Reverted to single-level `case` matching original working shape. Closes web#56.

---

## Repository restructure (pre-pilot) ✅ completed 2026-05-13

*Split the monorepo into a GitHub org with three independent repos before pilot outreach. Cleaner deployment story per surface, separate release cadence per layer, easier to hand a single repo to a contributor (e.g. AW collaboration) without exposing the rest.*

**Outcome:** `h2oflows-app/{api,web,docs}` all LIVE. API on EC2 at `api.h2oflows.app` (docker+Caddy), web on Netlify at `h2oflows.app`, docs on Netlify at `docs.h2oflows.app`. Original monorepo archived with `pre-split` tag + redirect README. `packages/gauge-core` flattened into `api/internal/gaugecore/`. Branch protection enabled on `api` + `web` main (1 approval, stale review dismissal, no force push). See `project_repo_split.md` memory for the full decision log.

The section below is preserved as the historical plan; for current deploy procedure see `h2oflows-app/api/CLAUDE.md`.

GitHub org is `h2oflows-app` (the bare `h2oflows` org name was unavailable). Production domain is `h2oflows.app`.

### Target topology

| Repo | Contents | Deploy target | Stack |
|---|---|---|---|
| **`h2oflows-app/api`** | Go backend (Chi, pgx v5, PostGIS) + migrations + `gauge-core` rolled in as `internal/gaugecore` + poller + AI handlers | TBD (Fly.io / Render / Railway — pick at split time) | Go 1.x, single module, no `go.work` |
| **`h2oflows-app/web`** | Nuxt 4 frontend, MapLibre, uPlot, Pinia, Nuxt UI Pro | **Netlify** | Nuxt 4 |
| **`h2oflows-app/docs`** | Pilot documentation site (per-feature walkthrough pages) | Netlify or Cloudflare Pages | Nuxt 4 + Nuxt UI + Nuxt Content, scaffolded from a Nuxt docs template (Docus or Nuxt UI Pro `docs` starter) so it matches the look of Nuxt module sites |

### What moves where

- `apps/api/**` → `h2oflows-app/api/` (root)
- `apps/api/migrations/**` → `h2oflows-app/api/migrations/` (unchanged structure)
- `packages/gauge-core/**` → `h2oflows-app/api/internal/gaugecore/` — only API consumes it; flatten to remove the workspace dep
- `apps/web/**` → `h2oflows-app/web/` (root)
- `apps/docs/**` (created during 2b for pilot) → `h2oflows-app/docs/` (root)
- `ROADMAP.md`, `ARCHITECTURE.md`, `NewFeatures.md` history — keep in `h2oflows-app/api` as the canonical planning home; symlink or duplicate `CLAUDE.md` per repo with repo-scoped guidance
- `.claude/memory/` stays local-only (gitignored everywhere)

### Split mechanics

1. Tag current monorepo HEAD as `pre-split` for forensic reference
2. Use `git filter-repo --path apps/api --path packages/gauge-core --path-rename apps/api:.` (and similar per repo) to preserve commit history per surface
3. Push each filtered branch to its new GitHub repo
4. Open a tracking issue in each new repo capturing residual cleanup
5. Archive (do not delete) the original monorepo with a top-level README pointing to the three new repos

### Cross-repo concerns

- **API URL** injected into `h2oflows-app/web` build via `NUXT_PUBLIC_API_URL` (Netlify env var); preview deploys point at staging API
- **CORS** on API explicitly allow-lists web's Netlify origins (production + preview wildcard) and docs origin if docs embed any live data
- **Auth (Supabase)** config duplicated as env vars in web + docs builds; API verifies same project's JWTs
- **Shared types** — Go API has no TS consumer today. Phase 4 introduces OpenAPI 3.1; defer codegen until that lands. In the interim, web maintains hand-written types matching the API contract.
- **Migrations** stay co-located with API; CI runs `migrate up` against staging DB on merge to `main`
- **Cross-repo references** — docs links to web pages; web links to docs pages; both link to API status / health. Use environment-aware base URLs in each.

### Versioning across the split

Each repo gets its own semver (independent git tags, independent CHANGELOGs). The product version (`0.1.0`, `0.2.0`, `1.0.0` from the Pilot rollout cadence) is a meta version recorded in `h2oflows-app/api/RELEASES.md`, mapping each product release to the specific commit / tag in each repo at that moment:

```
0.2.0 (Phase 2b shipped)
  api: v0.2.0   (commit abcd123)
  web: v0.2.0   (commit ef45678)
  docs: v0.1.0  (commit 9012abc)
```

Repos can ship patches independently (`api@v0.2.1` for a poller fix without touching web). The next coordinated product bump lifts whichever repos changed.

### Order of operations

1. Phase 2b + Demo Pack (3.1–3.4) ship in the monorepo (avoid restructuring mid-flight)
2. Confirm domain + create `h2oflows` org
3. Filter-split monorepo into `h2oflows-app/api` + `h2oflows-app/web` in one sitting; freeze monorepo writes during the cut
4. Stand up `h2oflows-app/docs` fresh post-split (no history to preserve — scaffolded directly in its own repo from a Nuxt docs template)
5. Reconfigure CI/CD per repo
6. Update local dev docs in each `CLAUDE.md`
7. Tag `0.3.0` across all three repos as the first post-split product release (Demo Pack + split)
8. Build docs pages per feature in `h2oflows-app/docs`
9. Begin pilot outreach against the new topology

### Risks + mitigations

- **History loss on filter-repo** — verify with `git log --follow` on a few key files before pushing; keep `pre-split` tag indefinitely as fallback
- **Netlify rebuild churn** during cutover — set up the new web repo's Netlify project with a placeholder before DNS flip, then move the domain once builds verify green
- **Auth env drift** — check Supabase keys identical across web + docs + API (single source: a 1Password vault entry referenced by all three CI configs)
- **Out-of-sync deploys** during a coordinated bump — release checklist in `RELEASES.md` enforces order: API first, web second, docs last (frontend can degrade gracefully behind a stale API; reverse is messier)

---

## Post-v0.4 backlog (deferred from UGC shift)

### PR 9 — LLM nightly audit

Scheduled Go job using Claude Haiku to auto-flag UGC outliers into the existing `abuse_flags` queue.

**What it checks:** new `user_reaches` + `reports` from the past 24h that haven't been audited (`llm_audited_at IS NULL`). Prompt asks: profanity, seriously dangerous misinformation, off-topic content. Structured JSON response `{flag: bool, reason: string}`.

**Implementation:**
- Migration: add `llm_audited_at TIMESTAMPTZ` to `user_reaches` and `reports`
- New `audit` package or handler with `RunAudit(ctx)` — called by cron scheduler in main.go
- On flag: insert into `abuse_flags` with `reporter_id = 'system'`, feeds existing admin queue
- Cost guard: cap 500 items/day at Haiku pricing (~$0.05/day ceiling)

**When to add:** once daily active UGC exceeds ~20 new items/day and the manual queue becomes burdensome.

---

## Pilot rollout (0.x)

*Validate Phases 2 + 2b with a small targeted group before public launch. Each pilot contact gets a tailored pitch + a feature-focused walkthrough in a docs site. Doubles as a mobile/device acid test.*

### Pilot group + tailored messaging

The app pivoted from a generic flow tracker to "build your own reaches and dashboards on top of curated content." Curated reaches stay; user reaches and custom gauges are the personal layer. Each contact below gets a distinct pitch.

Contacts are referenced by archetype only — actual names + contact details live in a private memory note outside the repo. Each archetype gets a distinct pitch.

| Archetype | Role / context | Lead with | Docs to link |
|---|---|---|---|
| **Pilot A** | Whitewater kayak instructor; AW stream team contributor | Custom gauges (gauge math is their world) | Custom gauge builder; user reach creation |
| **Pilot B** | AW tech team | Data schema standard for an AW pipeline; auto-share Reports → AW trip-report pre-fill (2b.3); public API contract | Reports + AW cross-post (2b.3); public API (Phase 4) |
| **Pilot C** | AW Stream Team Google Group; previously floated an alt whitewater DB | "Complementary, not competing" — H2OFlows as the load-shedding seam AW didn't want to host | Public API; reach + gauge data model framed as offload |
| **Pilot D** | Expert paddler; deep community presence | Custom reach creation + flow tracking + Reports | User reach flow; custom gauges; reports |
| **Pilot E** | Paddler, non-technical | Plain UX walkthrough — no jargon, will catch dreadful breakage | Dashboard + add reach + reports |
| **Pilot F** | PNW paddler, ex-CO; active community member | Reports + conditions across regions; flow tracking | Reports; basin / state navigation |

**Send strategy:**
- Pilot A gets a cold link — explore unprompted
- Everyone else gets a personalized DM/email with: (1) why them specifically, (2) one or two features tailored to them, (3) direct links to the docs pages for those features, (4) ask for an acid-test pass on phone + laptop
- Drafts kept in a `pilot-outreach/` scratch dir (not committed) until ready to send

### Pitch differentiation

- **Pilots A / D / E / F** — paddler users; pitch the personal-dashboard + custom-reach angle. They use the app, they file reports, they break the UX.
- **Pilot B** — AW-internal; pitch the data interop angle. Lead with the share-back-to-AW flow (2b.3) and the public API (Phase 4) as a way for AW to receive structured submissions without operating the public API themselves. Open the door to schema-standard collaboration.
- **Pilot C** — pitch H2OFlows as an *offload*, not a replacement. Frame the public API as the interop seam. Acknowledge their concern (AW server load from a public API) and demonstrate H2OFlows already shouldering it. Reframes earlier pushback against an AW alternative — the alternative is a complement, not a fork.

### Pilot docs site

Stand up a small Nuxt docs site (separate package or `apps/docs`). Lightweight — for the pilot, not SEO marketing. **Scaffold from a Nuxt docs template** so it has the polished look of Nuxt module documentation sites — candidates:

- **Docus** (`nuxtlabs/docus`) — the classic Nuxt module docs aesthetic
- **Nuxt UI Pro `docs` starter** (`nuxt-ui-pro/docs`) — newer, matches Nuxt UI v3/v4 styling, what powers the Nuxt UI Pro docs themselves

Pick at scaffold time based on which one is current and best supported when the docs repo is cut. Default leaning: Nuxt UI Pro `docs` starter, since the web app is already on Nuxt UI Pro and the design language stays consistent across web + docs.

Per-feature pages:

- Dashboard + watchlist
- Add a curated reach to your dashboard
- Create a custom gauge
- Create a user reach (with map walkthrough)
- File a report
- Share a report (social + AW cross-post)
- Theme picker

Each pilot message links directly to the doc pages relevant to that contact — no scrolling a generic landing page.

Tentative deploy: `docs.h2oflows.app` or a subpath on the main app.

### Acid test

The pilot is also a mobile/device matrix shakedown:

- Each contact runs the app on phone + laptop (whatever they own — iOS / Android / macOS / Windows mix expected)
- Targeted scenarios:
  - dashboard hydration on cold load
  - map gestures on touch (reach map zoom/pan, marker tap targets)
  - custom gauge formula builder on small screens
  - reach creation map flow on phone (anchor pick, take-out pick, auto-trim preview)
  - photo upload on report from mobile camera
  - tabbed dashboard interaction on mobile (horizontal scroll + long-press menu)
  - theme picker + dark mode toggle persistence across reloads
- Feedback collected via a single channel (Discord DM or email — TBD) and tracked in a lightweight log
- Bugs filed, prioritized, and folded into 0.x patch releases
- Diverse mix of technical expertise + paddling experience expected to surface different bug classes

### 0.x → 1.0 release cadence

Semantic versioning. The pilot lives on 0.x:

- `0.1.0` — Phase 2 (2.1–2.6) shipped; pilot can demo all current features (custom gauges, user reaches, polling resilience, discovery UX)
- `0.2.0` — Phase 2b shipped (Reports, multi-dashboards, theme picker, hero report stat)
- `0.x.y` patches — bug fixes from acid-testing
- `0.x.0` minors — new feature increments below the 1.0 threshold
- `1.0.0` — public launch

**1.0 gate criteria:**
- Stable across the pilot device matrix
- Load-tested (public API + poller under simulated traffic)
- Pilot UX feedback addressed (or explicitly deferred with rationale)
- Sufficient curated reach catalog to give a non-pilot user a reason to land
- All critical hazards in the issue tracker resolved
- Phase 3 OG images live (curated reaches + reports)

Each release cuts a git tag, a `CHANGELOG.md` entry, and (when relevant) a short post for the pilot channel. 1.0 is the public launch, not a version bump — feature freeze the week prior, full load test, finalize OG images, social prep.

---

## Phase 3 — SEO + Open Graph

*Organic discovery. No marketing budget — make every shared link count. Curated content only.*

### Dynamic OG images

- `/og/reaches/{slug}.png` — reach name, river, class, current CFS, flow band color, reach centerline thumbnail
- `/og/trip-reports/{slug}.png` — reach name, date, CFS at run, conditions summary, optional user photo
- `/og/gauges/{id}.png` — gauge name, current CFS, sparkline, flow status
- Generated server-side (Go + `gg` or headless Chromium); cached in Cloudflare R2

User reaches and custom gauges excluded — non-permanent pages, no indexing.

### Reach page SSR meta

- `<title>`, `og:title`, `og:description`, `og:image` populated from reach data + live gauge reading
- Structured data (`application/ld+json`): `Place`, `Event` (for trip reports)
- Canonical URLs for curated reach slugs

### Shareable links

- Trip report share → OG image with conditions + CFS
- Gauge alert share → "Browns Canyon is running at 850 CFS (optimal)" + link
- Dashboard snapshot URL — encodes current watchlist + gauge readings as shareable link (no account required)
- KML export already exists on reach pages; add GPX

---

## Phase 4 — Public API + route-tree split + user data export

*H2OFlows is infrastructure. The app is just the first consumer.*

### 4.1 Route-tree split (single binary, three trees)

Single repo, single Go binary. **Do not** split into separate API repos or ports — operational burden outweighs separation benefit until well past 50k users. Use Chi route groups with per-tree middleware.

```
/api/v1/public/*    no auth (or PAT), rate-limited, GET-only, CDN-friendly, OpenAPI documented
/api/v1/me/*        PAT or Supabase JWT, user-scoped data + exports
/internal/*         Supabase JWT only, web/pwa only, no versioning promise, no public docs
```

Middleware stacks per tree:
- `public`: rate limiter (per-IP + per-token), cache headers, response shaping, OpenAPI
- `me`: PAT-or-JWT auth, audit log of writes, no cache headers
- `internal`: Supabase JWT, free shape changes per release

Migration approach: move existing `/api/v1/*` handlers under appropriate tree without breaking web. Web client uses `/internal/*` going forward.

### 4.2 Personal Access Tokens (PATs)

User-generated tokens for `/api/v1/me/*` access. Single-scope v1 (full account access). Add scope strings later (`reports:read`, `reaches:write`) if needed.

```sql
CREATE TABLE user_api_tokens (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  name        text NOT NULL,         -- "iPhone Shortcuts", "trip log script"
  token_hash  text NOT NULL,         -- argon2id or sha256
  prefix      text NOT NULL,         -- "hflo_abc123…" visible identifier
  last_used_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  revoked_at  timestamptz
);
CREATE INDEX ON user_api_tokens (user_id) WHERE revoked_at IS NULL;
```

UI: Settings → API tokens. Show token plaintext once on creation, then never. List + revoke.

### 4.3 User data export (`/api/v1/me/export`)

Streaming zip of all user-owned data. GDPR-friendly; same endpoint serves "delete my account" flow later.

```
GET /api/v1/me/export
  → application/zip stream
```

Zip layout:
```
profile.json              # handle, email, created_at
reaches.json              # user-created reaches with geometry
gauges.json               # custom gauges (sum/difference formulas)
dashboards.json           # dashboard list with reach/gauge memberships
reports/
  {yyyy-mm-dd}-{slug}.md  # frontmatter (report_date, flow_cfs, flow_band,
                          #  reach_slug, paddled) + body
  {yyyy-mm-dd}-{slug}/
    photos/{n}.jpg        # if photos attached
```

Implementation notes:
- Use `archive/zip` writer directly to `http.ResponseWriter`; do not buffer.
- Photos streamed from R2 / S3 via signed-URL fetch into zip writer.
- Settings UI: "Export my data" button + last-export timestamp + confirmation modal.

### 4.4 Token issuance (legacy section — folds into 4.2 PATs)

- API token tied to user account — issued from profile/settings page
- Token scopes: `read` (public data), `write` (contributions), `elevated` (higher rate limits)
- Tokens stored hashed; revocable from settings

### 4.5 Rate limiting

- Anonymous: 100 req/hour
- Free token (`read`): 1000 req/hour
- Community contributor: 5000 req/hour (auto-granted on first verified trip report)
- Commercial/outfitter tier: paid, negotiated

### 4.6 Versioned public endpoints

All under `/api/v1/public/` after the route-tree split. Currently functional under `/api/v1/` but undocumented:

```
GET  /reaches                       paginated, filterable by region/class/state
GET  /reaches/{slug}
GET  /reaches/{slug}/gauges
GET  /reaches/{slug}/conditions
GET  /reaches/{slug}/trip-reports
GET  /reaches/{slug}/hazards
GET  /reaches/{slug}/flow-ranges
GET  /gauges/{id}/readings
GET  /gauges/{id}/readings?from=&to=
GET  /gauges/{id}/flow-ranges
GET  /gauges/{id}/seasonal
POST /reaches/{slug}/conditions      (write token)
POST /reaches/{slug}/hazards         (write token)
POST /reaches/{slug}/trip-reports    (write token)
```

New endpoints needed:
```
GET  /regions                        list states/basins with reach counts
GET  /regions/{slug}/reaches
GET  /reaches?bbox={w,s,e,n}         geographic filter
GET  /gauges?near={lat},{lng}&r={km} proximity search
```

### API docs

- OpenAPI 3.1 spec generated from route annotations or hand-maintained
- Hosted at `/api/docs` — Swagger UI or Scalar
- Attribution: "data sourced from H2OFlows community (h2oflows.app)"

---

## Phase 5 — American Whitewater integration

*Close the loop with the upstream data source. Frame to AW tech team as **interoperability spec + reference adapter**, not tight coupling. Goal: AW can push reach data into h2oflows on their terms, or h2oflows can pull on a schedule.*

### 5.0 Public interop spec (`reach-ingest-v1`)

Public, versioned JSON Schema document. Lives in `docs/specs/reach-ingest-v1.json`. Send to AW tech team as the contract.

**Scope (v1):** map/geometry data + descriptions/rapids/access. **Not in v1:** photos, trip reports, user-generated content. Photos + AI-on-trip-reports gated to v2.

**Required fields:** `name`, `geometry` (LineString put-in → take-out), `class_max`, `region` (state).
**Optional fields:** `common_name`, `description`, `rapids[]`, `access[]`, `gauges[]`, `flow_ranges[]`, `permit_required`, `multi_day_days`, `external_id` (AW reach ID), `external_url`.

**Derivation rules** (h2oflows-side enrichment when partner can't provide):
- **ComIDs missing** → derive from put-in/take-out coords via NLDI flowline trace.
- **River name missing** → derive via GNIS reverse lookup on put-in coord.
- **Basin missing** → derive via PostGIS containment against HUC8 basins table.
- **Gauge association missing** → propose nearest USGS gauge within 25 mi of put-in; partner confirms or rejects.

Source-of-truth field on every reach: `source` (`curated|user|aw|partner:slug`). Partners get their own slug for attribution + diff/conflict tooling.

### 5.1 Inbound: AW reach import (push + pull)

Two ingest paths — partner picks one:

**Push** (preferred — partner automates):
```
POST /api/v1/ingest/reaches
Authorization: Bearer <partner_token>
Content-Type: application/json

{ "reaches": [ ... reach-ingest-v1 objects ... ] }
```
- Partner token = scoped PAT issued to AW tech team. Separate table `partner_tokens` (not user-scoped).
- Idempotent: re-posting same `external_id` updates the existing reach.
- Response: per-reach status (created / updated / rejected with reason).

**Pull** (fallback — h2oflows automates):
- Worker hits AW REST endpoint on schedule (e.g. `www.americanwhitewater.org/content/River/list/`).
- Diff against existing `source='aw'` rows. Insert new, update changed, soft-delete missing.
- Weekly cron.

**Diff/review path:**
- Conflict with existing `source='curated'` row → write to `partner_reach_proposals` table, surface in admin UI.
- Stewards (Phase 6 roles) can accept/reject diff per field.

### 5.2 Reference adapter (`cmd/aw-ingest/`)

Standalone CLI tool that:
1. Fetches AW JSON
2. Maps to `reach-ingest-v1` schema
3. Enriches via NLDI/GNIS where needed
4. POSTs to local or production h2oflows ingest endpoint

Ships as Go binary + Dockerfile. Reference implementation AW can read; they can rewrite in their own stack.

### 5.3 Legacy inbound notes

- AW exposes a public JSON API (`www.americanwhitewater.org/content/River/list/`)
- Import script: map AW reach ID → H2OFlows slug, pull description + rapids + access
- Store with `source='aw'`, `external_id` = AW reach ID
- Diff against existing AI-seeded content; flag conflicts for human review
- One-time bulk import + periodic sync (weekly cron)

### Outbound: contribution pipeline back to AW

- When a trip report, hazard, or conditions update is published on H2OFlows, offer one-tap "Also post to AW"
- AW has a submission form API (undocumented but used by their mobile app); reverse-engineer or coordinate directly
- If AW API isn't available: generate formatted AW submission text + deep-link to AW's web form, pre-populated
- Track `aw_synced_at` on contributions — don't double-post
- User controls which data they share externally; default off

### AW reach linking

- Admin tool: search AW by river/state, link AW reach ID to H2OFlows slug
- Linked reaches show "Also on American Whitewater" badge + link
- AW gauge associations imported and cross-referenced with our USGS IDs

---

## Phase 6 — Data admin roles (scoped)

*Trusted local stewards, not just global admins.*

### Role model

Current: `data_admin` (global) and `site_admin` (global). Needed: scoped trust.

```
site_admin           global — full access, role assignment
data_admin           global — all reach/river data
basin_admin          scoped to a drainage basin (e.g. Arkansas River basin)
state_admin          scoped to a state (e.g. Colorado)
reach_steward        scoped to one or more specific reaches
```

### Implementation

- `user_roles` table gains optional `scope_type` (`basin|state|reach`) and `scope_id`
- Auth middleware: `RequireDataAdmin` checks scope before allowing reach mutations
- Admin UI: assign `reach_steward` role to a user + select reaches they steward
- Basin/state scopes defined by PostGIS containment check on reach put-in geometry
- Site admins can grant scoped roles; scoped admins cannot grant roles

### Steward features

- Stewards receive email digest of new trip reports, hazard warnings, and conditions posts for their reaches
- Stewards can verify/reject AI-seeded content on their reaches
- Stewards can close resolved hazards
- "Maintained by [name]" attribution on reach pages for verified stewards

---

## Phase 7 — Alerts + Discord

*Push notifications when the river comes up.*

### User-defined flow alerts

- Alert creation: gauge ID + threshold (min CFS, max CFS, or named flow band)
- Delivery channels: email (Phase 1), SMS (Phase 2, Twilio), push (Phase 2, PWA), Discord DM (Phase 3)
- Alert deduplication: don't re-fire until gauge crosses threshold again after going out of range
- Alert stored in DB; evaluated by poller on each gauge refresh

### Discord bot — Phase 1 (webhook, no OAuth)

Commands via text in designated channels:
```
!hflow flow arkansas-numbers
!hflow conditions poudre-mishawaka 340 "tobin clean, picnic washed out"
!hflow hazard arkansas-numbers "new strainer pine creek river left"
!hflow alert set cache-la-poudre 150 250
```
Every write returns confirmation link before touching DB.

Outbound alerts to subscribed channels:
```
🚨 Hazard — Arkansas / Numbers
Pine Creek Rapid · strainer river left
Reported at 920 CFS (currently 950, rising)
→ h2oflows.app/reaches/arkansas-numbers/hazards
```

### Discord bot — Phase 2 (slash commands + keyword nudges)

- Slash commands registered via Discord app
- Keyword watcher: strainer, hazard, portage, pin, washed out, undercut — nudges author to log it
- Never auto-posts; always prompts human confirmation

---

## Phase 8 — Trip planning

*From quick day trips to full permit expeditions.*

### Day trip planner

- Reach lookup → current conditions summary → shareable link
- Simple itinerary: date, reach, crew size, shuttle plan
- Link-only sharing (no account required to view)

### Overnight trip planner

- Multi-day itinerary builder
- Roster with roles (trip lead, safety, shuttle driver)
- Basic food notes per day
- Export: markdown / PDF / GPX

### AI post-trip extraction

After trip marked complete, AI reads trip notes and surfaces contribution cards:

```
  ✦ Hazard at Pine Creek rapid — new strainer river left
    → Log as hazard warning?  [ Yes ]  [ Edit ]  [ Skip ]

  ✦ You ran this at 850 CFS — community shows 800–1000 as optimal
    → Confirm flow band?  [ Confirm ]  [ Adjust ]  [ Skip ]
```

Never writes to DB without explicit user action.

### Trip export formats

- Markdown (Obsidian, static sites)
- PDF (printable trip binder)
- KML / GPX (Gaia GPS, CalTopo, Google Earth)
- Hosted trip page: flow graph, geotagged photo map, embedded video, food log, conditions summary

---

## Phase 9 — Permit trip module

*Full expedition coordination. Post-v1.*

- Full roster with roles, emergency contacts, dietary restrictions
- Gear matrix: who brings what, weight tracking
- Food planner: per-day meals, quantities, cook assignments
- Cost splitting: gear rental, shuttle, food, permit fees
- Shuttle coordination: vehicle assignments, meetup times, parking logistics
- Outfitter integration: guided trip roster management, paid API tier
- Permit tracking: application deadlines, lottery status, permit scan storage

---

## Phase 10 — Native mobile apps

*PWA first; native for GPS and push.*

- Capacitor-based iOS and Android apps wrapping the Nuxt PWA
- Background GPS for passive put-in/take-out detection (opt-in)
- Offline reach + gauge cache — works at the put-in without signal
- Push notifications for flow alerts
- Photo capture tied to trip reports — EXIF GPS auto-pins to reach map
- App Store and Google Play distribution

---

## Non-goals (intentionally out of scope)

- Social graph / follows / likes — Discord, Instagram, and SMS do this
- Photo/video hosting as a primary feature — R2 storage for trip reports only, not a media platform
- Outfitter booking / transactional flows — outfitter API for data only; booking stays on their platforms
- International reach registry — US-first until data model is proven; gauge adapters already extensible
- Public sharing of user-defined reaches or custom gauges — private only; share formula payloads via message instead

---

## Gauge adapter backlog

New sources require one file in `packages/gauge-core`:

| Source | Priority | Notes |
|---|---|---|
| CDEC (California) | High | Covers Sierra + N. California runs |
| Environment Canada | Medium | BC, Alberta, Quebec paddling |
| USGS stage-only gauges | Medium | Parameter `00065` instead of `00060` |
| Manual / community gauge | Low | Spreadsheet-defined readings for ungauged runs |

---

## Infrastructure scaling plan

*Capacity + cost model. Single-region is the right answer until well past pilot scale. Multi-region Postgres is hard, expensive, and unjustified under 50k users.*

### Current setup (May 2026)

- API on EC2 (`api.h2oflows.app`), docker-compose + Caddy
- Postgres + PostGIS on same EC2 instance
- Netlify for web (static + edge), Cloudflare DNS
- Single region: us-east-2 (geographic CONUS center, cheapest east-coast pricing)

### Pilot scale target (2000 users, 50-250 concurrent)

Current sizing is sufficient. Reference cost estimate:

| Tier            | Spec                          | $/mo (reserved) |
|-----------------|-------------------------------|-----------------|
| API EC2         | t3.large (2 vCPU, 8GB)        | ~$50            |
| Postgres        | RDS db.t3.small + 50GB gp3    | ~$30            |
| EBS / backups   | snapshots, S3                 | ~$10            |
| **Total**       |                               | **~$90/mo**     |

Cheaper variant: stay on docker-compose with Postgres on the same box → ~$50/mo. Move to RDS when ops burden (snapshots, point-in-time recovery, version upgrades) outweighs cost.

### Scaling order (when needed)

1. **Single region** stays correct under 50k users.
2. **CDN in front of public GETs** (CloudFront → API): cache `/api/v1/public/reaches/*` and `/api/v1/public/gauges/*` for 5–60s. Cheap, large wins.
3. **Vertical scale API box**: t3.large → t3.xlarge → m6g.xlarge.
4. **Postgres read replica** when reads dominate (analytics, AI queries).
5. **Multiple API instances behind ALB** when single box CPU-bound. Requires session stickiness only for SSE/poll endpoints.
6. **Multi-region**: only if international expansion happens, or west-coast p99 complaints become real. ~70ms cross-CONUS is invisible vs API processing time.

### Geo distribution rationale (NOT needed for pilot)

- 2000 users in CONUS with 250 concurrent does not feel coast-to-coast latency.
- Map tiles already CDN-served (MapLibre via external tile providers).
- Cacheable public endpoints get CloudFront treatment first; that solves the same problem at 1/10 the complexity.
- Postgres replication across regions = consistency hell. Avoid.

### Capacity projections

| Users     | Concurrent | API instance        | Postgres            | $/mo est. |
|-----------|------------|---------------------|---------------------|-----------|
| 2k        | 50-250     | t3.large            | db.t3.small         | $90       |
| 10k       | 200-1000   | t3.xlarge           | db.t3.medium        | $200      |
| 50k       | 1k-5k      | 2× m6g.large + ALB  | db.m6g.large + RR   | $600      |
| 200k      | 5k-20k     | k8s/ECS, 4+ pods    | Multi-AZ + RR + CDN | $1500-3k  |

100x growth is a good problem to have. Defer planning beyond Phase 7.

---

## Future ideas

*Speculative or low-priority concepts kept on the roadmap for memory but not scheduled. Revisit only when adjacent phases create the right conditions.*

- **Google Earth picture layer** — overlay user photos at their EXIF GPS points on a 3D terrain view. Design coupled to photos-on-map; only worth revisiting once Reports photo upload has enough data to know what EXIF metadata users actually attach. Originally scoped in 2b, moved here as not pilot-relevant.
- **In-app pin editor on map.** Click-to-place pin UI for both curated and user reaches, replacing the KML round-trip for manual edits. Pin types match existing taxonomy (rapid / hazard / put-in / take-out / parking / campsite / wave). KML import (admin global + user per-reach from 2c.8) stays as the bulk path. Build once 2c.8 ships and the polymorphic pin schema is in place.
- **Auto-anchor KML imports.** Extend user KML import (and re-extend admin import) so the importer reads put-in/take-out KML pins, snaps each to the nearest NHD ComID via the existing NLDI service, picks the up/down ComIDs, and runs the same trimmed-centerline preview the manual repin flow uses today. Output: imported reach lands with put-in/take-out, pins, AND centerline in one step. Gated behind the manual repin flow proving stable in production (which it has, per recent ComID work).
- Other speculative ideas land here as they come up.
