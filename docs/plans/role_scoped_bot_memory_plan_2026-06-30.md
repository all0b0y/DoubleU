# Role-Scoped Bot Memory Context Plan

This plan is the source of truth for implementing reusable role-scoped memory
contexts for bot chats in DoubleU. It is written for a coding agent that starts
without prior chat context and must implement the work one slice at a time.

## Purpose

When a user changes a bot from role A to role B, the AI must stop using role A
conversation memory while answering as role B. Existing history must remain
stored and visible where the product already shows it. If the user switches the
bot back to role A, the AI should reuse the previous role A memory instead of
starting from zero.

The correct model is not a destructive reset and not a single reset timestamp.
The correct model is a reusable role memory context identity.

## OpenAI Prompting Basis

- OpenAI GPT-5.5 prompt guidance favors outcome-first prompts with explicit
  success criteria, constraints, available context, validation rules, and clear
  stopping conditions:
  https://developers.openai.com/api/docs/guides/prompt-guidance
- OpenAI code generation guidance positions GPT-5.5 as the default model for
  broad coding workflows that inspect requirements, write code, and reason
  across a codebase:
  https://developers.openai.com/api/docs/guides/code-generation
- OpenAI ExecPlan guidance requires self-contained living plans with progress,
  decisions, concrete steps, validation, and observable outcomes:
  https://developers.openai.com/cookbook/articles/codex_exec_plans

## Non-Negotiable Rules

1. Use codebase-memory-mcp for code discovery before grep or file search:
   `index_status` or `index_repository`, then `search_graph`, `trace_path`,
   `get_code_snippet`, and only then `search_code` for literals, SQL, config,
   and non-code files.
2. One run implements exactly one slice. Stop after the slice and report.
3. Create a separate git commit after every completed slice.
4. Run `git status --short` before staging. Do not stage unrelated changes.
5. Do not add hidden defaults or hardcoded memory behavior.
6. If a bot role update payload includes `role_id`, it must also include the
   required parameter `role_context_policy`.
7. If `role_id` is present and `role_context_policy` is absent or invalid, the
   backend must return HTTP 400.
8. Allowed `role_context_policy` values:
   - `reuse_role_context`: activate an existing context for
     `(tenant_id, bot_id, role_id)` or create one if it does not exist.
   - `new_role_context`: create a new empty context for this role, even if an
     older context for the same role exists.
9. `role_id = 0` means no role. It must use the same context model without a
   magic role id. Represent it as SQL NULL or an explicit nullable field.
10. Do not delete old messages, facts, episodes, drafts, extraction logs, or
    profile data as part of role switching.
11. UI message history may keep showing all chat history. AI context assembly
    must be isolated by the active role memory context.
12. If HTTP contracts change, update Go contracts/tests, Mini App API/UI, and
    `contracts/openapi.yaml` in the same slice.
13. Every slice must include self-review before the final report:
    - what changed;
    - why the change preserves existing behavior;
    - which edge cases are covered;
    - what remains risky or unfinished.

## Current Code Orientation

Use MCP to re-check these before editing. The line numbers below are routing
hints, not permission to skip discovery.

- `backend-go/internal/core/ai_context.go`
  - `conversationContextAndMessages`: assembles memory context from recent
    messages, semantic facts, active episode, episode summaries, and drafts.
  - `resolveRole`, `aiRoleFor`, `botForChat`: resolve the governing bot role.
- `backend-go/internal/core/ai_turn.go`
  - `processContactMessage`: prepares `AIProcessMessageRequest`, saves
    messages, updates episodes, saves drafts, and launches async extraction.
- `backend-go/internal/httpapi/bots.go`
  - `apiSetBotRole`: current bot role update endpoint.
- `backend-go/internal/store/user_bots.go`
  - `SetUserBotRole`: current role swap on `user_bots.ai_role_id`.
- `backend-go/internal/store/messages.go`
  - `RecentMessages`: current chat-scoped recent message query.
- `backend-go/internal/store/semantic_facts.go`
  - `SemanticFactsByVector`, `SemanticFactsByBM25`, `SemanticFactsByIDs`,
    `hardIncludeChatFacts`: current fact retrieval surface.
- `backend-go/internal/store/episode_summaries.go`
  - `ActiveEpisodeByChat`, `EpisodeSummariesByVector`: current episodic
    retrieval surface.
- `backend-go/internal/store/drafts.go`
  - `DraftsForChat`: current pending draft retrieval surface.
- `miniapp/api/client.js`
  - `setBotRole`: Mini App bot role update client.
