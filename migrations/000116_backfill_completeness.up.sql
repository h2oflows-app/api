-- Backfill completeness_score for all existing runs (V18).
-- saveCompleteness() only fires on future mutations; existing rows have score=0.

UPDATE user_reaches ur
SET completeness_score = (
    CASE WHEN ur.note IS NOT NULL AND char_length(ur.note) > 0             THEN 0.2 ELSE 0 END
  + CASE WHEN ur.primary_gauge_id IS NOT NULL OR ur.custom_gauge_id IS NOT NULL THEN 0.2 ELSE 0 END
  + CASE WHEN ur.class_min IS NOT NULL AND ur.class_max IS NOT NULL         THEN 0.2 ELSE 0 END
  + CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN 0.2 ELSE 0 END
  + CASE WHEN EXISTS(SELECT 1 FROM rapids WHERE user_reach_id = ur.id)      THEN 0.2 ELSE 0 END
);
