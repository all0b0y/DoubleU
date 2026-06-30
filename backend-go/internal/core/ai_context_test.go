package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

// fixedClockService builds a Service whose clock is pinned to a known instant
// so timezone-conversion assertions are deterministic regardless of when the
// suite runs.
func fixedClockService(repository *store.Repository, now time.Time) *Service {
	return NewService(repository, &fakeAI{}, &fakeSender{}, WithClock(func() time.Time { return now }))
}

// TestConversationContextHeaderUsesOwnerLocalTimeWhenKnown verifies B: with a
// known owner IANA timezone, the [CURRENT DATE] header and transcript
// timestamps render in the owner's local clock (not bare UTC), and the
// A1 "unknown timezone" marker must never appear.
func TestConversationContextHeaderUsesOwnerLocalTimeWhenKnown(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	repository.SaveUserSettings(contracts.UserSettings{TenantID: "tenant-1", Timezone: "Europe/Moscow"})
	repository.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID:         "e1",
		TenantID:        "tenant-1",
		ChatID:          "42",
		SourceMessageID: "m1",
		MessageText:     "привет",
		CreatedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
	})

	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC) // 15:00 MSK (UTC+3)
	service := fixedClockService(repository, now)

	event := contracts.NormalizedTelegramEvent{TenantID: "tenant-1", ChatID: "42", SourceMessageID: "current"}
	parts := service.conversationContext(context.Background(), event, "", 20)

	var header, messageLine string
	for _, p := range parts {
		if strings.HasPrefix(p, "[CURRENT DATE:") {
			header = p
		}
		if strings.HasPrefix(p, "[2026-06-23 12:00") {
			messageLine = p
		}
	}

	if header == "" {
		t.Fatalf("CURRENT DATE header missing from context: %v", parts)
	}
	if !strings.Contains(header, "2026-06-23T15:00:00+03:00") || !strings.Contains(header, "Europe/Moscow") {
		t.Fatalf("header must show owner-local datetime+offset for a known timezone, got %q", header)
	}
	if strings.Contains(header, "неизвестен") {
		t.Fatalf("header must not carry the unknown-timezone marker when the zone is known: %q", header)
	}
	if messageLine == "" {
		t.Fatalf("transcript message line not rendered in owner-local time (expected prefix [2026-06-23 12:00): %v", parts)
	}
	if !strings.Contains(messageLine, "MSK") {
		t.Fatalf("transcript timestamp must carry the owner's zone abbreviation, got %q", messageLine)
	}
}

// TestConversationContextHeaderFallsBackToUTCWhenTimezoneUnknown verifies A1
// applied to B: a bot-only owner with no synced timezone must get an explicit
// "unknown" marker, never a silent UTC guess presented as authoritative.
func TestConversationContextHeaderFallsBackToUTCWhenTimezoneUnknown(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	// No SaveUserSettings call at all: timezone is unset (bot-only owner).
	repository.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID:         "e1",
		TenantID:        "tenant-1",
		ChatID:          "42",
		SourceMessageID: "m1",
		MessageText:     "привет",
		CreatedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
	})

	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	service := fixedClockService(repository, now)

	event := contracts.NormalizedTelegramEvent{TenantID: "tenant-1", ChatID: "42", SourceMessageID: "current"}
	parts := service.conversationContext(context.Background(), event, "", 20)

	var header, messageLine string
	for _, p := range parts {
		if strings.HasPrefix(p, "[CURRENT DATE:") {
			header = p
		}
		if strings.HasPrefix(p, "[2026-06-23 09:00") {
			messageLine = p
		}
	}

	if header == "" {
		t.Fatalf("CURRENT DATE header missing from context: %v", parts)
	}
	if !strings.Contains(header, "2026-06-23T12:00:00Z") || !strings.Contains(header, "пояс владельца неизвестен") {
		t.Fatalf("header must fall back to UTC with an explicit unknown-timezone marker, got %q", header)
	}
	if messageLine == "" || !strings.Contains(messageLine, "UTC") {
		t.Fatalf("transcript timestamp must render in UTC with an explicit marker when the zone is unknown: %v", parts)
	}
}

// TestConversationContextHeaderTreatsInvalidTimezoneAsUnknown verifies the
// IANA-validation invariant: a stored value that fails time.LoadLocation must
// degrade to "unknown" instead of panicking or silently mislabeling a bogus
// offset as the owner's real local time.
func TestConversationContextHeaderTreatsInvalidTimezoneAsUnknown(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	repository.SaveUserSettings(contracts.UserSettings{TenantID: "tenant-1", Timezone: "Not/AZone"})

	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	service := fixedClockService(repository, now)

	event := contracts.NormalizedTelegramEvent{TenantID: "tenant-1", ChatID: "42", SourceMessageID: "current"}
	parts := service.conversationContext(context.Background(), event, "", 20)

	var header string
	for _, p := range parts {
		if strings.HasPrefix(p, "[CURRENT DATE:") {
			header = p
		}
	}
	if !strings.Contains(header, "пояс владельца неизвестен") {
		t.Fatalf("an unrecognised IANA name must be treated as unknown, got %q", header)
	}
}
