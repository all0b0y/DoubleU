BEGIN;

-- ============================================================
-- Extensions
-- ============================================================
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS vector;

-- ============================================================
-- Enum types
-- ============================================================
DO $$ BEGIN CREATE TYPE chat_mode AS ENUM ('disabled','silent','manual','semi_auto','auto'); EXCEPTION WHEN duplicate_object THEN NULL; END $$;
DO $$ BEGIN CREATE TYPE privacy_flag AS ENUM ('no_memory','no_training','private','no_summary'); EXCEPTION WHEN duplicate_object THEN NULL; END $$;
DO $$ BEGIN CREATE TYPE risk_level AS ENUM ('low','medium','high','critical'); EXCEPTION WHEN duplicate_object THEN NULL; END $$;
DO $$ BEGIN CREATE TYPE decision AS ENUM ('draft','auto_reply','ignore','observe','ask_owner','error_fallback'); EXCEPTION WHEN duplicate_object THEN NULL; END $$;
DO $$ BEGIN CREATE TYPE message_state AS ENUM (
  'received','normalized','policy_checked','ai_requested','ai_generated',
  'decision_made','draft_sent_to_owner','approved_by_owner',
  'sent_to_recipient','ignored','failed'
); EXCEPTION WHEN duplicate_object THEN NULL; END $$;
DO $$ BEGIN CREATE TYPE idempotency_operation AS ENUM ('receive_update','send_reply','approval_callback'); EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- ============================================================
-- CORE TABLES
-- ============================================================
CREATE TABLE IF NOT EXISTS users (
  id          TEXT PRIMARY KEY,
  telegram_user_id TEXT UNIQUE,
  email       TEXT UNIQUE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tenants (
  id            TEXT PRIMARY KEY,
  owner_user_id TEXT REFERENCES users(id) ON DELETE RESTRICT,
  name          TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS telegram_business_connections (
  id                    TEXT PRIMARY KEY,
  tenant_id             TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  telegram_connection_id TEXT NOT NULL,
  telegram_user_id      TEXT NOT NULL,
  permissions_json      JSONB NOT NULL DEFAULT '{}'::jsonb,
  is_active             BOOLEAN NOT NULL DEFAULT true,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, telegram_connection_id),
  UNIQUE (telegram_connection_id)
);

CREATE TABLE IF NOT EXISTS chats (
  tenant_id     TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  chat_id       TEXT NOT NULL,
  telegram_chat_id TEXT NOT NULL,
  chat_type     TEXT NOT NULL DEFAULT 'private',
  display_name  TEXT NOT NULL DEFAULT '',
  -- status tracks ownership lifecycle; only 'managed' chats receive AI processing.
  status        TEXT NOT NULL DEFAULT 'managed'
                CHECK (status IN ('unclaimed','managed','dismissed','archived')),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id),
  UNIQUE (tenant_id, telegram_chat_id)
);
-- Idempotent: adds column to existing schemas without recreating the table.
ALTER TABLE chats ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'managed'
  CHECK (status IN ('unclaimed','managed','dismissed','archived'));
CREATE INDEX IF NOT EXISTS idx_chats_tenant ON chats(tenant_id);
CREATE INDEX IF NOT EXISTS idx_chats_status ON chats(tenant_id, status);

