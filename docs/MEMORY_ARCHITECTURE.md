# DoubleU — Memory Architecture & Data Design

**Status:** Draft v3
**Audience:** Engineering, Data, Product
**Scope:** Core memory subsystem — schema, lifecycle, retrieval, privacy, runtime state.
**Out of scope:** Telegram Business webhook handling, model selection, billing, UI, AI-роли (отдельный документ при появлении).

**Changelog:**
- v3: добавлен runtime-state слой (`active_episodes`); concurrency protection в dedup (advisory lock + normalized_fact_hash UNIQUE); composite FK `source_message_id`; денормализация `tenant_id` в `fact_occurrences`; явный `CREATE EXTENSION vector`; mandatory confirmation flow для global health-фактов; 4-step backfill для `risk_level`.
- v2: добавлен `scope_kind` (chat / owner_global); правило назначения scope и override; удалён глобальный стиль владельца.
- v1: первая редакция.

---

## 0. Why this document exists

Текущая реализация памяти **не работает как память**. Эмпирически на синтетическом датасете (1400 записей `memory_facts`):

- 62% записей — дубликаты (524 уникальных из 1400).
- 194 записи имеют рекурсивный префикс `[FACT] extracted: [FACT] extracted: ...` глубиной до 14 уровней — extractor читает свой output как input.
- `fact_type='extracted'` фактически хранит сырые сообщения, а не извлечённые факты.
- `owner_style` содержит сам system-prompt extractor'а — промпт утёк в данные.
- `summaries` пуста — слой ежедневных сводок физически не реализован.
- `memory_embeddings` использует `hash-placeholder-v1` с 16 dimensions — это не эмбеддинг.
- `contact_profiles` заполнена для 1 чата из 6.

Это не баги, а **отсутствие архитектурных контрактов**. Документ фиксирует контракты.

---

## 1. Принципы

1. **Память делится по роли в промпте, не по времени.** "Долгосрочная / краткосрочная" — продуктовый язык. Слои: User → Per-Chat Identity → Working → Runtime State → Episodic → Semantic.
2. **Сырые сообщения — не память, а аудит.** Они в `messages`, не копируются в memory-таблицы как "факты".
3. **Стиль общения — свойство пары (владелец, контакт).** Нет глобального baseline стиля. Каждый чат имеет свой стиль в `contact_profiles`.
4. **Факты о владельце — кросс-чатные, факты о других — per-chat.** Единственный класс памяти, пересекающий границу чата. Фиксируется через `scope_kind`.
5. **Runtime state ≠ persistent state.** Открытая тема, незавершённый вопрос — это runtime, живёт в `active_episodes`, удаляется при закрытии эпизода. Производные состояния (была ли пауза, здоровались ли) — это **запросы к messages**, не отдельные хранимые поля. Денормализация запрещена.
6. **Provenance везде.** Каждая запись имеет источник (source_message_id, timestamp, кем подтверждена).
7. **Dedup на write, защищённый от race.** Перед каждым insert в semantic-слой — semantic search + advisory lock + normalized_hash UNIQUE constraint как catch-net.
8. **Каждый факт смертен.** `expires_at` или `decay_rate` или `permanent` через `fact_type`.
9. **Owner-confirmed > LLM-extracted.** При merge / конфликте.
10. **RLS-ready, RLS-off.** На MVP — `tenant_id` в WHERE. Включение RLS — миграция, не рефакторинг.
11. **Schema-first.** Constraints в БД. Бизнес-правило, выразимое как CHECK / FK / UNIQUE, живёт в БД.

---

## 2. Слои памяти

```
┌─────────────────────────────────────────────────────────────────┐
│  USER LAYER                                                     │
│  user_settings (1 на user) — identity + behavior настройки      │
└─────────────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────────────┐
│  PER-CHAT IDENTITY                                              │
│  contact_profiles (1 на chat) — Собеседник + Я с ним + Чат      │
│  chat_settings    (1 на chat) — mode, privacy_flags, confidence │
└─────────────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────────────┐
│  WORKING — всегда в промпте, без поиска                         │
│  messages (последние N по chat_id)                              │
└─────────────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────────────┐
│  RUNTIME STATE — текущий незакрытый эпизод                      │
│  active_episodes (1 на chat, существует если эпизод открыт)     │
└─────────────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────────────┐
│  EPISODIC — сжатые сводки закрытых эпизодов, semantic search    │
│  episode_summaries                                              │
└─────────────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────────────┐
│  SEMANTIC — атомарные факты, двухуровневый scope                │
│  semantic_facts (scope=chat | owner_global)                     │
│  fact_embeddings + fact_occurrences                              │
└─────────────────────────────────────────────────────────────────┘
```

**Мэппинг "продуктовый язык → архитектурный":**

| Продуктовое название         | Архитектурный слой            | Таблица                       |
|------------------------------|-------------------------------|-------------------------------|
| Краткосрочная память (день)  | Working + Runtime             | `messages` + `active_episodes`|
| Долгосрочная (сводки)        | Episodic                      | `episode_summaries`           |
| Факты о контакте             | Semantic (scope=chat)         | `semantic_facts`              |
| Факты о владельце вообще     | Semantic (scope=owner_global) | `semantic_facts`              |
| Карточка контакта            | Per-chat identity             | `contact_profiles`            |
| Настройки владельца          | User layer                    | `user_settings`               |

**Чего НЕТ намеренно:**
- Глобального профиля стиля владельца (§16 — почему отклонено повторно).
- Ролей / персонажей (§13).
- `dialog_state` с булевыми флагами типа `greeted_today` (§6.2 — почему отклонено).

---

## 3. User Settings

Одна таблица на одного пользователя. Identity-поля владельца + поведенческие настройки приложения.

**Identity** (через UI владельца):
- `display_name`, `bio`, `timezone`, `primary_language`, `secondary_languages[]`.

**Behavior** (через UI владельца):
- `default_mode_for_new_chats` (дефолт `manual`).
- `learning_enabled`.
- `language_for_drafts` (`auto` / `primary`).

**Что НЕ входит:**
- ❌ Стиль владельца. Нет глобального стиля ни в каком виде (§16).
- ❌ AI-suggestions для автозаполнения. Identity-поля владелец заполняет руками.

---

## 4. Contact Profile (per-chat identity)

### 4.1. "Собеседник" — объективные факты о контакте
- `display_names[]`, `preferred_address`, `gender`, `role`.
- `relationship_type` — enum (family / romantic / friendship / professional / acquaintance / unknown). **Используется для cold-start defaults стиля** (§4.6).
- `language`, `contact_timezone`.

### 4.2. "Я с ним" — единственный источник стиля для этого чата
- `tone`, `emoji_usage`, `address_form` — NULL означает "не задано, использовать defaults по relationship_type".
- `style_notes` — free-text дополнение.

NULL ≠ fallback на глобальный профиль. Глобального профиля нет. NULL → defaults из §4.6.

### 4.3. "Чат" — о чём этот чат
- `topics[]`, `chat_context`, `do_not_discuss[]`.

