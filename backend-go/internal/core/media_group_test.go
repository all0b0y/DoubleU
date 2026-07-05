package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

// recordingAI records every process-message turn and returns a distinct OCR
// string per extraction, so a test can prove how many AI turns ran and that each
// album item's extracted text made it into the merged message. It is mutex-
// guarded because flushes run on the debounce-timer goroutine.
type recordingAI struct {
	mu       sync.Mutex
	procReqs []contracts.AIProcessMessageRequest
	ocrCount int
}

func (r *recordingAI) ProcessMessage(_ context.Context, req contracts.AIProcessMessageRequest) (contracts.AIProcessMessageResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.procReqs = append(r.procReqs, req)
	return contracts.AIProcessMessageResponse{Decision: contracts.DecisionObserve, RiskLevel: contracts.RiskLow}, nil
}

func (r *recordingAI) ExtractMedia(_ context.Context, _ aiclient.ExtractMediaRequest) (aiclient.ExtractMediaResponse, error) {
	r.mu.Lock()
	n := r.ocrCount
	r.ocrCount++
	r.mu.Unlock()
	return aiclient.ExtractMediaResponse{Text: fmt.Sprintf("OCR#%d", n), Provenance: "image_ocr", ModelUsed: "test"}, nil
}

func (r *recordingAI) turns() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.procReqs)
}

func (r *recordingAI) requestTexts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	texts := make([]string, len(r.procReqs))
	for i, req := range r.procReqs {
		texts[i] = req.Event.MessageText
	}
	return texts
}

// mediaGroupService builds a bot-transport service whose media extraction is
// backed by ai and whose album debounce window is tiny so tests stay fast.
func mediaGroupService(ai aiclient.Client, window time.Duration) *Service {
	cfg := DefaultMediaConfig()
	cfg.GroupWindow = window
	sender := &downloaderSender{fakeSender: &fakeSender{}, data: []byte("imgbytes"), downMime: "image/jpeg"}
	return NewService(store.NewRepository(), ai, &fakeSender{},
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithBotSenderProvider(mediaSenderProvider{sender: sender}),
		WithMediaConfig(cfg),
	)
}

// photoAlbumPayload is one item of a Telegram album: a photo carrying a shared
// media_group_id, with the caption present only on the first item (as Telegram
// delivers it).
func photoAlbumPayload(updateID, chatID, messageID int64, groupID, caption string) []byte {
	captionField := ""
	if caption != "" {
		captionField = `"caption": "` + caption + `",`
	}
	return []byte(`{
		"update_id": ` + intString(updateID) + `,
		"message": {
			"message_id": ` + intString(messageID) + `,
			"chat": {"id": ` + intString(chatID) + `, "type": "private", "first_name": "Контакт"},
			"from": {"id": ` + intString(chatID) + `},
			"media_group_id": "` + groupID + `",
			"photo": [{"file_id":"SMALL` + intString(messageID) + `","file_size":100},{"file_id":"BIG` + intString(messageID) + `","file_size":900}],
			` + captionField + `
			"date": 1700000000
		}
	}`)
}

func waitForTurns(t *testing.T, ai *recordingAI, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ai.turns() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d AI turns, got %d", want, ai.turns())
}

// TestMediaGroupParsedFromUpdate verifies media_group_id flows onto the media
// descriptor so the ingestion layer can recognise album items.
func TestMediaGroupParsedFromUpdate(t *testing.T) {
	media, err := telegram.MediaFromBotUpdate(photoAlbumPayload(1, 42, 10, "ALBUM7", "подпись"))
	if err != nil {
		t.Fatalf("parse media: %v", err)
	}
	if media == nil || media.GroupID != "ALBUM7" {
		t.Fatalf("descriptor group id = %+v, want ALBUM7", media)
	}
	// A standalone photo (no album) must leave GroupID empty.
	solo, err := telegram.MediaFromBotUpdate(photoWithCaptionPayload(2, 42, 11, "один"))
	if err != nil {
		t.Fatalf("parse solo: %v", err)
	}
	if solo == nil || solo.GroupID != "" {
		t.Fatalf("standalone photo group id = %+v, want empty", solo)
	}
}

