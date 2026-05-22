#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

POSTGRES_USER="${POSTGRES_USER:-doubleu}"
POSTGRES_DB="${POSTGRES_DB:-doubleu}"

if ! docker info >/dev/null 2>&1; then
  test -f infra/migrations/001_core_schema.sql
  test -f infra/seed/001_contract_scenarios.sql
  grep -q "CREATE TABLE IF NOT EXISTS tenants" infra/migrations/001_core_schema.sql
  grep -q "CREATE TABLE IF NOT EXISTS audit_events" infra/migrations/001_core_schema.sql
  grep -q "tenant_a" infra/seed/001_contract_scenarios.sql
  grep -q "tenant_b" infra/seed/001_contract_scenarios.sql
  grep -q "no_memory_chat" infra/seed/001_contract_scenarios.sql
  echo "Docker daemon unavailable; static migration and seed smoke checks passed."
  exit 0
fi

docker compose up -d postgres redis

for _ in {1..60}; do
  if docker compose exec -T postgres pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -f /migrations/001_core_schema.sql
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -f /seed/001_contract_scenarios.sql

docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 <<'SQL'
DO $$
DECLARE
  missing_count integer;
BEGIN
  SELECT count(*)
    INTO missing_count
    FROM (VALUES
      ('tenant_a'),
      ('tenant_b')
    ) expected(id)
    WHERE NOT EXISTS (SELECT 1 FROM tenants WHERE tenants.id = expected.id);
  IF missing_count <> 0 THEN
    RAISE EXCEPTION 'missing seeded tenants';
  END IF;

  IF NOT EXISTS (
    SELECT 1
    FROM chat_settings
    WHERE tenant_id = 'tenant_a'
      AND chat_id = 'no_memory_chat'
      AND 'no_memory' = ANY(privacy_flags)
  ) THEN
    RAISE EXCEPTION 'missing no_memory_chat privacy flags';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM memory_facts
    WHERE tenant_id = 'tenant_a'
      AND chat_id = 'no_memory_chat'
  ) THEN
    RAISE EXCEPTION 'no_memory_chat must not have persisted memory facts';
  END IF;

  IF NOT EXISTS (
    SELECT 1
    FROM audit_events
    WHERE tenant_id = 'tenant_a'
      AND source_message_id IN ('high_risk_message', 'low_risk_message')
  ) THEN
    RAISE EXCEPTION 'missing audit events for risk scenarios';
  END IF;
END $$;
SQL

echo "Migration and seed smoke checks passed."
