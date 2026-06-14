-- 013_upsert_semantic_fact_v3.sql
-- =============================================================================
-- Rewrites upsert_semantic_fact so semantic dedup works by design (MEM-1):
--
--   * The CANDIDATE embedding is now a parameter (p_embedding). The previous
--     version compared existing facts against the embedding of the *most
--     recently created* fact of the chat — a comparison unrelated to the new
--     candidate that produced random supersedes. PostgreSQL cannot generate
--     embeddings itself, so the caller (Go) supplies the vector synchronously;
--     NULL degrades dedup to hash-only (the async worker embeds later).
--   * Tier 2/3 search is restricted to the same (fact_type, subject) pair —
--     §5.6: a conflict is same type + subject only.
--   * Supersede now atomically sets deleted_at on the old fact (§7.3); before
--     this, retrieval could return both rows until nightly consolidation.
--   * Returns (action, fact_id, superseded_fact_id) so Go no longer needs the
--     FindSupersededFactID follow-up query.
--   * When p_embedding is supplied, the new fact's row in fact_embeddings is
--     written in the same transaction so the worker does not re-embed it.
--   * The dead owner_global/health branches are gone (global scope retired in
--     migration 011), as is the no-op `SET superseded_by_id = NULL` UPDATE.
-- =============================================================================

BEGIN;

-- The return type changes (BIGINT → TABLE), so the old signature must be dropped.
DROP FUNCTION IF EXISTS upsert_semantic_fact(
  TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, REAL, TEXT, TEXT, TIMESTAMPTZ
);

DO $do$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
    EXECUTE $fn$
CREATE FUNCTION upsert_semantic_fact(
  p_tenant_id         TEXT,
  p_scope_kind        TEXT,
  p_chat_id           TEXT,
  p_fact_text         TEXT,
  p_fact_type         TEXT,
  p_subject           TEXT,
  p_confidence        REAL,
  p_source            TEXT,
  p_source_message_id TEXT,
  p_expires_at        TIMESTAMPTZ DEFAULT NULL,
  p_embedding         vector(4096) DEFAULT NULL,
  p_embedding_model   TEXT DEFAULT 'qwen/qwen3-embedding-8b'
) RETURNS TABLE(action TEXT, fact_id BIGINT, superseded_fact_id BIGINT) AS $body$
DECLARE
  v_hash     TEXT;
  v_existing BIGINT;
  v_new_id   BIGINT;
  v_sim      REAL;
BEGIN
  v_hash := normalize_fact_text(p_fact_text);

  -- Serialise writers per (tenant, scope, chat). Coarser than (fact_type,
  -- subject) on purpose: contention is per chat and the lock is transaction-
  -- scoped (§16.2).
  PERFORM pg_advisory_xact_lock(
    hashtextextended(p_tenant_id || ':' || p_scope_kind || ':' || COALESCE(p_chat_id, ''), 0)
  );

  -- Tier 1: exact normalized-hash match → record an occurrence.
  SELECT sf.id INTO v_existing
    FROM semantic_facts sf
   WHERE sf.tenant_id = p_tenant_id
     AND sf.scope_kind = p_scope_kind
     AND COALESCE(sf.chat_id, '') = COALESCE(p_chat_id, '')
     AND sf.normalized_fact_hash = v_hash
     AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
   LIMIT 1;

  IF FOUND THEN
    INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
    VALUES (p_tenant_id, v_existing, p_source_message_id, COALESCE(p_chat_id, ''))
    ON CONFLICT DO NOTHING;
    RETURN QUERY SELECT 'occurrence_added'::TEXT, v_existing, NULL::BIGINT;
    RETURN;
  END IF;

  -- Tiers 2/3: cosine similarity against the CANDIDATE embedding, restricted
  -- to active facts of the same (fact_type, subject) pair (§5.6).
  IF p_embedding IS NOT NULL THEN
    SELECT sf.id, (1 - (fe.embedding <=> p_embedding))::REAL
      INTO v_existing, v_sim
      FROM semantic_facts sf
      JOIN fact_embeddings fe ON fe.fact_id = sf.id
     WHERE sf.tenant_id = p_tenant_id
       AND sf.scope_kind = p_scope_kind
       AND COALESCE(sf.chat_id, '') = COALESCE(p_chat_id, '')
       AND sf.fact_type = p_fact_type
       AND sf.subject = p_subject
       AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
       AND fe.embedding IS NOT NULL
     ORDER BY fe.embedding <=> p_embedding
     LIMIT 1;

    IF v_existing IS NOT NULL AND v_sim >= 0.97 THEN
      -- Tier 2: semantically identical → occurrence on the existing fact.
      INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
      VALUES (p_tenant_id, v_existing, p_source_message_id, COALESCE(p_chat_id, ''))
      ON CONFLICT DO NOTHING;
      RETURN QUERY SELECT 'occurrence_added'::TEXT, v_existing, NULL::BIGINT;
      RETURN;
    END IF;
    IF v_existing IS NOT NULL AND v_sim < 0.85 THEN
      v_existing := NULL;  -- not similar enough to supersede
    END IF;
  ELSE
    v_existing := NULL;
  END IF;

  -- Insert the new fact (plain insert, or tier-3 replacement of v_existing).
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
    -- Lost a race despite the advisory lock (or partial-index edge case):
    -- treat as an occurrence on whichever row won.
    SELECT sf.id INTO v_new_id
      FROM semantic_facts sf
     WHERE sf.tenant_id = p_tenant_id
       AND sf.scope_kind = p_scope_kind
       AND COALESCE(sf.chat_id,'') = COALESCE(p_chat_id,'')
       AND sf.normalized_fact_hash = v_hash
       AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
     LIMIT 1;
    IF v_new_id IS NOT NULL THEN
      INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
      VALUES (p_tenant_id, v_new_id, p_source_message_id, COALESCE(p_chat_id, ''))
      ON CONFLICT DO NOTHING;
    END IF;
    RETURN QUERY SELECT 'occurrence_added_race'::TEXT, COALESCE(v_new_id, 0), NULL::BIGINT;
    RETURN;
  END IF;

  -- Persist the candidate embedding in the same transaction so the async
  -- embedding worker does not re-embed this fact.
  IF p_embedding IS NOT NULL THEN
    INSERT INTO fact_embeddings (fact_id, tenant_id, scope_kind, chat_id, embedding_model, embedding)
    VALUES (v_new_id, p_tenant_id, p_scope_kind, p_chat_id, p_embedding_model, p_embedding)
    ON CONFLICT (fact_id) DO UPDATE SET
      embedding = EXCLUDED.embedding,
      embedding_model = EXCLUDED.embedding_model;
  END IF;

  INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
  VALUES (p_tenant_id, v_new_id, p_source_message_id, COALESCE(p_chat_id, ''))
  ON CONFLICT DO NOTHING;

  IF v_existing IS NOT NULL THEN
    -- Tier 3: supersede — retire the old fact atomically (§7.3).
    UPDATE semantic_facts
       SET superseded_by_id = v_new_id,
           deleted_at = now()
     WHERE id = v_existing;
    RETURN QUERY SELECT 'superseded'::TEXT, v_new_id, v_existing;
    RETURN;
  END IF;

  RETURN QUERY SELECT 'inserted'::TEXT, v_new_id, NULL::BIGINT;
