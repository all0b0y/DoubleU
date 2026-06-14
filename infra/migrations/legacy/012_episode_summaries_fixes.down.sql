-- Revert 012: restore the original closure_reason CHECK and drop embedding_model.
-- Rows written with closure_reason='daily_summary' are remapped to 'manual'
-- so the narrower constraint can be re-applied.

BEGIN;

UPDATE episode_summaries SET closure_reason = 'manual' WHERE closure_reason = 'daily_summary';

ALTER TABLE episode_summaries
  DROP CONSTRAINT IF EXISTS episode_summaries_closure_reason_check;
ALTER TABLE episode_summaries
  ADD CONSTRAINT episode_summaries_closure_reason_check
  CHECK (closure_reason IN ('silence_gap','message_count','topic_shift','manual'));

ALTER TABLE episode_summaries
  DROP COLUMN IF EXISTS embedding_model;

COMMIT;