-- ============================================================
-- UNCLAIMED CHATS
-- Contacts that arrived via Business API but the owner has not yet
-- added to a tenant.  Replaces the "staging:<bc_id>" fake-tenant pattern.
-- ============================================================
CREATE TABLE IF NOT EXISTS unclaimed_chats (
  staging_id            TEXT PRIMARY KEY,
  owner_tenant_id       TEXT REFERENCES tenants(id) ON DELETE CASCADE,
  business_connection_id TEXT NOT NULL,
  chat_id               TEXT NOT NULL,
  display_name          TEXT NOT NULL DEFAULT '',
  chat_type             TEXT NOT NULL DEFAULT 'private',
  first_message_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_message_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  message_count         INT NOT NULL DEFAULT 1,
  status                TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending','claimed','dismissed','expired')),
  claimed_at            TIMESTAMPTZ,
  dismissed_at          TIMESTAMPTZ,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ix_uc_pending
  ON unclaimed_chats(owner_tenant_id) WHERE status = 'pending';
CREATE UNIQUE INDEX IF NOT EXISTS ux_uc_active
  ON unclaimed_chats(business_connection_id, chat_id) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS chat_settings (
  tenant_id                 TEXT NOT NULL,
  chat_id                   TEXT NOT NULL,
  mode                      chat_mode NOT NULL,
  privacy_flags             privacy_flag[] NOT NULL DEFAULT ARRAY[]::privacy_flag[],
  auto_reply_min_confidence NUMERIC(4,3) NOT NULL DEFAULT 0.920,
  semi_auto_min_confidence  NUMERIC(4,3) NOT NULL DEFAULT 0.850,
  settings_json             JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id),
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE,
  CHECK (auto_reply_min_confidence BETWEEN 0 AND 1),
  CHECK (semi_auto_min_confidence  BETWEEN 0 AND 1)
);
CREATE INDEX IF NOT EXISTS idx_chat_settings_privacy_flags ON chat_settings USING gin(privacy_flags);

CREATE TABLE IF NOT EXISTS messages (
  tenant_id              TEXT NOT NULL,
  chat_id                TEXT NOT NULL,
  source_message_id      TEXT NOT NULL,
  event_id               TEXT NOT NULL,
  telegram_update_id     BIGINT NOT NULL,
  business_connection_id TEXT NOT NULL,
  sender_id              TEXT NOT NULL,
  message_text           TEXT NOT NULL,
  message_type           TEXT NOT NULL DEFAULT 'text',
  risk_level             risk_level,
  received_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id, source_message_id),
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE,
  UNIQUE (telegram_update_id, business_connection_id)
);
CREATE INDEX IF NOT EXISTS idx_messages_tenant_chat_received ON messages(tenant_id, chat_id, received_at DESC);

CREATE TABLE IF NOT EXISTS message_states (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         TEXT NOT NULL,
  chat_id           TEXT NOT NULL,
  source_message_id TEXT NOT NULL,
  state             message_state NOT NULL,
  state_reason      TEXT,
  actor             TEXT NOT NULL DEFAULT 'system',
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, chat_id, source_message_id)
    REFERENCES messages(tenant_id, chat_id, source_message_id) ON DELETE CASCADE,
  UNIQUE (tenant_id, chat_id, source_message_id, state, actor)
);
CREATE INDEX IF NOT EXISTS idx_message_states_lookup ON message_states(tenant_id, chat_id, source_message_id, created_at DESC);

CREATE TABLE IF NOT EXISTS audit_events (
  audit_id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         TEXT NOT NULL,
  chat_id           TEXT NOT NULL,
  source_message_id TEXT NOT NULL,
  decision          decision NOT NULL,
  risk_level        risk_level,
  reason            TEXT NOT NULL,
  privacy_flags     privacy_flag[] NOT NULL DEFAULT ARRAY[]::privacy_flag[],
  policy_snapshot   JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, chat_id, source_message_id)
    REFERENCES messages(tenant_id, chat_id, source_message_id) ON DELETE CASCADE,
  UNIQUE (tenant_id, chat_id, source_message_id, decision, reason)
);
CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_chat_created ON audit_events(tenant_id, chat_id, created_at DESC);

CREATE TABLE IF NOT EXISTS idempotency_records (
  idempotency_key TEXT PRIMARY KEY,
  operation       idempotency_operation NOT NULL,
  tenant_id       TEXT REFERENCES tenants(id) ON DELETE CASCADE,
  chat_id         TEXT,
  source_message_id TEXT,
  decision_id     TEXT,
  request_hash    TEXT,
  response_json   JSONB,
  locked_until    TIMESTAMPTZ,
  completed_at    TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (
    operation <> 'send_reply'
    OR (tenant_id IS NOT NULL AND chat_id IS NOT NULL
        AND source_message_id IS NOT NULL AND decision_id IS NOT NULL)
  )
);
CREATE INDEX IF NOT EXISTS idx_idempotency_records_operation ON idempotency_records(operation, created_at DESC);

