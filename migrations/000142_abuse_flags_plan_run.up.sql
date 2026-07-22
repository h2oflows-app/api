-- #246: allow flagging plan_run targets (plan-calendar equivalent of a
-- report flag). Additive — 'report' kept during the reports→plan_runs
-- transition (see 000143 backfill + A6 consumer repoints).

ALTER TABLE abuse_flags DROP CONSTRAINT abuse_flags_target_type_check;
ALTER TABLE abuse_flags ADD CONSTRAINT abuse_flags_target_type_check
  CHECK (target_type IN ('run', 'report', 'plan_run'));
