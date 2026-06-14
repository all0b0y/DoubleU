-- 011: AI roles + строго per-chat память.
--
-- Продуктовый разворот: вместо "цифровой копии владельца" система воспроизводит
-- ИИ-роль (персону), которую владелец описал сам и назначил на чат.
-- Память при этом становится строго чатовой: scope_kind='owner_global'
-- больше не пишется и не читается — каждый чат живёт со своей памятью.

-- ============================================================
-- AI ROLES — библиотека персон владельца.
-- Одна роль может быть назначена на несколько чатов (ai_role_id в chat_settings).
-- ============================================================
CREATE TABLE IF NOT EXISTS ai_roles (
  id               BIGSERIAL PRIMARY KEY,
  tenant_id        TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  -- Короткое имя роли ("Менеджер Алексея", "Техподдержка").
  name             TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
  -- Свободное описание персоны от владельца — главный материал для промпта.
  description      TEXT NOT NULL DEFAULT '',
  -- Как роль говорит: лексика, тон, длина сообщений.
  speaking_style   TEXT NOT NULL DEFAULT '',
  -- Чего роль добивается в переписке.
  goals            TEXT NOT NULL DEFAULT '',
  -- Чего роль не делает / о чём не говорит.
  constraints_text TEXT NOT NULL DEFAULT '',
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);
CREATE INDEX IF NOT EXISTS idx_ai_roles_tenant ON ai_roles(tenant_id);

-- Назначение роли на чат. NULL = роль не задана (дефолтная персона ассистента).
ALTER TABLE chat_settings
  ADD COLUMN IF NOT EXISTS ai_role_id BIGINT REFERENCES ai_roles(id) ON DELETE SET NULL;

-- ============================================================
-- Память строго per-chat: выводим owner_global-факты из чтения.
-- Soft-delete сохраняет provenance (принцип §6: каждый факт смертен,
-- история не уничтожается). Код больше не пишет scope_kind='owner_global'.
-- ============================================================
UPDATE semantic_facts
   SET deleted_at = now()
 WHERE scope_kind = 'owner_global'
   AND deleted_at IS NULL;

-- Очередь подтверждения глобальных health-фактов теряет смысл без global-scope:
-- health-факты теперь обычные chat-scoped факты (hard-include внутри своего чата).
DROP TABLE IF EXISTS pending_global_health_facts;
