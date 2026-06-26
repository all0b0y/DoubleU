# Spam Protection & Bot-Mode Persona Split — Implementation Plan

**Audience:** implementer (Sonnet 4.6). **Status:** ready to build. **Scope:** backend-go (+ one migration). No `/contracts/` change. No Python change required (Go-side gating only).

## Goal

Stop message floods/spam from (a) reaching long-term memory (`episode_summaries`, `semantic_facts`) and (b) polluting the short-term context window — without breaking normal chat. Then stop personal-assistant owner-identity leaking into multi-user bot chats.

Four tasks, build **in this order** (each is its own PR, independently shippable):

1. **② Episode-write gate** — flood cannot manufacture an episode summary.
2. **① Spam-tag (quarantine)** — tagged messages persist (audit) but leave memory surfaces.
3. **③ Rate-limit (chat+sender)** — feeds the quarantine flag from ① and skips replies under flood.
4. **Persona-split** — drop owner identity + feedback from bot-mode (`speak_as_owner=false`) requests.

## Guardrails (from CLAUDE.md — binding)

- **Contract frozen.** Do **not** add fields to `contracts.NormalizedTelegramEvent` or other `/contracts/` types. The quarantine flag lives only in `store.MessageRecord` + the `messages` table. Persona-split only changes which *optional* fields Go populates — additive, contract-safe.
- **Privacy preserved.** `no_memory` chats already skip `SaveMessage` ([service.go:934](../../backend-go/internal/core/service.go:934)); quarantine is a no-op there. Idempotency (`MarkReceive`, [service.go:954](../../backend-go/internal/core/service.go:954)) stays intact — quarantined messages still pass dedupe.
- **Tests are functional**, against behavior/contract, real in-memory store (`r.db == nil` path), not fitted to code paths. Keep all tests green.
- **slog only**, structured. Errors handled or `_`-ignored with a reason comment. No new global mutable state except `sync.Map` (matches existing `debounce`/`tenantLimiters`).

## Shared primitive: the quarantine flag

Introduced in Task ①, consumed by ② and ③. One boolean per message: **persist for audit, exclude from every memory surface.**

- DB: new column `messages.quarantined BOOLEAN NOT NULL DEFAULT false` (migration **112** — see note on the 111 collision below).
- Memory: `store.MessageRecord` ([store.go:22](../../backend-go/internal/store/store.go:22)) gains `Quarantined bool`.
- Exclusion surfaces: `RecentMessages` (window-20), `summarizeEpisode` (summary input), the extraction trigger.

---

## Task ② — Episode-write gate (do first)

**Problem:** 40 messages in seconds hit `episodeMessageLimit` → instant `summarizeEpisode("message_count")` → spam compressed into `episode_summaries`. Close trigger is count-only, time-blind.

**Files / anchors**
- Close-by-count block: [service.go:985](../../backend-go/internal/core/service.go:985) (`if ep.MessageCount >= episodeMessageLimit`).
- Const: `episodeMessageLimit = 40` [service.go:3752](../../backend-go/internal/core/service.go:3752).
- `ep` carries `StartedAt`, `LastMessageAt` (fresh `now`) — span is computable here.

**Change**
- Add `const minEpisodeSpan = 2 * time.Minute` next to `episodeMessageLimit`.
- At the trigger: always `CloseActiveEpisode` (reset runtime), but **only schedule `summarizeEpisode` when `ep.LastMessageAt.Sub(ep.StartedAt) >= minEpisodeSpan`.** Otherwise log `episode closed by flood, summary skipped` with `message_count` + `span_seconds` and do **not** summarize.
- Rationale to encode in the comment: a sub-`minEpisodeSpan` burst of 40 is a flood, not a conversation. Real content (if any) is still captured later by the time-based daily fallback (`summarizeChatDay`, [service.go:3836](../../backend-go/internal/core/service.go:3836)), which — once Task ① lands — sees only non-quarantined messages.

**Do not touch** `closeStaleEpisodes` (6h silence) or the daily fallback: both are time-based and cannot be flood-triggered.

**Tests** (`backend-go/internal/core/*_test.go`)
- 40 messages with timestamps spanning `< minEpisodeSpan` → **no** `episode_summaries` row; episode is closed.
- 40 messages spanning `>= minEpisodeSpan` → exactly one summary (current behavior preserved).
- Drive `s.now()` (injectable clock, [service.go:115](../../backend-go/internal/core/service.go:115)) to control span deterministically.

