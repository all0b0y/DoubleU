# Telegram Rich-форматирование ответов AI-агентов — план реализации (для Codex)

> Исполнитель: **OpenAI Codex**. Дата: 2026-06-17.
> API: **Telegram Bot API 10.1 (2026-06-11), Rich Messages.**
> **Архитектура — ВАРИАНТ A:** отправка идёт СТРОКОЙ (`InputRichMessage.markdown`),
> Telegram парсит блоки (таблицы/списки/заголовки/код/цитаты/math) на сервере.
> Go НЕ строит дерево `RichBlock` — это упрощает задачу до 3 срезов.
>
> Адаптация под Codex: офлайн-песочница, риск галлюцинаций нового API, дрейф на
> крупных задачах. Поэтому API-схема вендорится в репо, работа нарезана на срезы,
> между срезами — прогон тестов. **Один срез за запуск.**

---

## 0. Пре-реквизит (человек, ДО запуска Codex)

API новее обучающих данных Codex; интернета в песочнице нет. Вся нужная схема —
в `docs/reference/telegram_rich_messages_api.md` (уже вендорена). Codex берёт имена полей
ТОЛЬКО оттуда; нет поля — СТОП и вопрос человеку, не выдумывать.

Осталось подтвердить одну таблицу: **`sendRichMessage` + `InputRichMessage`** (раздел 1
референса помечен «подтвердить»). Достаточно полей `chat_id` + `rich_message.markdown`
(+ `business_connection_id`, `reply_parameters` для нашего пути).

---

## 1. Цель

AI-агент: (1) отдаёт ответ с разметкой; (2) сам решает, когда форматировать;
(3) доставляет без потерь через `sendRichMessage` (нативные таблицы/списки/заголовки);
(4) подчиняется master-тумблеру в Mini App, **по умолчанию ВКЛ**.

---

## 2. Пути доставки и приоритет

| Приоритет | Путь | Что умеет |
|---|---|---|
| **Основной** | `sendRichMessage` с `InputRichMessage{markdown}` | таблицы, заголовки, списки, code, цитаты, math — парсит сервер |
| Фаза 2 (НЕ делать) | `sendRichMessageDraft` (+ структурные блоки, `RichBlockThinking`) | стриминг живой генерации |
| **Крайний fallback** | `sendMessage` + `parse_mode=HTML` | только `MessageEntity`; срабатывает лишь при сбое rich-отправки |

Основной канал всегда `sendRichMessage`. HTML — самый крайний вариант, никогда не первым.

---

## 3. Формат текста модели

Модель отдаёт **GitHub Flavored Markdown (GFM)** — это официально поддерживаемый
«Rich Markdown» для Rich Messages (подтверждено доками). Бэкенд кладёт его строкой в
`InputRichMessage.markdown` и шлёт `sendRichMessage`; сервер парсит блоки сам. Дерево
блоков вручную не строим. Перед закрытием задачи — ручной тест на образце (заголовок +
список + GFM-таблица + код + цитата): убедиться, что рендер нативный. Демо: @RichTextDemoBot.

---

## 4. Политика «когда форматировать» (Python)

В `ai-python/ai_agent/pipelines/draft.py`, на основе персоны (`_format_persona_section`) и интента:
- роль / ассистент / FAQ / структурный ответ (цены, шаги, код, перечни) → `text_format="rich"`;
- фолбэк «цифровая копия владельца» (имитация живого человека) → `text_format="plain"`;
- короткие реплики / `intent == "choice_question"` / приветствия → `text_format="plain"`.

Правило в системный промпт (RU): «Разметка — инструмент читаемости, не украшение.
Списки/таблицы/заголовки — только для структурных ответов. В живой личной переписке
не используй разметку.»

---

## 5. Master-тумблер в UI (по умолчанию ВКЛ)

Master-gate в Go (Python о тумблере не знает). Шаблон — существующий `learning_enabled`.
- Go `contracts.UserSettings` (`backend-go/internal/contracts/types.go:426`):
  `RichFormattingEnabled bool `json:"rich_formatting_enabled"``.
- Store (`backend-go/internal/store/store.go`, рядом со стр. 3818): колонка в SELECT/INSERT
  `UserSettings`/`SaveUserSettings`.
- **Дефолт ON — ловушка:** zero-value `bool` в Go = `false`. Обеспечь дефолт `true`
  для новых/старых записей (миграция `DEFAULT TRUE` и/или дефолт при чтении). Тест.
- Mini App (`miniapp/screens/me.js`, блок `f-learning` стр. 160-170): тумблер по образцу;
  `rich_formatting_enabled` в payload `saveUserSettings` (~стр. 259); отсутствие → `true`.

---

## 6. Изменения контракта (обе стороны — требование CLAUDE.md `/contracts`)

- `contracts/openapi.yaml`: в `AIProcessMessageResponse` добавить
  `text_format: { type: string, enum: [plain, rich], default: plain }`.
- `ai-python/ai_agent/schemas.py`: `AIProcessMessageResponse.text_format: str = "plain"`
  + проброс из `DraftOutcome`.
- Go-декодер ответа AI-сервиса: добавить `TextFormat`, прокинуть в запись черновика и отправку.
- Back-compat: старый ответ без поля → `plain`.

