# System Diagrams

> Сгенерировано из реального кода (аудит 2026-06-26), не из плановых документов. При расхождении с
> кодом — верить коду, эти диаграммы поправить. Источники: `backend-go/internal/*`,
> `ai-python/ai_agent/*`, `infra/migrations/*`, `contracts/openapi.yaml`, `miniapp/*`.

Mermaid рендерится нативно на GitHub/GitLab — открыть файл напрямую в браузере репозитория.

---

## 1. Component diagram

Контейнеры и пакеты/модули внутри них. Стрелки — направление вызова (не направление данных).

```mermaid
graph TB
    subgraph EXT["External"]
        TG["Telegram Bot API"]
        LLMP["LLM Providers<br/>(DeepSeek / Qwen / OpenRouter)"]
        LF["Langfuse<br/>(observability sink)"]
        MCP["MCP servers<br/>(per-role, optional)"]
    end

    subgraph MA["Mini App — miniapp/ (vanilla JS)"]
        SCR["screens/*.js<br/>chats, roles, role-tools, settings, me"]
    end

    subgraph GO["Go Backend — backend-go/internal/"]
        POLL["botservice/poller.go + manager.go<br/>long-polling per user bot"]
        HTTPAPI["httpapi/server.go<br/>REST /api/v1/*, SSE /api/v1/events,<br/>webhook, approval/callback"]
        CORE["core/service.go<br/>Service + DecisionEngine"]
        STORE["store/store.go<br/>Repository"]
        AICLIENT["aiclient/http.go"]
        TGPKG["telegram/<br/>sender, mdrender, webhook parsing"]
    end

    subgraph PY["AI Service — ai-python/ai_agent/"]
        HSRV["http_server.py"]
        RUNTIME["agents/runtime.py<br/>AgentRuntime: orchestrator + writer"]
        CATALOG["agents/tool_catalog.py<br/>TOOL_CATALOG (read/schedule/interaction)"]
        TOOLS["agents/memory_tools.py,<br/>interaction_tools.py, schedule_tools.py"]
        PIPE["pipelines/draft.py, embedding.py,<br/>media.py, risk.py, intent.py"]
    end

    subgraph DATA["Infra"]
        PG[("PostgreSQL<br/>pgvector halfvec-HNSW")]
        LITELLM["LiteLLM Gateway<br/>(optional, LLM_GATEWAY=litellm)"]
    end

    TG <--> POLL
    TG <--> HTTPAPI
    SCR <-->|"REST + SSE"| HTTPAPI
    POLL --> CORE
    HTTPAPI --> CORE
    CORE --> STORE
    CORE --> TGPKG
    CORE --> AICLIENT
    STORE --> PG
    AICLIENT -->|"POST /ai/process-message<br/>/ai/extract-media, /ai/draft-role ..."| HSRV
    HSRV --> RUNTIME
    RUNTIME --> CATALOG
    RUNTIME --> TOOLS
    RUNTIME --> PIPE
    TOOLS --> PG
    RUNTIME -.->|"per-role mcp_servers"| MCP
    RUNTIME -->|"model calls"| LITELLM
    LITELLM --> LLMP
    RUNTIME -.->|"trace + token usage"| LF
```

**Что не показано намеренно:** `internal/config`, `internal/contracts`, `internal/observability`,
`internal/bot` — сквозные/инфраструктурные пакеты без отдельного потока вызовов, see
[`ai_backend_architecture_backlog.md`](../ai_backend_architecture_backlog.md) для деталей.

---

## 2. Sequence diagram — message lifecycle (bimodal orchestrator+writer)

Путь входящего сообщения от Telegram до ответа, включая двумодальное исполнение (commit `7364b14`) —
оркестратор выполняет инструменты и не пишет ответ, писатель пишет финал без инструментов.

