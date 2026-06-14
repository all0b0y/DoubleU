-- 016_suggestions_unique_pending.down.sql
-- Restores the non-unique pending index from 001.

BEGIN;

DROP INDEX IF EXISTS ix_cps_pending;
CREATE INDEX ix_cps_pending
  ON contact_profile_suggestions(tenant_id, chat_id)
  WHERE status = 'pending';

COMMIT;
