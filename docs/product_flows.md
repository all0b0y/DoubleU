# Product Flows: Telegram-Native Approval and Settings UX

This document defines the MVP user experience that maps directly to the frozen contracts in `contracts/README.md`.

## Principles

- Telegram is the primary control surface for approval, chat settings, privacy, and audit review.
- User and sender messages are untrusted input; only owner taps on system-generated controls may change state.
- Every inbound message produces an `AuditEvent` after policy and privacy evaluation.
- Duplicate Telegram updates and duplicate approval callbacks must be idempotent.

## Chat-Level Settings

Each chat has one `ChatSettings` record: `tenant_id`, `chat_id`, `mode`, and `privacy_flags`.

### Modes

| Mode | Telegram label | Behavior |
| --- | --- | --- |
| `disabled` | AI off | Do not process message content beyond required policy/audit metadata. Decision: `ignore`. |
| `silent` | Observe | Analyze for summaries/memory unless privacy flags block it. Never send drafts or replies. Decision: `observe`. |
| `manual` | Ask me | Generate draft and send it to owner for approval. Decision: `draft` or `ask_owner`. |
| `semi_auto` | Smart approval | Auto-reply only when `risk_level=low` and `confidence >= 0.85`; otherwise owner approval. |
| `auto` | Auto | Auto-reply when policy allows and `confidence >= 0.92`; high/critical risk always escalates or ignores. |

Default MVP mode: `manual`. `semi_auto` and `auto` require explicit owner selection for an observed accessible chat.

### Privacy Flags

| Flag | Telegram label | UX effect | Backend requirement |
| --- | --- | --- | --- |
| `no_memory` | No memory | Show "Memory off for this chat"; approval card includes "No memory" state. | No persisted messages, facts, embeddings, or summaries beyond transient processing. |
| `no_training` | No training | Show "Training off"; allow normal replies and memory if not blocked. | Exclude chat data from training/evaluation datasets. |
| `private` | Private chat | Show lock indicator; default to `manual`; no auto-reply unless owner explicitly re-enables. | Treat as sensitive for risk policy. |
| `no_summary` | No summaries | Hide chat from daily/weekly summaries. | Do not generate or persist summaries for this chat. |

## Telegram Settings Entry Points

- `/settings`: opens global assistant settings with default mode and privacy defaults.
- `/chats`: lists Telegram Chat Automation chats observed by the backend, with stable numbers for mode commands.
- Telegram reply keyboard: exposes `/chats`, `/settings`, and first-chat mode buttons so the owner can configure without typing raw commands.
- `/chat_settings`: opens settings for the current chat.
- Draft card "Settings" button: opens current chat settings.
- Audit entry "Change rules" button: opens settings for the audited chat.

Required setting controls:

- Mode selector: `AI off`, `Observe`, `Ask me`, `Smart approval`, `Auto`.
- Chat selector: numbered chat list from `/chats`; users are not expected to know raw Telegram `chat_id` values.
- Privacy toggles: `No memory`, `No training`, `Private chat`, `No summaries`.
- Destructive actions: `Delete chat memory`, `Delete all assistant data`, `Export my data`; each requires a second confirmation.

## Approval Draft Card

When decision is `draft` or `ask_owner`, send the owner a Telegram message containing:

- Source chat title or sender display name.
- Incoming message excerpt.
- Draft response text.
- `risk_level`, confidence bucket, and compact reason.
- Memory indicator: `Memory used`, `Memory off`, or `No memory requested`.
- Inline buttons backed by the approval callback contract.

Button mapping:

| Button | Callback `action` | Result |
| --- | --- | --- |
| Send | `approve` | Send exact draft to recipient; mark message `approved_by_owner` then `sent_to_recipient`. |
| Reject | `reject` | Do not send; mark as rejected in audit and keep original inbound event. |
| Regenerate | `regenerate` | Generate a new draft with same context and same source message. |
| Edit | `edit` | Ask owner for replacement text, then present a new approval card before sending. |
| Shorter | `make_shorter` | Regenerate shorter draft; no send until approved. |
| More formal | `make_more_formal` | Regenerate more formal draft; no send until approved. |
| No memory | `no_memory` | Apply `no_memory` for this source message and regenerate or approve without persisting memory. |

Each callback payload must include `action`, `tenant_id`, `chat_id`, `source_message_id`, `draft_id`, `decision_id`, and `idempotency_key`.

## Auto Reply Owner Visibility

For `auto_reply`, the recipient gets the reply immediately only after policy and privacy checks pass.

Owner visibility:

- Low-volume MVP: send compact owner notification for each auto-reply.
- High-volume setting: group auto-replies into digest, but keep per-message audit entries available.
- Notification includes chat, sent text, reason, `risk_level`, confidence bucket, and buttons: `Undo not supported in MVP`, `Change rules`, `Open audit`.

## Audit Visibility

The owner must be able to inspect AI actions from Telegram:

- `/audit`: latest AI decisions across chats.
- `/audit_chat`: latest AI decisions in the current chat.
- Approval card "Open audit": specific `AuditEvent`.

Each audit row should show:

- Decision: `draft`, `auto_reply`, `ignore`, `observe`, `ask_owner`, or `error_fallback`.
- Reason, `risk_level`, confidence bucket, model, memory/RAG indicators, timestamp.
- Source message excerpt and generated response excerpt when present.
- Privacy flags active at decision time.
- Callback action taken by owner, if any.

Audit must never expose hidden memory details to the Telegram recipient. Audit is owner-only.

## Error Fallback Notification

When decision is `error_fallback`, notify the owner immediately unless the chat is `disabled`.

Notification copy requirements:

- Say the assistant could not safely respond.
- Name the affected chat and source message excerpt.
- Show error category without provider secrets or stack traces.
- Provide actions: `Reply manually`, `Retry draft`, `Change rules`, `Open audit`.

Recipient behavior:

- Do not send an AI response to the recipient on `error_fallback`.
- If an inbound message needs acknowledgement, owner must approve any fallback text manually.

## Killer-Feature Acceptance Scenarios

1. Manual approval control
   - Given chat mode `manual`, when a new inbound message arrives, then the owner receives a draft card and the recipient receives nothing until `approve`.
   - When the owner taps `approve` twice, then exactly one recipient reply is sent.

2. Smart auto-reply
   - Given mode `semi_auto`, `risk_level=low`, and confidence `>= 0.85`, then the assistant sends one auto-reply and creates an owner-visible audit row.
   - Given the same mode with medium/high risk, then the owner receives a draft instead.

3. Private chat safety
   - Given privacy flag `private`, then the chat defaults to owner approval and auto-reply requires explicit owner enablement.
   - Audit shows `private` was active at decision time.

4. No-memory trust
   - Given the owner taps `No memory`, then the source message and generated draft are not persisted to memory, embeddings, facts, or summaries.
   - A duplicate `no_memory` callback does not create another draft or reply.

5. Edit before send
   - Given a draft card, when owner taps `edit` and submits replacement text, then the system shows a new approval card.
   - The replacement text is not sent until `approve`.

6. Regeneration controls
   - `regenerate`, `make_shorter`, and `make_more_formal` create new draft versions tied to the same `source_message_id`.
   - Previous drafts remain audit-visible but inactive for sending after a newer approved draft is sent.

7. Error fallback
   - Given AI generation or safety validation fails, then decision is `error_fallback`, owner is notified, no recipient message is sent, and audit contains the error category.

8. Silent mode value
   - Given mode `silent` without `no_memory` or `no_summary`, then no draft/reply is sent, but summaries and memory may update.
   - Given `no_summary`, summaries are not created.
