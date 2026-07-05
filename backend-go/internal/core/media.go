package core

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/telegram"
	tracepkg "doubleu/backend-go/internal/trace"
)

// MediaConfig controls the media -> text extraction step (Slice 3 hardening):
// a global toggle plus pre-download caps that keep provider cost bounded.
type MediaConfig struct {
	// Enabled is the global kill switch. When false the message is labelled with
	// its media kind (so the bot still acknowledges it) and no provider call is made.
	Enabled bool
	// MaxBytes caps any attachment; stays at/under Telegram's 20 MB download hard limit.
	MaxBytes int64
	// MaxAudioBytes caps voice/audio specifically (OpenRouter STT rejects >10 MB).
	MaxAudioBytes int64
	// MaxAudioSeconds caps voice/audio duration (OpenRouter STT caps at 5 min).
	MaxAudioSeconds int64
	// RatePerSec / Burst bound per-tenant extraction frequency; over-rate messages
	// fall back to a placeholder rather than triggering a provider call.
	RatePerSec float64
	Burst      int
	// GroupWindow is how long the ingestion layer waits for more items of the same
	// Telegram album (media_group_id) before processing them as one AI turn. It
	// only needs to bridge the sub-second delivery gap between an album's updates,
	// which Telegram sends as separate messages with no "album complete" signal.
	GroupWindow time.Duration
}

// DefaultMediaConfig returns production defaults: extraction on, caps aligned
// with Telegram (20 MB) and the OpenRouter STT limits (10 MB / 5 min).
func DefaultMediaConfig() MediaConfig {
	return MediaConfig{
		Enabled:         true,
		MaxBytes:        20 * 1024 * 1024,
		MaxAudioBytes:   10 * 1024 * 1024,
		MaxAudioSeconds: 300,
		RatePerSec:      1,
		Burst:           5,
		GroupWindow:     defaultMediaGroupWindow,
	}
}

// mediaExtractTimeout bounds the download + extract step so a slow or oversized
// attachment never stalls a bot poller's sequential loop indefinitely.
const mediaExtractTimeout = 60 * time.Second

// EnrichBotMedia turns a bot message's media attachment into text before AI
// processing so the agent stays text-only (media -> text). The enriched text is
// written to event.MessageText, where it flows through drafting, memory and
// embeddings unchanged.
//
// It is best-effort and never fails the message: with no media, no downloader,
// no media-capable AI client, or an extraction error/timeout it leaves a short
// placeholder so the bot still acknowledges the message instead of going silent.
// Raw media bytes are held only in memory for the call and never persisted.
func (s *Service) EnrichBotMedia(ctx context.Context, event contracts.NormalizedTelegramEvent, media *telegram.MediaDescriptor) contracts.NormalizedTelegramEvent {
	if media == nil {
		return event
	}
	// Pre-download gates (toggle, size, duration, rate) so an oversized or flood
	// of media never triggers a wasted download or provider call.
	if reason := s.mediaRejectReason(event.TenantID, media); reason != "" {
		s.logger.Info("bot media skipped before extraction",
			"tenant_id", event.TenantID, "chat_id", event.ChatID,
			"kind", media.Kind, "size", media.FileSize, "duration", media.Duration, "reason", reason)
		return s.placeholderEvent(event, media, reason)
	}
	extractor, ok := s.ai.(aiclient.MediaExtractor)
	if !ok {
		return s.placeholderEvent(event, media, mediaReasonUnprocessable)
	}
	text, err := s.extractBotMedia(ctx, event, media, extractor)
	if err != nil || text == "" {
		s.logger.Warn("bot media extraction failed; using placeholder",
			"tenant_id", event.TenantID, "chat_id", event.ChatID,
			"kind", media.Kind, "error", err)
		return s.placeholderEvent(event, media, mediaReasonUnprocessable)
	}
	event.MessageText = mergeMediaText(event.MessageText, text, media.Kind)
	s.logger.Info("bot media extracted to text",
		"tenant_id", event.TenantID, "chat_id", event.ChatID,
		"kind", media.Kind, "chars", len(text))
	return event
}

// Placeholder reasons shown (as a label suffix) when media is not turned into
// text, so the contact-facing model can respond appropriately.
const (
	mediaReasonUnprocessable = "не удалось обработать"
	mediaReasonDisabled      = "обработка медиа отключена"
	mediaReasonTooLarge      = "слишком большое"
	mediaReasonTooLong       = "слишком длинное"
	mediaReasonRateLimited   = "слишком много медиа"
)

