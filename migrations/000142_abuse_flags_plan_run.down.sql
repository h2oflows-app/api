ALTER TABLE abuse_flags DROP CONSTRAINT abuse_flags_target_type_check;
ALTER TABLE abuse_flags ADD CONSTRAINT abuse_flags_target_type_check
  CHECK (target_type IN ('run', 'report'));