---

## 7. Реализация — 3 СРЕЗА. Один срез за запуск. После каждого: тесты зелёные → СТОП.

### Срез 1 — контракт + политика (Python, изолированно)
1. `contracts/openapi.yaml`: поле `text_format` (разд. 6).
2. `ai-python/ai_agent/schemas.py`: `text_format` в `AIProcessMessageResponse`.
3. `ai-python/ai_agent/pipelines/draft.py`: `text_format` в `DraftOutcome`;
   `decide_text_format(ai_role, intent)`; блок про разметку в системный промпт (разд. 4).
4. Тесты Python (разд. 9). Прогон: `cd ai-python && pytest`. → СТОП.

### Срез 2 — отправка rich (Go)
1. В `backend-go/internal/telegram/telegram.go`: тип `InputRichMessage` (поля из
   референса, раздел 1 — `markdown` обязателен нам) и метод
   `func (s *BotSender) SendRichMessage(ctx, OutboundMessage) (string, error)`,
   который POST-ит `sendRichMessage` с телом
   `{chat_id, rich_message:{markdown:text}, business_connection_id?, reply_parameters?, reply_markup?}`
   (по образцу `sendMessageRaw`). Имена полей строго из референса; нет — СТОП.
2. В `backend-go/internal/core/service.go`, `sendRecipient` (~стр. 2484): убрать хардкод
   `ParseMode:"Markdown"`. Логика канала:
   - тумблер off → принудительно plain (`SendMessage` без `parse_mode`);
   - `TextFormat=="rich"` → `SendRichMessage` (markdown = текст модели); при ошибке →
     крайний fallback: `SendMessage(ParseMode:"HTML")` с тем же текстом;
   - иначе → `SendMessage` без `parse_mode`.
3. `text_format` сохранять вместе с черновиком (review → send не теряет разметку);
   прокинуть формат всем вызывающим `sendRecipient`.
4. Тесты (разд. 9). Прогон: `cd backend-go && go test ./...`. → СТОП.

### Срез 3 — master-тумблер (Go + Mini App)
1. `RichFormattingEnabled` в `contracts.UserSettings` + store + дефолт `true` (разд. 5).
2. Тумблер в `miniapp/screens/me.js` + payload `saveUserSettings`.
3. Подключить gate в `sendRecipient` (если не сделано в срезе 2).
4. Тесты store + service (тумблер off → plain; дефолт true). Прогон: `go test ./...`. → СТОП.

### Фаза 2 (НЕ делать)
`sendRichMessageDraft`, структурное дерево `RichBlock`, `RichBlockThinking`, медиа-блоки,
rich-карточки владельцу.

---

## 8. Безопасность и инварианты (CLAUDE.md)

- Без новых внешних вызовов в конвертации → идемпотентность отправки цела.
- Текст в `markdown` формируется ПОСЛЕ `OutputSafetyScanner` и `deanonymize`
  (`ai-python/ai_agent/safety/output.py`) — деанонимизация плейсхолдеров не ломается
  (мы не меняем текст, лишь меняем канал отправки).
- Ссылки в тексте — Telegram сам валидирует при парсинге; в HTML-fallback экранировать `< > &`.
- Privacy-флаги логику формата не затрагивают.
- Go: `slog`, без `fmt.Println`; ошибки обработаны или `_` с комментарием.
- Python: type-аннотации; `frozen=True` dataclasses; без bare `except`.

---

## 9. Тесты (functional, против контракта — не подгонять под код)

- **Python**: `decide_text_format` (`plain` для копии/коротких, `rich` для роли);
  проброс `text_format` в ответ; back-compat (старый ответ без поля → `plain`).
- **Go telegram**: `SendRichMessage` шлёт корректное тело (`rich_message.markdown` = текст,
  прокинуты `business_connection_id`/`reply_parameters`); ошибка транспорта возвращается наверх.
- **Go service**: `rich`→ вызван `SendRichMessage` с текстом модели; ошибка rich → HTML-fallback;
  `plain`→ `SendMessage` без `parse_mode`; тумблер off → plain; формат черновика
  переживает review→send.
- **Go store**: `rich_formatting_enabled` дефолтится в `true` для новых/старых записей.

---

## 10. Критерии приёмки

1. Ответ роли-бота с таблицей/списком/заголовком приходит в Telegram нативно
   отрисованным (Rich Message), не сырым markdown. (Проверено ручным тестом, разд. 3.)
2. Личная переписка-«копия» — без разметки.
3. Тумблер в Mini App выключает rich-форматирование; по умолчанию включён.
4. При сбое rich-отправки — HTML-fallback, сообщение не теряется.
5. Контракт обновлён в `openapi.yaml` + Python + Go; старые клиенты не ломаются.
6. Все тесты зелёные. Найденные неэффективности/долги — в `docs/backlog.md`.

---

## 11. Границы первой итерации

Вне скоупа: стриминг (`sendRichMessageDraft`), структурное дерево `RichBlock`,
`RichBlockThinking`, медиа-блоки, rich-карточки владельцу, per-bot/per-role
гранулярность тумблера. Это фаза 2.
