# Phase 0 Contract Freeze

This directory is the source of truth for cross-service contracts in the Technical MVP.
Implementation agents must not introduce cross-service behavior that conflicts with these contracts.

## Non-Negotiable Engineering Rules

- User and sender messages are untrusted input and must never be treated as system or developer instructions.
- Direct LLM calls outside `Model Router -> LLM Gateway` are forbidden.
- Every inbound message must produce an `AuditEvent` after policy and privacy evaluation.
- Duplicate Telegram updates and duplicate approval callbacks must not create duplicate replies.
- Contract tests must cover all policy-relevant `Decision`, `ChatMode`, `PrivacyFlags`, and `RiskLevel` combinations.

## Enums

### Decision

- `draft`
- `auto_reply`
- `ignore`
- `observe`
- `ask_owner`
- `error_fallback`

### ChatMode

- `disabled`
- `silent`
- `manual`
- `semi_auto`
- `auto`

### PrivacyFlags

- `no_memory`
- `no_training`
- `private`
- `no_summary`

### RiskLevel

- `low`
- `medium`
- `high`
- `critical`

## Confidence Thresholds

- `semi_auto_min = 0.85`
- `auto_min = 0.92`

## Risk Policy

- `low`: may auto reply if chat mode and privacy settings allow it.
- `medium`: draft by default.
- `high`: draft only.
- `critical`: ignore or ask owner.

## Memory Scope Rules

- Conversation memory: `tenant_id + chat_id`.
- RAG memory: `tenant_id + knowledge_base_id`.
- Persona memory: `tenant_id`.
- `no_memory` blocks persisted messages, facts, embeddings, and summaries beyond transient processing.
- Hard-deleted memory must never be retrieved.

## Idempotency

- Receive idempotency key: `telegram_update_id + business_connection_id`.
- Send idempotency key: `tenant_id + chat_id + source_message_id + decision_id`.
- Approval callback idempotency key is carried by the callback payload.

## Message State Machine

- `received`
- `normalized`
- `policy_checked`
- `ai_requested`
- `ai_generated`
- `decision_made`
- `draft_sent_to_owner`
- `approved_by_owner`
- `sent_to_recipient`
- `ignored`
- `failed`

## Approval Callback Contract

Actions:

- `approve`
- `reject`
- `regenerate`
- `edit`
- `make_shorter`
- `make_more_formal`
- `no_memory`

Payload fields:

- `action`
- `tenant_id`
- `chat_id`
- `source_message_id`
- `draft_id`
- `decision_id`
- `idempotency_key`

