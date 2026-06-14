-- 016_suggestions_unique_pending.sql
-- =============================================================================
-- MEM-4: the suggestions flow must not pile up duplicate pending rows for the
-- same profile field. The plain ix_cps_pending index becomes a UNIQUE partial
-- index over (tenant, chat, field_name) WHERE pending, which both serves the
-- old lookups and lets SaveProfileSuggestion dedup with ON CONFLICT DO NOTHING.
-- =============================================================================

BEGIN;

-- Collapse any historical duplicates before the unique index can be built.
DELETE FROM contact_profile_suggestions a
 USING contact_profile_suggestions b
 WHERE a.status = 'pending' AND b.status = 'pending'
   AND a.tenant_id = b.tenant_id AND a.chat_id = b.chat_id
   AND a.field_name = b.field_name
   AND a.id > b.id;

DROP INDEX IF EXISTS ix_cps_pending;
CREATE UNIQUE INDEX ix_cps_pending
  ON contact_profile_suggestions(tenant_id, chat_id, field_name)
  WHERE status = 'pending';

COMMIT;