### 4.4. Заполнение профиля
1. Владельцем через UI.
2. AI-предложениями через `contact_profile_suggestions` (§4.5).
3. Defaults при создании нового чата (§4.6).

### 4.5. Contact Profile Suggestions
AI имеет право **предлагать** обновления, но не имеет права писать напрямую. Все предложения → `contact_profile_suggestions` со статусом `pending`. Владелец делает accept/edit/reject. Только accepted применяется.

Защищает от: self-reinforcing loop, галлюцинированных свойств, дрейфа профиля.

### 4.6. Cold-start defaults по relationship_type

Применяются при retrieval, если соответствующее поле `contact_profiles` = NULL. Не записываются в БД.

| relationship_type | tone     | emoji_usage | address_form |
|-------------------|----------|-------------|--------------|
| family            | casual   | rare        | ty           |
| romantic          | casual   | frequent    | ty           |
| friendship        | casual   | frequent    | ty           |
| professional      | formal   | none        | vy           |
| acquaintance      | neutral  | rare        | vy           |
| unknown           | neutral  | rare        | vy           |

Это **структурное решение проблемы cold-start**, заменяющее глобальный стиль владельца. Учитывает категорию контакта, не усредняет по всем.

---

## 5. Semantic Facts: атомарные знания

### 5.1. Что считается фактом
Атомарное утверждение, релевантное для будущих ответов, не выводимое из identity-полей, имеющее смысл вне контекста.

**Факты**: "У контакта аллергия на морепродукты", "Контакт работает удалённо из Тбилиси", "Контакт обещал прислать договор до пятницы".

**Не-факты**: "Контакт написал 'привет'" (сообщение), "Контакт сегодня грустит" (эпизод), "Контакт — мой брат" (identity), "[FACT] extracted: ..." (лог).

### 5.2. Типизация

```
fact_type ENUM:
  preference     -- "не любит голосовые"
  biographical   -- "живёт в Тбилиси"
  commitment     -- "обещал прислать договор до пятницы"
  event          -- "сменил работу 1 марта"
  relationship   -- "познакомились через Y"
  health         -- "аллергия на X" (sensitive)
  taboo          -- "не обсуждать тему X"
```

### 5.3. Scope: chat vs owner_global

**`scope_kind = 'chat'`** — факт привязан к chat_id, виден только в этом чате.

**`scope_kind = 'owner_global'`** — факт о самом владельце, виден во всех чатах. Только `subject = 'owner'` может быть global.

### 5.4. Правило назначения scope (детерминированное)

```
IF subject = 'owner' AND fact_type IN ('health', 'biographical'):
    scope_kind = 'owner_global', chat_id = NULL
    IF fact_type = 'health': требуется confirmation flow (§7.1)
ELSE:
    scope_kind = 'chat', chat_id = $current_chat_id
```

Обоснование выбора `health` и `biographical`:
- **health** — критичная информация. Цена пропуска выше цены ложного срабатывания.
- **biographical** — устойчивые факты о владельце, релевантные в большинстве чатов.

Остальные типы — контекстные, авто-глобализация даст шум.

### 5.5. Override владельца: ручное изменение scope

**Promote `chat → owner_global`** (кнопка "Сделать глобальным"):
- Только для `subject = 'owner'`.
- `metadata.override_reason = 'manual_promote'`, `metadata.original_chat_id`.
- audit_events: `fact_scope_changed`.

**Demote `owner_global → chat`** (кнопка "Только для этого чата"):
- `metadata.override_reason = 'manual_demote'`, `metadata.original_scope = 'owner_global'`.
- audit_events: `fact_scope_changed`.

Обе операции обратимы.

### 5.6. Конфликты chat-факта и global-факта

Конфликт = два факта одного `fact_type`, одного `subject`, cosine similarity > 0.85.

**Правило: chat побеждает global.** Global исключается из промпта. Лог в metadata промпта (для отладки): `[chat fact overrides global fact #12345]`.

### 5.7. Поля semantic_facts

Полный DDL — в §9. Ключевые инварианты (через CHECK):
- `scope_kind='owner_global'` ⇒ `subject='owner' AND chat_id IS NULL`.
- `scope_kind='chat'` ⇒ `chat_id IS NOT NULL`.
- `fact_text` без префиксов `[FACT]`, `[EXTRACTED]`, `[OWNER STYLE]`.
- `char_length(fact_text) BETWEEN 5 AND 200`.
- `normalized_fact_hash` — детерминированный hash (lower + trim + collapse whitespace) для UNIQUE constraint.

### 5.8. Dedup-on-write контракт

Защищён двумя независимыми механизмами:

**Механизм 1: Advisory lock + semantic search в stored proc.**
```
1. pg_advisory_xact_lock(hashtext(tenant_id || ':' || chat_id || ':' || fact_type || ':' || subject))
   — сериализует параллельные dedup в пределах одного (tenant, chat, type, subject).
2. Сгенерировать embedding кандидата.
3. SELECT top-1 по cosine WHERE scope_matches AND fact_type AND subject.
4. similarity ≥ 0.97 → identical → fact_occurrences++ (idempotent).
   0.85 ≤ similarity < 0.97 → similar → supersede + insert новый.
   similarity < 0.85 → новый факт.
```

**Механизм 2: UNIQUE partial index по normalized_fact_hash как catch-net.**
Даже если advisory lock пропустил (split brain, разные транзакции на разных нодах PG в будущем) — точный дубликат не пройдёт уникальный индекс. Stored proc обрабатывает unique_violation как "occurrence_added".

Это **defense in depth**: lock — оптимистично для семантических дубликатов, UNIQUE — гарантия для точных. Прямые INSERT в `semantic_facts` запрещены через GRANT.

---

## 6. Working Memory + Runtime State

### 6.1. messages
Минимальные изменения к существующей таблице:
- `risk_level` → NOT NULL (после 4-step backfill, см. §12).

`message_states` — оставить как есть.

**Что НЕ добавляем:**
- ❌ `is_ai_assisted` — нет глобального learning стиля, нечего фильтровать.

### 6.2. active_episodes — runtime state

**Проблема, которую решает:** есть вопросы про текущий диалог, которые нельзя ответить ни через `messages`, ни через `episode_summaries`. Это **открытая тема прямо сейчас** — что обсуждаем, есть ли незакрытый вопрос, есть ли ожидание действия от владельца.

**Что НЕ хранить в этой таблице** (важно):
- ❌ `greeted_today BOOLEAN` — это запрос к messages: `EXISTS (SELECT 1 FROM messages WHERE chat_id=$X AND sender_id=$owner AND received_at >= today())`.
- ❌ `pause_6h_passed BOOLEAN` — это запрос: `(now() - MAX(received_at)) > interval '6 hours'`.
- ❌ `has_pending_commitment BOOLEAN` — это запрос к semantic_facts: `EXISTS (... fact_type='commitment' AND expires_at > now())`.

Любой денормализованный булевый флаг рассинхронизируется с источником. Удалили сообщение → "greeted_today=true" остался, приветствия нет. Это не state — это derived view. Делаем через SQL функции, не через хранимые поля.