**Acceptance:** a 2-second 40-message flood produces zero episode summaries; a normal long chat still summarizes.

---

## Task ① — Spam-tag (quarantine) + exclusion

**Problem:** every burst message is `SaveMessage`'d before debounce, so all land in the window-20 ([service.go:1472](../../backend-go/internal/core/service.go:1472)) and in summary input, even when the reply is debounced.

**Detector (intentionally minimal, extended by ③):** exact-duplicate-of-recent. A message is quarantined when its normalized text (`lower(trim(collapse-ws))`) equals one of the last ~5 contact messages in the chat. Cheap, local, no model call. Add `func classifyQuarantine(text string, recent []store.MessageRecord) bool`.

**Storage**
- Migration **112**: `ALTER TABLE messages ADD COLUMN quarantined BOOLEAN NOT NULL DEFAULT false;` (+ `.down.sql` dropping it).
- `MessageRecord.Quarantined bool` ([store.go:22](../../backend-go/internal/store/store.go:22)).
- New store method `SaveMessageQuarantined(event, quarantined bool)`; existing `SaveMessage(event)` delegates with `false` (keeps the unclaimed/synthetic call sites unchanged). Update the INSERT at [store.go:524](../../backend-go/internal/store/store.go:524) to write `quarantined` ($11) and set it in `ON CONFLICT`. Set `record.Quarantined` on the in-memory path too.

**Exclusion (all three)**
1. `RecentMessages` ([store.go:1346](../../backend-go/internal/store/store.go:1346)): add `AND quarantined = false` to the DB query; skip `record.Quarantined` in the in-memory loop.
2. `summarizeEpisode` texts loop ([service.go:3779](../../backend-go/internal/core/service.go:3779)): `if m.Quarantined { continue }`. Have `MessagesByDateRange` ([store.go:1972](../../backend-go/internal/store/store.go:1972)) and `Message` ([store.go:556](../../backend-go/internal/store/store.go:556)) select the column.
3. Extraction: thread a local `quarantined` bool through `processContactMessage` and short-circuit the extraction trigger (guarded by `shouldExtractFacts`, [service.go:2065](../../backend-go/internal/core/service.go:2065)) when set.

**Wire-in:** in `processContactMessage`, before `SaveMessage` ([service.go:935](../../backend-go/internal/core/service.go:935)), compute `quarantined := classifyQuarantine(event.MessageText, s.repository.RecentMessages(...,5))`, call `SaveMessageQuarantined(event, quarantined)`, and reuse the local for the extraction guard. **Audit is unchanged** — `AppendState`/`AppendAudit` still run.
- Optional hardening: skip `message_count++` on the active episode for quarantined messages (defense-in-depth with ②). Keep optional.

**Tests**
- Duplicate-of-recent message → `Quarantined==true`, absent from `RecentMessages`, present via `Message()` (audit intact), excluded from summary input, no extraction call.
- Unique message → unaffected.

**Acceptance:** repeated identical spam never appears in the window-20 or any summary, but is still retrievable as a raw audit row.

---

## Task ③ — Rate-limit on chat + sender

**Problem:** nothing caps messages/sec; a single user can flood a bot chat through the full persist→episode→reply path.

**Files / anchors**
- Struct fields ([service.go:108](../../backend-go/internal/core/service.go:108)): add `chatLimiters sync.Map`, `chatRate rate.Limit`, `chatBurst int`. `golang.org/x/time/rate` is already imported (see `tenantLimiters`).
- Option ([service.go:241](../../backend-go/internal/core/service.go:241) area): `WithChatRateLimit(r rate.Limit, burst int)`. Defaults (no Option): `chatRate = rate.Every(2*time.Second)` (0.5/s sustained), `chatBurst = 8`.
- Event sender: `event.SenderID` ([types.go:608](../../backend-go/internal/contracts/types.go:608)).

