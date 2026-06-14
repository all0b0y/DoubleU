-- 007: persist trace_id on each draft so owner approval / thumb feedback
-- attaches a Langfuse score to the trace the draft was generated under.
ALTER TABLE drafts ADD COLUMN IF NOT EXISTS trace_id TEXT;
CREATE INDEX IF NOT EXISTS idx_drafts_trace_id ON drafts(trace_id) WHERE trace_id IS NOT NULL;
