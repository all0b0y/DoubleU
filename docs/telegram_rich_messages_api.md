# Telegram Bot API 10.1 — Rich Messages (vendored reference)

> Офлайн-референс для Codex. Источник: https://core.telegram.org/bots/api (раздел
> Rich Messages, API 10.1, 2026-06-11). Выгружено 2026-06-17.
>
> **Решение по архитектуре: ВАРИАНТ A** — отправка идёт СТРОКОЙ (`markdown`/`html`),
> Telegram парсит блоки на сервере. Поэтому для фазы 1 Go НЕ строит дерево
> `RichBlock`/`RichText` — просто передаёт CommonMark в `InputRichMessage.markdown`.
> Структурные типы ниже (раздел 3) — справочник для приёма сообщений и фазы 2
> (стриминг). Реализовывать их Go-структурами в фазе 1 НЕ нужно.

---

## 1. Отправка — то, что нужно фазе 1

### method sendRichMessage
Отправляет rich-сообщение.
⚠️ ТОЧНАЯ ТАБЛИЦА ПАРАМЕТРОВ НЕ ВЫГРУЖЕНА — подтвердить с https://core.telegram.org/bots/api#sendrichmessage.
Ожидаемые параметры (по аналогии с sendMessage; подтвердить имена):
- `chat_id` (Integer | String, required)
- `rich_message` (InputRichMessage, required)
- `business_connection_id` (String, optional)
- `message_thread_id` (Integer, optional)
- `reply_parameters` (ReplyParameters, optional)
- `reply_markup` (InlineKeyboardMarkup, optional)

### InputRichMessage (ВАРИАНТ A — подтвердить точные имена/флаги)
Описывает отправляемое rich-сообщение. Отправляется СТРОКОЙ:
- `markdown` (String, optional) — текст в формате Markdown; сервер парсит в блоки.
- `html` (String, optional) — альтернатива: текст в HTML.
- `is_rtl` (Boolean, optional) — направление справа-налево. ⚠️ источник сторонний, подтвердить.
- `skip_entity_detection` (Boolean, optional) — отключить авто-распознавание сущностей. ⚠️ подтвердить.

> ⚠️ ДЛЯ CODEX: таблицы `sendRichMessage` и `InputRichMessage` помечены «подтвердить».
> Если при реализации обнаружишь расхождение имён полей с этим файлом — ОСТАНОВИСЬ и
> сообщи человеку. Не выдумывай. Достаточно полей `chat_id` + `rich_message.markdown`
> (+ business_connection_id / reply_parameters для нашего пути) — остальное опционально.

### editMessageText.rich_message
Добавлен параметр `rich_message` (InputRichMessage) для редактирования rich-сообщений. (Фаза 2.)

### Какой markdown принимает сервер (ПОДТВЕРЖДЕНО доками)
Telegram различает два уровня форматирования, оба рендерятся нативно во всех клиентах:
- **Rich Messages** — структурный контент. Принимают **Rich Markdown** и **Rich HTML**.
  **Rich Markdown = GitHub Flavored Markdown (GFM)**, где возможно, плюс допускает
  вставку поддерживаемых HTML-тегов прямо в тот же текст. Это наш путь: модель отдаёт
  GFM → строка в `InputRichMessage.markdown` → сервер парсит блоки.
- **Regular Messages** — MarkdownV2 / HTML (классический `parse_mode`). Это наш крайний
  fallback.

Вывод: модель генерирует **GFM**, таблицы/списки/заголовки/код/цитаты рендерятся
нативно. Интерактивное демо: @RichTextDemoBot. Полная грамматика синтаксиса — по ссылке
«full syntax grammar with examples» в доке. **Ручной тест на образце** (таблица + список
+ заголовок + код) всё равно прогнать перед закрытием задачи.

---

## 2. RichBlockThinking — только для drafts (фаза 2)
`RichBlockThinking` («Thinking…» placeholder, тег `<tg-thinking>`) можно использовать
ТОЛЬКО в `sendRichMessageDraft`, в обычных сообщениях не принимается. В фазе 1 не нужен.

---

## 3. Структурное дерево (справочник: приём сообщений + фаза 2)

### RichText
Либо `String` (plain), либо `Array of RichText`, либо один из типизированных узлов.
Большинство inline-узлов имеют форму `{ type: String, text: RichText }`:
`bold, italic, underline, strikethrough, spoiler, subscript, superscript, marked, code`.
Узлы с доп. полями:
- `date_time` — `{type, text, unix_time:Integer, date_time_format:String}`
- `text_mention` — `{type, text, user:User}`
- `custom_emoji` — `{type, custom_emoji_id:String, alternative_text:String}`
- `mathematical_expression` — `{type, expression:String (LaTeX)}`
- `url` — `{type, text, url:String}`
- `email_address` — `{type, text, email_address:String}`
- `phone_number` — `{type, text, phone_number:String}`
- `bank_card_number` — `{type, text, bank_card_number:String}`
- `mention` — `{type, text, username:String}`
- `hashtag` — `{type, text, hashtag:String}`
- `cashtag` — `{type, text, cashtag:String}`
- `bot_command` — `{type, text, bot_command:String}`
- `anchor` — `{type, name:String}`
- `anchor_link` — `{type, text, anchor_name:String}`
- `reference` — `{type, text, name:String}`
- `reference_link` — `{type, text, reference_name:String}`

### RichBlock (тип определяется полем `type`)
- `paragraph` — `{type, text:RichText}` (HTML `<p>`)
- `heading` — `{type, text:RichText, size:Integer 1-6}` (`<h1..h6>`, 1 — крупнейший)
- `pre` — `{type, text:RichText, language?:String}` (`<pre><code>`)
- `footer` — `{type, text:RichText}` (`<footer>`)
- `divider` — `{type}` (`<hr/>`)
- `mathematical_expression` — `{type, expression:String (LaTeX)}` (`<tg-math-block>`)
- `anchor` — `{type, name:String}`
- `list` — `{type, items:Array of RichBlockListItem}` (`<ul>`/`<ol>`)
- `blockquote` — `{type, blocks:Array of RichBlock, credit?:RichText}`
- `pullquote` — `{type, text:RichText, credit?:RichText}` (`<aside>`)
- `table` — `{type, cells:Array of Array of RichBlockTableCell, is_bordered?:True, is_striped?:True, caption?:RichText}`
- `details` — `{type, summary:RichText, blocks:Array of RichBlock, is_open?:True}` (`<details>`)
- медиа/прочее (фаза 2): `collage, slideshow, map, animation, audio, photo, video, voice_note, thinking`

### RichBlockListItem
`{label:String, blocks:Array of RichBlock, has_checkbox?:True, is_checked?:True,
value?:Integer, type?:String ("a"|"A"|"i"|"I"|"1")}`

### RichBlockTableCell
`{text?:RichText, is_header?:True, colspan?:Integer, rowspan?:Integer,
align:String ("left"|"center"|"right"), valign:String ("top"|"middle"|"bottom")}`

### RichBlockCaption
`{text:RichText, credit?:RichText}` (`<cite>`)
