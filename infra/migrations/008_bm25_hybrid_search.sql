-- Migration 008: BM25 full-text search on semantic_facts for hybrid retrieval.
--
-- Adds a generated tsvector column using the 'simple' text-search configuration,
-- which is language-agnostic and handles both Russian and English without requiring
-- a Russian dictionary (which is not installed by default in pgvector images).
-- A GIN index enables fast @@ plainto_tsquery lookups.
--
-- The hybrid search path in conversationContext fuses BM25 (lexical) and vector
-- (semantic) ranked lists via Reciprocal Rank Fusion before the reranker step.

ALTER TABLE semantic_facts
  ADD COLUMN IF NOT EXISTS fact_tsv TSVECTOR
    GENERATED ALWAYS AS (to_tsvector('simple', fact_text)) STORED;

CREATE INDEX IF NOT EXISTS ix_sf_fts
  ON semantic_facts USING GIN(fact_tsv)
  WHERE deleted_at IS NULL AND superseded_by_id IS NULL;
