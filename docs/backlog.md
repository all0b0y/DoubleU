# DoubleU — Engineering Backlog

---

## Inefficiency Backlog

- `applyMigrations` re-executes all migration files on every startup. Should track applied migrations in a `schema_migrations` table to run each file exactly once.
- `ContextScore` recomputes on every mode selection check — could be cached per chat with a short TTL.
- `RecentMessages` in in-memory mode iterates all messages on every call — O(n). Switch to sorted structure for large in-memory datasets.

---

## Bug Backlog

- `audit_events` and `message_states` still have FK constraints pointing to `messages` in new installations (002 migration drops them but the constraint name may differ across PostgreSQL versions — verify on fresh DB).
- Draft `OwnerMessageID` is not persisted to PostgreSQL (only in-memory). After restart, the bot cannot delete/edit old draft cards. Needs a `drafts` table column.

---

## Tech Debt

- No versioning scheme for migrations. Need a proper migration runner (e.g. goose, atlas) to track applied migrations and support rollback.
- The `SaveMessage` / `AppendState` ordering issue was worked around by checking `EventID` in-memory. The correct fix is to ensure `SaveMessage` always runs before any state/audit writes, with a refactor of `HandleTelegramWebhook` order.
- `BotSender.SendMessageGetID` duplicates most of `BotSender.SendMessage`. Extract a shared `sendMessageRaw(ctx, body) ([]byte, error)` helper.
- Hash-based placeholder embeddings in `saveMessageMemory` should be replaced with real semantic embeddings (OpenAI `text-embedding-3-small` or similar via OpenRouter). Each chat is a separate embedding space (tenant_id + chat_id scope). Enable pgvector on Postgres and switch `memory_embeddings.embedding` column to `vector(1536)` for cosine similarity retrieval. This replaces the current recency-based `RecentMessages` context window with semantic recall per chat.
- No connection pooling configuration for PostgreSQL (`db.SetMaxOpenConns`, `db.SetMaxIdleConns`).

---

## Iteration 2 (not started)

- Mini App UI — web dashboard for draft review, memory viewer, chat management
- AI Roles Marketplace — persona presets per chat (Sales, Support, Personal assistant)
- Telegram Client API / MTProto — historical message access for tenants granting full access
