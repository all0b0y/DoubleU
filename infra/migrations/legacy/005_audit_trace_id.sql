-- 005: add trace_id columns for cross-row Langfuse / OTel correlation.
--
-- Both message_states and audit_events get a TEXT trace_id column.  Nullable
-- so legacy rows and the synthetic normalization-error audit (which has no
-- usable event_id) keep working.  Indexed for fast lookup by trace.

ALTER TABLE message_states ADD COLUMN IF NOT EXISTS trace_id TEXT;
ALTER TABLE audit_events   ADD COLUMN IF NOT EXISTS trace_id TEXT;

CREATE INDEX IF NOT EXISTS idx_message_states_trace_id ON message_states(trace_id) WHERE trace_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_audit_events_trace_id   ON audit_events(trace_id)   WHERE trace_id IS NOT NULL;
