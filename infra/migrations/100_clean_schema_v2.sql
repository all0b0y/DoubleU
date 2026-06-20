-- 100_clean_schema_v2.sql — консолидированный baseline схемы v2.
--
-- Заменяет цепочку миграций 001–016 (перемещены в infra/migrations/legacy/)
-- одной полной чистой схемой. Сгенерирован применением 001–016 к чистой БД
-- (pgvector/pgvector:pg16) + pg_dump --schema-only, затем добавлены объекты v2:
--
--   * tenants.ingestion_mode  ('bot'|'personal'|'full', default 'bot') —
--     переключатель канала приёма сообщений (Bot API / Telegram Business / оба);
--   * chats.transport         ('business'|'bot', default 'bot') — канал, которым
--     пришло первое сообщение чата; ответы уходят тем же каналом;
--   * user_bots               — пользовательские бот-токены для bot-ingestion
--     (long polling getUpdates; токен НИКОГДА не пишется в логи);
--   * draft_feedback.rating расширен: up|down|approve|reject|edit|regenerate —
--     фидбек владельца о работе ассистента (источник датасета для evals).
--
-- Применяется ТОЛЬКО на чистую БД: данные из инсталляций 001–016 не мигрируются
-- (решение зафиксировано в плане — пересоздание схемы вместо миграции данных).

--
-- PostgreSQL database dump
--

-- Тела SQL-функций ссылаются на таблицы, создаваемые ниже по файлу, —
-- отключаем валидацию тел на время создания (как в оригинальном pg_dump).
SET check_function_bodies = false;

--
-- Name: litellm; Type: SCHEMA; Schema: -; Owner: -
--

CREATE SCHEMA IF NOT EXISTS litellm;

--
-- Name: pgcrypto; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

--
-- Name: vector; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;
DO $idempotent$ BEGIN

--
-- Name: chat_mode; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.chat_mode AS ENUM (
    'disabled',
    'silent',
    'manual',
    'semi_auto',
    'auto'
);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: decision; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.decision AS ENUM (
    'draft',
    'auto_reply',
    'ignore',
    'observe',
    'ask_owner',
    'error_fallback'
);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: idempotency_operation; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.idempotency_operation AS ENUM (
    'receive_update',
    'send_reply',
    'approval_callback'
);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: message_state; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.message_state AS ENUM (
    'received',
    'normalized',
    'policy_checked',
    'ai_requested',
    'ai_generated',
    'decision_made',
    'draft_sent_to_owner',
    'approved_by_owner',
    'sent_to_recipient',
    'ignored',
    'failed'
);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: privacy_flag; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.privacy_flag AS ENUM (
    'no_memory',
    'no_training',
    'private',
    'no_summary'
);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: risk_level; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.risk_level AS ENUM (
    'low',
    'medium',
    'high',
    'critical'
);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;

