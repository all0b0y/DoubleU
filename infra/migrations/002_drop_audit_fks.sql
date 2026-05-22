-- Migration 002: Drop FK constraints that reference messages from message_states and audit_events.
--
-- These tables record lifecycle and audit data for ALL messages, including those that
-- are not persisted to the messages table (e.g. because of privacy flags or because states
-- are recorded before SaveMessage is called). The FK prevents valid audit records from
-- being written and causes foreign-key violations in normal operation.
-- Logical consistency is enforced by the application layer, not the DB constraint.

BEGIN;

ALTER TABLE message_states
  DROP CONSTRAINT IF EXISTS message_states_tenant_id_chat_id_source_message_id_fkey;

ALTER TABLE audit_events
  DROP CONSTRAINT IF EXISTS audit_events_tenant_id_chat_id_source_message_id_fkey;

COMMIT;