**Что хранить:**
- `current_topic` — текущая тема разговора (обновляется topic-detector'ом по последним N сообщениям, может быть NULL если тема не определена).
- `open_question_message_id` — id сообщения, содержащего незакрытый вопрос (либо от контакта владельцу, либо наоборот). Закрывается при ответе.
- `pending_owner_action` — текстовое описание ожидаемого действия владельца ("ответить про встречу в четверг"), если есть. Заполняется extractor'ом.
- `started_at`, `last_message_at`, `message_count` — операционные метрики эпизода для триггеров закрытия.

**Жизненный цикл:**
1. Новое сообщение в чате, где нет записи в `active_episodes` → INSERT.
2. Каждое следующее сообщение → UPDATE last_message_at, message_count++, опционально update topic / open_question.
3. Триггер закрытия эпизода (silence_gap > 6h, message_count > 40, topic_shift) → materialize в `episode_summaries`, **DELETE из active_episodes**.
4. Следующее сообщение → INSERT новой записи.

Запись существует только пока эпизод открыт. PK `(tenant_id, chat_id)` гарантирует один активный эпизод на чат.

**Derived views для производных состояний** (создать в §9):
- `view_chat_paused_hours(tenant_id, chat_id)` — сколько часов прошло с последнего сообщения.
- `view_chat_greeted_today(tenant_id, chat_id)` — было ли приветствие сегодня.
- `view_open_commitments(tenant_id, chat_id)` — список незакрытых commitments.

Это позволяет промпт-сборщику задавать вопросы вида `SELECT view_chat_greeted_today($tenant, $chat)` без денормализации.

---

## 7. Lifecycle: как факт рождается, живёт и умирает

### 7.1. Рождение (write path)

```
[Incoming message] 
  → policy_check (заполняет risk_level)
  → INSERT messages 
  → UPSERT active_episodes (создать или обновить запись)
  → IF risk OK AND chat NOT in no_memory:
      → extraction_job (async, batched 5-10 сообщений)
          → LLM extract candidates (typed JSON output)
          → validate (длина 5-200, type из enum, subject не пуст)
          → assign scope (§5.4):
              IF subject=owner AND fact_type='health':
                  → INSERT в pending_global_health_facts (НЕ в semantic_facts!)
                  → notify owner: "AI хочет добавить медицинский факт"
                  → await accept/reject
                  → IF accepted: упсёрт в semantic_facts через stored proc
              ELIF subject=owner AND fact_type='biographical':
                  scope_kind='owner_global', chat_id=NULL
                  → upsert_semantic_fact(...)
              ELSE:
                  scope_kind='chat', chat_id=current
                  → upsert_semantic_fact(...)
          → IF target = contact_profile_field:
              INSERT contact_profile_suggestions (NOT direct write)
      → log в extraction_log
```

**Health-фактам нужно особое обращение:** global health-факт может повлиять на ответы в любом чате владельца, включая чувствительные контексты. Ложно-положительная экстракция ("AI решил, что у владельца диабет, потому что в чате обсуждалась диета") неприемлема. Поэтому:

- `pending_global_health_facts` — отдельная таблица (не `semantic_facts` со статусом!).
- Владельцу показывается UI-карточка с фактом, source-сообщением и явным выбором.
- Только accepted попадает в `semantic_facts`.
- Rejected попадает в `extraction_log` со статусом `rejected_by_owner` — для обучения extractor'а в будущем.

Chat-scoped health-факты (про контакта) идут через обычный extraction без confirmation, иначе UX невыносим.

**Запрещено** (физически — CHECK / GRANT, не "договорились"):
- Передавать в extractor предыдущий output extractor'а.
- Записывать в `semantic_facts` строки с префиксами `[FACT]`, `[EXTRACTED]`, `[OWNER STYLE]`.
- Тексты длиной > 200 символов.
- Прямой INSERT в `semantic_facts` минуя stored proc.
- Прямой UPDATE `contact_profiles` от extractor'а.
- INSERT global health-факта минуя `pending_global_health_facts`.

### 7.2. Жизнь (decay по типам)

| fact_type      | Политика                                                          |
|----------------|-------------------------------------------------------------------|
| `biographical` | Permanent, удаляется только при противоречии.                     |
| `relationship` | Permanent.                                                       |
| `preference`   | Permanent, retrieval-вес ↓ если 90+ дней без подтверждений.       |
| `commitment`   | `expires_at` = deadline + 7 дней. После — auto-archive.           |
| `event`        | Permanent (история), retrieval-вес ↓ с возрастом.                 |
| `health`       | Permanent. Sensitive — отдельные правила.                         |
| `taboo`        | Permanent до явной отмены владельцем.                            |

### 7.3. Обновление и противоречия

Факты не редактируются in-place. При противоречии: старый получает `superseded_by_id = new.id` + `deleted_at = now()`, новый — INSERT. Retrieval выдаёт только не-superseded и не-deleted.

При scope override (§5.5) `superseded_by_id` не используется — это перемещение, не противоречие.

### 7.4. Смерть

1. **Soft delete по expires_at** — фоновая job, retrieval исключает, 30 дней до hard delete.
2. **Soft delete cascade** — если удалён `source_message_id` (ON DELETE SET NULL на FK) и нет других occurrences → soft delete факта.
3. **Hard delete** — по запросу владельца / GDPR. Каскадит на `fact_embeddings`, `fact_occurrences`, `extraction_log`, `episode_summaries`, `active_episodes`.

### 7.5. Episodic summaries: границы эпизодов

Эпизод закрывается при:
- молчании > 6 часов,
- 40 сообщений с момента последней summary,
- topic_shift (опционально на v2).

При закрытии: INSERT в `episode_summaries`, DELETE из `active_episodes`.

Содержание summary: 3-6 предложений (тема, решения, commitments, тон), `period_start/end`, `source_message_ids[]`, `closure_reason`, `embedding`.

---

## 8. Read path: как собирается контекст под промпт

```
1. User block (без поиска):
   - SELECT * FROM user_settings WHERE user_id = $owner
   
2. Per-chat identity block (без поиска):
   - SELECT * FROM contact_profiles WHERE tenant_id=$T AND chat_id=$X
   - SELECT * FROM chat_settings   WHERE tenant_id=$T AND chat_id=$X
   - Применить cold-start defaults (§4.6) для NULL полей tone/emoji/address

3. Runtime state (без поиска):
   - SELECT * FROM active_episodes WHERE tenant_id=$T AND chat_id=$X
   - SELECT view_chat_paused_hours($T, $X)
   - SELECT view_chat_greeted_today($T, $X)
   - SELECT view_open_commitments($T, $X)

4. Working block (без поиска):
   - SELECT * FROM messages WHERE chat_id=$X 
       ORDER BY received_at DESC LIMIT 20

5. Episodic block (semantic search):
   - embed(incoming_message)
   - SELECT top-3 episode_summaries WHERE chat_id=$X 
       AND period_end > now() - 30 days AND deleted_at IS NULL

6. Semantic block (scope-aware):
   - 6a: chat-факты — SELECT top-K WHERE scope_kind='chat' AND chat_id=$X
   - 6b: global-факты — SELECT top-M WHERE scope_kind='owner_global'
   - 6c: разрешение конфликтов (§5.6) — chat побеждает global
   - 6d: hard-include всех taboo + health (с тем же conflict resolution)

7. Compose prompt:
   [System: персона, режим, дата]
   [User: display_name, timezone, language]
   [Contact: имена, отношения, как обращаться, стиль для этого чата]
   [Chat context: о чём чат, что не обсуждать]
   [Runtime: open_question, current_topic, pending_owner_action, paused_hours]
   [Owner global facts: ... (после фильтрации §5.6)]
   [Chat facts: ...]
   [Recent episodes: 1-3 summaries]
   [Conversation: последние 20 сообщений]
   [Incoming: текущее сообщение]
   [Instruction]
```

### 8.1. Token budget (8k context):
- User + identity + chat settings + runtime: ~500
- Chat-facts (5-8 × 30): ~200
- Global-facts (3-5 × 30): ~150
- Episodic (3 × 100): ~300
- Working (20 × 30): ~600
- Overhead: ~500

Итого ~2.3k. При урезании: working (20→10) → episodic (3→1) → chat-facts (8→5) → global-facts (5→3). Identity, runtime, hard-include — не режутся.

### 8.2. Retrieval-веса
- `taboo`, `health`: hard-include.
- `commitment` (активный): × 1.5
- `preference`: × 1.2
- `biographical`: × 1.0
- `event` (>30 дней): × 0.7
- `recency_weight` для preferences: `exp(-age_days / 180)`.

---

## 9. Schema (DDL)

PostgreSQL 15+ с pgvector. RLS-ready, RLS-off.

```sql
-- ============================================================
-- PREREQUISITE
-- ============================================================
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- для digest() в normalized_fact_hash

-- ============================================================
-- USER LAYER
-- ============================================================
CREATE TABLE user_settings (
  user_id              TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  tenant_id            TEXT NOT NULL,
  display_name         TEXT,
  bio                  TEXT,
  timezone             TEXT,
  primary_language     TEXT NOT NULL DEFAULT 'ru',
  secondary_languages  TEXT[] NOT NULL DEFAULT '{}',
  default_mode_for_new_chats TEXT NOT NULL DEFAULT 'manual'
    CHECK (default_mode_for_new_chats IN ('off','observe','manual','semi_auto','auto')),
  learning_enabled     BOOLEAN NOT NULL DEFAULT true,
  language_for_drafts  TEXT NOT NULL DEFAULT 'auto'
    CHECK (language_for_drafts IN ('auto','primary')),
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_user_settings_tenant ON user_settings(tenant_id);

-- ============================================================
-- PER-CHAT IDENTITY
-- ============================================================
CREATE TABLE contact_profiles (
  tenant_id           TEXT NOT NULL,
  chat_id             TEXT NOT NULL,
  display_names       TEXT[] NOT NULL DEFAULT '{}',
  preferred_address   TEXT,
  gender              TEXT CHECK (gender IN ('male','female','non_binary','unknown')),
  role                TEXT,
  relationship_type   TEXT CHECK (relationship_type IN
                      ('family','romantic','friendship','professional','acquaintance','unknown')),
  language            TEXT,
  contact_timezone    TEXT,
  tone                TEXT CHECK (tone IN ('formal','neutral','casual')),
  emoji_usage         TEXT CHECK (emoji_usage IN ('none','rare','frequent')),
  address_form        TEXT CHECK (address_form IN ('ty','vy')),
  style_notes         TEXT,
  topics              TEXT[] NOT NULL DEFAULT '{}',
  chat_context        TEXT,
  do_not_discuss      TEXT[] NOT NULL DEFAULT '{}',
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id)
);

CREATE TABLE contact_profile_suggestions (
  id              BIGSERIAL PRIMARY KEY,
  tenant_id       TEXT NOT NULL,
  chat_id         TEXT NOT NULL,
  field_name      TEXT NOT NULL,
  suggested_value JSONB NOT NULL,
  rationale       TEXT,
  source_window   JSONB,
  status          TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','accepted','rejected','expired')),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  reviewed_at     TIMESTAMPTZ
);
CREATE INDEX ix_cps_pending ON contact_profile_suggestions(tenant_id, chat_id) 
  WHERE status='pending';

-- ============================================================
-- WORKING LAYER (changes to existing messages)
-- ============================================================
-- Существующий PK: (tenant_id, chat_id, source_message_id) — оставляем.
-- После 4-step backfill (см. §12):
-- ALTER TABLE messages ALTER COLUMN risk_level SET NOT NULL;

-- ============================================================
-- RUNTIME STATE
-- ============================================================
CREATE TABLE active_episodes (
  tenant_id               TEXT NOT NULL,
  chat_id                 TEXT NOT NULL,
  started_at              TIMESTAMPTZ NOT NULL,
  last_message_at         TIMESTAMPTZ NOT NULL,
  message_count           INT NOT NULL DEFAULT 0,
  current_topic           TEXT,
  open_question_message_id TEXT,
  pending_owner_action    TEXT,
  metadata                JSONB NOT NULL DEFAULT '{}'::jsonb,
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, chat_id),
  FOREIGN KEY (tenant_id, chat_id, open_question_message_id)
    REFERENCES messages(tenant_id, chat_id, source_message_id) ON DELETE SET NULL
);
CREATE INDEX ix_ae_stale ON active_episodes(last_message_at) 
  WHERE last_message_at < now() - interval '6 hours';
-- Индекс под фоновую job, которая закрывает залежавшиеся эпизоды.

-- Derived views (производные состояния, без денормализации):
CREATE OR REPLACE FUNCTION view_chat_paused_hours(p_tenant TEXT, p_chat TEXT)
RETURNS NUMERIC AS $$
  SELECT EXTRACT(EPOCH FROM (now() - MAX(received_at))) / 3600
    FROM messages WHERE tenant_id=p_tenant AND chat_id=p_chat;
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION view_chat_greeted_today(p_tenant TEXT, p_chat TEXT, p_owner_id TEXT)
RETURNS BOOLEAN AS $$
  SELECT EXISTS (
    SELECT 1 FROM messages 
    WHERE tenant_id=p_tenant AND chat_id=p_chat 
      AND sender_id=p_owner_id 
      AND received_at >= date_trunc('day', now())
  );
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION view_open_commitments(p_tenant TEXT, p_chat TEXT)
RETURNS TABLE(fact_id BIGINT, fact_text TEXT, expires_at TIMESTAMPTZ) AS $$
  SELECT id, fact_text, expires_at FROM semantic_facts
   WHERE tenant_id=p_tenant AND chat_id=p_chat
     AND fact_type='commitment' AND deleted_at IS NULL 
     AND superseded_by_id IS NULL
     AND (expires_at IS NULL OR expires_at > now());
$$ LANGUAGE sql STABLE;

-- ============================================================
-- EPISODIC LAYER
-- ============================================================
CREATE TABLE episode_summaries (
  id                  BIGSERIAL PRIMARY KEY,
  tenant_id           TEXT NOT NULL,
  chat_id             TEXT NOT NULL,
  period_start        TIMESTAMPTZ NOT NULL,
  period_end          TIMESTAMPTZ NOT NULL,
  content             TEXT NOT NULL CHECK (char_length(content) BETWEEN 50 AND 1500),
  source_message_ids  TEXT[] NOT NULL,
  message_count       INT NOT NULL,
  closure_reason      TEXT NOT NULL CHECK (closure_reason IN
                      ('silence_gap','message_count','topic_shift','manual')),
  embedding           vector(1536),
  privacy_flags       JSONB NOT NULL DEFAULT '{}'::jsonb,
  deleted_at          TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_es_chat ON episode_summaries(tenant_id, chat_id, period_end DESC)
  WHERE deleted_at IS NULL;
CREATE INDEX ix_es_embedding ON episode_summaries
  USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);

-- ============================================================
-- SEMANTIC LAYER
-- ============================================================
CREATE TABLE semantic_facts (
  id                BIGSERIAL PRIMARY KEY,
  tenant_id         TEXT NOT NULL,
  
  scope_kind        TEXT NOT NULL CHECK (scope_kind IN ('chat','owner_global')),
  chat_id           TEXT,
  
  fact_text         TEXT NOT NULL
                    CHECK (char_length(fact_text) BETWEEN 5 AND 200)
                    CHECK (fact_text NOT LIKE '[FACT]%')
                    CHECK (fact_text NOT LIKE '[EXTRACTED]%')
                    CHECK (fact_text NOT LIKE '[OWNER STYLE%'),
  normalized_fact_hash TEXT NOT NULL,  -- catch-net для exact dedup
  
  fact_type         TEXT NOT NULL CHECK (fact_type IN
                    ('preference','biographical','commitment','event',
                     'relationship','health','taboo')),
  subject           TEXT NOT NULL CHECK (subject IN
                    ('contact','owner','relationship','third_party')),
  confidence        REAL NOT NULL CHECK (confidence BETWEEN 0 AND 1),
  source            TEXT NOT NULL CHECK (source IN
                    ('llm_extracted','owner_confirmed','owner_authored')),
  
  source_message_id TEXT,
  
  metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
  expires_at        TIMESTAMPTZ,
  superseded_by_id  BIGINT REFERENCES semantic_facts(id),
  deleted_at        TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  
  CONSTRAINT scope_chat_requires_chat_id 
    CHECK (scope_kind != 'chat' OR chat_id IS NOT NULL),
  CONSTRAINT scope_global_no_chat_id 
    CHECK (scope_kind != 'owner_global' OR chat_id IS NULL),
  CONSTRAINT scope_global_only_owner 
    CHECK (scope_kind != 'owner_global' OR subject = 'owner'),
  
  -- Composite FK для chat-scoped фактов (NULL chat_id для global, FK не применяется)
  FOREIGN KEY (tenant_id, chat_id, source_message_id)
    REFERENCES messages(tenant_id, chat_id, source_message_id) ON DELETE SET NULL
);

-- UNIQUE catch-net для точных дубликатов (даже при race)
CREATE UNIQUE INDEX ux_sf_normalized_active 
  ON semantic_facts(tenant_id, scope_kind, COALESCE(chat_id, ''), fact_type, subject, normalized_fact_hash)
  WHERE deleted_at IS NULL AND superseded_by_id IS NULL;

CREATE INDEX ix_sf_chat_active ON semantic_facts(tenant_id, chat_id, fact_type)
  WHERE scope_kind='chat' AND deleted_at IS NULL AND superseded_by_id IS NULL;
CREATE INDEX ix_sf_global_active ON semantic_facts(tenant_id, fact_type)
  WHERE scope_kind='owner_global' AND deleted_at IS NULL AND superseded_by_id IS NULL;
CREATE INDEX ix_sf_taboo_health_chat ON semantic_facts(tenant_id, chat_id)
  WHERE fact_type IN ('taboo','health') AND scope_kind='chat' 
    AND deleted_at IS NULL AND superseded_by_id IS NULL;
CREATE INDEX ix_sf_taboo_health_global ON semantic_facts(tenant_id)
  WHERE fact_type IN ('taboo','health') AND scope_kind='owner_global' 
    AND deleted_at IS NULL AND superseded_by_id IS NULL;
CREATE INDEX ix_sf_commitments ON semantic_facts(tenant_id, chat_id, expires_at)
  WHERE fact_type = 'commitment' AND deleted_at IS NULL;

CREATE TABLE fact_embeddings (
  fact_id           BIGINT PRIMARY KEY REFERENCES semantic_facts(id) ON DELETE CASCADE,
  tenant_id         TEXT NOT NULL,
  scope_kind        TEXT NOT NULL CHECK (scope_kind IN ('chat','owner_global')),
  chat_id           TEXT,
  embedding_model   TEXT NOT NULL,
  embedding         vector(1536) NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_fe_vector ON fact_embeddings
  USING ivfflat (embedding vector_cosine_ops) WITH (lists = 200);
CREATE INDEX ix_fe_chat_scope ON fact_embeddings(tenant_id, chat_id) 
  WHERE scope_kind='chat';
CREATE INDEX ix_fe_global_scope ON fact_embeddings(tenant_id) 
  WHERE scope_kind='owner_global';

-- fact_occurrences с tenant_id (RLS-ready)
CREATE TABLE fact_occurrences (
  tenant_id         TEXT NOT NULL,
  fact_id           BIGINT NOT NULL REFERENCES semantic_facts(id) ON DELETE CASCADE,
  source_message_id TEXT NOT NULL,
  chat_id           TEXT NOT NULL,
  observed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (fact_id, source_message_id),
  FOREIGN KEY (tenant_id, chat_id, source_message_id)
    REFERENCES messages(tenant_id, chat_id, source_message_id) ON DELETE CASCADE
);
CREATE INDEX ix_fo_source ON fact_occurrences(tenant_id, source_message_id);
CREATE INDEX ix_fo_fact ON fact_occurrences(fact_id);

-- ============================================================
-- PENDING GLOBAL HEALTH (confirmation queue)
-- ============================================================
CREATE TABLE pending_global_health_facts (
  id                BIGSERIAL PRIMARY KEY,
  tenant_id         TEXT NOT NULL,
  chat_id           TEXT NOT NULL,         -- из какого чата извлечён
  source_message_id TEXT NOT NULL,
  fact_text         TEXT NOT NULL CHECK (char_length(fact_text) BETWEEN 5 AND 200),
  confidence        REAL NOT NULL,
  status            TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','accepted','rejected','expired')),
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  reviewed_at       TIMESTAMPTZ,
  resulting_fact_id BIGINT REFERENCES semantic_facts(id),  -- если accepted
  FOREIGN KEY (tenant_id, chat_id, source_message_id)
    REFERENCES messages(tenant_id, chat_id, source_message_id) ON DELETE CASCADE
);
CREATE INDEX ix_pghf_pending ON pending_global_health_facts(tenant_id) WHERE status='pending';

-- ============================================================
-- EXTRACTION LOG
-- ============================================================
CREATE TABLE extraction_log (
  id                BIGSERIAL PRIMARY KEY,
  tenant_id         TEXT NOT NULL,
  chat_id           TEXT NOT NULL,
  input_message_ids TEXT[] NOT NULL,
  model_used        TEXT NOT NULL,
  raw_output        JSONB NOT NULL,
  facts_created     BIGINT[],
  facts_skipped     INT NOT NULL DEFAULT 0,
  facts_merged      INT NOT NULL DEFAULT 0,
  error             TEXT,
  duration_ms       INT,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 9.1. Stored procedure для записи фактов (с защитой от race)

```sql
CREATE OR REPLACE FUNCTION normalize_fact_text(p_text TEXT) RETURNS TEXT AS $$
  SELECT encode(digest(
    regexp_replace(lower(trim(p_text)), '\s+', ' ', 'g'), 
    'sha256'), 'hex');
$$ LANGUAGE sql IMMUTABLE;

CREATE OR REPLACE FUNCTION upsert_semantic_fact(
  p_tenant_id            TEXT,
  p_scope_kind           TEXT,
  p_chat_id              TEXT,
  p_fact_text            TEXT,
  p_fact_type            TEXT,
  p_subject              TEXT,
  p_confidence           REAL,
  p_source               TEXT,
  p_source_message_id    TEXT,
  p_embedding            vector(1536),
  p_similarity_threshold REAL DEFAULT 0.85
) RETURNS TABLE (action TEXT, fact_id BIGINT) AS $$
DECLARE
  v_lock_key BIGINT;
  v_existing_id BIGINT;
  v_existing_similarity REAL;
  v_new_id BIGINT;
  v_hash TEXT;
BEGIN
  -- Защита от health-фактов мимо confirmation flow
  IF p_scope_kind = 'owner_global' AND p_fact_type = 'health' THEN
    RAISE EXCEPTION 'Global health facts must go through pending_global_health_facts';
  END IF;

  v_hash := normalize_fact_text(p_fact_text);
  
  -- Advisory lock на scope, чтобы сериализовать параллельный dedup
  v_lock_key := hashtextextended(
    p_tenant_id || ':' || p_scope_kind || ':' || COALESCE(p_chat_id, '') 
    || ':' || p_fact_type || ':' || p_subject, 0);
  PERFORM pg_advisory_xact_lock(v_lock_key);

  -- Semantic search в пределах scope
  IF p_scope_kind = 'owner_global' THEN
    SELECT sf.id, 1 - (fe.embedding <=> p_embedding)
      INTO v_existing_id, v_existing_similarity
      FROM semantic_facts sf
      JOIN fact_embeddings fe ON fe.fact_id = sf.id
     WHERE sf.tenant_id = p_tenant_id
       AND sf.scope_kind = 'owner_global'
       AND sf.fact_type = p_fact_type AND sf.subject = p_subject
       AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
     ORDER BY fe.embedding <=> p_embedding LIMIT 1;
  ELSE
    SELECT sf.id, 1 - (fe.embedding <=> p_embedding)
      INTO v_existing_id, v_existing_similarity
      FROM semantic_facts sf
      JOIN fact_embeddings fe ON fe.fact_id = sf.id
     WHERE sf.tenant_id = p_tenant_id
       AND sf.scope_kind = 'chat' AND sf.chat_id = p_chat_id
       AND sf.fact_type = p_fact_type AND sf.subject = p_subject
       AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
     ORDER BY fe.embedding <=> p_embedding LIMIT 1;
  END IF;

  IF v_existing_similarity >= 0.97 THEN
    INSERT INTO fact_occurrences(tenant_id, fact_id, source_message_id, chat_id)
    VALUES (p_tenant_id, v_existing_id, p_source_message_id, 
            COALESCE(p_chat_id, (SELECT chat_id FROM semantic_facts WHERE id = v_existing_id)))
    ON CONFLICT DO NOTHING;
    RETURN QUERY SELECT 'occurrence_added'::TEXT, v_existing_id;

  ELSIF v_existing_similarity >= p_similarity_threshold THEN
    BEGIN
      INSERT INTO semantic_facts(
        tenant_id, scope_kind, chat_id, fact_text, normalized_fact_hash,
        fact_type, subject, confidence, source, source_message_id)
      VALUES (p_tenant_id, p_scope_kind, p_chat_id, p_fact_text, v_hash,
              p_fact_type, p_subject, p_confidence, p_source, p_source_message_id)
      RETURNING id INTO v_new_id;
    EXCEPTION WHEN unique_violation THEN
      -- Catch-net сработал — точный дубликат уже существует
      SELECT id INTO v_existing_id FROM semantic_facts
       WHERE tenant_id=p_tenant_id AND scope_kind=p_scope_kind 
         AND COALESCE(chat_id,'')=COALESCE(p_chat_id,'')
         AND fact_type=p_fact_type AND subject=p_subject 
         AND normalized_fact_hash=v_hash
         AND deleted_at IS NULL AND superseded_by_id IS NULL;
      INSERT INTO fact_occurrences(tenant_id, fact_id, source_message_id, chat_id)
      VALUES (p_tenant_id, v_existing_id, p_source_message_id, COALESCE(p_chat_id, ''))
      ON CONFLICT DO NOTHING;
      RETURN QUERY SELECT 'occurrence_added_race'::TEXT, v_existing_id;
      RETURN;
    END;
    INSERT INTO fact_embeddings(fact_id, tenant_id, scope_kind, chat_id, 
                                embedding_model, embedding)
    VALUES (v_new_id, p_tenant_id, p_scope_kind, p_chat_id, 
            'text-embedding-3-small', p_embedding);
    UPDATE semantic_facts SET superseded_by_id = v_new_id, deleted_at = now() 
      WHERE id = v_existing_id;
    RETURN QUERY SELECT 'superseded'::TEXT, v_new_id;

  ELSE
    BEGIN
      INSERT INTO semantic_facts(
        tenant_id, scope_kind, chat_id, fact_text, normalized_fact_hash,
        fact_type, subject, confidence, source, source_message_id)
      VALUES (p_tenant_id, p_scope_kind, p_chat_id, p_fact_text, v_hash,
              p_fact_type, p_subject, p_confidence, p_source, p_source_message_id)
      RETURNING id INTO v_new_id;
    EXCEPTION WHEN unique_violation THEN
      SELECT id INTO v_existing_id FROM semantic_facts
       WHERE tenant_id=p_tenant_id AND scope_kind=p_scope_kind 
         AND COALESCE(chat_id,'')=COALESCE(p_chat_id,'')
         AND fact_type=p_fact_type AND subject=p_subject 
         AND normalized_fact_hash=v_hash
         AND deleted_at IS NULL AND superseded_by_id IS NULL;
      INSERT INTO fact_occurrences(tenant_id, fact_id, source_message_id, chat_id)
      VALUES (p_tenant_id, v_existing_id, p_source_message_id, COALESCE(p_chat_id, ''))
      ON CONFLICT DO NOTHING;
      RETURN QUERY SELECT 'occurrence_added_race'::TEXT, v_existing_id;
      RETURN;
    END;
    INSERT INTO fact_embeddings(fact_id, tenant_id, scope_kind, chat_id, 
                                embedding_model, embedding)
    VALUES (v_new_id, p_tenant_id, p_scope_kind, p_chat_id, 
            'text-embedding-3-small', p_embedding);
    RETURN QUERY SELECT 'inserted'::TEXT, v_new_id;
  END IF;
END;
$$ LANGUAGE plpgsql;

-- GRANTs
REVOKE INSERT, UPDATE ON semantic_facts FROM app_role;
REVOKE INSERT, UPDATE ON fact_embeddings FROM app_role;
GRANT EXECUTE ON FUNCTION upsert_semantic_fact TO app_role;
GRANT EXECUTE ON FUNCTION change_fact_scope TO app_role;
GRANT EXECUTE ON FUNCTION accept_pending_health_fact TO app_role;
```

### 9.2. Stored procedure для override scope
(аналогично v2, без изменений — см. §5.5)

### 9.3. Stored procedure для accept health-факта

```sql
CREATE OR REPLACE FUNCTION accept_pending_health_fact(
  p_pending_id  BIGINT,
  p_embedding   vector(1536)
) RETURNS BIGINT AS $$
DECLARE
  v_row RECORD;
  v_new_fact_id BIGINT;
BEGIN
  SELECT * INTO v_row FROM pending_global_health_facts 
   WHERE id = p_pending_id AND status = 'pending' FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'Pending health fact not found or already processed';
  END IF;
  
  -- Прямой INSERT минуя upsert_semantic_fact, т.к. она блокирует global health
  INSERT INTO semantic_facts(
    tenant_id, scope_kind, chat_id, fact_text, normalized_fact_hash,
    fact_type, subject, confidence, source, source_message_id)
  VALUES (v_row.tenant_id, 'owner_global', NULL, v_row.fact_text, 
          normalize_fact_text(v_row.fact_text),
          'health', 'owner', v_row.confidence, 'owner_confirmed', v_row.source_message_id)
  RETURNING id INTO v_new_fact_id;
  
  INSERT INTO fact_embeddings(fact_id, tenant_id, scope_kind, chat_id, 
                              embedding_model, embedding)
  VALUES (v_new_fact_id, v_row.tenant_id, 'owner_global', NULL, 
          'text-embedding-3-small', p_embedding);
  
  UPDATE pending_global_health_facts 
     SET status='accepted', reviewed_at=now(), resulting_fact_id=v_new_fact_id
   WHERE id=p_pending_id;
  
  RETURN v_new_fact_id;
END;
$$ LANGUAGE plpgsql;
```

---

## 10. Privacy & deletion contract

### 10.1. Privacy flags

| Flag              | Эффект                                                              |
|-------------------|---------------------------------------------------------------------|
| `no_memory`       | Extraction не запускается. Chat-факты не создаются. Сводки не пишутся. Global-факты владельца, извлечённые из других чатов, остаются видны в этом чате (если не `no_global_facts`). |
| `no_global_facts` | В этом чате не использовать global-факты владельца при retrieval.   |
| `no_learning`     | Данные не идут в обучение модели. На retrieval не влияет.           |
| `no_summaries`    | Episode summaries не создаются. Chat-факты создаются.               |
| `private`         | mode=manual принудительно, авто-отправка заблокирована.             |

### 10.2. Deletion operations

| Операция                | Что делает                                                          |
|-------------------------|---------------------------------------------------------------------|
| `forget_chat`           | Hard delete: messages, message_states, episode_summaries, active_episodes, chat-факты, embeddings, occurrences, contact_profile, suggestions, extraction_log. Global-факты владельца НЕ удаляются. |
| `forget_chat_with_globals` | То же + global-факты, извлечённые из messages этого чата (через source_message_id). |
| `forget_all`            | Hard delete всего tenant'а. Cascade.                                |
| `unlearn_fact`          | Hard delete конкретного факта + embedding + occurrences.            |
| `export`                | JSONL экспорт всех таблиц. Async.                                   |

Все deletion-операции пишут в `audit_events`.

### 10.3. Sensitive data: health-факты

**Chat-scoped health** (о контакте) — обычный extraction без confirmation. Иначе UX невыносим.

**Global health** (о владельце) — обязательный confirmation flow:
1. Extractor создаёт запись в `pending_global_health_facts`.
2. UI показывает alert с фактом, источником и кнопками Accept / Reject.
3. Только Accept триггерит `accept_pending_health_fact` → запись в `semantic_facts`.
4. Reject → status='rejected', логируется в extraction_log как обучающий сигнал.
5. Pending факты с истёкшим TTL (например, 14 дней) → auto-expired.

Прямой INSERT global health в `semantic_facts` минуя этот flow заблокирован в `upsert_semantic_fact` (см. §9.1).

**Дополнительно для всех health-фактов:**
- Не попадают в `extraction_log.raw_output` в открытом виде (только fact_id).
- Не экспортируются через partner integrations.
- При `forget_chat` удаляются первыми, в отдельной транзакции.

---

## 11. Что меняется в коде

1. **Extractor:** убрать чтение собственного output. Источник — только `messages`.
2. **Structured output:** требовать `subject` и `fact_type` в каждом extracted candidate.
3. **Scope assignment:** детерминированно в application-коде (§5.4). Если subject=owner & fact_type=health → в `pending_global_health_facts`, не в `semantic_facts`.
4. **Suggestions UI:** экран pending suggestions + экран pending health-confirmations.
5. **Fact management UI:** карточка факта с promote/demote scope, показ scope в иконке.
6. **Confidence-gates auto-mode:** не повышать до accepted_facts ≥ N AND filled_contact_profile_fields ≥ K.
7. **Active episode handling:** на каждое incoming сообщение — upsert в `active_episodes` (создать или обновить last_message_at). Фоновая job каждые 15 минут проверяет `active_episodes WHERE last_message_at < now() - 6h` → materialize + delete.
8. **Repository layer:** прямые INSERT/UPDATE в `semantic_facts` запрещены через GRANT.

---

## 12. Migration plan

### Phase 1 — Stop the bleeding (день 0)
1. Отключить запись `fact_type='extracted'` в `memory_facts`.
2. Отключить запись system-промптов в `owner_style`.
3. Forced dedup по exact match `fact_text` (-876 строк из 1400).

### Phase 2 — Risk_level backfill (день 1)
1. Убедиться, что `risk_level` сейчас nullable (он уже такой по факту).
2. Запустить policy_check job на все existing messages WHERE risk_level IS NULL. Default результат при ошибке policy_check = `'safe'` (consciously — на existing dataset цена не учесть hidden risk низкая).
3. Проверить: `SELECT COUNT(*) FROM messages WHERE risk_level IS NULL` = 0.
4. `ALTER TABLE messages ALTER COLUMN risk_level SET NOT NULL;`

### Phase 3 — Schema migration (неделя 1)
1. `CREATE EXTENSION IF NOT EXISTS vector; CREATE EXTENSION IF NOT EXISTS pgcrypto;`
2. Создать новые таблицы: `user_settings`, `contact_profile_suggestions`, `active_episodes`, `semantic_facts`, `fact_embeddings`, `fact_occurrences`, `pending_global_health_facts`, `episode_summaries`, `extraction_log`. Создать views и functions §6.2, §9.1, §9.3.
3. Миграция `memory_facts` → `semantic_facts`:
   - фильтр длины ≤ 200,
   - исключение префиксов,
   - все записи получают `scope_kind='chat'` (без авто-глобализации),
   - `normalized_fact_hash` посчитать,
   - `source='llm_extracted'`, `confidence=0.5`,
   - конфликты UNIQUE indexa пропускаются (это desired behavior — дедуп).
4. Бэкфилл embeddings реальной моделью (`text-embedding-3-small`).
5. `memory_facts` → `memory_facts_deprecated`, read-only.

### Phase 4 — New pipeline (неделя 2)
1. Extractor с typed JSON output.
2. Scope assignment + pending_global_health routing.
3. Episode summarizer + active_episodes management.
4. Suggestions flow в UI (contact + health).
5. Repository layer с stored proc, REVOKE прямых INSERT'ов.

### Phase 5 — Override UI и cleanup (неделя 3-4)
1. UI для promote/demote scope.
2. UI для accept/reject health-confirmations.
3. Удалить `memory_facts_deprecated`.
4. Метрики: % дубликатов на write (advisory lock эффективность), % unique_violation на race (catch-net срабатывает), % accepted health-suggestions, % промоутов, retrieval latency p95.

---

## 13. Расширяемость под роли (без реализации сейчас)

`scope_kind` сейчас имеет два значения. При появлении ролей — добавится третье `'role'` + колонка `scope_role_id` + таблица `roles`. Существующие данные не мигрируют. Retrieval-логика расширяется одной веткой. Conflict resolution: `chat > role > global`.

**Чего не делать сейчас:** создавать пустую таблицу `roles`, добавлять nullable `role_id` в `semantic_facts`, закладывать role detection в LLM-промпт.

---

## 14. Когда переезжать с pgvector на Qdrant

Триггеры: > 10M активных facts, p95 latency > 200ms, нужен HNSW или hybrid search.

Переезжают: `fact_embeddings`, `episode_summaries.embedding`. PG остаётся source of truth, Qdrant — индекс.

Не делать сейчас: abstraction layer "VectorStore interface". Прямой pgvector + одна миграция позже дешевле.

---

## 15. Anti-patterns: что новая схема делает невозможным

| Дефект                                                | Защита                                                     |
|-------------------------------------------------------|------------------------------------------------------------|
| Рекурсивный префикс `[FACT] extracted: ...`           | CHECK `fact_text NOT LIKE '[FACT]%'`                       |
| Сырое сообщение как факт                              | CHECK длины ≤ 200, REVOKE direct INSERT                    |
| System-промпт в `owner_style`                         | Нет таблицы owner_style                                    |
| Семантические дубликаты                               | Stored proc + advisory lock + cosine similarity > 0.97     |
| Точные дубликаты при race                             | UNIQUE partial index на normalized_fact_hash               |
| AI пишет напрямую в contact_profiles                  | REVOKE UPDATE; только через suggestions accept             |
| AI пишет global health-факт без подтверждения         | RAISE EXCEPTION в upsert_semantic_fact для (owner_global, health) |
| Global-факт про контакта                              | CHECK `scope_kind != 'owner_global' OR subject='owner'`    |
| Chat-факт без chat_id                                 | CHECK + composite FK                                       |
| Удалили сообщение → факты с битым source              | ON DELETE SET NULL + cascade soft-delete                   |
| Денормализованный `greeted_today` рассинхронизирован  | Нет такого поля — derived view поверх messages             |
| Placeholder embeddings (16-dim hash)                  | NOT NULL embedding с явной моделью                         |

---

## 16. Open questions + отклонённые предложения

### 16.1. Открытые
1. **Cross-chat facts по telegram_user_id** — контакт пишет в два разных чата владельца. Сейчас факты разделены по chat_id. Обсудить при появлении ролей.
2. **Multi-language facts** — нормализация на `user_settings.primary_language` при extraction.
3. **Commitment updates** — supersede или отдельный тип `commitment_update`?
4. **Auto-suggestion демоушна** — если владелец последовательно нарушает global-факт в чате, AI предлагает demote. Через suggestions, не автоматика.
5. **RLS включение** — триггер: первый paying customer / B2B.

### 16.2. Отклонённые предложения (с обоснованием)

**❌ Глобальный стиль владельца как "слабый fallback для cold start".**
Отклонено повторно в v3. Cold start решается дефолтами по `relationship_type` (§4.6) — это лучше работает, потому что учитывает категорию контакта, а не усредняет по всем перепискам. "Слабый fallback" — самообман: если вес > 0, влияние есть; через месяц получим тот же self-reinforcing loop и дрейф, от которого ушли в v2. Manual style сильнее inferred — это уже структурно встроено через suggestions flow, не требует понятия глобального стиля.

**❌ Таблица `dialog_state` с булевыми флагами (greeted_today, has_pending_commitment, pause_6h_passed).**
Отклонено в v3. Денормализованные флаги рассинхронизируются с источником при первом edge case (удалили сообщение → флаг остался). Заменено двумя архитектурно правильными конструкциями: `active_episodes` для **runtime state** (текущая тема, открытый вопрос, ожидаемое действие — то, что нельзя вычислить из messages), и `view_*` функции для **derived state** (запросы к messages, всегда консистентные с источником).

**❌ Очередь extraction-job per chat (Redis / SQS) для concurrency.**
Отклонено для MVP. pg_advisory_xact_lock + UNIQUE catch-net достаточно. Очередь добавит инфраструктурной сложности без выигрыша на текущей нагрузке. Триггер пересмотра: lock contention > 10ms p95.

---

## Appendix A. Глоссарий
- **User layer / Per-chat identity / Working / Runtime state / Episodic / Semantic** — слои памяти (§2).
- **Scope (chat / owner_global)** — границы видимости факта.
- **Promote / Demote** — ручной перевод scope владельцем.
- **Dedup-on-write** — проверка дубликата перед insert (advisory lock + UNIQUE catch-net).
- **Supersede** — замена старого факта новым.
- **Hard-include** — taboo + health всегда в промпте.
- **Suggestion** — pending изменение в profile.
- **Confirmation flow** — owner-approval для sensitive фактов (global health).
- **Active episode** — открытый, не закрытый в summary эпизод.
- **Derived view** — SQL function поверх messages для производных состояний.

## Appendix B. Не делать
- ❌ Возвращать глобальный стиль владельца.
- ❌ Денормализовать "греетинги / паузы / pending" в булевы.
- ❌ Расширять `scope_kind` без явного дизайна.
- ❌ Позволять AI менять `scope_kind` напрямую.
- ❌ Записывать global health-факт без confirmation.
- ❌ Создавать пустые таблицы "на будущее".
- ❌ Прямые INSERT в `semantic_facts` минуя stored proc.
- ❌ AI-роли / маркетплейс ролей в текущей итерации.
- ❌ Повышать default mode до auto для новых чатов.
- ❌ LLM решает о scope.
