-- Rollback for 004_backfill_unclaimed_owner.sql.
--
-- The up-migration only updates existing rows; there is no schema change to
-- revert.  We cannot reconstruct the previous owner_tenant_id values because
-- the original data was overwritten by design, so this rollback is a no-op
-- and exists purely so MigrateDown can untrack version 4 if an operator
-- decides to redo the backfill from a different snapshot.
SELECT 1;
