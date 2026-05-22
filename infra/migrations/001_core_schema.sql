BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

DO $$
BEGIN
  CREATE TYPE chat_mode AS ENUM ('disabled', 'silent', 'manual', 'semi_auto', 'auto');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
  CREATE TYPE privacy_flag AS ENUM ('no_memory', 'no_training', 'private', 'no_summary');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
  CREATE TYPE risk_level AS ENUM ('low', 'medium', 'high', 'critical');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
  CREATE TYPE decision AS ENUM ('draft', 'auto_reply', 'ignore', 'observe', 'ask_owner', 'error_fallback');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
  CREATE TYPE message_state AS ENUM (
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
END $$;

DO $$
BEGIN
  CREATE TYPE idempotency_operation AS ENUM ('receive_update', 'send_reply', 'approval_callback');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
  CREATE TYPE memory_scope AS ENUM ('conversation', 'rag', 'persona');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
  CREATE TYPE summary_type AS ENUM ('conversation', 'daily', 'weekly', 'period', 'task', 'deal', 'relationship');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS users (
  id text PRIMARY KEY,
  telegram_user_id text UNIQUE,
  email text UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tenants (
  id text PRIMARY KEY,
  owner_user_id text REFERENCES users(id) ON DELETE RESTRICT,
  name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS telegram_business_connections (
  id text PRIMARY KEY,
  tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  telegram_connection_id text NOT NULL,
  telegram_user_id text NOT NULL,
  permissions_json jsonb NOT NULL DEFAULT '{}'::jsonb,
  is_active boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, telegram_connection_id),
  UNIQUE (telegram_connection_id)
);

CREATE TABLE IF NOT EXISTS chats (
  tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  chat_id text NOT NULL,
  telegram_chat_id text NOT NULL,
  chat_type text NOT NULL DEFAULT 'private',
  display_name text NOT NULL,
  relationship_type text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id),
  UNIQUE (tenant_id, telegram_chat_id)
);

CREATE TABLE IF NOT EXISTS assistant_profiles (
  id text PRIMARY KEY,
  tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name text NOT NULL,
  default_mode chat_mode NOT NULL DEFAULT 'manual',
  default_model text NOT NULL DEFAULT 'fast',
  style_config_json jsonb NOT NULL DEFAULT '{}'::jsonb,
  privacy_config_json jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS chat_settings (
  tenant_id text NOT NULL,
  chat_id text NOT NULL,
  mode chat_mode NOT NULL,
  privacy_flags privacy_flag[] NOT NULL DEFAULT ARRAY[]::privacy_flag[],
  auto_reply_min_confidence numeric(4,3) NOT NULL DEFAULT 0.920,
  semi_auto_min_confidence numeric(4,3) NOT NULL DEFAULT 0.850,
  settings_json jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id),
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE,
  CHECK (auto_reply_min_confidence >= 0 AND auto_reply_min_confidence <= 1),
  CHECK (semi_auto_min_confidence >= 0 AND semi_auto_min_confidence <= 1)
);

CREATE TABLE IF NOT EXISTS messages (
  tenant_id text NOT NULL,
  chat_id text NOT NULL,
  source_message_id text NOT NULL,
  event_id text NOT NULL,
  telegram_update_id bigint NOT NULL,
  business_connection_id text NOT NULL,
  sender_id text NOT NULL,
  message_text text NOT NULL,
  message_type text NOT NULL DEFAULT 'text',
  risk_level risk_level,
  received_at timestamptz NOT NULL DEFAULT now(),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id, source_message_id),
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE,
  UNIQUE (telegram_update_id, business_connection_id)
);

CREATE TABLE IF NOT EXISTS message_states (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id text NOT NULL,
  chat_id text NOT NULL,
  source_message_id text NOT NULL,
  state message_state NOT NULL,
  state_reason text,
  actor text NOT NULL DEFAULT 'system',
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, chat_id, source_message_id)
    REFERENCES messages(tenant_id, chat_id, source_message_id) ON DELETE CASCADE,
  UNIQUE (tenant_id, chat_id, source_message_id, state, actor)
);

CREATE TABLE IF NOT EXISTS audit_events (
  audit_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id text NOT NULL,
  chat_id text NOT NULL,
  source_message_id text NOT NULL,
  decision decision NOT NULL,
  risk_level risk_level,
  reason text NOT NULL,
  privacy_flags privacy_flag[] NOT NULL DEFAULT ARRAY[]::privacy_flag[],
  policy_snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, chat_id, source_message_id)
    REFERENCES messages(tenant_id, chat_id, source_message_id) ON DELETE CASCADE,
  UNIQUE (tenant_id, chat_id, source_message_id, decision, reason)
);

