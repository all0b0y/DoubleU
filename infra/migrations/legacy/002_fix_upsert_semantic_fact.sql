-- 002_fix_upsert_semantic_fact.sql
-- =============================================================================
-- Fixes the broken `upsert_semantic_fact` stored procedure (Bug Backlog item).
--
-- 001_clean_schema.sql guards the cosine-similarity branch with
-- `array_length(fe.embedding, 1) > 0`.  After the conditional ALTER promotes
-- `fact_embeddings.embedding` to the pgvector `vector(N)` type (line 439 of
-- 001), `array_length` is no longer defined for the column and every call to
-- `upsert_semantic_fact` raises
--   `function array_length(vector, integer) does not exist`.
--
-- The result: `semantic_facts`, `fact_embeddings`, and `fact_occurrences` are
-- silently left empty — no semantic facts can ever be persisted.
--
-- Fix: rebuild the function using `vector_dims(fe.embedding) > 0` which works
-- on the pgvector type. The function is otherwise identical to the original.
-- =============================================================================

BEGIN;

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
  v_hash     TEXT;
  v_existing BIGINT;
  v_new_id   BIGINT;
  v_sim      REAL;
  v_has_vector BOOLEAN;
BEGIN
  v_hash := normalize_fact_text(p_fact_text);

  -- Advisory lock scoped to (tenant, scope, chat) prevents concurrent races
  -- on the same chat's fact space.
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
    -- Fact already exists verbatim; record an occurrence.
    INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
    VALUES (p_tenant_id, v_existing, p_source_message_id, COALESCE(p_chat_id, ''))
    ON CONFLICT DO NOTHING;
    RETURN v_existing;
  END IF;

  -- Determine whether the embedding column is the pgvector type (so we can
  -- use vector_dims) or still the float8[] fallback (so we use array_length).
  -- Treat absence of the vector extension as "no semantic dedup possible".
  v_has_vector := EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector');

  -- Tiers 2 & 3: vector similarity (only when embeddings exist and pgvector is loaded).
  IF v_has_vector THEN
    SELECT fe.fact_id,
           (1 - (fe.embedding::vector <=> (
             SELECT fe2.embedding::vector
               FROM fact_embeddings fe2
              WHERE fe2.fact_id = (
                      SELECT id FROM semantic_facts
                       WHERE tenant_id = p_tenant_id
                         AND scope_kind = p_scope_kind
                         AND COALESCE(chat_id,'') = COALESCE(p_chat_id,'')
                         AND deleted_at IS NULL AND superseded_by_id IS NULL
                       ORDER BY created_at DESC LIMIT 1
                     )
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
       -- vector_dims is defined for pgvector `vector` (and halfvec) types,
       -- replaces the broken `array_length(fe.embedding, 1) > 0` check.
       AND vector_dims(fe.embedding::vector) > 0
     ORDER BY fe.embedding::vector <=> (
       SELECT fe2.embedding::vector FROM fact_embeddings fe2
        WHERE fe2.fact_id = (
                SELECT id FROM semantic_facts
                 WHERE tenant_id = p_tenant_id
                   AND scope_kind = p_scope_kind
                   AND COALESCE(chat_id,'') = COALESCE(p_chat_id,'')
                   AND deleted_at IS NULL AND superseded_by_id IS NULL
                 ORDER BY created_at DESC LIMIT 1
               )
        LIMIT 1
     )
     LIMIT 1;

    IF v_existing IS NOT NULL THEN
      IF v_sim >= 0.97 THEN
        -- Near-identical wording: record occurrence, keep existing.
        INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
        VALUES (p_tenant_id, v_existing, p_source_message_id, COALESCE(p_chat_id, ''))
        ON CONFLICT DO NOTHING;
        RETURN v_existing;
      ELSIF v_sim >= 0.85 THEN
        -- Similar enough to supersede: soft-delete the older fact.
        UPDATE semantic_facts
           SET superseded_by_id = NULL  -- will be set after insert
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
    -- Concurrent insert won; return its id.
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

  -- Close superseded fact with forward pointer.
  IF v_existing IS NOT NULL AND v_sim >= 0.85 AND v_sim < 0.97 THEN
    UPDATE semantic_facts SET superseded_by_id = v_new_id WHERE id = v_existing;
  END IF;

  -- Record occurrence for the new fact.
  INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
  VALUES (p_tenant_id, v_new_id, p_source_message_id, COALESCE(p_chat_id, ''))
  ON CONFLICT DO NOTHING;

  RETURN v_new_id;
END;
$$ LANGUAGE plpgsql;

COMMIT;
