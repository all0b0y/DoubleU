-- 005 down: drop trace_id index + column on message_states and audit_events.
DROP INDEX IF EXISTS idx_message_states_trace_id;
DROP INDEX IF EXISTS idx_audit_events_trace_id;
ALTER TABLE message_states DROP COLUMN IF EXISTS trace_id;
ALTER TABLE audit_events   DROP COLUMN IF EXISTS trace_id;
