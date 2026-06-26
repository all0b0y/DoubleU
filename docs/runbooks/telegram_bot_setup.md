# Telegram Bot Setup

This document describes the first-run setup flow for the Telegram Chat Automation bot.

## Developer Setup

1. Create a bot in `@BotFather` and save the token.
2. Enable Business Mode for the bot in BotFather.
3. Expose the backend through a public HTTPS URL.
4. Set environment variables:

```bash
TELEGRAM_BOT_TOKEN=...
PUBLIC_WEBHOOK_URL=https://your-public-domain/telegram/webhook
TELEGRAM_WEBHOOK_SECRET=optional-random-secret
TELEGRAM_AUTO_SETUP=true
MOCK_LLM=false
OPENROUTER_API_KEY=...
OPENROUTER_MODEL=provider/model-name
```

When `TELEGRAM_AUTO_SETUP=true`, backend startup configures:

- `setWebhook` with allowed updates:
  - `message`
  - `callback_query`
  - `business_connection`
  - `business_message`
  - `edited_business_message`
  - `deleted_business_messages`
- Telegram command menu:
  - `/start`
  - `/settings`
  - `/chats`
  - `/help`
  - `/mode_manual`
  - `/mode_semi_auto`
  - `/mode_auto`
  - `/mode_silent`
  - `/mode_off`

## User First-Run Flow

1. User opens the bot and sends `/start`.
2. Bot explains how to enable Telegram Chat Automation.
3. User opens Telegram Settings -> Chat Automation.
4. User connects this bot to their profile.
5. User grants access to selected chats. Full access is not required.
6. If user wants auto replies, user enables reply permission. Limited access without reply permission is valid, but backend will block recipient sends and produce `ask_owner`.
7. Telegram sends a `business_connection` update to the backend.
8. Backend stores:
   - `business_connection_id`
   - owner user id
   - owner chat id
   - `can_reply`
   - `is_enabled`
9. Bot sends a connection status message to the owner.
10. When an accessible chat sends a business message, backend stores the observed chat with display name and last-seen metadata.
11. User sends `/chats` and chooses a chat by list number.
12. In a managed chat, user can also tap “Manage Bot”.
13. Telegram sends a deep link command like:

```text
/start bizChat<chat_id>
```

14. Bot explains the current chat mode and points the user to `/chats` so they do not need to know raw Telegram chat IDs.

## Runtime Commands

```text
/settings
/chats
/mode_manual <number>
/mode_semi_auto <number>
/mode_auto <number>
/mode_silent <number>
/mode_off <number>
```

Default mode for unknown chats is `manual`.

`/chats` lists chats observed through Telegram Chat Automation. Users can select by number, for example `/mode_auto 1`. Direct chat IDs are still accepted internally for support/debugging, but the user-facing flow does not require knowing them.

If Telegram sends business messages before the backend receives or retains the matching `business_connection` owner mapping, the chat can first appear under a temporary `tenant:<business_connection_id>`. When the owner selects it through `/mode_* 1`, backend claims that business connection for the owner tenant and future messages use the same tenant as owner commands.

If `/chats` is empty after limited access setup, the bot explains that Telegram has not delivered an accessible chat yet. The user should verify the chat is selected in Telegram Settings -> Chat Automation or use “Manage Bot” in that chat.

Recommended rollout:

1. Start with `/mode_manual 1`.
2. Move trusted low-risk chats to `/mode_semi_auto 1`.
3. Use `/mode_auto 1` only where full automation is acceptable.

## Backend Behavior

- Ordinary `/start` and settings commands are handled as owner onboarding messages.
- `business_connection` updates are saved and audited.
- `business_message` updates enter the AI decision pipeline.
- Observed business chats are saved for owner selection in `/chats`.
- If the saved business connection has `can_reply=false`, backend blocks auto reply and returns `ask_owner`.
- Backend emits structured logs for received business messages, AI analysis start/end/failure, final decision, draft creation, skipped chats, and successful auto sends.
- Every onboarding, connection, duplicate, and business-message path creates an audit event.

## AI Service Provider Mode

Local and CI runs should keep `MOCK_LLM=true` for deterministic tests.

Real provider mode uses OpenRouter only:

```bash
MOCK_LLM=false
OPENROUTER_API_KEY=...
OPENROUTER_MODEL=provider/model-name
```

The AI service still keeps the boundary `Model Router -> LLM Gateway`; no service code calls OpenRouter directly.

Optional fallback model:

```bash
OPENROUTER_FALLBACK_MODEL=provider/backup-model
OPENROUTER_TIMEOUT_SECONDS=10
```

If the primary OpenRouter model times out or fails through the gateway, the AI service retries once with `OPENROUTER_FALLBACK_MODEL`.

## Sources

Telegram Bot API:

- Business connection updates: `business_connection`.
- Business messages: `business_message`, `edited_business_message`, `deleted_business_messages`.
- Sending as a connected business account: `business_connection_id` in `sendMessage`.
- Managed chat entry point: `/start bizChat<user_chat_id>` from “Manage Bot”.
