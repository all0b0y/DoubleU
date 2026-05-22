# Definition of Done

The Technical MVP is complete only when every item below is true.

## Runtime

- `docker compose up` starts the Go backend, Python AI service, Postgres, and Redis.
- Backend and AI service healthchecks return success.
- The main flow works end to end: Telegram event, normalization, policy/privacy check, AI request, decision, draft/auto/ask/ignore/error, audit event.
- `MOCK_LLM=true` runs deterministic local and CI scenarios without external LLM credentials.

## Product Behavior

- The bot can autonomously answer in explicitly allowed low-risk chats.
- The bot never auto replies in disabled, private, high-risk, critical-risk, low-confidence, or policy-blocked cases.
- Silent mode may analyze or summarize only when `no_memory` and `no_summary` do not block those actions.
- Error fallback never sends a message to the recipient.

## Quality

- Go backend test coverage is at least 70%.
- Python AI service test coverage is at least 70%.
- QA has no unresolved critical or high severity findings.
- Contract tests cover policy-relevant combinations of `Decision`, `ChatMode`, `PrivacyFlags`, and `RiskLevel`.

## Security

- Sender messages are treated as untrusted input.
- Prompt injection cannot reveal memory, change chat mode, or force auto reply.
- Tenant-scoped retrieval never crosses tenant or chat scope.
- Hard-deleted memory is not retrieved.
- `no_memory` blocks persisted facts, embeddings, messages, and summaries.
- `no_training` blocks feedback learning and style updates.

## Idempotency and Audit

- Every inbound message creates an audit event after policy/privacy evaluation.
- Duplicate Telegram updates do not create duplicate replies.
- Duplicate approval callbacks do not create duplicate replies.
- Send retries use the send idempotency key.
