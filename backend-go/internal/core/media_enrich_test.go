package core

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

// mediaAI implements the base Client plus the optional MediaExtractor capability.
type mediaAI struct {
	text     string
	extErr   error
	extCalls int
	lastReq  aiclient.ExtractMediaRequest
}

func (m *mediaAI) ProcessMessage(_ context.Context, _ contracts.AIProcessMessageRequest) (contracts.AIProcessMessageResponse, error) {
	return contracts.AIProcessMessageResponse{}, nil
}

func (m *mediaAI) ExtractMedia(_ context.Context, req aiclient.ExtractMediaRequest) (aiclient.ExtractMediaResponse, error) {
	m.extCalls++
	m.lastReq = req
	if m.extErr != nil {
		return aiclient.ExtractMediaResponse{}, m.extErr
	}
	return aiclient.ExtractMediaResponse{Text: m.text, Provenance: "voice_transcript", ModelUsed: "test"}, nil
}

// downloaderSender satisfies telegram.Sender (via the embedded *fakeSender) plus
// telegram.FileDownloader so EnrichBotMedia can fetch attachment bytes in tests.
type downloaderSender struct {
	*fakeSender
	data       []byte
	downMime   string
	getPathErr error
	downErr    error
}

func (d *downloaderSender) GetFilePath(_ context.Context, _ string) (string, error) {
	if d.getPathErr != nil {
		return "", d.getPathErr
	}
	return "voice/file_1.oga", nil
}

func (d *downloaderSender) DownloadFile(_ context.Context, _ string) ([]byte, string, error) {
	if d.downErr != nil {
		return nil, "", d.downErr
	}
	mime := d.downMime
	if mime == "" {
		mime = "audio/ogg"
	}
	return d.data, mime, nil
}

// mediaSenderProvider resolves every tenant/bot to one downloader-capable sender.
type mediaSenderProvider struct{ sender telegram.Sender }

func (m mediaSenderProvider) SenderFor(_, _ string) (telegram.Sender, bool) {
	return m.sender, m.sender != nil
}

func voiceUpdatePayload(updateID, chatID, messageID int64) []byte {
	return []byte(`{
		"update_id": ` + intString(updateID) + `,
		"message": {
			"message_id": ` + intString(messageID) + `,
			"chat": {"id": ` + intString(chatID) + `, "type": "private", "first_name": "Контакт"},
			"from": {"id": ` + intString(chatID) + `},
			"voice": {"file_id": "VOICEFID", "mime_type": "audio/ogg", "duration": 4},
			"date": 1700000000
		}
	}`)
}

func voiceUpdateSized(updateID, chatID, messageID, fileSize, duration int64) []byte {
	return []byte(`{
		"update_id": ` + intString(updateID) + `,
		"message": {
			"message_id": ` + intString(messageID) + `,
			"chat": {"id": ` + intString(chatID) + `, "type": "private", "first_name": "Контакт"},
			"from": {"id": ` + intString(chatID) + `},
			"voice": {"file_id": "VOICEFID", "mime_type": "audio/ogg", "file_size": ` + intString(fileSize) + `, "duration": ` + intString(duration) + `},
			"date": 1700000000
		}
	}`)
}

func photoWithCaptionPayload(updateID, chatID, messageID int64, caption string) []byte {
	return []byte(`{
		"update_id": ` + intString(updateID) + `,
		"message": {
			"message_id": ` + intString(messageID) + `,
			"chat": {"id": ` + intString(chatID) + `, "type": "private", "first_name": "Контакт"},
			"from": {"id": ` + intString(chatID) + `},
			"photo": [{"file_id":"SMALL","file_size":100},{"file_id":"BIG","file_size":900}],
			"caption": "` + caption + `",
			"date": 1700000000
		}
	}`)
}

func mediaTestService(ai aiclient.Client, sender telegram.Sender) *Service {
	return NewService(store.NewRepository(), ai, &fakeSender{},
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithBotSenderProvider(mediaSenderProvider{sender: sender}),
	)
}

func botMediaInputs(t *testing.T, raw []byte) (contracts.NormalizedTelegramEvent, *telegram.MediaDescriptor) {
	t.Helper()
	event, ok, err := telegram.NormalizeBotUpdate(raw, 555, "tenant:user:77", time.Unix(1700000000, 0).UTC())
	if err != nil || !ok {
		t.Fatalf("normalize: ok=%v err=%v", ok, err)
	}
	media, err := telegram.MediaFromBotUpdate(raw)
	if err != nil {
		t.Fatalf("media parse: %v", err)
	}
	return event, media
}