// mediaRejectReason returns a non-empty placeholder reason when the attachment
// must be skipped before any download (disabled, oversized, too long, or the
// tenant is over its media rate), or "" when it may be extracted. Missing size
// or duration (0) never rejects — the download hard cap still backstops size.
func (s *Service) mediaRejectReason(tenantID string, media *telegram.MediaDescriptor) string {
	if !s.media.Enabled {
		return mediaReasonDisabled
	}
	isAudio := media.Kind == "voice" || media.Kind == "audio"
	maxBytes := s.media.MaxBytes
	if isAudio && s.media.MaxAudioBytes > 0 && (maxBytes == 0 || s.media.MaxAudioBytes < maxBytes) {
		maxBytes = s.media.MaxAudioBytes
	}
	if maxBytes > 0 && media.FileSize > maxBytes {
		return mediaReasonTooLarge
	}
	if isAudio && s.media.MaxAudioSeconds > 0 && media.Duration > s.media.MaxAudioSeconds {
		return mediaReasonTooLong
	}
	// Rate gate last so a token is consumed only for an otherwise-acceptable
	// attachment we are about to extract.
	if tenantID != "" && !s.mediaLimiter(tenantID).Allow() {
		return mediaReasonRateLimited
	}
	return ""
}

// mediaLimiter returns the per-tenant media-extraction token bucket, creating it
// on first use. A non-positive configured rate disables the gate (infinite).
func (s *Service) mediaLimiter(tenantID string) *rate.Limiter {
	if v, ok := s.mediaLimiters.Load(tenantID); ok {
		return v.(*rate.Limiter)
	}
	limit := rate.Limit(s.media.RatePerSec)
	if s.media.RatePerSec <= 0 {
		limit = rate.Inf
	}
	burst := s.media.Burst
	if burst <= 0 {
		burst = 1
	}
	v, _ := s.mediaLimiters.LoadOrStore(tenantID, rate.NewLimiter(limit, burst))
	return v.(*rate.Limiter)
}

// extractBotMedia downloads the attachment through the message's own bot and
// converts it to text via the Python AI service. The download uses the per-bot
// token resolved by SenderFor (multibot), keeping the token on the Go side.
func (s *Service) extractBotMedia(ctx context.Context, event contracts.NormalizedTelegramEvent, media *telegram.MediaDescriptor, extractor aiclient.MediaExtractor) (string, error) {
	if s.botSenders == nil {
		return "", fmt.Errorf("no bot sender provider")
	}
	sender, ok := s.botSenders.SenderFor(event.TenantID, event.BotID)
	if !ok {
		return "", fmt.Errorf("no sender for bot %s", event.BotID)
	}
	downloader, ok := sender.(telegram.FileDownloader)
	if !ok {
		return "", fmt.Errorf("sender does not support file download")
	}
	dlCtx, cancel := context.WithTimeout(ctx, mediaExtractTimeout)
	defer cancel()
	path, err := downloader.GetFilePath(dlCtx, media.FileID)
	if err != nil {
		return "", err
	}
	data, downloadedMime, err := downloader.DownloadFile(dlCtx, path)
	if err != nil {
		return "", err
	}
	// The message's declared mime (voice.mime_type, document.mime_type) is more
	// reliable than the download response's generic Content-Type.
	mime := media.MimeType
	if mime == "" {
		mime = downloadedMime
	}
	resp, err := extractor.ExtractMedia(dlCtx, aiclient.ExtractMediaRequest{
		Kind:       media.Kind,
		DataBase64: base64.StdEncoding.EncodeToString(data),
		MimeType:   mime,
		Duration:   media.Duration,
		TraceID:    tracepkg.DeriveTraceID(event.EventID),
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// placeholderEvent labels the message with its media kind and the reason the
// content was not extracted, so the AI can still respond (e.g. ask the user to
// resend as text) rather than seeing an empty message.
func (s *Service) placeholderEvent(event contracts.NormalizedTelegramEvent, media *telegram.MediaDescriptor, reason string) contracts.NormalizedTelegramEvent {
	placeholder := "[" + mediaLabel(media.Kind) + " — " + reason + "]"
	if existing := strings.TrimSpace(event.MessageText); existing != "" {
		event.MessageText = existing + "\n" + placeholder
	} else {
		event.MessageText = placeholder
	}
	return event
}

// mergeMediaText combines any caption with the extracted media text under a
// label so the model knows the content was transcribed/OCR'd from an attachment
// rather than typed by the contact.
func mergeMediaText(caption, extracted, kind string) string {
	var b strings.Builder
	if c := strings.TrimSpace(caption); c != "" {
		b.WriteString(c)
		b.WriteString("\n\n")
	}
	b.WriteString("[")
	b.WriteString(mediaLabel(kind))
	b.WriteString("]: ")
	b.WriteString(extracted)
	return b.String()
}

// mediaLabel is the contact-language (Russian) label for a media kind, used in
// the text handed to the model.
func mediaLabel(kind string) string {
	switch kind {
	case "voice":
		return "голосовое сообщение"
	case "audio":
		return "аудио"
	case "photo":
		return "изображение"
	case "document":
		return "документ"
	default:
		return "вложение"
	}
}
