# Multi-bot support — implementation plan

**Goal:** one tenant can connect several Telegram bots; each bot has its own chats,
memory, and governing role. Today the backend is hard single-bot.

## Current single-bot assumptions (must change)

1. `user_bots` keyed by `tenant_id` alone (`ON CONFLICT (tenant_id)`); in-memory
   `Repository.userBots map[tenantID]UserBotRecord`. New bot **replaces** old.
2. Governing role is per-tenant-bot: `SetUserBotRole(tenantID, roleID)`.
3. **Chat identity has no bot scoping.** `telegram.NormalizeBotUpdate` sets
   `event.ChatID = message.Chat.ID` (raw Telegram id). Synthetic `chats.chat_id`
   == `telegram_chat_id` for bot transport. Two bots talking to the same person
   ⇒ same `chat_id` ⇒ merged chats/facts/drafts. **This is the blocker.**
4. Outbound: `sendRecipient` sends to `event.ChatID` directly, and
   `botSenders.SenderFor(tenantID)` picks the bot by tenant.

## Key simplifier

Every per-chat table (`messages`, `drafts`, `chat_settings`, `semantic_facts`,
`episode_summaries`, `active_episodes`, `contact_profiles`,
`contact_profile_suggestions`, `fact_*`, `message_states`, `audit_events`,
`extraction_log`, `idempotency_records`) keys on the **synthetic `chat_id`**.
The `chats` table already carries a separate `telegram_chat_id`.

⇒ If we make the bot-transport `chat_id` = `bot:{botID}:{telegramChatID}` while
keeping `telegram_chat_id` raw, **all memory becomes bot-scoped with no schema
change to those tables**. The work concentrates in: `user_bots` (multi), chat
creation/identity, the send path, the bots API, and the Mini App.

## Phases

### Phase 0 — invariants & tests first
- Write failing tests that encode the contract:
  - two bots, same Telegram user → two distinct chats, no fact/draft bleed;
  - per-bot role applies only to that bot's chats;
  - removing one bot leaves the other's chats/memory intact;
  - **personal/business chat identity is unchanged** by the multi-bot work
    (same `chat_id` for the same contact, never bot-namespaced).

### Phase 1 — store: multiple bots per tenant
- Migration `101_multibot.sql`:
  - `user_bots` PK → `(tenant_id, bot_id)` (drop the tenant-unique constraint);
    `ai_role_id` already per-row.
- `store/user_bots.go`: re-key in-memory `userBots` to `tenant:bot`; add
  `UserBotsByTenant(tenantID) []UserBotRecord`, `UserBotByID(tenantID, botID)`,
  `SetUserBotRole(tenantID, botID, roleID)`, `DeleteUserBot(tenantID, botID)`.
  Keep a `PrimaryUserBot` shim only if needed for back-compat during rollout.

### Phase 2 — chat identity is TRANSPORT-BRANCHED (two logics)

There are two distinct identity logics; bot namespacing applies to **bot
transport only**. Never apply the bot namespace to personal/business chats.

- **Bot transport (`transport == "bot"`)** — multi-bot, namespaced:
  - `chat_id = BotChatID(botID, telegramChatID)` (e.g. `bot:{botID}:{tgChatID}`).
  - `event.BotID` is already populated by `NormalizeBotUpdate`; thread it into
    chat creation/lookup (ingestion + `botservice` + `bot.Layer`).
  - raw id stored in `chats.telegram_chat_id`; outbound uses it (Phase 3).
  - this is what isolates two bots talking to the same Telegram user.

- **Personal / business transport (`transport == "business"`)** — unchanged:
  - identity stays keyed by the **user's own chat id** under the owner tenant
    (`NormalizeBusinessUpdate`: `tenantID` = owner / `tenant:{bcID}`,
    `chatID = message.Chat.ID`). **No bot scoping, no `bot_id` in the key.**
  - one logical conversation per contact, exactly as today.

- Mechanics: add `TelegramChatID` to `store.ChatRecord`; persist/read it (column
  exists). Centralise the branch in one helper, e.g.
  `func chatIdentity(ev NormalizedTelegramEvent) (chatID, telegramChatID string)`
  that returns the namespaced id for `transport=="bot"` and the raw id otherwise,
  so no call site re-implements the rule.
- Backfill: only bot-transport chats need `chat_id` rewritten → namespaced across
  per-chat tables (business chats untouched). Or, pre-launch, reset bot data.
  Decide at implementation time.

### Phase 3 — outbound routing
- `sendRecipient` / all `OutboundMessage{ChatID: …}` sites: send to the chat's
  `telegram_chat_id`, not the synthetic `chat_id`.
- `BotSenderProvider.SenderFor(tenantID)` → `SenderFor(tenantID, botID)`; resolve
  the bot from the chat record so replies go out the bot the message arrived on.

### Phase 4 — manager / pollers
- `Manager.RegisterBot` already runs one poller per `botID` and replaces by
  tenant — change "replace tenant's bot" to "add/replace this `botID`"; keep one
  poller per active bot. `ActiveUserBots` already lists all active rows.

### Phase 5 — HTTP API (+ contract/OpenAPI)
- `GET /api/v1/bots` → list of bots (was single).
- `POST /api/v1/bots` → add a bot (no longer replaces).
- `DELETE /api/v1/bots/{bot_id}` → disconnect one.
- `PUT /api/v1/bots/{bot_id}/role` → per-bot role.
- Update `contracts/openapi.yaml` + `definition_of_done.md`; bump miniapp
  `api/client.js` accordingly. Token still input-only, never echoed.

### Phase 6 — Mini App
- Chats screen: a section per connected bot (each its own chats), or a bot
  switcher; "+" → add bot always available.
- Bot settings screen keyed by `bot_id` (role picker already built in
  `add-bot.js`; generalise from single to selected bot).
- `nav` / routing: `/bot/{bot_id}` settings.

## Risks
- **Data bleed** if any per-chat read forgets the namespaced id — Phase 0 tests
  guard this.
- **Backfill correctness** for existing rows (or accept a reset pre-launch).
- **Send path** must never use the synthetic id as a Telegram id again.
- Frozen Go↔Python contract: confirm the AI service doesn't assume one bot per
  tenant (it operates per chat_id, so namespacing should be transparent — verify).

## Test strategy
- Unit: store multi-bot CRUD + per-bot role.
- Integration (in-memory repo): two bots / same user → isolation; per-bot role;
  disconnect isolation; send routes to correct bot + raw telegram id.
- HTTP: bots list/add/remove/role-by-bot.
- Keep every existing test green; no contract change to per-chat tables.