// TestEnrichBotMediaVoiceBecomesTranscript verifies a voice message is downloaded
// through the message's bot and replaced with the transcript before AI processing.
func TestEnrichBotMediaVoiceBecomesTranscript(t *testing.T) {
	ai := &mediaAI{text: "привет это голос"}
	sender := &downloaderSender{fakeSender: &fakeSender{}, data: []byte("oggbytes")}
	service := mediaTestService(ai, sender)

	event, media := botMediaInputs(t, voiceUpdatePayload(9500, 42, 20))
	enriched := service.EnrichBotMedia(context.Background(), event, media)

	if ai.extCalls != 1 {
		t.Fatalf("ExtractMedia calls = %d, want 1", ai.extCalls)
	}
	if !strings.Contains(enriched.MessageText, "привет это голос") {
		t.Fatalf("MessageText = %q", enriched.MessageText)
	}
	if ai.lastReq.Kind != "voice" || ai.lastReq.MimeType != "audio/ogg" || ai.lastReq.Duration != 4 {
		t.Fatalf("extract request = %+v", ai.lastReq)
	}
	if ai.lastReq.DataBase64 != base64.StdEncoding.EncodeToString([]byte("oggbytes")) {
		t.Fatalf("data_base64 = %q", ai.lastReq.DataBase64)
	}
}

// TestEnrichBotMediaPhotoWithCaptionMerges verifies a photo caption and the OCR
// text are combined so neither is lost.
func TestEnrichBotMediaPhotoWithCaptionMerges(t *testing.T) {
	ai := &mediaAI{text: "на фото счёт"}
	sender := &downloaderSender{fakeSender: &fakeSender{}, data: []byte("jpg"), downMime: "image/jpeg"}
	service := mediaTestService(ai, sender)

	event, media := botMediaInputs(t, photoWithCaptionPayload(9503, 42, 23, "вот мой чек"))
	enriched := service.EnrichBotMedia(context.Background(), event, media)

	if media.Kind != "photo" || media.FileID != "BIG" {
		t.Fatalf("descriptor = %+v (want largest photo size)", media)
	}
	if !strings.Contains(enriched.MessageText, "вот мой чек") || !strings.Contains(enriched.MessageText, "на фото счёт") {
		t.Fatalf("MessageText = %q (want caption + ocr)", enriched.MessageText)
	}
}

// TestEnrichBotMediaFailureFallsBackToPlaceholder verifies an extraction error
// leaves a placeholder so the bot still acknowledges the message.
func TestEnrichBotMediaFailureFallsBackToPlaceholder(t *testing.T) {
	ai := &mediaAI{extErr: errors.New("provider down")}
	sender := &downloaderSender{fakeSender: &fakeSender{}, data: []byte("x")}
	service := mediaTestService(ai, sender)

	event, media := botMediaInputs(t, voiceUpdatePayload(9501, 42, 21))
	enriched := service.EnrichBotMedia(context.Background(), event, media)

	if !strings.Contains(enriched.MessageText, "не удалось обработать") {
		t.Fatalf("expected placeholder, got %q", enriched.MessageText)
	}
}

// TestEnrichBotMediaNoDownloaderFallsBack verifies a sender that cannot download
// (no FileDownloader) degrades to a placeholder rather than crashing.
func TestEnrichBotMediaNoDownloaderFallsBack(t *testing.T) {
	ai := &mediaAI{text: "unused"}
	service := mediaTestService(ai, &fakeSender{}) // plain sender: no FileDownloader

	event, media := botMediaInputs(t, voiceUpdatePayload(9504, 42, 24))
	enriched := service.EnrichBotMedia(context.Background(), event, media)

	if ai.extCalls != 0 {
		t.Fatalf("ExtractMedia should not be called without a downloader, calls=%d", ai.extCalls)
	}
	if !strings.Contains(enriched.MessageText, "не удалось обработать") {
		t.Fatalf("expected placeholder, got %q", enriched.MessageText)
	}
}

// TestEnrichBotMediaNilMediaUnchanged verifies a plain-text message passes through
// untouched (no media descriptor).
func TestEnrichBotMediaNilMediaUnchanged(t *testing.T) {
	service := mediaTestService(&mediaAI{}, &downloaderSender{fakeSender: &fakeSender{}})
	event := botEvent(t, 9502, 42, 22, "просто текст")

	enriched := service.EnrichBotMedia(context.Background(), event, nil)

	if enriched.MessageText != "просто текст" {
		t.Fatalf("MessageText = %q, want unchanged", enriched.MessageText)
	}
}