END;
$body$ LANGUAGE plpgsql;
    $fn$;
  ELSE
    -- pgvector unavailable: hash-only dedup; the embedding parameter is typed
    -- FLOAT8[] (ignored) so callers can pass the same arguments either way.
    EXECUTE $fn$
CREATE FUNCTION upsert_semantic_fact(
  p_tenant_id         TEXT,
  p_scope_kind        TEXT,
  p_chat_id           TEXT,
  p_fact_text         TEXT,
  p_fact_type         TEXT,
  p_subject           TEXT,
  p_confidence        REAL,
  p_source            TEXT,
  p_source_message_id TEXT,
  p_expires_at        TIMESTAMPTZ DEFAULT NULL,
  p_embedding         FLOAT8[] DEFAULT NULL,
  p_embedding_model   TEXT DEFAULT 'qwen/qwen3-embedding-8b'
) RETURNS TABLE(action TEXT, fact_id BIGINT, superseded_fact_id BIGINT) AS $body$
DECLARE
  v_hash     TEXT;
  v_existing BIGINT;
  v_new_id   BIGINT;
BEGIN
  v_hash := normalize_fact_text(p_fact_text);
  PERFORM pg_advisory_xact_lock(
    hashtextextended(p_tenant_id || ':' || p_scope_kind || ':' || COALESCE(p_chat_id, ''), 0)
  );

  SELECT sf.id INTO v_existing
    FROM semantic_facts sf
   WHERE sf.tenant_id = p_tenant_id
     AND sf.scope_kind = p_scope_kind
     AND COALESCE(sf.chat_id, '') = COALESCE(p_chat_id, '')
     AND sf.normalized_fact_hash = v_hash
     AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
   LIMIT 1;
  IF FOUND THEN
    INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
    VALUES (p_tenant_id, v_existing, p_source_message_id, COALESCE(p_chat_id, ''))
    ON CONFLICT DO NOTHING;
    RETURN QUERY SELECT 'occurrence_added'::TEXT, v_existing, NULL::BIGINT;
    RETURN;
  END IF;

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
    SELECT sf.id INTO v_new_id
      FROM semantic_facts sf
     WHERE sf.tenant_id = p_tenant_id
       AND sf.scope_kind = p_scope_kind
       AND COALESCE(sf.chat_id,'') = COALESCE(p_chat_id,'')
       AND sf.normalized_fact_hash = v_hash
       AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
     LIMIT 1;
    RETURN QUERY SELECT 'occurrence_added_race'::TEXT, COALESCE(v_new_id, 0), NULL::BIGINT;
    RETURN;
  END IF;

  INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
  VALUES (p_tenant_id, v_new_id, p_source_message_id, COALESCE(p_chat_id, ''))
  ON CONFLICT DO NOTHING;
  RETURN QUERY SELECT 'inserted'::TEXT, v_new_id, NULL::BIGINT;
END;
$body$ LANGUAGE plpgsql;
    $fn$;
  END IF;
END;
$do$;

COMMIT;
