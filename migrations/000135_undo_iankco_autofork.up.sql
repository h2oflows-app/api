-- #314 correction: remove the bulk auto-forks that migration 000134 copied from
-- @h2oflows onto @iankco. Forking every h2oflows run to iankco was a misread of
-- intent — iankco should start empty and runs get forked manually.
--
-- Scope: iankco-owned runs whose attribution says they were forked from
-- h2oflows AND that have NOT been edited since the fork
-- (last_modified_after_fork_at IS NULL — the fingerprint of an untouched
-- auto-fork). Any run a user has since opened and modified is preserved, so a
-- later manual fork that gets edited is safe even if this somehow re-ran.
--
-- Idempotent: a no-op once iankco holds no pristine h2oflows forks (e.g. after
-- the equivalent ad-hoc cleanup already run on prod).
--
-- Deploy note: apply this BEFORE doing manual forks in a given environment —
-- an unedited manual fork of an h2oflows run shares the same fingerprint.

-- Votes first (run_upvotes is ON DELETE RESTRICT, mig 000113).
DELETE FROM run_upvotes
WHERE user_reach_id IN (
    SELECT ur.id
    FROM user_reaches ur
    JOIN user_profiles up ON up.owner_id = ur.owner_id AND up.handle = 'iankco'
    WHERE ur.original_author_handle = 'h2oflows'
      AND ur.last_modified_after_fork_at IS NULL
);

-- Runs (rapids/access/flow_ranges/reports cascade; fork + watchlist refs SET NULL).
DELETE FROM user_reaches ur
USING user_profiles up
WHERE up.owner_id = ur.owner_id
  AND up.handle = 'iankco'
  AND ur.original_author_handle = 'h2oflows'
  AND ur.last_modified_after_fork_at IS NULL;
