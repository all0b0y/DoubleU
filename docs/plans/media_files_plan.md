# Media Files Plan — голос, изображения, документы (без видео)

**Executor:** Sonnet 4.6 (coding agent).
**Цель:** научить AI-агента понимать входящие голосовые, изображения и документы.
**Out of scope:** видео и video_note (отдельный план), исходящие медиа, real-time стрим.

## СТАТУС (2026-06-19) — Вариант B выбран владельцем
- ✅ **Slice 0 (Go)** — `BotSender.GetFilePath`/`DownloadFile` (+20 MB cap, seam `apiBase`),
  `MediaDescriptor`+`extractMedia`. 7 тестов. Весь бэкенд зелёный (146 тестов / 12 пакетов).
- ✅ **Slice 1 (Python `/ai/extract-media`)** — `pipelines/media.py` (STT/vision через urllib +
  локальные парсеры документов, MOCK seam, graceful degrade), схемы `ExtractMedia*`+parse,
  `service.extract_media`, route/handler/purpose, base64 редактируется в логах, deps в
  requirements. 11 media-тестов + route-shape тест. Локально 94 passed (pydantic_ai-тесты идут в CI).
  Контракт `process-message` НЕ тронут; aux-endpoint в openapi.yaml не нужен (как embed-texts).
- ✅ **Slice 2 (Go wiring)** — фича работает end-to-end. `telegram.FileDownloader` +
  `MediaFromBotUpdate`; `aiclient.MediaExtractor`+`ExtractMedia` (60s timeout); `Handler`/
  `HandlerFunc` несут `*MediaDescriptor` (поллер парсит); main-замыкание зовёт
  `Service.EnrichBotMedia` перед `HandleBotMessage`. `EnrichBotMedia`: скачивание per-bot токеном
  через `SenderFor`→`FileDownloader`, таймаут, **graceful placeholder** при ошибке (бот не молчит),
  merge caption+транскрипт, сырые байты не персистятся. 5 новых тестов. **151 тест зелёный, -race чистый.**
- ✅ **Slice 3 (тоглы/лимиты)** — `MediaConfig`{Enabled, MaxBytes 20MB, MaxAudioBytes 10MB,
  MaxAudioSeconds 300, RatePerSec/Burst} + `DefaultMediaConfig` + `WithMediaConfig`. Пре-чеки в
  `EnrichBotMedia` (`mediaRejectReason`): отключено/слишком большое/длинное/rate → placeholder БЕЗ
  скачивания и вызова провайдера. Per-tenant media rate-лимитер (`mediaLimiter`, как `tenantLimiter`).
  Env kill-switch `MEDIA_EXTRACTION_ENABLED` (default on) в config.go → main.go. 4 новых теста.
  **155 тестов зелёные, -race + vet чистые.** Сырые байты не персистятся (со Slice 2).
- ⏳ **Deferred:** per-bot/per-role тогл (нужны миграция + API + Mini App — отдельный слайс,
  глобальный kill-switch уже закрывает cost-control). Числовые лимиты пока не из env (YAGNI —
  есть `WithMediaConfig`). Видео — отдельный план.

## ВСЯ ФИЧА ГОТОВА (Slices 0–3). Не закоммичено. Что нужно для прода:
1. `OPENROUTER_API_KEY` — модели qwen ASR/VL уже дефолтны.
2. Прогнать CI (pydantic_ai-тесты идут только там, локально 94 passed).
3. Живой тест с реальным голосом/фото через бот (всё тестировалось на MOCK).

**ПРОВАЙДЕРЫ (owner override): всё через OpenRouter.** STT `qwen/qwen3-asr-flash-2026-02-10`
(endpoint `/audio/transcriptions`, JSON `input_audio`), vision `qwen/qwen-2.5-vl-7b-instruct`
(`/chat/completions`). Один ключ `OPENROUTER_API_KEY`. Модели override через STT_MODEL/VISION_MODEL.

---

