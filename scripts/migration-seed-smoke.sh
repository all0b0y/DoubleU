#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

POSTGRES_USER="${POSTGRES_USER:-doubleu}"
POSTGRES_DB="${POSTGRES_DB:-doubleu}"

SCHEMA_FILE="infra/migrations/100_clean_schema_v2.sql"
SEED_FILE="infra/seed/001_contract_scenarios.sql"

if ! docker info >/dev/null 2>&1; then
  test -f "$SCHEMA_FILE"
  test -f "$SEED_FILE"
  grep -q "CREATE TABLE IF NOT EXISTS public.tenants" "$SCHEMA_FILE"
  grep -q "CREATE TABLE IF NOT EXISTS public.audit_events" "$SCHEMA_FILE"
  grep -q "tenant_a" "$SEED_FILE"
  grep -q "tenant_b" "$SEED_FILE"
  grep -q "no_memory_chat" "$SEED_FILE"
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

# Apply every up-migration in lexical order (skip the *.down.sql rollbacks).
for migration in $(ls infra/migrations/*.sql | grep -v '\.down\.sql$' | sort); do
  docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
    -v ON_ERROR_STOP=1 -f "/migrations/$(basename "$migration")"
done
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -f "/seed/$(basename "$SEED_FILE")"

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
    FROM semantic_facts
    WHERE tenant_id = 'tenant_a'
      AND scope_kind = 'chat'
      AND chat_id = 'no_memory_chat'
  ) THEN
    RAISE EXCEPTION 'no_memory_chat must not have persisted memory facts';
  END IF;

  INSERT INTO drafts (
    draft_id,
    tenant_id,
    chat_id,
    source_message_id,
    source_message_text,
    response_text,
    draft_type
  ) VALUES (
    'smoke_warning_draft',
    'tenant_a',
    'semi_auto_chat',
    'high_risk_message',
    'Smoke warning draft',
    '',
    'warning'
  )
  ON CONFLICT (draft_id) DO UPDATE SET
    draft_type = EXCLUDED.draft_type,
    updated_at = now();

  DELETE FROM drafts
  WHERE draft_id = 'smoke_warning_draft';

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
