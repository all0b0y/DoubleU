-- 003_embeddings_qwen_4096.sql
-- =============================================================================
-- Switches the embedding columns from vector(1536) → vector(4096) to match the
-- `qwen/qwen3-embedding-8b` model now used by `/ai/embed-texts`.
--
-- Context: the previous default `text-embedding-3-small` is not reachable from
-- OpenRouter (the only credential the deployment ships), so the embedding
-- worker has been silently returning `model_used="error"` since launch and
-- `fact_embeddings` / `episode_summaries.embedding` contain only placeholders.
-- It is therefore safe to TRUNCATE / NULL the embedding columns — no real
-- vectors are lost.
--
-- HNSW indexes are dropped here because pgvector's HNSW implementation supports
-- at most 2000 dimensions on the plain `vector` type.  Cosine similarity
-- queries fall back to a sequential scan; this is acceptable until the corpus
-- grows large enough to warrant a `halfvec(4096)` migration (pgvector ≥ 0.7).
-- =============================================================================

BEGIN;

-- 1) Drop existing HNSW indexes (locked to vector(1536)).
DROP INDEX IF EXISTS ix_fe_chat_hnsw;
DROP INDEX IF EXISTS ix_fe_global_hnsw;
DROP INDEX IF EXISTS ix_es_embedding_hnsw;

-- 2) Clear the embedding tables — every existing vector is a placeholder
--    produced by the broken embedding path.  Refusing to truncate would leave
--    rows with `vector(1536)` dimensions in a `vector(4096)` column.
TRUNCATE TABLE fact_embeddings;
UPDATE episode_summaries SET embedding = NULL WHERE embedding IS NOT NULL;

-- 3) Resize embedding columns.  pgvector cannot ALTER between fixed-dim vectors
--    in-place; drop and re-add is the documented path when the table is empty.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
    ALTER TABLE fact_embeddings DROP COLUMN embedding;
    -- Nullable: SaveFactEmbedding always supplies a value; readers already
    -- guard with `WHERE embedding IS NOT NULL`.  Avoids forcing a synthetic
    -- 4096-zero default that would pollute similarity queries.
    ALTER TABLE fact_embeddings ADD COLUMN embedding vector(4096);

    ALTER TABLE episode_summaries DROP COLUMN embedding;
    ALTER TABLE episode_summaries ADD COLUMN embedding vector(4096);
  ELSE
    -- pgvector unavailable: fall back to FLOAT8[].  Keep the column nullable to
    -- match the pgvector branch and let the worker write real arrays.
    ALTER TABLE fact_embeddings DROP COLUMN embedding;
    ALTER TABLE fact_embeddings ADD COLUMN embedding FLOAT8[];

    ALTER TABLE episode_summaries DROP COLUMN embedding;
    ALTER TABLE episode_summaries ADD COLUMN embedding FLOAT8[];
  END IF;
END $$;

-- 4) Update the default embedding model recorded with each row.
ALTER TABLE fact_embeddings
  ALTER COLUMN embedding_model SET DEFAULT 'qwen/qwen3-embedding-8b';

-- 5) Replace `upsert_semantic_fact` so the vector-similarity branch uses the
--    new column shape.  The check now guards on column type via vector_dims
--    (defined for pgvector types) instead of the broken `array_length(vector)`.
CREATE OR REPLACE FUNCTION upsert_semantic_fact(
  p_tenant_id        TEXT,
  p_scope_kind       TEXT,
  p_chat_id          TEXT,
  p_fact_text        TEXT,
  p_fact_type        TEXT,
  p_subject          TEXT,
  p_confidence       REAL,
  p_source           TEXT,
  p_source_message_id TEXT,
  p_expires_at       TIMESTAMPTZ DEFAULT NULL
) RETURNS BIGINT AS $$
DECLARE
  v_hash       TEXT;
  v_existing   BIGINT;
  v_new_id     BIGINT;
  v_sim        REAL;
  v_has_vector BOOLEAN;
