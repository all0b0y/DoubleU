package contracts

import "testing"

func TestEnumValidationAndHelpers(t *testing.T) {
	if !DecisionAutoReply.IsValid() || Decision("bad").IsValid() {
		t.Fatalf("decision validation failed")
	}
	if !ChatModeSemiAuto.IsValid() || ChatMode("bad").IsValid() {
		t.Fatalf("chat mode validation failed")
	}
	if !PrivacyNoMemory.IsValid() || PrivacyFlag("bad").IsValid() {
		t.Fatalf("privacy flag validation failed")
	}
	if !RiskCritical.IsValid() || RiskLevel("bad").IsValid() {
		t.Fatalf("risk validation failed")
	}
	if !ApprovalApprove.IsValid() || ApprovalAction("bad").IsValid() {
		t.Fatalf("approval validation failed")
	}

	flags := PrivacyFlags{PrivacyNoMemory}.Add(PrivacyPrivate).Add(PrivacyNoMemory)
	if !flags.Contains(PrivacyNoMemory) || !flags.Contains(PrivacyPrivate) || len(flags) != 2 {
		t.Fatalf("privacy flags add/contains failed: %+v", flags)
	}
	if !flags.IsValid() || (PrivacyFlags{PrivacyFlag("bad")}).IsValid() {
		t.Fatalf("privacy flags validation failed")
	}
}

func TestContractKeysAndDefaults(t *testing.T) {
	event := NormalizedTelegramEvent{TelegramUpdateID: 123, BusinessConnectionID: "bc-1"}
	if event.ReceiveIdempotencyKey() != "123:bc-1" {
		t.Fatalf("receive key = %s", event.ReceiveIdempotencyKey())
	}
	if SendIdempotencyKey("t", "c", "m", "d") != "t:c:m:d" {
		t.Fatalf("send key mismatch")
	}
	settings := ChatSettings{}.WithDefaults()
	if settings.Mode != ChatModeManual || settings.PrivacyFlags == nil {
		t.Fatalf("defaults = %+v", settings)
	}
}
