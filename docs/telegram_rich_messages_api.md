# Telegram Bot API 10.1 — Rich Messages (vendored reference)

> Офлайн-референс для Codex. Источник: https://core.telegram.org/bots/api (раздел
> Rich Messages, API 10.1, 2026-06-11). Полностью подтверждено по официальной доке
> (вставлено человеком 2026-06-18).
>
> **Архитектура отправки = ВАРИАНТ A:** контент передаётся СТРОКОЙ в
> `InputRichMessage.markdown` (или `.html`), Telegram парсит блоки сервером. Go НЕ
> строит дерево `RichBlock` для отправки. Структурные типы (раздел 4) — справочник
> для приёма сообщений; реализовывать их Go-структурами не нужно.

---

## 1. Отправка — `sendRichMessage` (ПОДТВЕРЖДЕНО)

`Use this method to send rich messages. On success, the sent Message is returned.`
Если есть медиа-блок — бот должен иметь право слать медиа в чат.

| Параметр | Тип | Обяз. | Назначение (наш интерес) |
|---|---|---|---|
| `chat_id` | Integer or String | Yes | целевой чат |
| `rich_message` | InputRichMessage | Yes | сам контент |
| `business_connection_id` | String | опц. | для business-аккаунта |
| `message_thread_id` | Integer | опц. | топик форума/приватный с topic mode |
| `direct_messages_topic_id` | Integer | опц. | для direct-messages чатов |
| `reply_parameters` | ReplyParameters | опц. | ответ на сообщение |
| `reply_markup` | InlineKeyboardMarkup и др. | опц. | клавиатура |
| `disable_notification`, `protect_content`, `allow_paid_broadcast`, `message_effect_id`, `suggested_post_parameters` | — | опц. | прочее |

→ Фаза-1 реализация (`chat_id` + `rich_message.markdown` + `business_connection_id` +
`reply_parameters` + inline `reply_markup`) корректна.

### InputRichMessage (ПОДТВЕРЖДЕНО)
`Exactly one of the fields html or markdown must be used.`