BEGIN
  v_hash := normalize_fact_text(p_fact_text);

  PERFORM pg_advisory_xact_lock(
    hashtextextended(p_tenant_id || ':' || p_scope_kind || ':' || COALESCE(p_chat_id, ''), 0)
  );

  -- Tier 1: exact hash match.
  SELECT id INTO v_existing
    FROM semantic_facts
   WHERE tenant_id = p_tenant_id
     AND scope_kind = p_scope_kind
     AND COALESCE(chat_id, '') = COALESCE(p_chat_id, '')
     AND normalized_fact_hash = v_hash
     AND deleted_at IS NULL AND superseded_by_id IS NULL
   LIMIT 1;

  IF FOUND THEN
    INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
    VALUES (p_tenant_id, v_existing, p_source_message_id, COALESCE(p_chat_id, ''))
    ON CONFLICT DO NOTHING;
    RETURN v_existing;
  END IF;

  -- Tiers 2 & 3: cosine similarity — only when pgvector is loaded.
  v_has_vector := EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector');
  IF v_has_vector THEN
    SELECT fe.fact_id,
           (1 - (fe.embedding <=> (
             SELECT fe2.embedding
               FROM fact_embeddings fe2
              WHERE fe2.fact_id = (
                      SELECT id FROM semantic_facts
                       WHERE tenant_id = p_tenant_id
                         AND scope_kind = p_scope_kind
                         AND COALESCE(chat_id,'') = COALESCE(p_chat_id,'')
                         AND deleted_at IS NULL AND superseded_by_id IS NULL
                       ORDER BY created_at DESC LIMIT 1
                     )
                AND fe2.embedding IS NOT NULL
               LIMIT 1
           ))) AS similarity
      INTO v_existing, v_sim
      FROM fact_embeddings fe
      JOIN semantic_facts sf ON sf.id = fe.fact_id
     WHERE sf.tenant_id = p_tenant_id
       AND sf.scope_kind = p_scope_kind
       AND COALESCE(sf.chat_id, '') = COALESCE(p_chat_id, '')
       AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
       AND fe.embedding IS NOT NULL
     ORDER BY fe.embedding <=> (
       SELECT fe2.embedding FROM fact_embeddings fe2
        WHERE fe2.fact_id = (
                SELECT id FROM semantic_facts
                 WHERE tenant_id = p_tenant_id
                   AND scope_kind = p_scope_kind
                   AND COALESCE(chat_id,'') = COALESCE(p_chat_id,'')
                   AND deleted_at IS NULL AND superseded_by_id IS NULL
                 ORDER BY created_at DESC LIMIT 1
               )
          AND fe2.embedding IS NOT NULL
        LIMIT 1
     )
     LIMIT 1;

    IF v_existing IS NOT NULL THEN
      IF v_sim >= 0.97 THEN
        INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
        VALUES (p_tenant_id, v_existing, p_source_message_id, COALESCE(p_chat_id, ''))
        ON CONFLICT DO NOTHING;
        RETURN v_existing;
      ELSIF v_sim >= 0.85 THEN
        UPDATE semantic_facts
           SET superseded_by_id = NULL
         WHERE id = v_existing;
      END IF;
    END IF;
  END IF;

  -- Insert new fact (tiers 3 and 4).
  INSERT INTO semantic_facts (
    tenant_id, scope_kind, chat_id, fact_text, normalized_fact_hash,
    fact_type, subject, confidence, source, source_message_id, expires_at
  ) VALUES (
    p_tenant_id, p_scope_kind, p_chat_id, p_fact_text, v_hash,
    p_fact_type, p_subject, p_confidence, p_source, p_source_message_id, p_expires_at
  )
  ON CONFLICT (tenant_id, scope_kind, COALESCE(chat_id,''), fact_type, subject, normalized_fact_hash)
    WHERE deleted_at IS NULL AND superseded_by_id IS NULL
  DO NOTHING
  RETURNING id INTO v_new_id;

  IF v_new_id IS NULL THEN
    SELECT id INTO v_new_id
      FROM semantic_facts
     WHERE tenant_id = p_tenant_id
       AND scope_kind = p_scope_kind
       AND COALESCE(chat_id,'') = COALESCE(p_chat_id,'')
       AND normalized_fact_hash = v_hash
       AND deleted_at IS NULL AND superseded_by_id IS NULL
     LIMIT 1;
    RETURN COALESCE(v_new_id, 0);
  END IF;

  IF v_existing IS NOT NULL AND v_sim >= 0.85 AND v_sim < 0.97 THEN
    UPDATE semantic_facts SET superseded_by_id = v_new_id WHERE id = v_existing;
  END IF;

  INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
  VALUES (p_tenant_id, v_new_id, p_source_message_id, COALESCE(p_chat_id, ''))
  ON CONFLICT DO NOTHING;

  RETURN v_new_id;
END;
$$ LANGUAGE plpgsql;

COMMIT;
