-- Down: no automated rollback for the fork backfill. Forking is destructive in
-- the sense that new user_reaches rows were created and watchlist rows were
-- repointed. Reverting would require knowing the original (user_id, slug) pairs.
-- Deliberately a no-op — operators can manually delete the auto-created forks
-- via forked_from_reach_id IS NOT NULL AND original_forked_at > <migration_ts>.
SELECT 1;
