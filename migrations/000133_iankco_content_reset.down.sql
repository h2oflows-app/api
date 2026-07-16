-- Partial rollback only. The hard-deleted iankco/iank-tc personal runs are
-- NOT recoverable (product decision: hard delete). This down removes the
-- bulk-forked copies created by the up: iankco-owned runs whose
-- forked_from_user_reach_id points at an h2oflows-owned run. Child rows
-- cascade; run_upvotes on these fresh forks are deleted first (RESTRICT FK).
WITH forked AS (
    SELECT dst.id
    FROM user_reaches dst
    JOIN user_profiles dp ON dp.owner_id = dst.owner_id AND dp.handle = 'iankco'
    JOIN user_reaches src ON src.id = dst.forked_from_user_reach_id
    JOIN user_profiles sp ON sp.owner_id = src.owner_id AND sp.handle = 'h2oflows'
)
DELETE FROM run_upvotes WHERE user_reach_id IN (SELECT id FROM forked);

DELETE FROM user_reaches dst
USING user_profiles dp, user_reaches src, user_profiles sp
WHERE dp.owner_id = dst.owner_id AND dp.handle = 'iankco'
  AND src.id = dst.forked_from_user_reach_id
  AND sp.owner_id = src.owner_id AND sp.handle = 'h2oflows';
