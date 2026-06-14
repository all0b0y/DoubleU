-- 012: episode_summaries fixes — two silent-write bugs found in the memory audit.
--
-- 1. The Go daily summary scheduler closes episodes with
--    closure_reason='daily_summary', which the original CHECK
--    ('silence_gap','message_count','topic_shift','manual') rejects.
--    SaveEpisodeSummary swallows the error, so daily summaries were never
--    persisted in DB mode. Extend the CHECK with 'daily_summary'.
--
-- 2. SaveEpisodeSummaryEmbedding updates episode_summaries.embedding_model,
--    but no migration ever created that column — every embedding UPDATE
--    failed silently and the embedding worker re-fetched the same rows
--    forever. Add the column.

BEGIN;

ALTER TABLE episode_summaries
  DROP CONSTRAINT IF EXISTS episode_summaries_closure_reason_check;
ALTER TABLE episode_summaries
  ADD CONSTRAINT episode_summaries_closure_reason_check
  CHECK (closure_reason IN ('silence_gap','message_count','topic_shift','manual','daily_summary'));

ALTER TABLE episode_summaries
  ADD COLUMN IF NOT EXISTS embedding_model TEXT;

COMMIT;