- `contracts/openapi.yaml`
  - public contract surface for bot role update.

## Target Architecture

Add a durable role memory context model:

- New table: `bot_role_contexts`.
- Suggested fields:
  - `id BIGSERIAL PRIMARY KEY`
  - `tenant_id TEXT NOT NULL`
  - `bot_id BIGINT NOT NULL`
  - `ai_role_id BIGINT NULL`
  - `created_at TIMESTAMPTZ NOT NULL DEFAULT now()`
  - `last_activated_at TIMESTAMPTZ NOT NULL DEFAULT now()`
  - `archived_at TIMESTAMPTZ NULL`
- Add `user_bots.active_role_context_id BIGINT NULL`.
- Add `role_context_id BIGINT NULL` to memory-bearing tables used by AI
  context:
  - `messages`
  - `semantic_facts`
  - `episode_summaries`
  - `active_episodes`
  - `drafts`
  - include `draft_feedback`, `extraction_log`, profile suggestion/profile
    tables if implementation inspection shows they affect AI context or
    consistency.
- Add indexes for role-context-aware AI reads, for example:
  - `(tenant_id, chat_id, role_context_id, received_at)` on `messages`;
  - `(tenant_id, chat_id, role_context_id, created_at)` on semantic facts;
  - vector/BM25 compatible filters must include `role_context_id`;
  - drafts and episodes must support lookup by active role context.

Backfill requirement:

- Existing bot memory must be assigned to a context matching the bot's current
  role at migration time, so existing deployments do not lose current AI
  context.
- Non-bot chats may keep legacy NULL `role_context_id` semantics unless a safer
  unified path is discovered during implementation.

## Slice 1: Schema And Store Primitives

Goal: introduce the role memory context model without changing runtime AI
behavior yet.

Tasks:

1. Add SQL migrations for `bot_role_contexts`, `active_role_context_id`, and
   `role_context_id` fields.
2. Add indexes needed for future AI-context filters.
3. Backfill existing bot memory into a context for the bot's current role.
4. Add store records and methods:
   - `RoleContextPolicy` with values `reuse_role_context` and
     `new_role_context`.
   - `RoleContextRecord`.
   - `EnsureBotRoleContext(tenantID string, botID int64, roleID *int64, policy RoleContextPolicy) (RoleContextRecord, error)`.
   - `ActiveBotRoleContext(tenantID string, botID int64) (RoleContextRecord, bool)`.
   - role update method that requires an explicit policy when role id changes.
5. Keep current public runtime behavior unchanged.

Tests:

- `reuse_role_context` returns the same active context for role A.
- `new_role_context` creates a distinct context for role A.
- no-role context is represented without a magic role id.
- migration/backfill preserves existing current bot memory.

Validation:

- `cd backend-go && go test ./internal/store`

Commit:

- `feat(memory): add bot role context store model`

Stop after this slice and report commit hash, changed files, tests, self-review,
and next slice.

## Slice 2: Tag New Memory With Active Role Context

Goal: new memory records created on bot paths carry the active role context.

Tasks:

1. Resolve the active role context before saving bot incoming messages.
2. Persist `role_context_id` on:
   - incoming messages;
   - outgoing bot replies;
   - semantic facts from synchronous and asynchronous extraction;
   - active episodes;
   - episode summaries;
   - drafts.
3. Capture the role context id before launching async extraction so delayed
   goroutines do not accidentally write into a newer active context.
4. Preserve non-bot chat behavior.
5. Do not filter AI context yet in this slice.

Tests:

- a message in role A is tagged with context A.
- after switching to role B, new memory is tagged with context B.
- async extraction writes into the captured context, not whatever context is
  active when the goroutine finishes.

Validation:

- `cd backend-go && go test ./internal/core ./internal/store`

Commit:

- `feat(memory): tag bot memory with role context`

Stop after this slice and report commit hash, changed files, tests, self-review,
and next slice.

## Slice 3: Filter AI Context By Active Role Context

Goal: AI context assembly reads only the active role context for bot chats.

Tasks:

1. Add explicit role-context-aware store methods or parameters. Do not silently
   change UI-facing list methods if those methods should continue to show all
   chat history.
2. Update `conversationContextAndMessages` so bot chats filter:
   - recent messages;
   - semantic facts vector retrieval;
   - semantic facts BM25 retrieval;
   - hard-include taboo/health facts;
   - active episodes;
   - episode summaries;
   - pending drafts.
