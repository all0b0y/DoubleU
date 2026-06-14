-- 014_risk_level_not_null.down.sql
-- Reverts messages.risk_level to nullable without a default (001 shape).

BEGIN;

ALTER TABLE messages
  ALTER COLUMN risk_level DROP NOT NULL,
  ALTER COLUMN risk_level DROP DEFAULT;

COMMIT;
