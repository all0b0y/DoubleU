package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

// deliveryTestService builds a service whose contact replies leave through the
// recorded control sender (used for non-bot events).
func deliveryTestService(sender *fakeSender) *Service {
	return NewService(store.NewRepository(), &fakeAI{}, sender,
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
	)
}

func plainEvent(chatID string) contracts.NormalizedTelegramEvent {
	return contracts.NormalizedTelegramEvent{TenantID: "tenant:user:77", ChatID: chatID, SourceMessageID: "1"}
}

// TestPlainFormatConvertsMarkdownInsteadOfLeaking reproduces the "**09:00** shown
// literally" bug: a plain-format reply that still carries markdown must render as
// Telegram HTML, never leak raw ** tokens into the chat.
func TestPlainFormatConvertsMarkdownInsteadOfLeaking(t *testing.T) {
	sender := &fakeSender{}
	service := deliveryTestService(sender)

	text := "🕘 **09:00** — Зарядка\n🕙 **11:00** — Встреча с командой"
	_, err := service.sendRecipient(context.Background(), plainEvent("55"), "dec1", text,
		contracts.TextFormatPlain, false, deliverySettings{})
	if err != nil {
		t.Fatalf("sendRecipient: %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(sender.sent))
	}
	got := sender.sent[0]
	if got.ParseMode != "HTML" {
		t.Fatalf("plain reply with markdown must be sent as HTML, parse_mode=%q", got.ParseMode)
	}
	if strings.Contains(got.Text, "**") {
		t.Fatalf("raw markdown leaked into the chat: %q", got.Text)
	}
	for _, want := range []string{"<b>09:00</b>", "<b>11:00</b>", "🕘"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("plain HTML output %q missing %q", got.Text, want)
		}
	}
}

// TestPlainFormatDoesNotMangleIncidentalMarkers guards the fix: MarkdownToHTML
// only transforms matched constructs, so an unmatched single "*" (arithmetic) or
// snake_case identifier in a plain reply must pass through untouched.
func TestPlainFormatDoesNotMangleIncidentalMarkers(t *testing.T) {
	sender := &fakeSender{}
	service := deliveryTestService(sender)

	text := "Оплата 5 * 5 рублей за file_name.pdf"
	_, err := service.sendRecipient(context.Background(), plainEvent("56"), "dec2", text,
		contracts.TextFormatPlain, false, deliverySettings{})
	if err != nil {
		t.Fatalf("sendRecipient: %v", err)
	}
	got := sender.sent[0].Text
	if !strings.Contains(got, "5 * 5") || !strings.Contains(got, "file_name.pdf") {
		t.Fatalf("incidental markers were mangled: %q", got)
	}
}
