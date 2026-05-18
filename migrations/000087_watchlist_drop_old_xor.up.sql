-- Drop the old XOR check constraint that was not removed by migration 000086.
-- It blocked reach-only watchlist entries (both gauge_id and custom_gauge_id null).
-- The replacement constraint user_watchlists_gauge_xor was added in 000086.
ALTER TABLE user_watchlists DROP CONSTRAINT IF EXISTS user_watchlists_one_target_chk;
