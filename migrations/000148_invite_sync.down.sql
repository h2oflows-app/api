-- API-1 rollback. Schema-smoke only (mirrors 000143-000147's down
-- conventions) — NOT a data-fidelity guarantee: every captured email in
-- user_emails and every run's accumulated SEQUENCE counter are gone the
-- instant this runs. Nothing to reconstruct on the way back up either
-- direction (ADD COLUMN ... DEFAULT 0 / CREATE TABLE both regenerate
-- cleanly from empty).

DROP TABLE user_emails;
ALTER TABLE calendar_runs DROP COLUMN ics_sequence;
