-- 013_upsert_semantic_fact_v3.down.sql
-- Restores the migration-003 version of upsert_semantic_fact (BIGINT return,
-- last-created-fact similarity heuristic). The v3 signatures are dropped for
-- both the pgvector and FLOAT8[] variants.

BEGIN;

DO $do$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
    EXECUTE 'DROP FUNCTION IF EXISTS upsert_semantic_fact(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, REAL, TEXT, TEXT, TIMESTAMPTZ, vector, TEXT)';
  END IF;
  EXECUTE 'DROP FUNCTION IF EXISTS upsert_semantic_fact(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, REAL, TEXT, TEXT, TIMESTAMPTZ, FLOAT8[], TEXT)';
END;
$do$;

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
BEGIN
  v_hash := normalize_fact_text(p_fact_text);

  PERFORM pg_advisory_xact_lock(
    hashtextextended(p_tenant_id || ':' || p_scope_kind || ':' || COALESCE(p_chat_id, ''), 0)
  );

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

  INSERT INTO fact_occurrences (tenant_id, fact_id, source_message_id, chat_id)
  VALUES (p_tenant_id, v_new_id, p_source_message_id, COALESCE(p_chat_id, ''))
  ON CONFLICT DO NOTHING;

  RETURN v_new_id;
END;
$$ LANGUAGE plpgsql;

COMMIT;