## 0. Ключевое архитектурное решение: «media → text» в транспортном слое

Агент остаётся **текстовым**. Любое медиа конвертируется в текст ДО основного контракта
`/ai/process-message`, и результат кладётся в `event.MessageText`. Тогда драфт, память,
эмбеддинги и BM25 работают без изменений — медиа прозрачно ровно как bot-namespaced `chat_id`
из мультибота.

Почему так (а не мультимодальный вызов через OpenRouter):
- OpenRouter-роутинг на vision «не всегда гладкий» (слова владельца) → выносим vision/STT на
  дешёвые специализированные провайдеры, а дорогой Sonnet 4.6 видит только текст.
- Контракт `/ai/process-message` заморожен — не трогаем его, добавляем отдельный endpoint.
- `event.MessageText` уже течёт сквозь весь пайплайн (`service.go:829, 871, 1019, 1203, 1219`).

### Где живёт извлечение текста — РЕШЕНИЕ ВЛАДЕЛЬЦА (по умолчанию: Вариант B)

- **Вариант B (рекомендуется):** новый stateless Python endpoint `/ai/extract-media`.
  Go скачивает байты (у Go есть токен бота), шлёт base64 → Python возвращает чистый текст +
  provenance-тег. Vision/STT-клиенты живут в Python, где уже есть провайдеры и `MOCK_LLM`.
  Аналогично тому, как для рефлексии (#5) добавили stateless `/ai/synthesize-pattern`.
- **Вариант A (ship-fast):** Go зовёт Groq/Gemini напрямую. Ноль изменений контракта и Python,
  но ML-glue (multipart upload, ретраи, OCR) переезжает в Go — менее естественно.

Дальнейшие слайсы написаны под **Вариант B**. Если владелец выберет A — Slice 1 переезжает в Go.

### Безопасность (binding, из CLAUDE.md)
- Транскрипт/OCR — это **недоверенный пользовательский ввод**. Картинка может содержать
  prompt-injection текстом («ignore previous instructions»). Извлечённый текст обязан проходить
  то же XML-оборачивание, что и обычный `message_text` (он и проходит — общий путь). Добавить
  нейтральный provenance-маркер, НЕ давать OCR-тексту трактоваться как инструкция.
- Privacy: `no_memory`/`private` → не сохранять транскрипт как факт (уже гейтится тем, что текст
  идёт общим путём, `service.go:1019`). **Сырые байты медиа не персистить нигде.** `no_training`
  → не класть в обучающий корпус.

---

## 1. Выбор провайдеров (дёшево + надёжно, минимум зависимости от OpenRouter)

> Цены актуальны на 2026-06; перед вшиванием — проверить live (привычка проекта).

### Голос (`voice`, `audio`, `video_note`-аудиодорожка опционально)
- **Telegram-нативной транскрипции для бота НЕТ.** `messages.transcribeAudio`
  (`core.telegram.org/api/transcribe`) — это **MTProto Client API**, требует юзер-сессию и
  Telegram Premium (2 конвертации/нед бесплатно). По `CLAUDE.md` MTProto — отложенная Iteration 2.
  → закрытый путь для бота, не использовать.
- **Выбор: Groq `whisper-large-v3-turbo`** — ~$0.04/час (≈89% дешевле OpenAI $0.36/час), ~228x
  realtime. OpenAI-совместимый API → ложится на существующий `OpenAIChatModel`/provider-паттерн
  (другой `base_url` + ключ).
- Fallback: локальный `faster-whisper` (бесплатно, нужен CPU/GPU) или OpenAI `whisper-1`.
- Формат Telegram voice = OGG/OPUS — Groq/Whisper принимают напрямую, перекодировка не нужна.

### Изображения (`photo`) — двухуровнево
- **Уровень 1 (текст на картинке: скриншоты, ошибки, чеки, документы-фото):** локальный **OCR
  Tesseract** (или PaddleOCR) — бесплатно, без API. Большинство «постов» в саппорт — это
  скриншоты текста.
- **Уровень 2 (нужно понять сцену/контент):** **Gemini 2.5 Flash-Lite** — ~$0.10/MTok input
  (free tier ~1000 req/день), прямой Google-эндпоинт → мимо OpenRouter-флакости. Возвращает
  caption + извлечённый текст одним промптом.
- Fallback: `qwen/qwen-2.5-vl-7b-instruct` через OpenRouter (ключ уже есть).
- Роутинг уровней: эвристика «много ли текста» (Tesseract confidence / доля непустых слов).
  Для v1 допустимо просто всегда звать Gemini Flash-Lite с промптом «опиши + извлеки весь текст»,
  а Tesseract включить как удешевление позже.

### Документы (`document`: pdf, docx, txt, csv) — без модели, бесплатно
- getFile → скачать → извлечь текст: PDF (`pdftotext`/`pdfminer.six`), DOCX (`python-docx`),
  txt/csv (прямое чтение в UTF-8). Никакого LLM — чистый парсинг.
- Большие/бинарные/неподдерживаемые → graceful skip с пометкой.

### Лимиты Telegram Bot API (binding)
- `getFile` для ботов отдаёт файлы **≤ 20 MB**. Больше — скачать нельзя → graceful fallback
  («файл слишком большой, опишите текстом»). Длинные voice обычно укладываются.

---

## 2. Слайсы реализации (test-gated, каждый зелёный перед следующим)

### Slice 0 — Go: примитив скачивания + дескрипторы медиа
**Файлы:** `backend-go/internal/telegram/telegram.go`, `telegram_test.go`
- Добавить на `BotSender` методы:
  - `GetFilePath(ctx, fileID) (string, error)` — вызов `getFile`, вернуть `result.file_path`.
  - `DownloadFile(ctx, filePath) ([]byte, string, error)` — GET
    `https://api.telegram.org/file/bot{token}/{file_path}`, вернуть байты + mime, лимит 20 MB.
  - Использовать существующий `s.token`/`s.client`; пустой токен → ошибка (как в `post`, `:241`).
- В `telegramMessage` (`:695`) распарсить нужные подполя из `json.RawMessage`: `file_id`,
  `mime_type`, `file_size`, `duration` (для voice), у `photo[]` — взять **последний** элемент
  (наибольшее разрешение).
- Новая структура `MediaDescriptor{Kind, FileID, MimeType, FileSize, Duration}` и функция
  `extractMedia(*telegramMessage) *MediaDescriptor` (nil если медиа нет). `detectMessageType`
  (`:729`) переиспользовать для `Kind`.
- Тесты: парсинг photo/voice/document из реальных webhook-фикстур; выбор max-photo; nil для
  чистого текста; лимит размера.

### Slice 1 — Python: stateless endpoint `/ai/extract-media`
**Файлы:** `ai-python/ai_agent/http_server.py`, `schemas.py`, `agents/models.py`,
новый `ai-python/ai_agent/pipelines/media.py`, тесты в `ai-python/tests/`
- Schema (frozen dataclass): `ExtractMediaRequest{request_id, kind, mime_type, data_base64,
  duration?}` → `ExtractMediaResponse{text, provenance, model_used, chars}`.
- Хендлер мирроит существующие (`/ai/process-message`, `/ai/synthesize-pattern`): валидация на
  границе, без состояния между запросами.
- `media.py` диспетчеризует по `kind`:
  - `voice`/`audio` → STT-провайдер (Groq Whisper, OpenAI-совместимый клиент; конфиг через env
    `STT_API_KEY`/`STT_MODEL`/`STT_BASE_URL`, по аналогии с `models.py`).
  - `photo` → vision-провайдер (Gemini Flash-Lite via `GEMINI_API_KEY`/`VISION_MODEL`; fallback
    Qwen-VL через OpenRouter). Промпт: «Опиши изображение и дословно извлеки весь текст».
  - `document` → парсеры (pdf/docx/txt/csv), без модели.
- `MOCK_LLM=true` → детерминированный фейк-текст (как у остального пайплайна) для тестов/CI.
- Каждый ответ помечается `provenance` (`voice_transcript|image_ocr|image_caption|document_text`).
- Тесты: каждый `kind` под MOCK; неподдерживаемый mime → понятная ошибка; пустой результат.

### Slice 2 — Go: стадия обогащения между normalize и ProcessMessage
**Файлы:** `backend-go/internal/core/service.go`, новый клиентский метод в `aiclient`,
`service_test.go`
- После `NormalizeBotUpdate`, внутри `runAsyncLLMTask` (`:463`), ПЕРЕД построением запроса
  (`:829`): если у события есть `MediaDescriptor` и `MessageText` пустой/только caption →
  1. `SenderFor(tenantID, botID)` → правильный токен бота (мультибот, уже есть).
  2. `GetFilePath` + `DownloadFile` (Slice 0).
  3. `aiClient.ExtractMedia(ctx, base64, kind, …)` → текст (Slice 1).
  4. `event.MessageText = provenanceWrap(caption, extractedText)` — например
     `"<voice_transcript>…</voice_transcript>"` + исходный caption, если был.
- Таймаут на download+extract; при ошибке/таймауте — graceful fallback: оставить
  `message_type`-плейсхолдер (`"[голосовое сообщение]"`/`"[изображение]"`), чтобы бот всё равно
  ответил/уведомил, а не молчал. Логировать через `slog`, без сырых байтов в логах.
- Идемпотентность: download+extract — часть обработки одного `source_message_id`; повторный
  webhook не должен скачивать дважды (опереться на существующий idempotency-гейт обработки).
- Тесты: media-событие → `MessageText` обогащается до ProcessMessage; ошибка extract →
  fallback-плейсхолдер; privacy `no_memory` → транскрипт не уходит в факты.

### Slice 3 — Тоглы, лимиты, стоимость
**Файлы:** конфиг бота/роли, `service.go`, тесты
- Пер-бот/пер-роль тогл «обрабатывать медиа» (по умолчанию ON для voice+image+doc).
- Жёсткие лимиты: размер файла (≤20 MB hard, мягкий конфиг-кап), длительность voice (напр. ≤10
  мин), число медиа-сообщений в минуту на тенанта (переиспользовать fair-share лимитер #6).
- Никогда не персистить сырые байты. Транскрипт/OCR хранится только как обычный текст сообщения
  под общими privacy-флагами.
- Тесты: превышение лимитов → graceful skip; тогл OFF → медиа игнорируется с плейсхолдером.

---

## 3. Контракт и зависимости
- `/ai/process-message` — **без изменений** (медиа прозрачно).
- Новый `/ai/extract-media` — аддитивный, описать в `contracts/openapi.yaml` + `schemas.py` +
  Go-клиент (правило frozen-контракта: обе стороны).
- Python deps: STT-клиент (openai-совместимый, уже есть), `google-genai` (или Gemini через
  OpenAI-совм. слой), `pdfminer.six`, `python-docx`, опц. `pytesseract`+Tesseract бинарь в
  Docker-образ (`Dockerfile.ai-python`).
- Go deps: ничего нового (stdlib `net/http` + `encoding/json`).

## 4. Оценка
- Slice 0+2 (Go): ~1–1.5 дня. Slice 1 (Python): ~1.5–2 дня. Slice 3: ~0.5–1 день.
- Стоимость в проде (порядок): voice ~$0.04/час; image ~$0.10/MTok (или $0 на Tesseract);
  doc — $0. Дорогой Sonnet 4.6 видит только текст → без vision-наценки.

## 5. Открытые вопросы для владельца
1. Вариант B (Python endpoint) или A (Go напрямую)?
2. Для картинок — сразу двухуровнево (Tesseract + Gemini) или v1 только Gemini Flash-Lite?
3. STT: Groq (дёшево, внешний) или локальный faster-whisper (бесплатно, нужен compute в образе)?