**Change**
- Key: bot transport → `tenant|chat|sender` (bot serves many people); business → `tenant|chat`. Lazily build a `*rate.Limiter` per key (mirror `tenantLimiters` construction).
- Check **after** dedupe ([service.go:960](../../backend-go/internal/core/service.go:960)), before debounce. **Owner-exempt:** skip when `isOwnerSideMessage(event, ownerID, event.Transport)` ([service.go:2200](../../backend-go/internal/core/service.go:2200)).
- On `!limiter.Allow()`: set the Task-① quarantine flag for this message (so it leaves memory), **skip the reply**, return `InboundResult{Decision: contracts.DecisionObserve, Reason: "rate_limited"}`, log once. Still `SaveMessageQuarantined(event, true)` (audit). This is backpressure, not a block — chat stays open.

**Tests**
- Burst beyond `chatBurst` → excess messages `rate_limited` + quarantined; under-limit unaffected.
- Owner messages never rate-limited.
- Distinct senders in one bot chat get independent buckets.

**Acceptance:** a flooding sender is throttled (no reply storm, nothing to memory) while other users in the same bot chat are unaffected.

---

## Task Persona-split (Q2/Q3) — separate pass

**Problem:** owner identity (incl. `Bio`) and owner-voice feedback are sent into **every** bot request and only *demoted* in the prompt ([draft.py:283](../../ai-python/ai_agent/pipelines/draft.py:283), [draft.py:574](../../ai-python/ai_agent/pipelines/draft.py:574)). For a bot serving strangers that is an owner-data leak and noise.

**Definition (mirror draft.py):** `personaSpeaksAsOwner(role *contracts.AIRole) bool { return role == nil || role.SpeakAsOwner }`. When **false**, the chat is a bot persona — strip owner identity.

**Files / anchors** (request build, [service.go:1079](../../backend-go/internal/core/service.go:1079)–1101)
- `aiRole` already resolved at [service.go:1066](../../backend-go/internal/core/service.go:1066).
- UserSettings: at [service.go:1096](../../backend-go/internal/core/service.go:1096), keep `hotDelivery = deliverySettingsFromUser(event, &us)` computed from the **full** `us`. But assign `request.UserSettings` from a reduced copy when `!personaSpeaksAsOwner(aiRole)`: clear `Bio` and `DisplayName` (identity); keep `Timezone` + languages (operational defaults). Fields: `contracts.UserSettings` [types.go:454](../../backend-go/internal/contracts/types.go:454).
- Feedback: at [service.go:1085](../../backend-go/internal/core/service.go:1085), only call `s.feedbackContext(...)` when `personaSpeaksAsOwner(aiRole)`; else leave `FeedbackContext` nil. (Also applies to the redraft path at [service.go:2665](../../backend-go/internal/core/service.go:2665) — apply the same gate there.)

**No contract change** — `UserSettings`/`FeedbackContext` are optional; nil/reduced is valid. No Python change required (the prompt already renders nothing when these are empty); add a Python test only if convenient.

**Tests**
- `role.SpeakAsOwner=false` → `request.UserSettings.Bio==""` and `DisplayName==""`, `FeedbackContext` empty.
- `role==nil` or `SpeakAsOwner=true` → identity + feedback populated (legacy preserved).
- `deliverySettings` still derived correctly from full `us` in both cases.

**Acceptance:** a bot-persona request carries no owner bio/name/voice; personal/business owner-mode is unchanged.

---

## Cross-cutting

- **Migration numbering:** there is a pre-existing collision — two `111_*.sql` files (`111_ai_role_speak_as_owner`, `111_role_model_tier`). Use **112** for the quarantine column. Do **not** fix the 111 collision here (out of scope; add a one-line note to `docs/backlog.md` Tech Debt instead).
- **Config/env:** wire `WithChatRateLimit` from an env knob if the service reads env elsewhere; otherwise defaults are fine. `minEpisodeSpan`/`episodeMessageLimit` stay consts.
- **Observability:** counters/logs for `burst_debounce_skipped` (exists), new `rate_limited`, `episode closed by flood`, and quarantine rate — enough to tune thresholds later.

## Verify

```
make test-go        # all green; add the new tests above
make test           # go + python
make migrate-up     # 112 applies cleanly; make migrate-down reverts
```

These are backend changes — no browser preview. Verification = the functional tests above, run against the in-memory store and (for the migration) a real DB via `make migrate-up`.

## Suggested PR sequence

1. `feat(memory): episode-write flood gate (②)`
2. `feat(memory): message quarantine + exclusion (①)` — includes migration 112
3. `feat(ingest): per chat+sender rate limit (③)` — depends on ①
4. `feat(bot): strip owner identity/feedback from bot-persona requests (persona-split)`
