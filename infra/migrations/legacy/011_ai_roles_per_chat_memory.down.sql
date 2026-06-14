-- Rollback 011: убрать AI-роли и вернуть owner_global-факты в чтение.

ALTER TABLE chat_settings DROP COLUMN IF EXISTS ai_role_id;
DROP TABLE IF EXISTS ai_roles;

-- Реактивировать глобальные факты, скрытые up-миграцией.
UPDATE semantic_facts
   SET deleted_at = NULL
 WHERE scope_kind = 'owner_global';

-- Восстановить очередь подтверждения health-фактов (схема из 001).
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
