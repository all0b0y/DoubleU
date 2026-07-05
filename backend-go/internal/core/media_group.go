package core

import (
	"context"
	"strings"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/telegram"
)

// defaultMediaGroupWindow is the debounce used when MediaConfig.GroupWindow is
// unset. Telegram delivers an album's photos/documents as separate messages
// with the same media_group_id and no "album complete" signal, so the ingestion
// layer buffers items for a short quiet period and then processes them as one
// turn. The items are created server-side at the same instant and normally
// arrive in a single getUpdates batch, so a couple of seconds comfortably
// bridges the delivery gap without noticeably delaying the reply.
const defaultMediaGroupWindow = 2 * time.Second

// botMediaItem is one buffered message plus its downloadable attachment, awaiting
// coalescing with the rest of its Telegram album.
type botMediaItem struct {
	event contracts.NormalizedTelegramEvent
	media *telegram.MediaDescriptor
}

// mediaGroupBuffer accumulates the items of a single album and holds the debounce
// timer that flushes them once no more have arrived within GroupWindow.
type mediaGroupBuffer struct {
	items []botMediaItem
	timer *time.Timer
}

// HandleBotMediaGroup is the ingestion entry point for bot-transport messages.
// It coalesces the items of a Telegram album (attachments sharing a
// media_group_id) into a single AI turn so the agent reasons over the whole
// message — e.g. "evaluate my two canvases" with both images — instead of seeing
// each file or photo in isolation. Plain text and standalone attachments flow
// straight through unchanged.
func (s *Service) HandleBotMediaGroup(ctx context.Context, event contracts.NormalizedTelegramEvent, media *telegram.MediaDescriptor) error {
	groupID := ""
	if media != nil {
		groupID = media.GroupID
	}
	if groupID == "" {
		// A non-album message must not overtake an album that is still buffering in
		// the same chat, so drain any pending group first (preserves send ordering).
		s.flushChatMediaGroups(event.TenantID, event.ChatID)
		return s.handleBotMediaItems(ctx, []botMediaItem{{event: event, media: media}})
	}
	s.bufferMediaGroupItem(event, media, groupID)
	return nil
}

// bufferMediaGroupItem appends one album item and (re)arms the debounce timer.
// Re-arming on every item means the group flushes only once the album has gone
// quiet for a full GroupWindow, regardless of how many items it holds.
func (s *Service) bufferMediaGroupItem(event contracts.NormalizedTelegramEvent, media *telegram.MediaDescriptor, groupID string) {
	window := s.media.GroupWindow
	if window <= 0 {
		window = defaultMediaGroupWindow
	}
	key := mediaGroupKey(event.TenantID, event.ChatID, groupID)
	s.mediaGroupsMu.Lock()
	defer s.mediaGroupsMu.Unlock()
	buf, ok := s.mediaGroups[key]
	if !ok {
		buf = &mediaGroupBuffer{}
		s.mediaGroups[key] = buf
	}
	buf.items = append(buf.items, botMediaItem{event: event, media: media})
	if buf.timer != nil {
		buf.timer.Stop()
	}
	buf.timer = time.AfterFunc(window, func() { s.flushMediaGroup(key) })
}

// flushMediaGroup removes the buffered album under key and processes it as one
// turn. It is safe to call more than once (timer fire + a manual drain race):
// the first caller detaches the buffer under the lock, so the second finds
// nothing. Detaching before processing also lets a late item that arrives after
// the window start a fresh group rather than mutating one already in flight.
func (s *Service) flushMediaGroup(key string) {
	s.mediaGroupsMu.Lock()
	buf := s.mediaGroups[key]
	delete(s.mediaGroups, key)
	s.mediaGroupsMu.Unlock()
	if buf == nil || len(buf.items) == 0 {
		return
	}
	if buf.timer != nil {
		buf.timer.Stop()
	}
	// The poll-iteration context is already gone by the time the timer fires, so
	// use a detached context; the AI client enforces its own request timeout and
	// HandleBotMessage derives its own trace id from the event.
	if err := s.handleBotMediaItems(context.Background(), buf.items); err != nil {
		first := buf.items[0].event
		s.logger.Error("bot media group processing failed",
			"tenant_id", first.TenantID, "chat_id", first.ChatID,
			"items", len(buf.items), "error", err)
	}
}

// flushChatMediaGroups synchronously drains every album still buffering for a
// chat. Used before a non-album message so the album's turn is emitted first.
func (s *Service) flushChatMediaGroups(tenantID, chatID string) {
	prefix := tenantID + "\x00" + chatID + "\x00"
	s.mediaGroupsMu.Lock()
	var keys []string
	for k := range s.mediaGroups {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	s.mediaGroupsMu.Unlock()
	for _, k := range keys {
		s.flushMediaGroup(k)
	}
}

// handleBotMediaItems extracts each item's attachment to text and hands the
// whole album to the pipeline as one message. The first item carries the
// caption (Telegram only attaches it there) and lends its identity — event id,
// source message id and receive-idempotency key — to the merged turn, so a
// redelivered album dedupes on the same key instead of replying twice.
func (s *Service) handleBotMediaItems(ctx context.Context, items []botMediaItem) error {
	if len(items) == 0 {
		return nil
	}
	base := items[0].event
	parts := make([]string, 0, len(items))
	for _, item := range items {
		enriched := s.EnrichBotMedia(ctx, item.event, item.media)
		if text := strings.TrimSpace(enriched.MessageText); text != "" {
			parts = append(parts, text)
		}
	}
	base.MessageText = strings.Join(parts, "\n\n")
	_, err := s.HandleBotMessage(ctx, base)
	return err
}

// mediaGroupKey scopes a buffered album to its chat. The NUL separators keep the
// tenant|chat prefix used by flushChatMediaGroups unambiguous even if an id
// contained a delimiter-like character.
func mediaGroupKey(tenantID, chatID, groupID string) string {
	return tenantID + "\x00" + chatID + "\x00" + groupID
}
