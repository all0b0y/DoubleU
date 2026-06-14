# DoubleU — Memory Architecture & Data Design

**Status:** v6 (синхронизирован с кодом и миграциями 001–016, 2026-06-11)
**Audience:** Engineering, Data, Product
**Scope:** Core memory subsystem — schema, lifecycle, retrieval, privacy, runtime state, AI-роли.
**Out of scope:** Telegram Business webhook handling, model selection, billing, UI.

**Changelog:**
- v6 (выравнивание с реальностью, MEM-12): эмбеддинги — `qwen/qwen3-embedding-8b`, `vector(4096)`, асинхронный embedding-worker в Go + halfvec-HNSW expression-индексы (миграция 015); `upsert_semantic_fact` v3 принимает **embedding кандидата параметром** и возвращает `(action, fact_id, superseded_fact_id)` (миграция 013) — PG эмбеддинги не генерирует; deletion contract §10.2 реализован (`ForgetChat`/`UnlearnFact`/`ForgetAll` + бот-команды `/forget`, `/forget_all`); suggestions flow §4.5 реализован (extraction → `contact_profile_suggestions` → accept/reject в боте, экспирация 14 дней); идемпотентность сводок — по пересечению периодов эпизода, не по дате (`summarizeEpisode` + daily-fallback по непокрытому окну); `messages.risk_level NOT NULL DEFAULT 'low'` (миграция 014); описаны подсистемы, появившиеся в коде после v5: temporal knowledge graph (`fact_relations`, 009), sleep-time consolidation (`consolidate_facts`, 010), embedding worker с nudge-каналом, topic из структурированного draft-ответа (ARCH-4), детерминированный cosine-rerank вместо LLM-reranker (ARCH-5), токен-бюджет контекста §8.1 (4000 ток.), runtime state `open_question_message_id`/`pending_owner_action` (MEM-5). Удалены из документа DDL `pending_global_health_facts` и процедуры global-health (наследие ≤v4). GRANT-защита прямых INSERT отложена до выделенной DB-роли — в коде DB-путь записи фактов идёт только через процедуру (MEM-11).
- v5 (продуктовый разворот): **память строго per-chat** — scope `owner_global` упразднён (миграция 011 soft-delete'ит существующие глобальные факты, код их не пишет и не читает); **AI-роли** — вместо "цифровой копии владельца" система воспроизводит описанную владельцем персону (`ai_roles` + `chat_settings.ai_role_id`, §13); промпт перестроен role-first; очередь `pending_global_health_facts` удалена (health-факты — обычные chat-scoped с hard-include).
- v3: добавлен runtime-state слой (`active_episodes`); concurrency protection в dedup (advisory lock + normalized_fact_hash UNIQUE); composite FK `source_message_id`; денормализация `tenant_id` в `fact_occurrences`; явный `CREATE EXTENSION vector`; mandatory confirmation flow для global health-фактов; 4-step backfill для `risk_level`.
- v2: добавлен `scope_kind` (chat / owner_global); правило назначения scope и override; удалён глобальный стиль владельца.
- v1: первая редакция.

---

## 0. Why this document exists

> **Историческая справка (v1, 2025).** Метрики ниже описывают давно удалённую
> таблицу `memory_facts` — её не существует с clean-schema миграции 001.
> Раздел сохранён как мотивация контрактов, не как описание текущего состояния.

Реализация памяти на момент v1 **не работала как память**. Эмпирически на синтетическом датасете (1400 записей `memory_facts`):

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
3. **Стиль общения — свойство пары (роль, контакт).** Нет глобального baseline стиля. Каждый чат имеет свою назначенную AI-роль (§13) и свой стиль в `contact_profiles`.
4. **Память строго per-chat. Общей памяти нет и не существует.** (v5) Каждый чат живёт со своей памятью: факт, наблюдённый в одном чате, никогда не виден в другом — включая факты о владельце. Ни один класс памяти не пересекает границу чата.
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
│  SEMANTIC — атомарные факты, строго per-chat                    │
│  semantic_facts (scope=chat — единственный scope, v5)           │
│  fact_embeddings + fact_occurrences                              │
└─────────────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────────────┐
│  AI ROLES — персоны для ответов (§13, v5)                       │
│  ai_roles (библиотека) + chat_settings.ai_role_id (назначение)  │
└─────────────────────────────────────────────────────────────────┘
```

**Мэппинг "продуктовый язык → архитектурный":**

| Продуктовое название         | Архитектурный слой            | Таблица                       |
|------------------------------|-------------------------------|-------------------------------|
| Краткосрочная память (день)  | Working + Runtime             | `messages` + `active_episodes`|
| Долгосрочная (сводки)        | Episodic                      | `episode_summaries`           |
| Факты о контакте             | Semantic (scope=chat)         | `semantic_facts`              |
| Факты о владельце в чате     | Semantic (scope=chat)         | `semantic_facts`              |
| Карточка контакта            | Per-chat identity             | `contact_profiles`            |
| AI-роль (персона) чата       | AI Roles (§13)                | `ai_roles` + `chat_settings`  |
| Настройки владельца          | User layer                    | `user_settings`               |

**Чего НЕТ намеренно:**
- Общей (кросс-чатной) памяти в любом виде — scope `owner_global` упразднён в v5.
- Глобального профиля стиля владельца (§16 — почему отклонено повторно).
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

### 5.3. Scope: только chat (v5)

**`scope_kind = 'chat'`** — единственный действующий scope. Факт привязан к chat_id и виден только в этом чате.

Scope `owner_global` упразднён в v5: память строго per-chat, кросс-чатных фактов не существует. Колонка `scope_kind` сохранена ради provenance soft-deleted legacy-строк (миграция 011), новые записи всегда `'chat'`.

### 5.4. Правило назначения scope (детерминированное)

```
scope_kind = 'chat', chat_id = $current_chat_id   -- всегда, без исключений
```

Go-бэкенд игнорирует любые scope-хинты от Python-сервиса (защита от
галлюцинаций LLM и от legacy-клиентов, всё ещё присылающих `owner_global`).
Health-факты больше не требуют confirmation flow: они chat-scoped и попадают
только в контекст своего чата (hard-include §8.2); очередь
`pending_global_health_facts` удалена.

### 5.5. Override владельца: ручное изменение scope

Упразднено в v5 вместе с global-scope: единственный scope — `chat`,
переводить факт некуда. Управление фактами сводится к удалению/правке
текста факта (через suggestions flow).

### 5.6. Конфликты фактов

Конфликт = два факта одного `fact_type`, одного `subject`, cosine similarity > 0.85 **внутри одного чата**. Разрешается dedup-on-write (§5.8): новый факт supersede'ит старый. Межчатовых конфликтов не существует — память чатов не пересекается.

### 5.7. Поля semantic_facts

Полный DDL — в §9. Ключевые инварианты (через CHECK):
- `scope_kind='chat'` ⇒ `chat_id IS NOT NULL`.
- `fact_text` без префиксов `[FACT]`, `[EXTRACTED]`, `[OWNER STYLE]`.
- `char_length(fact_text) BETWEEN 5 AND 200`.
- `normalized_fact_hash` — детерминированный hash (lower + trim + collapse whitespace) для UNIQUE constraint.

### 5.8. Dedup-on-write контракт

Защищён двумя независимыми механизмами:

**Механизм 1: Advisory lock + semantic search в stored proc** (`upsert_semantic_fact` v3, миграция 013).
```
0. Go-бэкенд синхронно эмбеддит кандидата (/ai/embed-texts, таймаут 5 с) и передаёт
   вектор параметром p_embedding. PG эмбеддинги не генерирует; p_embedding=NULL
   деградирует дедуп до hash-only (асинхронный worker доэмбеддит позже).
1. pg_advisory_xact_lock(hashtextextended(tenant_id || ':' || scope || ':' || chat_id))
   — сериализует параллельные dedup в пределах чата. Гранулярность сознательно
   грубее, чем (type, subject): contention per-chat, lock транзакционный (§16.2).
2. Tier 1: exact match по normalized_fact_hash → fact_occurrences++ → ('occurrence_added', id, NULL).
3. Tier 2/3 (только при p_embedding IS NOT NULL): top-1 по cosine к p_embedding
   WHERE тот же chat AND fact_type AND subject.
   similarity ≥ 0.97 → identical → occurrence.
   0.85 ≤ similarity < 0.97 → insert новый + у старого superseded_by_id=new, deleted_at=now()
                              → ('superseded', new_id, old_id).
   similarity < 0.85 → ('inserted', new_id, NULL); embedding кандидата пишется в
                       fact_embeddings в той же транзакции.
4. Возврат TABLE(action, fact_id, superseded_fact_id) — Go создаёт ребро
   fact_relations(supersedes) из superseded_fact_id без дополнительного запроса.
```

**Механизм 2: UNIQUE partial index по normalized_fact_hash как catch-net.**
Даже если advisory lock пропустил (split brain, разные транзакции на разных нодах PG в будущем) — точный дубликат не пройдёт уникальный индекс. Stored proc обрабатывает unique_violation как "occurrence_added_race".

Это **defense in depth**: lock — оптимистично для семантических дубликатов, UNIQUE — гарантия для точных. В коде DB-путь записи фактов идёт исключительно через процедуру (`SaveSemanticFact` — только in-memory fallback при отсутствии DB); GRANT-защита от прямых INSERT отложена до появления выделенной DB-роли — подключение пока использует одну роль (см. §11 п.8).

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
  → IF risk OK AND chat NOT in no_memory AND message проходит pre-filter
        (≥15 символов ИЛИ содержит цифру/имя — MEM-13; «ок»/«да» LLM не зовут):
      → extraction_job (async, per-message + окно последних 5 сообщений
        как контекст; батчинг по эпизодам — целевое состояние, гейтится
        замером extraction_log, см. MEM-13 в memory_architecture_backlog.md)
          → LLM extract candidates (typed JSON output:
              {"facts": [...], "profile_suggestions": [...]})
          → validate (Python-валидатор: fact_text 5-120 символов — строже
            контрактных 200 сознательно, length-cap по промпту; type из enum,
            subject не пуст)
          → assign scope (§5.4, v5):
              scope_kind='chat', chat_id=current   -- всегда
              → embed кандидатов (батч, 5 с) → upsert_semantic_fact(..., p_embedding)
          → profile_suggestions (whitelist полей + enum-значений):
              INSERT contact_profile_suggestions (NOT direct write) →
              карточка владельцу accept/reject; pending > 14 дней → expired
      → log в extraction_log (health-тексты заменены на маркеры [health fact #id], §10.3)
```

**Health-факты** (v5): chat-scoped, как и всё остальное, поэтому ложно-положительная экстракция влияет максимум на один чат — тот, где факт прозвучал. Confirmation flow и таблица `pending_global_health_facts` удалены вместе с global-scope (миграция 011). В retrieval health-факты по-прежнему hard-include (§8.2) — внутри своего чата.

**Запрещено** (физически — CHECK / GRANT, не "договорились"):
- Передавать в extractor предыдущий output extractor'а.
- Записывать в `semantic_facts` строки с префиксами `[FACT]`, `[EXTRACTED]`, `[OWNER STYLE]`.
- Тексты длиной > 200 символов.
- Прямой INSERT в `semantic_facts` минуя stored proc.
- Прямой UPDATE `contact_profiles` от extractor'а.

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

### 7.4. Смерть

1. **Soft delete по expires_at** — фоновая job, retrieval исключает, 30 дней до hard delete.
2. **Soft delete cascade** — если удалён `source_message_id` (ON DELETE SET NULL на FK) и нет других occurrences → soft delete факта.
3. **Hard delete** — по запросу владельца / GDPR. Каскадит на `fact_embeddings`, `fact_occurrences`, `extraction_log`, `episode_summaries`, `active_episodes`.

### 7.5. Episodic summaries: границы эпизодов

Эпизод закрывается при:
- молчании > 6 часов (`closeStaleEpisodes`, тикер 15 мин, `closure_reason='silence_gap'`),
- 40 сообщений (`closure_reason='message_count'`),
- topic_shift (опционально, не реализован).

При закрытии: `summarizeEpisode` берёт сообщения **реальных границ эпизода**
(`started_at..last_message_at`, не календарных суток), пишет
`period_start/end` = границы эпизода и заполняет `source_message_ids[]`
реальными ID (provenance §6). Идемпотентность — по пересечению периодов
(`EpisodeSummaryOverlaps`): два эпизода в один день дают две сводки, повторное
закрытие того же — ни одной. Дополнительный слой — ежедневный fallback
(`closure_reason='daily_summary'`, миграция 012 расширила CHECK): в полночь
суммаризуется только окно дня, **не покрытое** ни одной сводкой
(`LatestCoveredEnd`), закрывая случай «чат активен, но эпизод не закрылся до
полуночи». Summary короче 50 символов логируется и пропускается (CHECK 50..1500);
длиннее 1500 — обрезается. После записи embedding-worker получает nudge.

Содержание summary: 3-6 предложений (тема, решения, commitments, тон) —
структурированный вызов `{summary, action_items[], recommendations[]}` (ARCH-10).

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

6. Semantic block (только этот чат, v5):
   - 6a: chat-факты — SELECT top-K WHERE scope_kind='chat' AND chat_id=$X
        (vector + BM25 → RRF fusion → детерминированный cosine-rerank в SQL
         по embedding кандидатов, top-8; LLM-reranker /ai/rerank-facts из
         горячего пути убран — ARCH-5, эндпоинт жив до cutover-замера)
   - 6b: hard-include всех taboo + health этого чата (не вытесняются ранжированием)

   Независимые чтения шагов 2-6 выполняются параллельно (errgroup-паттерн):
   embed-вызов (~2-5 с) перекрывает DB-roundtrip'ы (ARCH-8).

7. Compose prompt (role-first, v5):
   [System: КТО ТЫ — ai_role чата: имя, описание, стиль речи, цели,
            ограничения; без роли — дефолтная персона "ассистент владельца,
            пиши в его стиле"]
   [О владельце, от чьего имени идёт переписка: display_name, timezone, language]
   [Contact: имена, отношения, как обращаться, стиль для этого чата]
   [Chat context: о чём чат, что не обсуждать]
   [Runtime: open_question, current_topic, pending_owner_action, paused_hours]
   [Chat facts: ... — только факты этого чата]
   [Recent episodes: 1-3 summaries]
   [Conversation: последние 20 сообщений]
   [Incoming: текущее сообщение]
   [Instruction: приоритет — инструкция владельца > ограничения роли >
                 описание/стиль роли > профиль контакта > переписка]
```

### 8.1. Token budget (8k context):
- User + identity + chat settings + runtime + role: ~600
- Chat-facts (5-8 × 30): ~200
- Episodic (3 × 100): ~300
- Working (20 × 30): ~600
- Overhead: ~500

Итого ~2.2k. При урезании: working (20→10) → episodic (3→1) → chat-facts (8→5) → pending-drafts (→2). Identity, role, runtime, commitments, hard-include — не режутся.

**Реализация (MEM-9/ARCH-15):** бюджет в Go (`conversationContext`) — оценка
`len(text)/3` токенов, потолок `contextTokenBudget = 4000` на собранный
memory-контекст (identity/role собираются на Python-стороне и сюда не входят);
урезание строго в порядке выше, срабатывание логируется
(`context budget trimming applied`). `paused_hours` инжектится строкой
`[RUNTIME: paused_hours=N.N]` из `view_chat_paused_hours`.

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
-- Индекс под фоновую job, которая закрывает залежавшиеся эпизоды.
-- ВАЖНО: partial index с предикатом now() невозможен (now() не immutable) —
-- 001 создаёт обычный индекс; predicate-вариант из ранних версий документа
-- не создался бы вовсе.
CREATE INDEX ix_ae_stale ON active_episodes(last_message_at);

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
                      ('silence_gap','message_count','topic_shift','manual','daily_summary')),  -- 'daily_summary' добавлен миграцией 012
  embedding           vector(4096),       -- qwen/qwen3-embedding-8b (миграция 003)
  embedding_model     TEXT,               -- добавлена миграцией 012
  privacy_flags       JSONB NOT NULL DEFAULT '{}'::jsonb,
  deleted_at          TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_es_chat ON episode_summaries(tenant_id, chat_id, period_end DESC)
  WHERE deleted_at IS NULL;
-- ANN: pgvector HNSW не поддерживает vector >2000 dims; миграция 015 создаёт
-- expression-индекс по halfvec-касту (требует pgvector ≥ 0.7):
CREATE INDEX ix_es_halfvec_hnsw ON episode_summaries
  USING hnsw ((embedding::halfvec(4096)) halfvec_cosine_ops)
  WHERE deleted_at IS NULL AND embedding IS NOT NULL;

-- ============================================================
-- SEMANTIC LAYER
-- v5: DDL ниже — историческая схема 001. CHECK всё ещё допускает
-- 'owner_global' (строки сохранены как soft-deleted provenance, миграция 011),
-- но код пишет и читает ТОЛЬКО scope_kind='chat'. Таблица
-- pending_global_health_facts удалена миграцией 011. Роли — в ai_roles (§13).
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
  embedding_model   TEXT NOT NULL DEFAULT 'qwen/qwen3-embedding-8b',
  -- vector(4096) с миграции 003; NULLABLE: строка появляется при insert факта,
  -- вектор пишет либо upsert_semantic_fact (если кандидат пришёл с embedding),
  -- либо асинхронный embedding-worker (тикер 5 мин + nudge-канал при insert).
  embedding         vector(4096),
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- ANN (миграция 015, pgvector ≥ 0.7):
CREATE INDEX ix_fe_halfvec_hnsw ON fact_embeddings
  USING hnsw ((embedding::halfvec(4096)) halfvec_cosine_ops)
  WHERE embedding IS NOT NULL;
CREATE INDEX ix_fe_chat_scope ON fact_embeddings(tenant_id, chat_id) 
  WHERE scope_kind='chat';
-- Запросы ранжирования используют двухступенчатую схему: внутренний ANN-подзапрос
-- упорядочивает чисто по cosine (только такую форму обслуживает HNSW-индекс) с
-- oversample K×3, type-боусты пересортировывают снаружи (MEM-7).

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

-- (v6: исторический DDL pending_global_health_facts удалён из документа —
--  таблица не существует с миграции 011, health-факты chat-scoped.)

-- ============================================================
-- TEMPORAL KNOWLEDGE GRAPH (миграция 009, появился после v3)
-- ============================================================
CREATE TABLE fact_relations (
  id             BIGSERIAL PRIMARY KEY,
  tenant_id      TEXT NOT NULL,
  fact_id_from   BIGINT NOT NULL REFERENCES semantic_facts(id) ON DELETE CASCADE,
  fact_id_to     BIGINT NOT NULL REFERENCES semantic_facts(id) ON DELETE CASCADE,
  relation_type  TEXT NOT NULL,  -- supersedes | fulfills | contradicts | updates | related
  confidence     REAL,
  UNIQUE (fact_id_from, fact_id_to, relation_type)
);
-- Ребро 'supersedes' создаёт Go из superseded_fact_id результата
-- upsert_semantic_fact; view current_facts резолвит «текущую истину».

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

-- Актуальная версия процедуры — миграция 013 (upsert_semantic_fact v3).
-- Сигнатура (источник истины — файл миграции, не этот документ):
CREATE FUNCTION upsert_semantic_fact(
  p_tenant_id         TEXT,
  p_scope_kind        TEXT,
  p_chat_id           TEXT,
  p_fact_text         TEXT,
  p_fact_type         TEXT,
  p_subject           TEXT,
  p_confidence        REAL,
  p_source            TEXT,
  p_source_message_id TEXT,
  p_expires_at        TIMESTAMPTZ DEFAULT NULL,
  p_embedding         vector(4096) DEFAULT NULL,   -- embedding КАНДИДАТА от вызывающего
  p_embedding_model   TEXT DEFAULT 'qwen/qwen3-embedding-8b'
) RETURNS TABLE(action TEXT, fact_id BIGINT, superseded_fact_id BIGINT);
```

Семантика — §5.8 (Механизм 1). Ключевые отличия от псевдокода ранних версий:
- embedding кандидата приходит **параметром** (PG не умеет генерировать
  эмбеддинги); `NULL` → hash-only дедуп;
- advisory lock — на `(tenant, scope, chat)` без `(fact_type, subject)`:
  сознательно более грубая гранулярность (§16.2), contention per-chat;
- supersede атомарно ставит `deleted_at = now()` у старого факта (§7.3);
- возврат `superseded_fact_id` избавил Go от follow-up запроса
  (`FindSupersededFactID` удалён);
- веток global-scope и global-health нет (v5);
- при pgvector без `halfvec`/`vector` создаётся hash-only вариант процедуры
  с параметром `FLOAT8[]` (тот же интерфейс для вызывающего).

**GRANT-защита (§11 п.8) — отложена.** Бэкенд подключается одной ролью;
`app_role` не существует. Инвариант обеспечен в коде: DB-путь записи фактов
идёт только через процедуру (`SaveSemanticFact` — in-memory fallback при
`db == nil`). Полные REVOKE/GRANT — при появлении выделенной роли
(MEM-11 в memory_architecture_backlog.md).

### 9.2. (упразднено в v5 — override scope не существует, см. §5.5)

### 9.3. (упразднено в v5 — health-confirmation flow удалён, см. §10.3)

---

## 10. Privacy & deletion contract

### 10.1. Privacy flags

Имена флагов — из frozen contract (`contract_enums`); v6 синхронизировал
документ с кодом (`no_summary` в ед. числе, `no_training` вместо `no_learning`;
`no_global_facts` упразднён вместе с global scope в v5).

| Flag          | Эффект                                                              |
|---------------|---------------------------------------------------------------------|
| `no_memory`   | Extraction не запускается. Chat-факты не создаются. Сообщения не персистятся. |
| `no_training` | Данные не идут в обучение модели. На retrieval не влияет.           |
| `no_summary`  | Episode summaries не создаются. Chat-факты создаются.               |
| `private`     | Авто-отправка заблокирована (decision никогда не auto_reply).       |

### 10.2. Deletion operations (реализовано, MEM-2)

| Операция       | Что делает                                                          |
|----------------|---------------------------------------------------------------------|
| `forget_chat`  | `store.ForgetChat`: hard delete в одной транзакции — health-факты первыми (§10.3), затем все chat-факты (cascade: embeddings, occurrences, relations), episode_summaries, active_episodes, extraction_log, suggestions, contact_profile, draft_feedback, drafts, messages (cascade: states, audits). Чат и его настройки остаются. Бот: `/forget <N>` + inline-подтверждение. |
| `forget_all`   | `store.ForgetAll`: hard delete всех данных tenant'а (chats cascade + connections, unclaimed, idempotency, user_settings, ai_roles). Бот: `/forget_all` + подтверждение фразой «УДАЛИТЬ ВСЁ». |
| `unlearn_fact` | `store.UnlearnFact`: hard delete конкретного факта (cascade: embedding, occurrences, relations). |
| `export`       | JSONL экспорт всех таблиц. Async. **Не реализован** — отдельная задача (см. memory_architecture_backlog.md, MEM-2). |

Все deletion-операции пишут audit-запись в той же транзакции
(`memory_forget_chat` / `memory_unlearn_fact` / `memory_forget_all`).
Нюанс схемы: `audit_events` несёт NOT NULL FK на `messages` с ON DELETE
CASCADE, поэтому audit-строка ссылается на синтетическое marker-сообщение,
вставляемое после удалений в той же транзакции — иначе каскад удалил бы и сам
audit. Атомарность транзакции сохраняет инвариант «нет удаления без audit».

### 10.3. Sensitive data: health-факты

**v5:** все health-факты chat-scoped — обычный extraction без confirmation (ложно-положительная экстракция влияет максимум на один чат). Confirmation flow и `pending_global_health_facts` удалены вместе с global-scope.

**Дополнительно для всех health-фактов:**
- Не попадают в `extraction_log.raw_output` в открытом виде (только fact_id).
- Не экспортируются через partner integrations.
- При `forget_chat` удаляются первыми, в отдельной транзакции.

---

## 11. Что меняется в коде

1. **Extractor:** убрать чтение собственного output. Источник — только `messages`.
2. **Structured output:** требовать `subject` и `fact_type` в каждом extracted candidate.
3. **Scope assignment (v5):** всегда `scope_kind='chat'` в application-коде (§5.4); хинты Python игнорируются.
4. **Suggestions UI:** экран pending suggestions.
5. **AI-роли (v5):** CRUD ролей + назначение на чат (бот-команды §13.3); резолв роли в `AIProcessMessageRequest.ai_role`; role-first промпт в Python.
6. **Confidence-gates auto-mode:** не повышать до accepted_facts ≥ N AND filled_contact_profile_fields ≥ K.
7. **Active episode handling:** на каждое incoming сообщение — upsert в `active_episodes` (создать или обновить last_message_at). Фоновая job каждые 15 минут проверяет `active_episodes WHERE last_message_at < now() - 6h` → materialize + delete.
8. **Repository layer:** прямые INSERT/UPDATE в `semantic_facts` запрещены через GRANT.

---

## 12. Migration plan — выполнен / неприменим (v6)

План писался против таблицы `memory_facts`, которой больше нет: миграция 001 —
clean schema, фазы 1 и 3 неприменимы. Текущий статус пунктов:

- **Phase 2 (risk_level NOT NULL)** — ✅ выполнено миграцией 014
  (`UPDATE ... SET 'low' WHERE NULL` + `SET DEFAULT 'low'` + `SET NOT NULL`;
  inbound-сообщение персистится до AI-классификации, поэтому дефолт сознательный).
- **Phase 4 (typed extractor, episode summarizer, suggestions flow)** — ✅
  реализовано (extraction.py typed JSON, `summarizeEpisode`, suggestions
  §4.5 / MEM-4 с бот-карточками accept/reject).
- **Phase 5.4 (метрики write-path)** — частично: действия upsert'а
  (inserted / occurrence_added / superseded / rejected) возвращаются и
  логируются; counters в observability — TODO (MEM-10).
- **GRANT/REVOKE** — отложено до выделенной DB-роли (§9.1, MEM-11).

---

## 13. AI-роли (реализовано в v5)

**Продуктовый разворот.** Раньше цель была "спародировать владельца и стать его цифровой копией". Теперь система воспроизводит **роль** — персону, которую владелец описал сам: "мой менеджер", "техподдержка", любой выдуманный персонаж. ИИ отвечает в чатах от лица владельца, выступая в назначенной роли. Один владелец ведёт разные чаты в разных амплуа.

### 13.1. Модель данных

```
ai_roles (миграция 011):
  id               BIGSERIAL PK
  tenant_id        → tenants
  name             UNIQUE per tenant ("Менеджер Алексея")
  description      свободное описание персоны — главный материал промпта
  speaking_style   как роль говорит (лексика, тон, длина)
  goals            чего роль добивается в переписке
  constraints_text чего роль не делает / о чём молчит

chat_settings.ai_role_id BIGINT NULL → ai_roles(id) ON DELETE SET NULL
```

Роль — библиотечная сущность: создаётся один раз, назначается на любое число чатов. `ai_role_id IS NULL` = роль не задана → дефолтная персона "ассистент владельца" (legacy-поведение цифровой копии, теперь просто fallback).

### 13.2. Доставка в промпт

Бэкенд резолвит роль чата и кладёт объект `ai_role {name, description, speaking_style, goals, constraints}` в `AIProcessMessageRequest`. Python собирает role-first system prompt: блок «КТО ТЫ В ЭТОМ ЧАТЕ» — идентичность говорящего; стиль роли важнее наблюдённого стиля владельца; при пустом `speaking_style` per-chat стиль владельца служит подсказкой. Приоритет: инструкция владельца > ограничения роли > описание роли > профиль контакта > переписка.

### 13.3. Владение и UI

Роли создаёт только владелец (бот-команды `/role_create`, `/roles`, `/role_assign`, `/role`, `/role_clear`). AI не имеет права создавать или править роли — это identity-данные, как `contact_profiles` (§4.5).

### 13.4. Роли и память

Память остаётся строго per-chat и привязана к чату, **не к роли**: переназначение роли не переносит и не стирает память чата. Факты, накопленные в чате, продолжают подаваться в контекст независимо от того, какая роль отвечает.

---

## 14. Когда переезжать с pgvector на Qdrant

Триггер «нужен HNSW» из ранних версий уже наступил и решён внутри pgvector:
halfvec-HNSW expression-индексы (миграция 015, MEM-7). Hybrid search тоже
закрыт на PG-стороне (BM25 + RRF, миграция 008). Актуальные триггеры переезда:

- > 10M активных facts на tenant-наборе;
- p95 retrieval latency > 200 ms на ANN-запросах при прогретых индексах;
- потребность в фильтрованном ANN, который halfvec-HNSW не тянет
  (высокоселективные предикаты поверх миллионов векторов).

Переезжают: `fact_embeddings`, `episode_summaries.embedding`. PG остаётся source of truth, Qdrant — индекс.

Не делать сейчас: abstraction layer "VectorStore interface". Прямой pgvector + одна миграция позже дешевле.

---

## 15. Anti-patterns: что новая схема делает невозможным

| Дефект                                                | Защита                                                     |
|-------------------------------------------------------|------------------------------------------------------------|
| Рекурсивный префикс `[FACT] extracted: ...`           | CHECK `fact_text NOT LIKE '[FACT]%'`                       |
| Сырое сообщение как факт                              | CHECK длины ≤ 200; DB-путь записи только через stored proc (GRANT — после выделенной роли, MEM-11) |
| System-промпт в `owner_style`                         | Нет таблицы owner_style                                    |
| Семантические дубликаты                               | Stored proc + advisory lock + cosine similarity > 0.97     |
| Точные дубликаты при race                             | UNIQUE partial index на normalized_fact_hash               |
| AI пишет напрямую в contact_profiles                  | Extraction-путь пишет только в suggestions; профиль меняет лишь ResolveSuggestion(accept) с whitelist полей (MEM-4) |
| Факт утекает в чужой чат (общая память)               | v5: код пишет/читает только scope='chat'; legacy owner_global soft-deleted (011) |
| AI создаёт или правит роль                            | Нет write-пути: ai_roles меняются только бот-командами владельца |
| Chat-факт без chat_id                                 | CHECK + composite FK                                       |
| Удалили сообщение → факты с битым source              | ON DELETE SET NULL + cascade soft-delete                   |
| Денормализованный `greeted_today` рассинхронизирован  | Нет такого поля — derived view поверх messages             |
| Placeholder embeddings (16-dim hash)                  | NOT NULL embedding с явной моделью                         |

---

## 16. Open questions + отклонённые предложения

### 16.1. Открытые
1. **Multi-language facts** — нормализация на `user_settings.primary_language` при extraction.
2. **Commitment updates** — supersede или отдельный тип `commitment_update`?
3. **RLS включение** — триггер: первый paying customer / B2B.
4. **Шаблоны ролей** — преднастроенные роли ("менеджер", "поддержка") для быстрого старта; смежно с Marketplace из Iteration 2.

### 16.2. Отклонённые предложения (с обоснованием)

**❌ Глобальный стиль владельца как "слабый fallback для cold start".**
Отклонено повторно в v3. Cold start решается дефолтами по `relationship_type` (§4.6) — это лучше работает, потому что учитывает категорию контакта, а не усредняет по всем перепискам. "Слабый fallback" — самообман: если вес > 0, влияние есть; через месяц получим тот же self-reinforcing loop и дрейф, от которого ушли в v2. Manual style сильнее inferred — это уже структурно встроено через suggestions flow, не требует понятия глобального стиля.

**❌ Таблица `dialog_state` с булевыми флагами (greeted_today, has_pending_commitment, pause_6h_passed).**
Отклонено в v3. Денормализованные флаги рассинхронизируются с источником при первом edge case (удалили сообщение → флаг остался). Заменено двумя архитектурно правильными конструкциями: `active_episodes` для **runtime state** (текущая тема, открытый вопрос, ожидаемое действие — то, что нельзя вычислить из messages), и `view_*` функции для **derived state** (запросы к messages, всегда консистентные с источником).

**❌ Очередь extraction-job per chat (Redis / SQS) для concurrency.**
Отклонено для MVP. pg_advisory_xact_lock + UNIQUE catch-net достаточно. Очередь добавит инфраструктурной сложности без выигрыша на текущей нагрузке. Триггер пересмотра: lock contention > 10ms p95.

---

## 17. Подсистемы, реализованные сверх исходного дизайна (v6)

Появились в коде после v5 и теперь являются частью контракта:

1. **Temporal knowledge graph** (миграция 009): `fact_relations` с типами
   supersedes / fulfills / contradicts / updates / related; view `current_facts`
   резолвит «текущую истину» цепочки supersession. Ребро `supersedes` создаёт
   Go из `superseded_fact_id` результата upsert'а.
2. **Sleep-time consolidation** (миграция 010): nightly worker (02:00 UTC)
   вызывает `consolidate_facts(tenant)` — чистка expired/superseded, схлопывание
   глубоких цепочек. Там же экспирируются pending-suggestions старше 14 дней.
3. **Embedding worker**: тикер 5 минут + nudge-канал — `saveTypedFacts` будит
   воркер сразу после вставки фактов (ARCH-12), новые факты не ждут тика.
   Write-path дедуп получает embedding кандидата синхронно (батч, 5 с таймаут).
4. **Topic из структурированного draft-ответа** (ARCH-4): `/ai/process-message`
   возвращает `{reply, intent, options, confidence, topic, owner_note}`; topic
   пишется в `active_episodes.current_topic` без отдельного LLM-вызова.
   `/ai/tag-topic` остаётся для эпизодных воркеров.
5. **Runtime state lifecycle** (MEM-5): `open_question_message_id` ставится по
   intent=question/choice_question при decision=draft/ask_owner;
   `pending_owner_action` — по типу решения; оба сбрасываются
   (`ClearEpisodeRuntimeState`) при owner-ответе или успешной отправке.
6. **Детерминированный rerank** (ARCH-5): финальное упорядочивание фактов —
   косинус в SQL по fused-кандидатам (`RankFactsByCosine`), hard-include не
   вытесняются; listwise-LLM `/ai/rerank-facts` из горячего пути убран
   (эндпоинт жив до cutover-замера).
7. **Webhook-очередь** (ARCH-7): приём и обработка разделены — HTTP ACK сразу,
   обработка в шардированном по чату пуле воркеров (порядок per chat);
   fire-and-forget LLM-задачи (extraction, summarize) — через общий семафор.

---

## Appendix A. Глоссарий
- **User layer / Per-chat identity / Working / Runtime state / Episodic / Semantic / AI Roles** — слои памяти (§2).
- **Scope (chat)** — границы видимости факта; единственный scope с v5.
- **AI-роль** — описанная владельцем персона, от лица которой ИИ отвечает в чате (§13).
- **Дефолтная персона** — fallback без назначенной роли: "ассистент владельца, пиши в его стиле".
- **Dedup-on-write** — проверка дубликата перед insert (advisory lock + UNIQUE catch-net).
- **Supersede** — замена старого факта новым.
- **Hard-include** — taboo + health всегда в промпте (в пределах своего чата).
- **Suggestion** — pending изменение в profile.
- **Active episode** — открытый, не закрытый в summary эпизод.
- **Derived view** — SQL function поверх messages для производных состояний.

## Appendix B. Не делать
- ❌ Возвращать кросс-чатную (общую) память в любом виде — включая owner_global.
- ❌ Возвращать глобальный стиль владельца.
- ❌ Денормализовать "греетинги / паузы / pending" в булевы.
- ❌ Позволять AI создавать или менять `ai_roles` — только владелец.
- ❌ Привязывать semantic_facts к роли вместо чата.
- ❌ Создавать пустые таблицы "на будущее".
- ❌ Прямые INSERT в `semantic_facts` минуя stored proc.
- ❌ Маркетплейс ролей в текущей итерации (Iteration 2).
- ❌ Повышать default mode до auto для новых чатов.
- ❌ LLM решает о scope.