CREATE TABLE IF NOT EXISTS idempotency_records (
  idempotency_key text PRIMARY KEY,
  operation idempotency_operation NOT NULL,
  tenant_id text REFERENCES tenants(id) ON DELETE CASCADE,
  chat_id text,
  source_message_id text,
  decision_id text,
  request_hash text,
  response_json jsonb,
  locked_until timestamptz,
  completed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (
    operation <> 'send_reply'
    OR (tenant_id IS NOT NULL AND chat_id IS NOT NULL AND source_message_id IS NOT NULL AND decision_id IS NOT NULL)
  )
);

CREATE TABLE IF NOT EXISTS memory_facts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  chat_id text,
  scope memory_scope NOT NULL,
  fact_type text NOT NULL,
  fact_text text NOT NULL,
  confidence numeric(4,3) NOT NULL,
  source_message_id text,
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  hard_deleted boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz,
  CHECK (confidence >= 0 AND confidence <= 1),
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE,
  CHECK (
    (scope = 'conversation' AND chat_id IS NOT NULL)
    OR (scope IN ('rag', 'persona'))
  ),
  UNIQUE (tenant_id, chat_id, scope, fact_type, source_message_id, fact_text)
);

CREATE TABLE IF NOT EXISTS memory_embeddings (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  knowledge_base_id text,
  chat_id text,
  memory_fact_id uuid REFERENCES memory_facts(id) ON DELETE CASCADE,
  embedding_model text NOT NULL DEFAULT 'placeholder',
  embedding_dimensions integer NOT NULL DEFAULT 0,
  embedding float8[] NOT NULL DEFAULT ARRAY[]::float8[],
  vector_backend text NOT NULL DEFAULT 'placeholder_float8_array',
  content_hash text,
  hard_deleted boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz,
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE,
  CHECK (embedding_dimensions >= 0),
  UNIQUE (tenant_id, content_hash)
);

CREATE TABLE IF NOT EXISTS summaries (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  chat_id text,
  summary_type summary_type NOT NULL,
  period_start timestamptz,
  period_end timestamptz,
  content text NOT NULL,
  source_message_ids text[] NOT NULL DEFAULT ARRAY[]::text[],
  privacy_flags privacy_flag[] NOT NULL DEFAULT ARRAY[]::privacy_flag[],
  hard_deleted boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz,
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE,
  CHECK (
    summary_type IN ('daily', 'weekly', 'period')
    OR chat_id IS NOT NULL
  ),
  UNIQUE (tenant_id, chat_id, summary_type, period_start, period_end)
);

CREATE TABLE IF NOT EXISTS draft_feedback (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  draft_id text NOT NULL,
  tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  chat_id text NOT NULL,
  rating text NOT NULL CHECK (rating IN ('up', 'down')),
  feedback_text text NOT NULL DEFAULT '',
  response_text text NOT NULL DEFAULT '',
  model_used text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (draft_id, rating)
);

CREATE TABLE IF NOT EXISTS contact_profiles (
  tenant_id text NOT NULL,
  chat_id text NOT NULL,
  name text NOT NULL DEFAULT '',
  gender text NOT NULL DEFAULT '',
  role text NOT NULL DEFAULT '',
  relationship_type text NOT NULL DEFAULT '',
  notes text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id),
  FOREIGN KEY (tenant_id, chat_id) REFERENCES chats(tenant_id, chat_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_draft_feedback_chat ON draft_feedback(tenant_id, chat_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_chats_tenant ON chats(tenant_id);
CREATE INDEX IF NOT EXISTS idx_chat_settings_privacy_flags ON chat_settings USING gin(privacy_flags);
CREATE INDEX IF NOT EXISTS idx_messages_tenant_chat_received ON messages(tenant_id, chat_id, received_at DESC);
CREATE INDEX IF NOT EXISTS idx_message_states_lookup ON message_states(tenant_id, chat_id, source_message_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_chat_created ON audit_events(tenant_id, chat_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_idempotency_records_operation ON idempotency_records(operation, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_memory_facts_active_scope ON memory_facts(tenant_id, chat_id, scope) WHERE hard_deleted = false;
CREATE INDEX IF NOT EXISTS idx_memory_embeddings_active ON memory_embeddings(tenant_id, knowledge_base_id) WHERE hard_deleted = false;
CREATE INDEX IF NOT EXISTS idx_summaries_active ON summaries(tenant_id, chat_id, summary_type, created_at DESC) WHERE hard_deleted = false;

COMMIT;