--
-- Name: bootstrap_chat_settings(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE OR REPLACE FUNCTION public.bootstrap_chat_settings() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.status = 'managed' THEN
    INSERT INTO chat_settings (tenant_id, chat_id, mode)
    VALUES (NEW.tenant_id, NEW.chat_id, 'manual')
    ON CONFLICT DO NOTHING;
  END IF;
  RETURN NEW;
END;
$$;

--
-- Name: bootstrap_user_settings(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE OR REPLACE FUNCTION public.bootstrap_user_settings() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.owner_user_id IS NOT NULL THEN
    INSERT INTO user_settings (user_id, tenant_id)
    VALUES (NEW.owner_user_id, NEW.id)
    ON CONFLICT DO NOTHING;
  END IF;
  RETURN NEW;
END;
$$;

--
-- Name: consolidate_facts(text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE OR REPLACE FUNCTION public.consolidate_facts(p_tenant_id text) RETURNS integer
    LANGUAGE plpgsql
    AS $$
DECLARE
  v_cleaned INT := 0;
  v_rows    INT := 0;
BEGIN
  -- 1. Hard-delete expired facts.
  DELETE FROM semantic_facts
  WHERE tenant_id = p_tenant_id
    AND expires_at IS NOT NULL
    AND expires_at < now()
    AND deleted_at IS NULL;
  GET DIAGNOSTICS v_rows = ROW_COUNT;
  v_cleaned := v_cleaned + v_rows;

  -- 2. Soft-delete facts that are superseded but not yet marked deleted.
  UPDATE semantic_facts
     SET deleted_at = now()
   WHERE tenant_id = p_tenant_id
     AND superseded_by_id IS NOT NULL
     AND deleted_at IS NULL;
  GET DIAGNOSTICS v_rows = ROW_COUNT;
  v_cleaned := v_cleaned + v_rows;

  -- 3. Collapse supersession chains: if A→B→C exists, update A.superseded_by_id = C.
  --    Iterate until no more updates (handles arbitrary chain depth).
  LOOP
    WITH chain_step AS (
      SELECT sf.id AS old_id, sf2.superseded_by_id AS new_target
        FROM semantic_facts sf
        JOIN semantic_facts sf2 ON sf2.id = sf.superseded_by_id
       WHERE sf.tenant_id = p_tenant_id
         AND sf2.superseded_by_id IS NOT NULL
         AND sf.superseded_by_id != sf2.superseded_by_id
    )
    UPDATE semantic_facts sf
       SET superseded_by_id = cs.new_target
      FROM chain_step cs
     WHERE sf.id = cs.old_id;

    GET DIAGNOSTICS v_rows = ROW_COUNT;
    EXIT WHEN v_rows = 0;
  END LOOP;

  RETURN v_cleaned;
END;
$$;

--
-- Name: normalize_fact_text(text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE OR REPLACE FUNCTION public.normalize_fact_text(p_text text) RETURNS text
    LANGUAGE sql IMMUTABLE
    AS $$
  SELECT encode(digest(regexp_replace(lower(trim(p_text)), '\s+', ' ', 'g'), 'sha256'), 'hex');
$$;

--
-- Name: prevent_direct_global_health_insert(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE OR REPLACE FUNCTION public.prevent_direct_global_health_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.scope_kind = 'owner_global'
     AND NEW.fact_type = 'health'
     AND NEW.source = 'llm_extracted' THEN
    RAISE EXCEPTION 'Global health facts must go through pending_global_health_facts confirmation flow'
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END;
$$;

--
-- Name: upsert_semantic_fact(text, text, text, text, text, text, real, text, text, timestamp with time zone, public.vector, text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE OR REPLACE FUNCTION public.upsert_semantic_fact(p_tenant_id text, p_scope_kind text, p_chat_id text, p_fact_text text, p_fact_type text, p_subject text, p_confidence real, p_source text, p_source_message_id text, p_expires_at timestamp with time zone DEFAULT NULL::timestamp with time zone, p_embedding public.vector DEFAULT NULL::public.vector, p_embedding_model text DEFAULT 'qwen/qwen3-embedding-8b'::text) RETURNS TABLE(action text, fact_id bigint, superseded_fact_id bigint)
    LANGUAGE plpgsql
    AS $$
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
    ON CONFLICT ON CONSTRAINT fact_embeddings_pkey DO UPDATE SET
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
$$;

--
-- Name: view_chat_greeted_today(text, text, text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE OR REPLACE FUNCTION public.view_chat_greeted_today(p_tenant text, p_chat text, p_owner_id text) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
  SELECT EXISTS (
    SELECT 1 FROM messages
     WHERE tenant_id = p_tenant AND chat_id = p_chat
       AND sender_id = p_owner_id
       AND received_at >= date_trunc('day', now())
  );
$$;

--
-- Name: view_chat_paused_hours(text, text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE OR REPLACE FUNCTION public.view_chat_paused_hours(p_tenant text, p_chat text) RETURNS numeric
    LANGUAGE sql STABLE
    AS $$
  SELECT EXTRACT(EPOCH FROM (now() - MAX(received_at))) / 3600
    FROM messages WHERE tenant_id = p_tenant AND chat_id = p_chat;
$$;

--
-- Name: view_open_commitments(text, text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE OR REPLACE FUNCTION public.view_open_commitments(p_tenant text, p_chat text) RETURNS TABLE(fact_id bigint, fact_text text, expires_at timestamp with time zone)
    LANGUAGE sql STABLE
    AS $$
  SELECT id, fact_text, expires_at
    FROM semantic_facts
   WHERE tenant_id = p_tenant AND chat_id = p_chat
     AND fact_type = 'commitment'
     AND deleted_at IS NULL AND superseded_by_id IS NULL
     AND (expires_at IS NULL OR expires_at > now());
$$;

--
-- Name: active_episodes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.active_episodes (
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    last_message_at timestamp with time zone NOT NULL,
    message_count integer DEFAULT 0 NOT NULL,
    current_topic text,
    open_question_message_id text,
    pending_owner_action text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: ai_roles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.ai_roles (
    id bigint NOT NULL,
    tenant_id text NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    speaking_style text DEFAULT ''::text NOT NULL,
    goals text DEFAULT ''::text NOT NULL,
    constraints_text text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ai_roles_name_check CHECK (((length(name) >= 1) AND (length(name) <= 64)))
);

--
-- Name: ai_roles_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.ai_roles_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: ai_roles_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.ai_roles_id_seq OWNED BY public.ai_roles.id;

--
-- Name: audit_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.audit_events (
    audit_id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    source_message_id text NOT NULL,
    decision public.decision NOT NULL,
    risk_level public.risk_level,
    reason text NOT NULL,
    privacy_flags public.privacy_flag[] DEFAULT ARRAY[]::public.privacy_flag[] NOT NULL,
    policy_snapshot jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    trace_id text
);

--
-- Name: chat_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.chat_settings (
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    mode public.chat_mode NOT NULL,
    privacy_flags public.privacy_flag[] DEFAULT ARRAY[]::public.privacy_flag[] NOT NULL,
    auto_reply_min_confidence numeric(4,3) DEFAULT 0.920 NOT NULL,
    semi_auto_min_confidence numeric(4,3) DEFAULT 0.850 NOT NULL,
    settings_json jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    ai_role_id bigint,
    CONSTRAINT chat_settings_auto_reply_min_confidence_check CHECK (((auto_reply_min_confidence >= (0)::numeric) AND (auto_reply_min_confidence <= (1)::numeric))),
    CONSTRAINT chat_settings_semi_auto_min_confidence_check CHECK (((semi_auto_min_confidence >= (0)::numeric) AND (semi_auto_min_confidence <= (1)::numeric)))
);

--
-- Name: chats; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.chats (
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    telegram_chat_id text NOT NULL,
    chat_type text DEFAULT 'private'::text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'managed'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    transport text DEFAULT 'bot'::text NOT NULL,
    CONSTRAINT chats_status_check CHECK ((status = ANY (ARRAY['unclaimed'::text, 'managed'::text, 'dismissed'::text, 'archived'::text]))),
    CONSTRAINT chats_transport_check CHECK ((transport = ANY (ARRAY['business'::text, 'bot'::text])))
);

--
-- Name: contact_profile_suggestions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.contact_profile_suggestions (
    id bigint NOT NULL,
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    field_name text NOT NULL,
    suggested_value jsonb NOT NULL,
    rationale text,
    source_window jsonb,
    status text DEFAULT 'pending'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    reviewed_at timestamp with time zone,
    CONSTRAINT contact_profile_suggestions_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'accepted'::text, 'rejected'::text, 'expired'::text])))
);

--
-- Name: contact_profile_suggestions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.contact_profile_suggestions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: contact_profile_suggestions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.contact_profile_suggestions_id_seq OWNED BY public.contact_profile_suggestions.id;

--
-- Name: contact_profiles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.contact_profiles (
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    gender text DEFAULT ''::text NOT NULL,
    role text DEFAULT ''::text NOT NULL,
    relationship_type text DEFAULT ''::text NOT NULL,
    notes text DEFAULT ''::text NOT NULL,
    display_names text[] DEFAULT '{}'::text[] NOT NULL,
    preferred_address text,
    language text,
    contact_timezone text,
    tone text,
    emoji_usage text,
    address_form text,
    style_notes text,
    topics text[] DEFAULT '{}'::text[] NOT NULL,
    chat_context text,
    do_not_discuss text[] DEFAULT '{}'::text[] NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT contact_profiles_address_form_check CHECK ((address_form = ANY (ARRAY['ty'::text, 'vy'::text]))),
    CONSTRAINT contact_profiles_emoji_usage_check CHECK ((emoji_usage = ANY (ARRAY['none'::text, 'rare'::text, 'frequent'::text]))),
    CONSTRAINT contact_profiles_tone_check CHECK ((tone = ANY (ARRAY['formal'::text, 'neutral'::text, 'casual'::text])))
);

--
-- Name: fact_relations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.fact_relations (
    id bigint NOT NULL,
    tenant_id text NOT NULL,
    fact_id_from bigint NOT NULL,
    fact_id_to bigint NOT NULL,
    relation_type text NOT NULL,
    valid_from timestamp with time zone DEFAULT now() NOT NULL,
    valid_until timestamp with time zone,
    confidence real DEFAULT 1.0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT fact_relations_confidence_check CHECK (((confidence >= (0)::double precision) AND (confidence <= (1)::double precision))),
    CONSTRAINT fact_relations_relation_type_check CHECK ((relation_type = ANY (ARRAY['supersedes'::text, 'fulfills'::text, 'contradicts'::text, 'updates'::text, 'related'::text])))
);

--
-- Name: semantic_facts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.semantic_facts (
    id bigint NOT NULL,
    tenant_id text NOT NULL,
    scope_kind text NOT NULL,
    chat_id text,
    fact_text text NOT NULL,
    normalized_fact_hash text NOT NULL,
    fact_type text NOT NULL,
    subject text NOT NULL,
    confidence real NOT NULL,
    source text NOT NULL,
    source_message_id text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    expires_at timestamp with time zone,
    superseded_by_id bigint,
    deleted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    fact_tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple'::regconfig, fact_text)) STORED,
    CONSTRAINT scope_chat_requires_chat_id CHECK (((scope_kind <> 'chat'::text) OR (chat_id IS NOT NULL))),
    CONSTRAINT scope_global_no_chat_id CHECK (((scope_kind <> 'owner_global'::text) OR (chat_id IS NULL))),
    CONSTRAINT scope_global_only_owner CHECK (((scope_kind <> 'owner_global'::text) OR (subject = 'owner'::text))),
    CONSTRAINT semantic_facts_confidence_check CHECK (((confidence >= (0)::double precision) AND (confidence <= (1)::double precision))),
    CONSTRAINT semantic_facts_fact_text_check CHECK (((char_length(fact_text) >= 5) AND (char_length(fact_text) <= 200))),
    CONSTRAINT semantic_facts_fact_text_check1 CHECK ((fact_text !~~ '[FACT]%'::text)),
    CONSTRAINT semantic_facts_fact_text_check2 CHECK ((fact_text !~~ '[EXTRACTED]%'::text)),
    CONSTRAINT semantic_facts_fact_text_check3 CHECK ((fact_text !~~ '[OWNER STYLE%'::text)),
    CONSTRAINT semantic_facts_fact_type_check CHECK ((fact_type = ANY (ARRAY['preference'::text, 'biographical'::text, 'commitment'::text, 'event'::text, 'relationship'::text, 'health'::text, 'taboo'::text]))),
    CONSTRAINT semantic_facts_scope_kind_check CHECK ((scope_kind = ANY (ARRAY['chat'::text, 'owner_global'::text]))),
    CONSTRAINT semantic_facts_source_check CHECK ((source = ANY (ARRAY['llm_extracted'::text, 'owner_confirmed'::text, 'owner_authored'::text]))),
    CONSTRAINT semantic_facts_subject_check CHECK ((subject = ANY (ARRAY['contact'::text, 'owner'::text, 'relationship'::text, 'third_party'::text])))
);

--
-- Name: current_facts; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.current_facts AS
 SELECT id,
    tenant_id,
    scope_kind,
    chat_id,
    fact_text,
    normalized_fact_hash,
    fact_type,
    subject,
    confidence,
    source,
    source_message_id,
    metadata,
    expires_at,
    superseded_by_id,
    deleted_at,
    created_at,
    updated_at,
    fact_tsv
   FROM public.semantic_facts sf
  WHERE ((deleted_at IS NULL) AND (superseded_by_id IS NULL) AND (NOT (EXISTS ( SELECT 1
           FROM public.fact_relations fr
          WHERE ((fr.fact_id_from = sf.id) AND (fr.relation_type = 'supersedes'::text) AND (fr.valid_until IS NULL))))));

--
-- Name: draft_feedback; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.draft_feedback (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    draft_id text NOT NULL,
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    rating text NOT NULL,
    feedback_text text DEFAULT ''::text NOT NULL,
    response_text text DEFAULT ''::text NOT NULL,
    model_used text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT draft_feedback_rating_check CHECK ((rating = ANY (ARRAY['up'::text, 'down'::text, 'approve'::text, 'reject'::text, 'edit'::text, 'regenerate'::text])))
);

--
-- Name: drafts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.drafts (
    draft_id text NOT NULL,
    decision_id text DEFAULT ''::text NOT NULL,
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    business_connection_id text DEFAULT ''::text NOT NULL,
    source_message_id text DEFAULT ''::text NOT NULL,
    source_message_text text DEFAULT ''::text NOT NULL,
    response_text text DEFAULT ''::text NOT NULL,
    draft_type text DEFAULT 'reply'::text NOT NULL,
    suggested_options text[] DEFAULT '{}'::text[] NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    owner_chat_id text DEFAULT ''::text NOT NULL,
    owner_message_id text DEFAULT ''::text NOT NULL,
    sent_message_id text DEFAULT ''::text NOT NULL,
    action_history text[] DEFAULT '{}'::text[] NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    trace_id text,
    CONSTRAINT drafts_draft_type_check CHECK ((draft_type = ANY (ARRAY['reply'::text, 'question'::text]))),
    CONSTRAINT drafts_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'approved'::text, 'rejected'::text, 'sent'::text])))
);

--
-- Name: episode_summaries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.episode_summaries (
    id bigint NOT NULL,
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    period_start timestamp with time zone NOT NULL,
    period_end timestamp with time zone NOT NULL,
    content text NOT NULL,
    source_message_ids text[] DEFAULT '{}'::text[] NOT NULL,
    message_count integer DEFAULT 0 NOT NULL,
    closure_reason text DEFAULT 'manual'::text NOT NULL,
    privacy_flags jsonb DEFAULT '{}'::jsonb NOT NULL,
    deleted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    embedding public.vector(4096),
    embedding_model text,
    CONSTRAINT episode_summaries_closure_reason_check CHECK ((closure_reason = ANY (ARRAY['silence_gap'::text, 'message_count'::text, 'topic_shift'::text, 'manual'::text, 'daily_summary'::text]))),
    CONSTRAINT episode_summaries_content_check CHECK (((char_length(content) >= 50) AND (char_length(content) <= 1500)))
);

--
-- Name: episode_summaries_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.episode_summaries_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: episode_summaries_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.episode_summaries_id_seq OWNED BY public.episode_summaries.id;

--
-- Name: extraction_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.extraction_log (
    id bigint NOT NULL,
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    input_message_ids text[] DEFAULT '{}'::text[] NOT NULL,
    model_used text DEFAULT ''::text NOT NULL,
    raw_output jsonb DEFAULT '[]'::jsonb NOT NULL,
    facts_created bigint[] DEFAULT '{}'::bigint[] NOT NULL,
    facts_skipped integer DEFAULT 0 NOT NULL,
    facts_merged integer DEFAULT 0 NOT NULL,
    error text,
    duration_ms integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: extraction_log_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.extraction_log_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: extraction_log_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.extraction_log_id_seq OWNED BY public.extraction_log.id;

--
-- Name: fact_embeddings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.fact_embeddings (
    fact_id bigint NOT NULL,
    tenant_id text NOT NULL,
    scope_kind text NOT NULL,
    chat_id text,
    embedding_model text DEFAULT 'qwen/qwen3-embedding-8b'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    embedding public.vector(4096),
    CONSTRAINT fact_embeddings_scope_kind_check CHECK ((scope_kind = ANY (ARRAY['chat'::text, 'owner_global'::text])))
);

--
-- Name: fact_occurrences; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.fact_occurrences (
    tenant_id text NOT NULL,
    fact_id bigint NOT NULL,
    source_message_id text NOT NULL,
    chat_id text NOT NULL,
    observed_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: fact_relations_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.fact_relations_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: fact_relations_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.fact_relations_id_seq OWNED BY public.fact_relations.id;

--
-- Name: idempotency_records; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.idempotency_records (
    idempotency_key text NOT NULL,
    operation public.idempotency_operation NOT NULL,
    tenant_id text,
    chat_id text,
    source_message_id text,
    decision_id text,
    request_hash text,
    response_json jsonb,
    locked_until timestamp with time zone,
    completed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT idempotency_records_check CHECK (((operation <> 'send_reply'::public.idempotency_operation) OR ((tenant_id IS NOT NULL) AND (chat_id IS NOT NULL) AND (source_message_id IS NOT NULL) AND (decision_id IS NOT NULL))))
);

--
-- Name: message_states; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.message_states (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    source_message_id text NOT NULL,
    state public.message_state NOT NULL,
    state_reason text,
    actor text DEFAULT 'system'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    trace_id text
);

--
-- Name: messages; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.messages (
    tenant_id text NOT NULL,
    chat_id text NOT NULL,
    source_message_id text NOT NULL,
    event_id text NOT NULL,
    telegram_update_id bigint NOT NULL,
    business_connection_id text NOT NULL,
    sender_id text NOT NULL,
    message_text text NOT NULL,
    message_type text DEFAULT 'text'::text NOT NULL,
    risk_level public.risk_level DEFAULT 'low'::public.risk_level NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: semantic_facts_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.semantic_facts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: semantic_facts_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.semantic_facts_id_seq OWNED BY public.semantic_facts.id;

--
-- Name: telegram_business_connections; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.telegram_business_connections (
    id text NOT NULL,
    tenant_id text NOT NULL,
    telegram_connection_id text NOT NULL,
    telegram_user_id text NOT NULL,
    permissions_json jsonb DEFAULT '{}'::jsonb NOT NULL,
    is_active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: tenants; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.tenants (
    id text NOT NULL,
    owner_user_id text,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    ingestion_mode text DEFAULT 'bot'::text NOT NULL,
    CONSTRAINT tenants_ingestion_mode_check CHECK ((ingestion_mode = ANY (ARRAY['bot'::text, 'personal'::text, 'full'::text])))
);

--
-- Name: unclaimed_chats; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.unclaimed_chats (
    staging_id text NOT NULL,
    owner_tenant_id text,
    business_connection_id text NOT NULL,
    chat_id text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    chat_type text DEFAULT 'private'::text NOT NULL,
    first_message_at timestamp with time zone DEFAULT now() NOT NULL,
    last_message_at timestamp with time zone DEFAULT now() NOT NULL,
    message_count integer DEFAULT 1 NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    claimed_at timestamp with time zone,
    dismissed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT unclaimed_chats_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'claimed'::text, 'dismissed'::text, 'expired'::text])))
);

--
-- Name: user_bots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.user_bots (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id text NOT NULL,
    bot_id bigint NOT NULL,
    bot_username text DEFAULT ''::text NOT NULL,
    token text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    last_update_offset bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_bots_status_check CHECK ((status = ANY (ARRAY['active'::text, 'disabled'::text, 'invalid'::text])))
);

--
-- Name: user_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.user_settings (
    user_id text NOT NULL,
    tenant_id text NOT NULL,
    display_name text,
    bio text,
    timezone text,
    primary_language text DEFAULT 'ru'::text NOT NULL,
    secondary_languages text[] DEFAULT '{}'::text[] NOT NULL,
    default_mode_for_new_chats text DEFAULT 'manual'::text NOT NULL,
    learning_enabled boolean DEFAULT true NOT NULL,
    language_for_drafts text DEFAULT 'auto'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_settings_default_mode_for_new_chats_check CHECK ((default_mode_for_new_chats = ANY (ARRAY['off'::text, 'observe'::text, 'manual'::text, 'semi_auto'::text, 'auto'::text]))),
    CONSTRAINT user_settings_language_for_drafts_check CHECK ((language_for_drafts = ANY (ARRAY['auto'::text, 'primary'::text])))
);

--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.users (
    id text NOT NULL,
    telegram_user_id text,
    email text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: ai_roles id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ai_roles ALTER COLUMN id SET DEFAULT nextval('public.ai_roles_id_seq'::regclass);

--
-- Name: contact_profile_suggestions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.contact_profile_suggestions ALTER COLUMN id SET DEFAULT nextval('public.contact_profile_suggestions_id_seq'::regclass);

--
-- Name: episode_summaries id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.episode_summaries ALTER COLUMN id SET DEFAULT nextval('public.episode_summaries_id_seq'::regclass);

--
-- Name: extraction_log id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.extraction_log ALTER COLUMN id SET DEFAULT nextval('public.extraction_log_id_seq'::regclass);

--
-- Name: fact_relations id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fact_relations ALTER COLUMN id SET DEFAULT nextval('public.fact_relations_id_seq'::regclass);

--
-- Name: semantic_facts id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.semantic_facts ALTER COLUMN id SET DEFAULT nextval('public.semantic_facts_id_seq'::regclass);
DO $idempotent$ BEGIN

--
-- Name: active_episodes active_episodes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.active_episodes
    ADD CONSTRAINT active_episodes_pkey PRIMARY KEY (tenant_id, chat_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: ai_roles ai_roles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ai_roles
    ADD CONSTRAINT ai_roles_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: ai_roles ai_roles_tenant_id_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ai_roles
    ADD CONSTRAINT ai_roles_tenant_id_name_key UNIQUE (tenant_id, name);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: audit_events audit_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_pkey PRIMARY KEY (audit_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: audit_events audit_events_tenant_id_chat_id_source_message_id_decision_r_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_tenant_id_chat_id_source_message_id_decision_r_key UNIQUE (tenant_id, chat_id, source_message_id, decision, reason);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: chat_settings chat_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_settings
    ADD CONSTRAINT chat_settings_pkey PRIMARY KEY (tenant_id, chat_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: chats chats_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chats
    ADD CONSTRAINT chats_pkey PRIMARY KEY (tenant_id, chat_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: chats chats_tenant_id_telegram_chat_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chats
    ADD CONSTRAINT chats_tenant_id_telegram_chat_id_key UNIQUE (tenant_id, telegram_chat_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: contact_profile_suggestions contact_profile_suggestions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.contact_profile_suggestions
    ADD CONSTRAINT contact_profile_suggestions_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: contact_profiles contact_profiles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.contact_profiles
    ADD CONSTRAINT contact_profiles_pkey PRIMARY KEY (tenant_id, chat_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: draft_feedback draft_feedback_draft_id_rating_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.draft_feedback
    ADD CONSTRAINT draft_feedback_draft_id_rating_key UNIQUE (draft_id, rating);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: draft_feedback draft_feedback_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.draft_feedback
    ADD CONSTRAINT draft_feedback_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: drafts drafts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.drafts
    ADD CONSTRAINT drafts_pkey PRIMARY KEY (draft_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: episode_summaries episode_summaries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.episode_summaries
    ADD CONSTRAINT episode_summaries_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: extraction_log extraction_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.extraction_log
    ADD CONSTRAINT extraction_log_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: fact_embeddings fact_embeddings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fact_embeddings
    ADD CONSTRAINT fact_embeddings_pkey PRIMARY KEY (fact_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: fact_occurrences fact_occurrences_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fact_occurrences
    ADD CONSTRAINT fact_occurrences_pkey PRIMARY KEY (fact_id, source_message_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: fact_relations fact_relations_fact_id_from_fact_id_to_relation_type_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fact_relations
    ADD CONSTRAINT fact_relations_fact_id_from_fact_id_to_relation_type_key UNIQUE (fact_id_from, fact_id_to, relation_type);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: fact_relations fact_relations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fact_relations
    ADD CONSTRAINT fact_relations_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: idempotency_records idempotency_records_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.idempotency_records
    ADD CONSTRAINT idempotency_records_pkey PRIMARY KEY (idempotency_key);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: message_states message_states_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.message_states
    ADD CONSTRAINT message_states_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: message_states message_states_tenant_id_chat_id_source_message_id_state_ac_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.message_states
    ADD CONSTRAINT message_states_tenant_id_chat_id_source_message_id_state_ac_key UNIQUE (tenant_id, chat_id, source_message_id, state, actor);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: messages messages_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.messages
    ADD CONSTRAINT messages_pkey PRIMARY KEY (tenant_id, chat_id, source_message_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: messages messages_telegram_update_id_business_connection_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.messages
    ADD CONSTRAINT messages_telegram_update_id_business_connection_id_key UNIQUE (telegram_update_id, business_connection_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: semantic_facts semantic_facts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.semantic_facts
    ADD CONSTRAINT semantic_facts_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: telegram_business_connections telegram_business_connections_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.telegram_business_connections
    ADD CONSTRAINT telegram_business_connections_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: telegram_business_connections telegram_business_connections_telegram_connection_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.telegram_business_connections
    ADD CONSTRAINT telegram_business_connections_telegram_connection_id_key UNIQUE (telegram_connection_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: telegram_business_connections telegram_business_connections_tenant_id_telegram_connection_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.telegram_business_connections
    ADD CONSTRAINT telegram_business_connections_tenant_id_telegram_connection_key UNIQUE (tenant_id, telegram_connection_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: tenants tenants_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenants
    ADD CONSTRAINT tenants_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: unclaimed_chats unclaimed_chats_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.unclaimed_chats
    ADD CONSTRAINT unclaimed_chats_pkey PRIMARY KEY (staging_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: user_bots user_bots_bot_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_bots
    ADD CONSTRAINT user_bots_bot_id_key UNIQUE (bot_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: user_bots user_bots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_bots
    ADD CONSTRAINT user_bots_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: user_bots user_bots_tenant_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_bots
    ADD CONSTRAINT user_bots_tenant_id_key UNIQUE (tenant_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: user_settings user_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_settings
    ADD CONSTRAINT user_settings_pkey PRIMARY KEY (user_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: users users_email_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_email_key UNIQUE (email);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: users users_telegram_user_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_telegram_user_id_key UNIQUE (telegram_user_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;

--
-- Name: idx_ai_roles_tenant; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_ai_roles_tenant ON public.ai_roles USING btree (tenant_id);

--
-- Name: idx_audit_events_tenant_chat_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_chat_created ON public.audit_events USING btree (tenant_id, chat_id, created_at DESC);

--
-- Name: idx_audit_events_trace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_audit_events_trace_id ON public.audit_events USING btree (trace_id) WHERE (trace_id IS NOT NULL);

--
-- Name: idx_chat_settings_privacy_flags; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_chat_settings_privacy_flags ON public.chat_settings USING gin (privacy_flags);

--
-- Name: idx_chats_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_chats_status ON public.chats USING btree (tenant_id, status);

--
-- Name: idx_chats_tenant; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_chats_tenant ON public.chats USING btree (tenant_id);

--
-- Name: idx_draft_feedback_chat; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_draft_feedback_chat ON public.draft_feedback USING btree (tenant_id, chat_id, created_at DESC);

--
-- Name: idx_drafts_source_msg; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_drafts_source_msg ON public.drafts USING btree (tenant_id, chat_id, source_message_id);

--
-- Name: idx_drafts_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_drafts_status ON public.drafts USING btree (tenant_id, status) WHERE (status = 'pending'::text);

--
-- Name: idx_drafts_tenant_chat; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_drafts_tenant_chat ON public.drafts USING btree (tenant_id, chat_id, created_at DESC);

--
-- Name: idx_drafts_trace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_drafts_trace_id ON public.drafts USING btree (trace_id) WHERE (trace_id IS NOT NULL);

--
-- Name: idx_idempotency_records_operation; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_idempotency_records_operation ON public.idempotency_records USING btree (operation, created_at DESC);

--
-- Name: idx_message_states_lookup; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_message_states_lookup ON public.message_states USING btree (tenant_id, chat_id, source_message_id, created_at DESC);

--
-- Name: idx_message_states_trace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_message_states_trace_id ON public.message_states USING btree (trace_id) WHERE (trace_id IS NOT NULL);

--
-- Name: idx_messages_tenant_chat_received; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_messages_tenant_chat_received ON public.messages USING btree (tenant_id, chat_id, received_at DESC);

--
-- Name: idx_user_bots_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_user_bots_status ON public.user_bots USING btree (status);

--
-- Name: ix_ae_stale; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_ae_stale ON public.active_episodes USING btree (last_message_at);

--
-- Name: ix_cps_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX IF NOT EXISTS ix_cps_pending ON public.contact_profile_suggestions USING btree (tenant_id, chat_id, field_name) WHERE (status = 'pending'::text);

--
-- Name: ix_es_chat; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_es_chat ON public.episode_summaries USING btree (tenant_id, chat_id, period_end DESC) WHERE (deleted_at IS NULL);

--
-- Name: ix_fe_chat_scope; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_fe_chat_scope ON public.fact_embeddings USING btree (tenant_id, chat_id) WHERE (scope_kind = 'chat'::text);

--
-- Name: ix_fe_global_scope; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_fe_global_scope ON public.fact_embeddings USING btree (tenant_id) WHERE (scope_kind = 'owner_global'::text);

--
-- Name: ix_fo_fact; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_fo_fact ON public.fact_occurrences USING btree (fact_id);

--
-- Name: ix_fr_from; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_fr_from ON public.fact_relations USING btree (tenant_id, fact_id_from);

--
-- Name: ix_fr_to; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_fr_to ON public.fact_relations USING btree (tenant_id, fact_id_to);

--
-- Name: ix_sf_chat_active; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_sf_chat_active ON public.semantic_facts USING btree (tenant_id, chat_id, fact_type) WHERE ((scope_kind = 'chat'::text) AND (deleted_at IS NULL) AND (superseded_by_id IS NULL));

--
-- Name: ix_sf_commitments; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_sf_commitments ON public.semantic_facts USING btree (tenant_id, chat_id, expires_at) WHERE ((fact_type = 'commitment'::text) AND (deleted_at IS NULL));

--
-- Name: ix_sf_fts; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_sf_fts ON public.semantic_facts USING gin (fact_tsv) WHERE ((deleted_at IS NULL) AND (superseded_by_id IS NULL));

--
-- Name: ix_sf_global_active; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_sf_global_active ON public.semantic_facts USING btree (tenant_id, fact_type) WHERE ((scope_kind = 'owner_global'::text) AND (deleted_at IS NULL) AND (superseded_by_id IS NULL));

--
-- Name: ix_uc_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_uc_pending ON public.unclaimed_chats USING btree (owner_tenant_id) WHERE (status = 'pending'::text);

--
-- Name: ix_user_settings_tenant; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS ix_user_settings_tenant ON public.user_settings USING btree (tenant_id);

--
-- Name: ux_sf_normalized_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX IF NOT EXISTS ux_sf_normalized_active ON public.semantic_facts USING btree (tenant_id, scope_kind, COALESCE(chat_id, ''::text), fact_type, subject, normalized_fact_hash) WHERE ((deleted_at IS NULL) AND (superseded_by_id IS NULL));

--
-- Name: ux_uc_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX IF NOT EXISTS ux_uc_active ON public.unclaimed_chats USING btree (business_connection_id, chat_id) WHERE (status = 'pending'::text);

--
-- Name: chats trg_bootstrap_chat_settings; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_bootstrap_chat_settings AFTER INSERT OR UPDATE OF status ON public.chats FOR EACH ROW WHEN ((new.status = 'managed'::text)) EXECUTE FUNCTION public.bootstrap_chat_settings();
DROP TRIGGER IF EXISTS trg_bootstrap_user_settings ON public.tenants;

--
-- Name: tenants trg_bootstrap_user_settings; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_bootstrap_user_settings AFTER INSERT ON public.tenants FOR EACH ROW EXECUTE FUNCTION public.bootstrap_user_settings();
DROP TRIGGER IF EXISTS trg_no_direct_global_health ON public.semantic_facts;

--
-- Name: semantic_facts trg_no_direct_global_health; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_no_direct_global_health BEFORE INSERT ON public.semantic_facts FOR EACH ROW EXECUTE FUNCTION public.prevent_direct_global_health_insert();
DO $idempotent$ BEGIN

--
-- Name: active_episodes active_episodes_tenant_id_chat_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.active_episodes
    ADD CONSTRAINT active_episodes_tenant_id_chat_id_fkey FOREIGN KEY (tenant_id, chat_id) REFERENCES public.chats(tenant_id, chat_id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: ai_roles ai_roles_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ai_roles
    ADD CONSTRAINT ai_roles_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: audit_events audit_events_tenant_id_chat_id_source_message_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_tenant_id_chat_id_source_message_id_fkey FOREIGN KEY (tenant_id, chat_id, source_message_id) REFERENCES public.messages(tenant_id, chat_id, source_message_id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: chat_settings chat_settings_ai_role_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_settings
    ADD CONSTRAINT chat_settings_ai_role_id_fkey FOREIGN KEY (ai_role_id) REFERENCES public.ai_roles(id) ON DELETE SET NULL;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: chat_settings chat_settings_tenant_id_chat_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_settings
    ADD CONSTRAINT chat_settings_tenant_id_chat_id_fkey FOREIGN KEY (tenant_id, chat_id) REFERENCES public.chats(tenant_id, chat_id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: chats chats_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chats
    ADD CONSTRAINT chats_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: contact_profiles contact_profiles_tenant_id_chat_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.contact_profiles
    ADD CONSTRAINT contact_profiles_tenant_id_chat_id_fkey FOREIGN KEY (tenant_id, chat_id) REFERENCES public.chats(tenant_id, chat_id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: draft_feedback draft_feedback_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.draft_feedback
    ADD CONSTRAINT draft_feedback_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: drafts drafts_tenant_id_chat_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.drafts
    ADD CONSTRAINT drafts_tenant_id_chat_id_fkey FOREIGN KEY (tenant_id, chat_id) REFERENCES public.chats(tenant_id, chat_id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: drafts drafts_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.drafts
    ADD CONSTRAINT drafts_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: episode_summaries episode_summaries_tenant_id_chat_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.episode_summaries
    ADD CONSTRAINT episode_summaries_tenant_id_chat_id_fkey FOREIGN KEY (tenant_id, chat_id) REFERENCES public.chats(tenant_id, chat_id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: fact_embeddings fact_embeddings_fact_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fact_embeddings
    ADD CONSTRAINT fact_embeddings_fact_id_fkey FOREIGN KEY (fact_id) REFERENCES public.semantic_facts(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: fact_occurrences fact_occurrences_fact_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fact_occurrences
    ADD CONSTRAINT fact_occurrences_fact_id_fkey FOREIGN KEY (fact_id) REFERENCES public.semantic_facts(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: fact_relations fact_relations_fact_id_from_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fact_relations
    ADD CONSTRAINT fact_relations_fact_id_from_fkey FOREIGN KEY (fact_id_from) REFERENCES public.semantic_facts(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: fact_relations fact_relations_fact_id_to_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fact_relations
    ADD CONSTRAINT fact_relations_fact_id_to_fkey FOREIGN KEY (fact_id_to) REFERENCES public.semantic_facts(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: idempotency_records idempotency_records_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.idempotency_records
    ADD CONSTRAINT idempotency_records_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: message_states message_states_tenant_id_chat_id_source_message_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.message_states
    ADD CONSTRAINT message_states_tenant_id_chat_id_source_message_id_fkey FOREIGN KEY (tenant_id, chat_id, source_message_id) REFERENCES public.messages(tenant_id, chat_id, source_message_id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: messages messages_tenant_id_chat_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.messages
    ADD CONSTRAINT messages_tenant_id_chat_id_fkey FOREIGN KEY (tenant_id, chat_id) REFERENCES public.chats(tenant_id, chat_id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: semantic_facts semantic_facts_superseded_by_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.semantic_facts
    ADD CONSTRAINT semantic_facts_superseded_by_id_fkey FOREIGN KEY (superseded_by_id) REFERENCES public.semantic_facts(id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: semantic_facts semantic_facts_tenant_id_chat_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.semantic_facts
    ADD CONSTRAINT semantic_facts_tenant_id_chat_id_fkey FOREIGN KEY (tenant_id, chat_id) REFERENCES public.chats(tenant_id, chat_id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: telegram_business_connections telegram_business_connections_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.telegram_business_connections
    ADD CONSTRAINT telegram_business_connections_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: tenants tenants_owner_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenants
    ADD CONSTRAINT tenants_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES public.users(id) ON DELETE RESTRICT;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: unclaimed_chats unclaimed_chats_owner_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.unclaimed_chats
    ADD CONSTRAINT unclaimed_chats_owner_tenant_id_fkey FOREIGN KEY (owner_tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: user_bots user_bots_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_bots
    ADD CONSTRAINT user_bots_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;
DO $idempotent$ BEGIN

--
-- Name: user_settings user_settings_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_settings
    ADD CONSTRAINT user_settings_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL;
END $idempotent$;

--
-- PostgreSQL database dump complete
--