// TestMediaGroupCoalescesIntoOneTurn is the core fix: two photos sent as one
// album (shared media_group_id) become ONE AI turn whose text carries the
// caption plus BOTH images' extracted content — not two isolated turns.
func TestMediaGroupCoalescesIntoOneTurn(t *testing.T) {
	ai := &recordingAI{}
	service := mediaGroupService(ai, 40*time.Millisecond)

	first, m1 := botMediaInputs(t, photoAlbumPayload(9600, 42, 100, "GRP1", "оцени мои канвасы"))
	second, m2 := botMediaInputs(t, photoAlbumPayload(9601, 42, 101, "GRP1", ""))
	if err := service.HandleBotMediaGroup(context.Background(), first, m1); err != nil {
		t.Fatalf("first item: %v", err)
	}
	if err := service.HandleBotMediaGroup(context.Background(), second, m2); err != nil {
		t.Fatalf("second item: %v", err)
	}
	// Before the window elapses, nothing has been processed yet.
	if got := ai.turns(); got != 0 {
		t.Fatalf("album must not process before the window; turns=%d", got)
	}

	waitForTurns(t, ai, 1)
	if got := ai.turns(); got != 1 {
		t.Fatalf("album produced %d turns, want exactly 1", got)
	}
	merged := ai.requestTexts()[0]
	for _, want := range []string{"оцени мои канвасы", "OCR#0", "OCR#1"} {
		if !strings.Contains(merged, want) {
			t.Fatalf("merged message %q missing %q", merged, want)
		}
	}
}

// TestPlainMessageProcessedImmediately guards the regression surface: a text
// message (no attachment) is handled synchronously, never buffered.
func TestPlainMessageProcessedImmediately(t *testing.T) {
	ai := &recordingAI{}
	service := mediaGroupService(ai, time.Hour) // huge window: proves no buffering path is taken

	if err := service.HandleBotMediaGroup(context.Background(), botEvent(t, 9610, 42, 110, "просто текст"), nil); err != nil {
		t.Fatalf("plain message: %v", err)
	}
	if got := ai.turns(); got != 1 {
		t.Fatalf("plain message turns=%d, want 1 (synchronous)", got)
	}
	if texts := ai.requestTexts(); texts[0] != "просто текст" {
		t.Fatalf("plain message text = %q", texts[0])
	}
}

// TestStandaloneMediaProcessedImmediately verifies a single photo (no album) is
// not delayed by the album debounce.
func TestStandaloneMediaProcessedImmediately(t *testing.T) {
	ai := &recordingAI{}
	service := mediaGroupService(ai, time.Hour)

	event, media := botMediaInputs(t, photoWithCaptionPayload(9620, 42, 120, "чек"))
	if err := service.HandleBotMediaGroup(context.Background(), event, media); err != nil {
		t.Fatalf("standalone media: %v", err)
	}
	if got := ai.turns(); got != 1 {
		t.Fatalf("standalone media turns=%d, want 1 (synchronous)", got)
	}
	if merged := ai.requestTexts()[0]; !strings.Contains(merged, "чек") || !strings.Contains(merged, "OCR#0") {
		t.Fatalf("standalone media text = %q, want caption + ocr", merged)
	}
}

// TestPlainMessageFlushesPendingAlbumFirst verifies ordering: a text message
// arriving while an album is still buffering drains the album (its turn runs
// first) before the text is processed, so replies stay in send order.
func TestPlainMessageFlushesPendingAlbumFirst(t *testing.T) {
	ai := &recordingAI{}
	service := mediaGroupService(ai, time.Hour) // window never elapses on its own

	first, m1 := botMediaInputs(t, photoAlbumPayload(9630, 42, 130, "GRP2", "два файла"))
	if err := service.HandleBotMediaGroup(context.Background(), first, m1); err != nil {
		t.Fatalf("album item: %v", err)
	}
	if got := ai.turns(); got != 0 {
		t.Fatalf("album should still be buffering; turns=%d", got)
	}

	// A plain text message must flush the pending album first, then process itself.
	if err := service.HandleBotMediaGroup(context.Background(), botEvent(t, 9631, 42, 131, "и ещё вопрос"), nil); err != nil {
		t.Fatalf("plain message: %v", err)
	}
	if got := ai.turns(); got != 2 {
		t.Fatalf("expected 2 turns (album then text), got %d", got)
	}
	texts := ai.requestTexts()
	if !strings.Contains(texts[0], "два файла") {
		t.Fatalf("first turn should be the album, got %q", texts[0])
	}
	if texts[1] != "и ещё вопрос" {
		t.Fatalf("second turn should be the text, got %q", texts[1])
	}
}
