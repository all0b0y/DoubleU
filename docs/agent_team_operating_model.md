# Agent Team Operating Model

This repository may be edited by multiple implementation agents at the same time. Agents must work inside their assigned ownership boundaries and must not revert unrelated work.

## Current Product UX Ownership

Agent 6 owns only:

- `docs/product_flows.md`
- `docs/agent_team_operating_model.md`

Agent 6 must read, but not edit, the contract and architecture files needed to keep UX decisions aligned:

- `contracts/README.md`
- `contracts/openapi.yaml`
- existing `docs/*.md`

## Shared Contract Rules

- Contract enums in `contracts/README.md` are source of truth.
- Product UX labels may be human-friendly, but implementation specs must include exact enum values.
- Approval UX must use only the frozen callback actions: `approve`, `reject`, `regenerate`, `edit`, `make_shorter`, `make_more_formal`, `no_memory`.
- Chat settings must map to `ChatMode` and `PrivacyFlags` exactly.
- Every flow must preserve idempotency for duplicate Telegram updates and duplicate callback payloads.

## Collaboration Rules

- Do not modify service, infra, or contract files while implementation agents are active unless explicitly assigned.
- Do not reformat unrelated documents.
- When adding product requirements, keep them implementation-ready: state trigger, user-visible behavior, backend state, and audit requirement.
- If UX needs a contract change, document it as a future proposal instead of silently changing contracts.

## Definition of Ready for UX Specs

A product flow is ready for implementation when it defines:

- Telegram entry point or callback trigger.
- Exact contract enum or payload fields involved.
- Owner-visible copy or required information.
- Recipient-visible behavior.
- State transition or audit requirement.
- Idempotency expectation.
- Acceptance scenario.

## Review Checklist for Agent 6

- Chat modes include `disabled`, `silent`, `manual`, `semi_auto`, and `auto`.
- Privacy flags include `no_memory`, `no_training`, `private`, and `no_summary`.
- Approval callbacks include `approve`, `reject`, `regenerate`, `edit`, `make_shorter`, `make_more_formal`, and `no_memory`.
- Audit visibility covers drafts, auto replies, ignored/observed messages, owner callbacks, and `error_fallback`.
- Error fallback notifies the owner and does not send unsafe content to the recipient.
- Acceptance scenarios cover the product's trust-building feature set: approval, auto-reply, privacy, audit, and recovery.

