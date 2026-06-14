-- 014_risk_level_not_null.sql
-- =============================================================================
-- MEM-3 (design §12 Phase 2): enforce the invariant that every stored message
-- carries a risk level.
--
-- Write-path note: the Go backend persists the inbound message BEFORE the AI
-- risk classification runs (the row must exist for message_states FKs), so at
-- INSERT time the level is genuinely unknown. The column therefore gets a
-- DEFAULT 'low' — the design explicitly allows the conservative default on
-- rows the classifier has not stamped (§12 Phase 2: "дефолт 'safe' на
-- историческом датасете"; our enum spells it 'low'). Owner-authored and
-- passive-mode messages also land here as 'low' by the same default.
-- =============================================================================

BEGIN;

UPDATE messages SET risk_level = 'low' WHERE risk_level IS NULL;

ALTER TABLE messages
  ALTER COLUMN risk_level SET DEFAULT 'low',
  ALTER COLUMN risk_level SET NOT NULL;

COMMIT;