| Поле | Тип | Назначение |
|---|---|---|
| `markdown` | String | контент в Rich Markdown (наш путь) |
| `html` | String | контент в Rich HTML (альтернатива) |
| `is_rtl` | Boolean | опц. — показать справа-налево |
| `skip_entity_detection` | Boolean | опц. — отключить авто-детект сущностей (URL/email/@/#/$/команды/телефоны) |

### Лимиты rich-сообщения
- ≤ **32768** UTF-8 символов (вкл. alt-текст эмодзи и исходник формул).
- ≤ **500** блоков (вкл. вложенные, пункты списков, строки таблиц, цитаты, details).
- ≤ **16** уровней вложенности.
- ≤ **50** медиа-вложений. ≤ **20** колонок в таблице.

---

## 2. Стриминг — `sendRichMessageDraft` (ПОДТВЕРЖДЕНО, фаза 3)

`Stream a partial rich message while it is being generated. The streamed draft is
EPHEMERAL and acts as a temporary 30-second preview — once the output is finalized you
MUST call sendRichMessage with the complete message to persist it. Returns True on success.`

| Параметр | Тип | Обяз. | Назначение |
|---|---|---|---|
| `chat_id` | **Integer** | Yes | **только приватный чат** |
| `draft_id` | **Integer** | Yes | ненулевой id черновика; изменения с тем же id **анимируются** |
| `rich_message` | InputRichMessage | Yes | частичный контент |
| `message_thread_id` | Integer | опц. | топик |

**Семантика (критично для реализации):**
- Возвращает `True`, **НЕ** Message и НЕ message_id.
- Черновик живёт ~30 сек как превью; **не персистится сам по себе**.
- Поток = повторные вызовы `sendRichMessageDraft` с ОДНИМ `draft_id` и растущим
  `rich_message` (каждый вызов анимирует изменение).
- **Финализация = вызвать `sendRichMessage`** с полным сообщением (он и возвращает
  persisted `Message` → отсюда реальный message_id).
- Нет полей `business_connection_id` / `reply_parameters` / `reply_markup` — они только
  на финальном `sendRichMessage`. ⇒ **draft-стрим применим только к собственным приватным
  чатам бота, НЕ к business-connection отправкам.**
- `RichBlockThinking` (`<tg-thinking>`) можно использовать ТОЛЬКО в drafts.

### editMessageText.rich_message
Параметр `rich_message: InputRichMessage` добавлен в `editMessageText` (использовался
псевдо-стримом фазы 2). Для фазы 3 (вариант A) не нужен — финал идёт через `sendRichMessage`.

---

## 3. Rich Markdown — синтаксис (то, что отдаёт модель)

Rich Markdown = **GitHub Flavored Markdown** + произвольный HTML (поддерживаемые теги).
Передаётся строкой в `InputRichMessage.markdown`. Опорные конструкции:

- `**bold**`/`__bold__`, `*italic*`/`_italic_`, `~~strike~~`, `` `code` ``, `==marked==`, `||spoiler||`
- `[text](url)`, `[mail](mailto:..)`, `[tel](tel:..)`, `[mention](tg://user?id=..)`
- `# .. ######` заголовки 1-6; `---` divider; абзацы
- ` ```python … ``` ` код с языком
- списки `-`/`*`/`+`/`1.`; задачи `- [ ]` / `- [x]`
- `>` блок-цитаты (многострочные через `>`)
- таблицы GFM `| h | h |` с выравниванием `:--`, `:--:`, `--:`
- формулы `$inline$`, `$$block$$`, ` ```math `
- сноски `[^id]` + `[^id]: def`
- медиа ТОЛЬКО отдельным блоком: `![](http(s)://… "caption")` (тип по MIME/URL)
- HTML-теги для того, чего нет в md: `<u>`,`<ins>`,`<sub>`,`<sup>`,`<details>`,`<aside><cite>`,
  `<tg-map>`,`<tg-collage>`,`<tg-slideshow>`; вложенный markdown парсится внутри
  `<details>`/`<tg-collage>`/`<tg-slideshow>`, в остальных блочных HTML — только HTML.
- авто-детект URL/email/@/#/$/команд/телефонов (отключается `skip_entity_detection`).

> Для нашей фазы 1-3: модель отдаёт обычный GFM-текст, бэкенд кладёт его в `markdown`.
> Медиа/карты/коллажи/details/сноски — вне текущего скоупа (фаза «структурное дерево»).

---

## 4. Структурное дерево (справочник: приём сообщений; реализовывать НЕ нужно)

`RichMessage` = `{ blocks: Array of RichBlock, is_rtl?: Boolean }`.

### RichText
Либо `String`, либо `Array of RichText`, либо типизированный узел. Базовая форма
`{ type, text: RichText }` для: `bold, italic, underline, strikethrough, spoiler,
subscript, superscript, marked, code`. С доп. полями: `date_time {unix_time,
date_time_format}`, `text_mention {user}`, `custom_emoji {custom_emoji_id,
alternative_text}`, `mathematical_expression {expression}`, `url {url}`,
`email_address {email_address}`, `phone_number {phone_number}`, `bank_card_number
{bank_card_number}`, `mention {username}`, `hashtag {hashtag}`, `cashtag {cashtag}`,
`bot_command {bot_command}`, `anchor {name}`, `anchor_link {anchor_name}`,
`reference {name}`, `reference_link {reference_name}`.

### RichBlock (по полю `type`)
`paragraph {text}`, `heading {text, size:1-6}`, `pre {text, language?}`, `footer {text}`,
`divider`, `mathematical_expression {expression}`, `anchor {name}`,
`list {items:[RichBlockListItem]}`, `blockquote {blocks, credit?}`,
`pullquote {text, credit?}`, `table {cells:[[RichBlockTableCell]], is_bordered?,
is_striped?, caption?}`, `details {summary, blocks, is_open?}`, `thinking {text}`
(только в drafts), медиа: `collage/slideshow {blocks, caption?}`, `map {location, zoom,
width, height, caption?}`, `animation/audio/photo/video/voice_note {media, has_spoiler?,
caption?}`.

`RichBlockListItem {label, blocks, has_checkbox?, is_checked?, value?, type?("a"|"A"|"i"|"I"|"1")}`
`RichBlockTableCell {text?, is_header?, colspan?, rowspan?, align(left|center|right), valign(top|middle|bottom)}`
`RichBlockCaption {text, credit?}`