```mermaid
sequenceDiagram
    actor Contact as Собеседник
    participant TG as Telegram
    participant Core as Go core/service.go
    participant Store as Go store (Postgres)
    participant Orch as AI orchestrator<br/>(MODEL_ROUTER)
    participant Writer as AI writer<br/>(role model_tier)
    actor Owner as Владелец (Mini App)

    Contact->>TG: сообщение
    TG->>Core: webhook / poll update
    Core->>Core: idempotency (receiveKeys), rate limit (chatLimiters)
    Core->>Store: persist message
    Core->>Store: media enrich (voice/photo/doc → текст), fire-and-forget

    alt роль с включёнными инструментами
        Core->>Orch: POST /ai/process-message
        Orch->>Store: tool calls (search_facts, get_episode_summaries, ...)
        Note over Orch: privacy flags гейтят инструменты<br/>(no_memory/private блокируют scheduler)
        Orch->>Writer: gathered_context + suggested_intent
        Writer->>Writer: пишет финальный текст (без инструментов)
    else инструменты выключены
        Core->>Writer: POST /ai/process-message (single-stage, полный toolset)
    end

    Writer-->>Core: decision, risk_level, text, confidence, scheduled_jobs

    Core->>Core: DecisionEngine.Decide(chatSettings, proposal)

    alt AUTO + низкий риск (или SEMI_AUTO + confidence ≥0.6)
        Core->>TG: send reply
        TG->>Contact: ответ
    else Draft / AskOwner
        Core->>Store: persist draft (status=pending)
        Core->>Owner: SSE draft_created
        Owner->>Core: POST approval/callback (approve/edit/reject/regenerate)
        Core->>Store: idempotency (callbackKeys), update draft
        Core->>TG: send approved text
        TG->>Contact: ответ
    else Critical risk, no safe draft
        Core->>Owner: AskOwner card (без авто-отправки)
    end

    Core->>Store: audit_events, message state transitions
```

---

## 3. Data flow diagram

Хранилища и кто их читает/пишет. `Go` = `backend-go/internal/store`, `Py` = `ai-python` (через прямой
asyncpg pool или Go repository, в зависимости от `AI_MEMORY_SOURCE`).

```mermaid
flowchart LR
    TGMSG(["Telegram message"]) -->|Go write| MESSAGES[("messages<br/>state, text, event_id")]
    OWNER(["Owner action<br/>(Mini App)"]) -->|Go write| CHATSET[("chat_settings<br/>mode, privacy_flags")]
    OWNER -->|Go write| AIROLES[("ai_roles<br/>tool_settings, model_tier")]

    MESSAGES -->|context| AIRT{{"AI Runtime<br/>orchestrator + writer"}}
    CHATSET -->|policy gate| AIRT
    AIROLES -->|tools + model| AIRT

    AIRT -->|search_facts / get_contact_profile<br/>tool read| FACTS[("semantic_facts +<br/>fact_embeddings")]
    AIRT -->|get_episode_summaries<br/>tool read| EPISODES[("episode_summaries")]
    AIRT -->|get_recent_messages<br/>tool read| MESSAGES

    AIRT -->|save_fact<br/>Py write| FACTS
    AIRT -->|episode close<br/>Go write| EPISODES
    AIRT -->|suggest_profile_update<br/>Py write| SUGGEST[("contact_profile_suggestions")]
    AIRT -->|scheduler.create_reminder<br/>Py write| JOBS[("scheduled_jobs<br/>fire_at, payload")]
    AIRT -->|draft proposal| DRAFTS[("drafts<br/>status=pending")]

    OWNER -->|approve/edit/reject| DRAFTS
    SUGGEST -->|accept/reject| PROFILES[("contact_profiles")]
    DRAFTS -->|on send| MESSAGES
    DRAFTS -->|every transition| AUDIT[("audit_events")]
    JOBS -->|fire_at worker| MESSAGES

    PROFILES -->|get_contact_profile<br/>tool read| AIRT
```

**Инварианты на каждой стрелке записи** (см. [`/docs/backlog.md`](../../backlog.md) и `CLAUDE.md`):
privacy-флаги (`no_memory`/`no_training`/`no_summary`/`private`) проверяются перед записью в
`semantic_facts`/`episode_summaries`/`scheduled_jobs`; все записи через `drafts`/`scheduled_jobs`/
`audit_events` идемпотентны (`receiveKeys`/`sendKeys`/`callbackKeys`/`idempotency_key`).

---

_Last verified against code: 2026-06-26. Regenerate by re-reading the files listed in the header, not by editing this file from memory._
