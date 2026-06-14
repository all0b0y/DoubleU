-- 015_halfvec_hnsw.sql
-- =============================================================================
-- MEM-7: restore ANN indexes for 4096-dim embeddings.
--
-- pgvector's HNSW supports ≤2000 dims on the plain `vector` type, so migration
-- 003 (vector(4096)) dropped all HNSW indexes and every cosine query has been
-- a sequential scan since. pgvector ≥ 0.7 ships `halfvec` (16-bit floats,
-- ≤4096 dims under HNSW); expression indexes over `embedding::halfvec(4096)`
-- restore ANN without changing the column type.
--
-- Guarded: indexes are created only when the halfvec type exists (pgvector
-- ≥ 0.7). On older pgvector this migration is a no-op and queries keep their
-- sequential-scan behaviour; the Go repository detects halfvec at startup and
-- only emits halfvec-cast ORDER BY clauses when the type is present.
--
-- The dedup procedure (013) intentionally keeps the plain `<=>` comparison:
-- its candidate set is pre-filtered to one (tenant, chat, fact_type, subject)
-- group via btree indexes, so an ANN index would not help there.
-- =============================================================================

BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_type WHERE typname = 'halfvec') THEN
    BEGIN
      -- Facts: partial over rows that actually have a vector.
      EXECUTE 'CREATE INDEX IF NOT EXISTS ix_fe_halfvec_hnsw
                 ON fact_embeddings
              USING hnsw ((embedding::halfvec(4096)) halfvec_cosine_ops)
              WHERE embedding IS NOT NULL';
      -- Episode summaries: partial over live, embedded rows.
      EXECUTE 'CREATE INDEX IF NOT EXISTS ix_es_halfvec_hnsw
                 ON episode_summaries
              USING hnsw ((embedding::halfvec(4096)) halfvec_cosine_ops)
              WHERE deleted_at IS NULL AND embedding IS NOT NULL';
    EXCEPTION WHEN program_limit_exceeded THEN
      -- pgvector < 0.8 caps halfvec HNSW at 4000 dims; 4096-dim index not possible.
      -- Queries fall back to sequential scans; upgrade pgvector to enable ANN.
      RAISE NOTICE 'pgvector HNSW dim limit exceeded for 4096-dim halfvec — skipping HNSW indexes (upgrade pgvector for ANN support)';
    END;
  ELSE
    RAISE NOTICE 'pgvector halfvec type not available (need >= 0.7) — skipping HNSW indexes';
  END IF;
END $$;

COMMIT;
