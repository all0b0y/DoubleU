package contracts

import "testing"

func TestBotChatIDRoundTrip(t *testing.T) {
	id := BotChatID("555", "42")
	if id != "bot:555:42" {
		t.Fatalf("BotChatID = %q, want bot:555:42", id)
	}
	botID, raw, ok := ParseBotChatID(id)
	if !ok || botID != "555" || raw != "42" {
		t.Fatalf("ParseBotChatID(%q) = (%q,%q,%v)", id, botID, raw, ok)
	}
	if got := RawChatID(id); got != "42" {
		t.Fatalf("RawChatID = %q, want 42", got)
	}

	// Negative chat ids (groups/supergroups) survive the round trip.
	gid := BotChatID("555", "-100123456")
	if b, raw, ok := ParseBotChatID(gid); !ok || b != "555" || raw != "-100123456" {
		t.Fatalf("group chat round trip = (%q,%q,%v)", b, raw, ok)
	}

	// Personal/business ids are not bot-scoped: they must pass through untouched.
	if _, _, ok := ParseBotChatID("42"); ok {
		t.Fatal("plain chat id must not parse as bot-scoped")
	}
	if got := RawChatID("42"); got != "42" {
		t.Fatalf("RawChatID(plain) = %q, want 42", got)
	}
}