// TestEnrichBotMediaTooLargeAudioSkipped verifies a voice clip over the audio
// byte cap is rejected before any download or provider call.
func TestEnrichBotMediaTooLargeAudioSkipped(t *testing.T) {
	ai := &mediaAI{text: "should not run"}
	sender := &downloaderSender{fakeSender: &fakeSender{}, data: []byte("x")}
	service := mediaTestService(ai, sender)

	event, media := botMediaInputs(t, voiceUpdateSized(9510, 42, 30, 11*1024*1024, 5))
	enriched := service.EnrichBotMedia(context.Background(), event, media)

	if ai.extCalls != 0 {
		t.Fatalf("oversized audio must not be extracted, calls=%d", ai.extCalls)
	}
	if !strings.Contains(enriched.MessageText, "слишком большое") {
		t.Fatalf("MessageText = %q", enriched.MessageText)
	}
}

// TestEnrichBotMediaTooLongAudioSkipped verifies a voice clip over the duration
// cap is rejected even when its byte size is small.
func TestEnrichBotMediaTooLongAudioSkipped(t *testing.T) {
	ai := &mediaAI{text: "should not run"}
	sender := &downloaderSender{fakeSender: &fakeSender{}, data: []byte("x")}
	service := mediaTestService(ai, sender)

	event, media := botMediaInputs(t, voiceUpdateSized(9511, 42, 31, 1000, 360))
	enriched := service.EnrichBotMedia(context.Background(), event, media)

	if ai.extCalls != 0 {
		t.Fatalf("over-long audio must not be extracted, calls=%d", ai.extCalls)
	}
	if !strings.Contains(enriched.MessageText, "слишком длинное") {
		t.Fatalf("MessageText = %q", enriched.MessageText)
	}
}

// TestEnrichBotMediaDisabledSkips verifies the global toggle off skips the
// provider entirely while still labelling the message.
func TestEnrichBotMediaDisabledSkips(t *testing.T) {
	ai := &mediaAI{text: "should not run"}
	sender := &downloaderSender{fakeSender: &fakeSender{}, data: []byte("oggbytes")}
	cfg := DefaultMediaConfig()
	cfg.Enabled = false
	service := NewService(store.NewRepository(), ai, &fakeSender{},
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithBotSenderProvider(mediaSenderProvider{sender: sender}),
		WithMediaConfig(cfg),
	)

	event, media := botMediaInputs(t, voiceUpdatePayload(9512, 42, 32))
	enriched := service.EnrichBotMedia(context.Background(), event, media)

	if ai.extCalls != 0 {
		t.Fatalf("disabled extraction must not call provider, calls=%d", ai.extCalls)
	}
	if !strings.Contains(enriched.MessageText, "отключена") {
		t.Fatalf("MessageText = %q", enriched.MessageText)
	}
}

// TestEnrichBotMediaRateLimited verifies a per-tenant media flood is shed to a
// placeholder after the burst is spent.
func TestEnrichBotMediaRateLimited(t *testing.T) {
	ai := &mediaAI{text: "распознано"}
	sender := &downloaderSender{fakeSender: &fakeSender{}, data: []byte("oggbytes")}
	cfg := DefaultMediaConfig()
	cfg.RatePerSec = 0.001 // effectively no refill during the test
	cfg.Burst = 1
	service := NewService(store.NewRepository(), ai, &fakeSender{},
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithBotSenderProvider(mediaSenderProvider{sender: sender}),
		WithMediaConfig(cfg),
	)

	first, m1 := botMediaInputs(t, voiceUpdatePayload(9513, 42, 33))
	second, m2 := botMediaInputs(t, voiceUpdatePayload(9514, 42, 34))
	e1 := service.EnrichBotMedia(context.Background(), first, m1)
	e2 := service.EnrichBotMedia(context.Background(), second, m2)

	if !strings.Contains(e1.MessageText, "распознано") {
		t.Fatalf("first message should extract, got %q", e1.MessageText)
	}
	if !strings.Contains(e2.MessageText, "слишком много медиа") {
		t.Fatalf("second message should be rate-limited, got %q", e2.MessageText)
	}
	if ai.extCalls != 1 {
		t.Fatalf("only the first message should reach the provider, calls=%d", ai.extCalls)
	}
}
