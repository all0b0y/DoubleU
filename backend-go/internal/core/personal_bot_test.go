package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

// personalBotService wires a bot-transport service whose replies (and stubs) go
// through botSender, with a registered personal-mode bot 555 owned by user 77.
func personalBotService(t *testing.T, ai *fakeAI, botSender *fakeSender, personal bool) *store.Repository {
	t.Helper()
	repo := store.NewRepository()
	repo.SaveUserBot(store.UserBotRecord{TenantID: "tenant:user:77", BotID: 555, Status: "active"})
	repo.SetUserBotPersonalMode("tenant:user:77", 555, personal)
	return repo
}

func newPersonalBotService(repo *store.Repository, ai *fakeAI, botSender *fakeSender) *Service {
	return NewService(repo, ai, &fakeSender{},
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithDefaultIngestionMode(contracts.IngestionModeFull),
		WithBotSenderProvider(&fakeBotSenders{sender: botSender}),
	)
}

func draftingAI() *fakeAI {
	return &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "черновик",
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
}

// TestPersonalBotServesOwner verifies the owner is always answered by their own
// personal bot (owner DM to a bot is a contact turn — personal mode must not
// suppress it).
func TestPersonalBotServesOwner(t *testing.T) {
	ai := draftingAI()
	botSender := &fakeSender{}
	repo := personalBotService(t, ai, botSender, true)
	service := newPersonalBotService(repo, ai, botSender)

	// Owner DM: from.id == 77 == ownerID (tenant:user:77).
	result, err := service.HandleBotMessage(context.Background(), botEvent(t, 9700, 77, 300, "привет"))
	if err != nil {
		t.Fatalf("owner message: %v", err)
	}
	if ai.calls != 1 {
		t.Fatalf("owner must reach the AI, calls=%d", ai.calls)
	}
	if result.Decision != contracts.DecisionDraft {
		t.Fatalf("owner decision = %s, want draft", result.Decision)
	}
}

// TestPersonalBotStubsStrangerOnceThenSilent is the core behaviour: a non-owner,
// non-allowlisted sender gets the stub exactly once and never reaches the AI;
// subsequent messages are silently ignored.
func TestPersonalBotStubsStrangerOnceThenSilent(t *testing.T) {
	ai := draftingAI()
	botSender := &fakeSender{}
	repo := personalBotService(t, ai, botSender, true)
	service := newPersonalBotService(repo, ai, botSender)

	// Stranger 42 → stub once.
	first, err := service.HandleBotMessage(context.Background(), botEvent(t, 9710, 42, 310, "привет"))
	if err != nil {
		t.Fatalf("stranger first message: %v", err)
	}
	if ai.calls != 0 {
		t.Fatalf("stranger must not reach the AI, calls=%d", ai.calls)
	}
	if first.Reason != "personal_bot_stub" {
		t.Fatalf("first stranger reason = %q, want personal_bot_stub", first.Reason)
	}
	if len(botSender.sent) != 1 {
		t.Fatalf("expected exactly one stub send, got %d", len(botSender.sent))
	}
	if !strings.Contains(botSender.sent[0].Text, "личный бот") {
		t.Fatalf("stub text = %q", botSender.sent[0].Text)
	}
	// The stub must go to the raw Telegram chat id, not the bot-namespaced id.
	if botSender.sent[0].ChatID != "42" {
		t.Fatalf("stub chat id = %q, want raw 42", botSender.sent[0].ChatID)
	}

	// Second message from the same stranger → silence (no new send, no AI).
	second, err := service.HandleBotMessage(context.Background(), botEvent(t, 9711, 42, 311, "ещё раз"))
	if err != nil {
		t.Fatalf("stranger second message: %v", err)
	}
	if second.Reason != "personal_bot_silent" {
		t.Fatalf("second stranger reason = %q, want personal_bot_silent", second.Reason)
	}
	if len(botSender.sent) != 1 {
		t.Fatalf("stub must be sent at most once, got %d sends", len(botSender.sent))
	}
	if ai.calls != 0 {
		t.Fatalf("stranger flood must never reach the AI, calls=%d", ai.calls)
	}
}

// TestPersonalBotServesAllowlistedUser verifies an allowlisted Telegram user is
// answered like the owner.
func TestPersonalBotServesAllowlistedUser(t *testing.T) {
	ai := draftingAI()
	botSender := &fakeSender{}
	repo := personalBotService(t, ai, botSender, true)
	repo.AddBotAllowedUser("tenant:user:77", 555, "42")
	service := newPersonalBotService(repo, ai, botSender)

	result, err := service.HandleBotMessage(context.Background(), botEvent(t, 9720, 42, 320, "привет"))
	if err != nil {
		t.Fatalf("allowlisted message: %v", err)
	}
	if ai.calls != 1 {
		t.Fatalf("allowlisted sender must reach the AI, calls=%d", ai.calls)
	}
	if len(botSender.sent) != 0 {
		t.Fatalf("allowlisted sender must not get a stub, got %d", len(botSender.sent))
	}
	if result.Decision != contracts.DecisionDraft {
		t.Fatalf("allowlisted decision = %s, want draft", result.Decision)
	}
}

// TestPersonalBotOffServesEveryone guards the regression surface: with the toggle
// off, a stranger is answered normally (prior multi-user bot behaviour).
func TestPersonalBotOffServesEveryone(t *testing.T) {
	ai := draftingAI()
	botSender := &fakeSender{}
	repo := personalBotService(t, ai, botSender, false) // personal mode OFF
	service := newPersonalBotService(repo, ai, botSender)

	result, err := service.HandleBotMessage(context.Background(), botEvent(t, 9730, 42, 330, "привет"))
	if err != nil {
		t.Fatalf("stranger message: %v", err)
	}
	if ai.calls != 1 {
		t.Fatalf("with personal mode off, stranger must reach the AI, calls=%d", ai.calls)
	}
	if result.Decision != contracts.DecisionDraft {
		t.Fatalf("decision = %s, want draft", result.Decision)
	}
}
