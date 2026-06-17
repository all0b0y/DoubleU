#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

python3 - <<'PY'
from pathlib import Path
import re

root = Path(".")
readme = (root / "contracts" / "README.md").read_text()
openapi = (root / "contracts" / "openapi.yaml").read_text()
migration = (root / "infra" / "migrations" / "100_clean_schema_v2.sql").read_text()
seed = (root / "infra" / "seed" / "001_contract_scenarios.sql").read_text()

expected = {
    "Decision": ["draft", "auto_reply", "ignore", "observe", "ask_owner", "error_fallback"],
    "ChatMode": ["disabled", "silent", "manual", "semi_auto", "auto"],
    "PrivacyFlags": ["no_memory", "no_training", "private", "no_summary"],
    "RiskLevel": ["low", "medium", "high", "critical"],
    "MessageState": [
        "received",
        "normalized",
        "policy_checked",
        "ai_requested",
        "ai_generated",
        "decision_made",
        "draft_sent_to_owner",
        "approved_by_owner",
        "sent_to_recipient",
        "ignored",
        "failed",
    ],
}

for group, values in expected.items():
    for value in values:
        if f"`{value}`" not in readme and value not in readme:
            raise SystemExit(f"contracts/README.md missing {group} value {value}")
        if group != "MessageState" and value not in openapi:
            raise SystemExit(f"contracts/openapi.yaml missing {group} value {value}")
        if value not in migration:
            raise SystemExit(f"migration missing {group} value {value}")

# Table names as of the consolidated schema (100_clean_schema_v2.sql). The
# clean schema schema-qualifies tables with `public.` and renamed the memory
# tables: memory_facts -> semantic_facts, memory_embeddings -> fact_embeddings,
# summaries -> episode_summaries.
required_tables = [
    "audit_events",
    "message_states",
    "semantic_facts",
    "fact_embeddings",
    "episode_summaries",
    "idempotency_records",
    "chat_settings",
]
for table in required_tables:
    match = re.search(
        rf"CREATE TABLE IF NOT EXISTS public\.{table} \((.*?)\n\);", migration, re.S
    )
    if not match:
        raise SystemExit(f"migration missing table {table}")
    body = match.group(1)
    if table != "idempotency_records" and "tenant_id" not in body:
        raise SystemExit(f"{table} must be tenant-scoped")

for scenario in [
    "tenant_a",
    "tenant_b",
    "disabled_chat",
    "no_memory_chat",
    "semi_auto_chat",
    "high_risk_message",
    "low_risk_message",
]:
    if scenario not in seed:
        raise SystemExit(f"seed missing scenario {scenario}")

if "no_memory" not in seed or "memory_persisted" not in seed:
    raise SystemExit("seed must include no_memory privacy behavior")

print("Contract enum, schema, and seed checks passed.")
PY

if find backend-go ai-python -type f 2>/dev/null | grep -q .; then
  if rg -n "openai|anthropic|gemini|cohere|llm" backend-go ai-python \
      -g '!**/README*' \
      -g '!**/*test*' \
      -g '!**/__pycache__/**' >/tmp/doubleu_llm_hits.txt; then
    if ! rg -n "Model Router|ModelRouter|LLM Gateway|LLMGateway|model_router|llm_gateway" backend-go ai-python >/dev/null; then
      cat /tmp/doubleu_llm_hits.txt
      echo "Potential direct LLM provider reference without Model Router / LLM Gateway boundary." >&2
      exit 1
    fi
  fi
fi

echo "Security boundary checks passed."
