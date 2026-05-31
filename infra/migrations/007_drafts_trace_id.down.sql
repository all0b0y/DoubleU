DROP INDEX IF EXISTS idx_drafts_trace_id;
ALTER TABLE drafts DROP COLUMN IF EXISTS trace_id;
