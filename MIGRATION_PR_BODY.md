## Summary

Companion migration for **web#252** (theme-indexed flow-band palette). Rewrites flow-band color values from legacy keys (`green-3`) to positional palette indices (`p<n>`) so the web client recolors them per theme.

Flow bands are stored as columns (not JSON), so only two are touched:
- `user_reach_flow_ranges.color` (per-threshold)
- `user_reaches.base_color`

(`flow_range_proposals` is numeric-only — nothing to migrate.)

## Mapping

8 families `red(0) orange(1) yellow(2) green(3) blue(4) purple(5) pink(6) neutral(7)` × 5 levels; `index = family*5 + (level-1)` → `"p<index>"`.

- **up** is idempotent (skips values already in `^p\d+$`).
- **down** reverses losslessly.

## Tested

Run against a fresh prod snapshot inside `BEGIN/ROLLBACK`:
- `color`: 281 rows mapped (`green-3`→p17, `blue-3`→p22, `orange-3`→p7, `yellow-1`→p10, `purple-2`→p26, …)
- `base_color`: 175 rows → p37 (neutral-3)
- **up → down round-trip: 0 mismatches**

## ⚠ Deploy order

Ship the **web frontend first** (it reads both legacy keys + `p<n>`), then run this migration. Never run the migration before the new frontend is live, or old clients render unknown values as grey.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
