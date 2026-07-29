-- web#354 A4 rollback. Schema-smoke only (mirrors the 000143/000144/000145/
-- 000146 down conventions) — NOT a data-fidelity guarantee: every
-- user-supplied calendar-run name is gone the instant this runs; there is no
-- way to tell a backfilled name (was derived from ur.name/slug) apart from a
-- genuinely user-typed one to selectively preserve.
ALTER TABLE calendar_runs DROP COLUMN name;