-- ============================================================
-- DRAFTS
-- Owner-facing draft cards with their full lifecycle and Telegram message IDs.
-- owner_message_id enables the bot to delete/edit the draft card after restart.
-- ============================================================
CREATE TABLE IF NOT EXISTS drafts (
  draft_id              TEXT PRIMARY KEY,
  decision_id           TEXT NOT NULL DEFAULT '',
  tenant_id             TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  chat_id               TEXT NOT NULL,
  business_connection_id TEXT NOT NULL DEFAULT '',
  source_message_id     TEXT NOT NULL DEFAULT '',
  source_message_text   TEXT NOT NULL DEFAULT '',
  response_text         TEXT NOT NULL DEFAULT '',
  draft_type            TEXT NOT NULL DEFAULT 'reply'
                        CHECK (draft_type IN ('reply','question')),
  suggested_options     TEXT[] NOT NULL DEFAULT '{}',
  status                TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending','approved','rejected','sent')),
  -- Telegram message_id of the draft card sent to the owner's bot chat.
  -- Persisted so the bot can delete/edit the card after restart.
  owner_chat_id         TEXT NOT NULL DEFAULT '',
  owner_message_id      TEXT NOT NULL DEFAULT '',
  -- Telegram message_id of the reply actually sent to the contact.
  sent_message_id       TEXT NOT NULL DEFAULT '',
  action_history        TEXT[] NOT NULL DEFAULT '{}',
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_drafts_tenant_chat ON drafts(tenant_id, chat_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_drafts_source_msg ON drafts(tenant_id, chat_id, source_message_id);
CREATE INDEX IF NOT EXISTS idx_drafts_status ON drafts(tenant_id, status) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS draft_feedback (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  draft_id      TEXT NOT NULL,
  tenant_id     TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  chat_id       TEXT NOT NULL,
  rating        TEXT NOT NULL CHECK (rating IN ('up','down')),
  feedback_text TEXT NOT NULL DEFAULT '',
  response_text TEXT NOT NULL DEFAULT '',
  model_used    TEXT NOT NULL DEFAULT '',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (draft_id, rating)
);
CREATE INDEX IF NOT EXISTS idx_draft_feedback_chat ON draft_feedback(tenant_id, chat_id, created_at DESC);

-- ============================================================
-- USER LAYER (§3)
-- ============================================================
CREATE TABLE IF NOT EXISTS user_settings (
  user_id                   TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  tenant_id                 TEXT NOT NULL,
  display_name              TEXT,
  bio                       TEXT,
  timezone                  TEXT,
  primary_language          TEXT NOT NULL DEFAULT 'ru',
  secondary_languages       TEXT[] NOT NULL DEFAULT '{}',
  default_mode_for_new_chats TEXT NOT NULL DEFAULT 'manual'
    CHECK (default_mode_for_new_chats IN ('off','observe','manual','semi_auto','auto')),
  learning_enabled          BOOLEAN NOT NULL DEFAULT true,
  language_for_drafts       TEXT NOT NULL DEFAULT 'auto'
    CHECK (language_for_drafts IN ('auto','primary')),
  created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ix_user_settings_tenant ON user_settings(tenant_id);

-- ============================================================
-- PER-CHAT IDENTITY LAYER (§4)
-- ============================================================
CREATE TABLE IF NOT EXISTS contact_profiles (
  tenant_id         TEXT NOT NULL,
  chat_id           TEXT NOT NULL,
  name              TEXT NOT NULL DEFAULT '',
  gender            TEXT NOT NULL DEFAULT '',
  role              TEXT NOT NULL DEFAULT '',
  relationship_type TEXT NOT NULL DEFAULT '',
  notes             TEXT NOT NULL DEFAULT '',
  -- Extended fields from memory architecture
  display_names     TEXT[] NOT NULL DEFAULT '{}',
  preferred_address TEXT,
  language          TEXT,
  contact_timezone  TEXT,
  tone              TEXT CHECK (tone IN ('formal','neutral','casual')),
  emoji_usage       TEXT CHECK (emoji_usage IN ('none','rare','frequent')),
  address_form      TEXT CHECK (address_form IN ('ty','vy')),
  style_notes       TEXT,
  topics            TEXT[] NOT NULL DEFAULT '{}',
  chat_context      TEXT,
  do_not_discuss    TEXT[] NOT NULL DEFAULT '{}',
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id),
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS contact_profile_suggestions (
  id              BIGSERIAL PRIMARY KEY,
  tenant_id       TEXT NOT NULL,
  chat_id         TEXT NOT NULL,
  field_name      TEXT NOT NULL,
  suggested_value JSONB NOT NULL,
  rationale       TEXT,
  source_window   JSONB,
  status          TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','accepted','rejected','expired')),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  reviewed_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ix_cps_pending
  ON contact_profile_suggestions(tenant_id, chat_id)
  WHERE status = 'pending';

-- ============================================================
-- RUNTIME STATE LAYER (§5)
-- active_episodes: one row per open conversation.
-- Deleted and re-created on episode boundary (silence > 6h or 40 msgs).
-- ============================================================
CREATE TABLE IF NOT EXISTS active_episodes (
  tenant_id               TEXT NOT NULL,
  chat_id                 TEXT NOT NULL,
  started_at              TIMESTAMPTZ NOT NULL,
  last_message_at         TIMESTAMPTZ NOT NULL,
  message_count           INT NOT NULL DEFAULT 0,
  current_topic           TEXT,
  open_question_message_id TEXT,
  pending_owner_action    TEXT,
  metadata                JSONB NOT NULL DEFAULT '{}'::jsonb,
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id),
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS ix_ae_stale ON active_episodes(last_message_at);

-- ============================================================
-- EPISODIC LAYER (§6)
-- episode_summaries: compressed summaries of closed episodes.
-- ============================================================
CREATE TABLE IF NOT EXISTS episode_summaries (
  id                 BIGSERIAL PRIMARY KEY,
  tenant_id          TEXT NOT NULL,
  chat_id            TEXT NOT NULL,
  period_start       TIMESTAMPTZ NOT NULL,
  period_end         TIMESTAMPTZ NOT NULL,
  content            TEXT NOT NULL CHECK (char_length(content) BETWEEN 50 AND 1500),
  source_message_ids TEXT[] NOT NULL DEFAULT '{}',
  message_count      INT NOT NULL DEFAULT 0,
  closure_reason     TEXT NOT NULL DEFAULT 'manual'
                     CHECK (closure_reason IN ('silence_gap','message_count','topic_shift','manual')),
  privacy_flags      JSONB NOT NULL DEFAULT '{}'::jsonb,
  deleted_at         TIMESTAMPTZ,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS ix_es_chat
  ON episode_summaries(tenant_id, chat_id, period_end DESC)
  WHERE deleted_at IS NULL;
-- Embedding column for episodic semantic search (populated by embedding worker).
ALTER TABLE episode_summaries ADD COLUMN IF NOT EXISTS embedding FLOAT8[] NOT NULL DEFAULT '{}';
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
    BEGIN
      ALTER TABLE episode_summaries
        ALTER COLUMN embedding TYPE vector(1536) USING embedding::vector(1536);
    EXCEPTION WHEN others THEN
      NULL;
    END;
    BEGIN
      CREATE INDEX IF NOT EXISTS ix_es_embedding_hnsw
        ON episode_summaries USING hnsw (embedding vector_cosine_ops)
        WHERE deleted_at IS NULL;
    EXCEPTION WHEN others THEN
      NULL;
    END;
  END IF;
END $$;

-- ============================================================
-- SEMANTIC LAYER (§7)
-- semantic_facts: typed atomic facts extracted from conversations.
-- ============================================================
CREATE TABLE IF NOT EXISTS semantic_facts (
  id                BIGSERIAL PRIMARY KEY,
  tenant_id         TEXT NOT NULL,
  scope_kind        TEXT NOT NULL CHECK (scope_kind IN ('chat','owner_global')),
  chat_id           TEXT,
  fact_text         TEXT NOT NULL
                    CHECK (char_length(fact_text) BETWEEN 5 AND 200)
                    CHECK (fact_text NOT LIKE '[FACT]%')
                    CHECK (fact_text NOT LIKE '[EXTRACTED]%')
                    CHECK (fact_text NOT LIKE '[OWNER STYLE%'),
  normalized_fact_hash TEXT NOT NULL,
  fact_type         TEXT NOT NULL CHECK (fact_type IN
                    ('preference','biographical','commitment','event',
                     'relationship','health','taboo')),
  subject           TEXT NOT NULL CHECK (subject IN
                    ('contact','owner','relationship','third_party')),
  confidence        REAL NOT NULL CHECK (confidence BETWEEN 0 AND 1),
  source            TEXT NOT NULL CHECK (source IN
                    ('llm_extracted','owner_confirmed','owner_authored')),
  source_message_id TEXT,
  metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
  expires_at        TIMESTAMPTZ,
  superseded_by_id  BIGINT REFERENCES semantic_facts(id),
  deleted_at        TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT scope_chat_requires_chat_id  CHECK (scope_kind != 'chat'         OR chat_id IS NOT NULL),
  CONSTRAINT scope_global_no_chat_id      CHECK (scope_kind != 'owner_global' OR chat_id IS NULL),
  CONSTRAINT scope_global_only_owner      CHECK (scope_kind != 'owner_global' OR subject = 'owner'),

  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_sf_normalized_active
  ON semantic_facts(tenant_id, scope_kind, COALESCE(chat_id,''), fact_type, subject, normalized_fact_hash)
  WHERE deleted_at IS NULL AND superseded_by_id IS NULL;
CREATE INDEX IF NOT EXISTS ix_sf_chat_active
  ON semantic_facts(tenant_id, chat_id, fact_type)
  WHERE scope_kind = 'chat' AND deleted_at IS NULL AND superseded_by_id IS NULL;
CREATE INDEX IF NOT EXISTS ix_sf_global_active
  ON semantic_facts(tenant_id, fact_type)
  WHERE scope_kind = 'owner_global' AND deleted_at IS NULL AND superseded_by_id IS NULL;
CREATE INDEX IF NOT EXISTS ix_sf_commitments
  ON semantic_facts(tenant_id, chat_id, expires_at)
  WHERE fact_type = 'commitment' AND deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS fact_embeddings (
  fact_id         BIGINT PRIMARY KEY REFERENCES semantic_facts(id) ON DELETE CASCADE,
  tenant_id       TEXT NOT NULL,
  scope_kind      TEXT NOT NULL CHECK (scope_kind IN ('chat','owner_global')),
  chat_id         TEXT,
  embedding_model TEXT NOT NULL DEFAULT 'text-embedding-3-small',
  -- 1536-dim vector for OpenAI text-embedding-3-small.
  -- Falls back to FLOAT8[] when the vector extension is unavailable;
  -- ALTER below promotes to vector type when pgvector is present.
  embedding       FLOAT8[] NOT NULL DEFAULT '{}',
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Promote embedding column to vector(1536) if pgvector is available and column is still FLOAT8[].
-- This ALTER is idempotent: it no-ops if the column is already vector type.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
    BEGIN
      ALTER TABLE fact_embeddings
        ALTER COLUMN embedding TYPE vector(1536) USING embedding::vector(1536);
    EXCEPTION WHEN others THEN
      NULL; -- Column already vector type or incompatible default; skip.
    END;
  END IF;
END $$;
CREATE INDEX IF NOT EXISTS ix_fe_chat_scope   ON fact_embeddings(tenant_id, chat_id) WHERE scope_kind = 'chat';
CREATE INDEX IF NOT EXISTS ix_fe_global_scope ON fact_embeddings(tenant_id)          WHERE scope_kind = 'owner_global';
-- HNSW index for cosine similarity search (activates after embeddings are populated).
-- Uses IF NOT EXISTS to remain idempotent on repeated migration runs.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
    BEGIN
      CREATE INDEX IF NOT EXISTS ix_fe_chat_hnsw
        ON fact_embeddings USING hnsw (embedding vector_cosine_ops)
        WHERE scope_kind = 'chat';
      CREATE INDEX IF NOT EXISTS ix_fe_global_hnsw
        ON fact_embeddings USING hnsw (embedding vector_cosine_ops)
        WHERE scope_kind = 'owner_global';
    EXCEPTION WHEN others THEN
      NULL;
    END;
  END IF;
END $$;

CREATE TABLE IF NOT EXISTS fact_occurrences (
  tenant_id         TEXT NOT NULL,
  fact_id           BIGINT NOT NULL REFERENCES semantic_facts(id) ON DELETE CASCADE,
  source_message_id TEXT NOT NULL,
  chat_id           TEXT NOT NULL,
  observed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (fact_id, source_message_id)
);
CREATE INDEX IF NOT EXISTS ix_fo_fact ON fact_occurrences(fact_id);

-- ============================================================
-- PENDING GLOBAL HEALTH FACTS (owner confirmation queue)
-- ============================================================
CREATE TABLE IF NOT EXISTS pending_global_health_facts (
  id                BIGSERIAL PRIMARY KEY,
  tenant_id         TEXT NOT NULL,
  chat_id           TEXT NOT NULL,
  source_message_id TEXT NOT NULL,
  fact_text         TEXT NOT NULL CHECK (char_length(fact_text) BETWEEN 5 AND 200),
  confidence        REAL NOT NULL,
  status            TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','accepted','rejected','expired')),
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  reviewed_at       TIMESTAMPTZ,
  resulting_fact_id BIGINT REFERENCES semantic_facts(id)
);
CREATE INDEX IF NOT EXISTS ix_pghf_pending
  ON pending_global_health_facts(tenant_id)
  WHERE status = 'pending';

-- ============================================================
-- EXTRACTION LOG
-- ============================================================
CREATE TABLE IF NOT EXISTS extraction_log (
  id                BIGSERIAL PRIMARY KEY,
  tenant_id         TEXT NOT NULL,
  chat_id           TEXT NOT NULL,
  input_message_ids TEXT[] NOT NULL DEFAULT '{}',
  model_used        TEXT NOT NULL DEFAULT '',
  raw_output        JSONB NOT NULL DEFAULT '[]',
  facts_created     BIGINT[] NOT NULL DEFAULT '{}',
  facts_skipped     INT NOT NULL DEFAULT 0,
  facts_merged      INT NOT NULL DEFAULT 0,
  error             TEXT,
  duration_ms       INT,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ============================================================
-- SQL FUNCTIONS
-- ============================================================

-- Dedup hash for semantic_facts.
CREATE OR REPLACE FUNCTION normalize_fact_text(p_text TEXT) RETURNS TEXT AS $$
  SELECT encode(digest(regexp_replace(lower(trim(p_text)), '\s+', ' ', 'g'), 'sha256'), 'hex');
$$ LANGUAGE sql IMMUTABLE;

-- Derived state: hours since last message in a chat.
CREATE OR REPLACE FUNCTION view_chat_paused_hours(p_tenant TEXT, p_chat TEXT)
RETURNS NUMERIC AS $$
  SELECT EXTRACT(EPOCH FROM (now() - MAX(received_at))) / 3600
    FROM messages WHERE tenant_id = p_tenant AND chat_id = p_chat;
$$ LANGUAGE sql STABLE;

-- Derived state: whether the owner has greeted this contact today.
CREATE OR REPLACE FUNCTION view_chat_greeted_today(p_tenant TEXT, p_chat TEXT, p_owner_id TEXT)
RETURNS BOOLEAN AS $$
  SELECT EXISTS (
    SELECT 1 FROM messages
     WHERE tenant_id = p_tenant AND chat_id = p_chat
       AND sender_id = p_owner_id
       AND received_at >= date_trunc('day', now())
  );
$$ LANGUAGE sql STABLE;

-- Derived state: open commitments for a chat.
CREATE OR REPLACE FUNCTION view_open_commitments(p_tenant TEXT, p_chat TEXT)
RETURNS TABLE(fact_id BIGINT, fact_text TEXT, expires_at TIMESTAMPTZ) AS $$
  SELECT id, fact_text, expires_at
    FROM semantic_facts
   WHERE tenant_id = p_tenant AND chat_id = p_chat
     AND fact_type = 'commitment'
     AND deleted_at IS NULL AND superseded_by_id IS NULL
     AND (expires_at IS NULL OR expires_at > now());
$$ LANGUAGE sql STABLE;

-- ============================================================
-- BOOTSTRAP TRIGGERS
-- ============================================================

-- Auto-create user_settings when a tenant with a known owner is inserted.
CREATE OR REPLACE FUNCTION bootstrap_user_settings() RETURNS TRIGGER AS $$
BEGIN
  IF NEW.owner_user_id IS NOT NULL THEN
    INSERT INTO user_settings (user_id, tenant_id)
    VALUES (NEW.owner_user_id, NEW.id)
    ON CONFLICT DO NOTHING;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_trigger WHERE tgname = 'trg_bootstrap_user_settings'
  ) THEN
    CREATE TRIGGER trg_bootstrap_user_settings
      AFTER INSERT ON tenants
      FOR EACH ROW EXECUTE FUNCTION bootstrap_user_settings();
  END IF;
END $$;

-- Auto-create chat_settings (mode=manual) when a managed chat row is inserted.
-- Unclaimed chats intentionally get no settings until the owner claims them.
CREATE OR REPLACE FUNCTION bootstrap_chat_settings() RETURNS TRIGGER AS $$
BEGIN
  IF NEW.status = 'managed' THEN
    INSERT INTO chat_settings (tenant_id, chat_id, mode)
    VALUES (NEW.tenant_id, NEW.chat_id, 'manual')
    ON CONFLICT DO NOTHING;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Fires on INSERT (new managed chat) AND UPDATE (unclaimed → managed on claim).
DROP TRIGGER IF EXISTS trg_bootstrap_chat_settings ON chats;
CREATE TRIGGER trg_bootstrap_chat_settings
  AFTER INSERT OR UPDATE OF status ON chats
  FOR EACH ROW
  WHEN (NEW.status = 'managed')
  EXECUTE FUNCTION bootstrap_chat_settings();

-- ============================================================
-- SAFETY CONSTRAINTS
-- ============================================================

-- Prevent LLM-extracted global health facts from being inserted directly into semantic_facts.
-- Only llm_extracted source is blocked: owner_confirmed (from accept proc) and
-- owner_authored (from UI) are allowed through without a session variable.
CREATE OR REPLACE FUNCTION prevent_direct_global_health_insert() RETURNS TRIGGER AS $$
BEGIN
  IF NEW.scope_kind = 'owner_global'
     AND NEW.fact_type = 'health'
     AND NEW.source = 'llm_extracted' THEN
    RAISE EXCEPTION 'Global health facts must go through pending_global_health_facts confirmation flow'
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_trigger WHERE tgname = 'trg_no_direct_global_health'
  ) THEN
    CREATE TRIGGER trg_no_direct_global_health
      BEFORE INSERT ON semantic_facts
      FOR EACH ROW EXECUTE FUNCTION prevent_direct_global_health_insert();
  END IF;
END $$;

-- ============================================================
-- UPSERT_SEMANTIC_FACT: race-safe, semantically-aware deduplication (design §9.1)
-- ============================================================
-- Three-tier dedup logic:
--   1. Exact hash match (UNIQUE index)  → add occurrence, return existing id
--   2. Cosine similarity ≥ 0.97        → add occurrence (same fact, different wording)
--   3. Cosine similarity 0.85–0.97     → supersede older fact with this one
--   4. No match (< 0.85 or no embeddings yet) → insert new fact
--
-- When pgvector embeddings are not yet computed (embedding IS NULL or '{}') the
-- similarity tiers are skipped and the function falls back to exact-hash dedup only.
-- This allows the proc to be used immediately; semantic dedup activates automatically
-- once embeddings are populated by the embedding worker.
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

  -- Tiers 2 & 3: vector similarity (only when embeddings exist).
  -- Find the most similar active fact in the same scope for this tenant/chat.
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
     -- Only compare when embedding is not the placeholder empty array.
     AND array_length(fe.embedding, 1) > 0
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

-- ============================================================
-- BACKFILL: bootstrap settings for any rows that pre-date triggers
-- ============================================================
-- user_settings: one row per tenant that has a known owner user.
INSERT INTO user_settings (user_id, tenant_id)
SELECT t.owner_user_id, t.id
  FROM tenants t
 WHERE t.owner_user_id IS NOT NULL
ON CONFLICT DO NOTHING;

-- chat_settings: one row per managed chat with default mode=manual.
INSERT INTO chat_settings (tenant_id, chat_id, mode)
SELECT c.tenant_id, c.chat_id, 'manual'
  FROM chats c
 WHERE c.status = 'managed'
ON CONFLICT DO NOTHING;

COMMIT;