3. Ensure role A -> role B prevents role A memory from entering role B prompts.
4. Ensure role B -> role A reuses role A memory with `reuse_role_context`.
5. Ensure `new_role_context` for role A starts clean even if older role A
   context exists.

Tests:

- role A fact/message is visible in role A context.
- role B does not see role A fact/message.
- switching back to role A with `reuse_role_context` sees role A memory again.
- switching to role A with `new_role_context` does not see old role A memory.
- non-bot chat behavior remains unchanged.

Validation:

- `cd backend-go && go test ./internal/core ./internal/store`

Commit:

- `feat(memory): isolate AI context by bot role`

Stop after this slice and report commit hash, changed files, tests, self-review,
and next slice.

## Slice 4: Explicit HTTP Contract And Mini App Flow

Goal: role switching requires explicit memory policy from API/UI.

Tasks:

1. Update `PATCH /api/v1/bots/{bot_id}/role` handling:
   - if `role_id` is present, require `role_context_policy`;
   - if only delivery settings are present, do not require policy;
   - reject missing or invalid policy with HTTP 400.
2. Update Go request parsing and handler tests.
3. Update `contracts/openapi.yaml`.
4. Update Mini App API client.
5. Update bot role UI so the user explicitly chooses:
   - reuse existing memory for this role;
   - start a new memory context for this role.
6. Do not add hidden JS defaults that bypass the explicit choice.

Tests:

- role change without `role_context_policy` returns 400.
- role change with `reuse_role_context` succeeds.
- role change with `new_role_context` succeeds.
- delivery-only patch succeeds without policy.
- Mini App sends the selected policy.

Validation:

- `cd backend-go && go test ./internal/httpapi ./internal/store`
- run the existing Mini App check if the repo defines one.

Commit:

- `feat(bots): require explicit role context policy`

Stop after this slice and report commit hash, changed files, tests, self-review,
and next slice.

## Slice 5: Regression Sweep And Cleanup

Goal: prove the role-scoped memory behavior end to end and remove accidental
legacy reads from the bot AI path.

Tasks:

1. Use MCP and targeted `search_code` to find all bot AI context reads:
   - `RecentMessages(`
   - `SemanticFactsByVector(`
   - `SemanticFactsByBM25(`
   - `SemanticFactsByIDs(`
   - `hardIncludeChatFacts`
   - `DraftsForChat(`
   - `EpisodeSummariesByVector(`
   - `ActiveEpisodeByChat(`
2. Verify bot path uses role-context-aware reads.
3. Verify UI/history path still shows expected chat history.
4. Update docs/comments only where needed to explain role-context isolation.
5. Remove dead helper code introduced during migration if it is no longer used.

Acceptance scenarios:

- A bot in role A accumulates memory.
- The same bot switches to role B with `reuse_role_context`.
- AI in role B does not see role A memory.
- Switching back to role A with `reuse_role_context` restores role A memory.
- Switching to role A with `new_role_context` creates a clean context.
- Missing `role_context_policy` on role change returns 400.
- Non-bot chats still use existing chat-scoped memory behavior.

Validation:

- `cd backend-go && go test ./...`
- if shared Python/OpenAPI contracts changed in a way Python consumes, run
  `ai-python/.venv/bin/python -m pytest`.

Commit:

- `test(memory): verify reusable role-scoped bot context`

Stop after this slice and report commit hash, changed files, tests, self-review,
and completion status.

## Progress

- [ ] Slice 1: schema and store primitives.
- [ ] Slice 2: tag new memory with active role context.
- [ ] Slice 3: filter AI context by active role context.
- [ ] Slice 4: explicit HTTP contract and Mini App flow.
- [ ] Slice 5: regression sweep and cleanup.

## Decision Log

- Decision: Use reusable role context identity instead of `role_context_reset_at`.
  Rationale: reset timestamps cannot naturally restore old role memory when the
  user switches back to a prior role.
  Date/Author: 2026-06-30 / planning agent.
- Decision: Require `role_context_policy` when `role_id` is present.
  Rationale: hidden defaults would make memory behavior surprising and would
  violate the product requirement that the user explicitly chooses reuse or a
  clean context.
  Date/Author: 2026-06-30 / planning agent.
- Decision: Keep UI history and AI memory filtering separate.
  Rationale: the user should not lose visible history, but AI context must not
  mix role-specific memory.
  Date/Author: 2026-06-30 / planning agent.

## Retrospective Template

After each slice, append:

- Date:
- Slice:
- Commit:
- Validation:
- What changed:
- Self-review:
- Remaining risks:
