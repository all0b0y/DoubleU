package store

import (
	"testing"

	"doubleu/backend-go/internal/contracts"
)

// TestAutoApplyProfileSuggestionsAppliesAndIsRevertible verifies the auto-learn
// path: a pending suggestion is applied straight to the profile, the pre-change
// value is captured, re-running is a no-op (idempotent), the row is surfaced as
// revertible, and reverting restores the original value.
func TestAutoApplyProfileSuggestionsAppliesAndIsRevertible(t *testing.T) {
	const tenant, chat = "tenant:user:7", "bot:1:42"
	repo := NewRepository()
	// Seed an existing profile value so previous_value capture is meaningful.
	repo.SaveContactProfile(contactProfileWith(tenant, chat, "tone", "neutral"))

	id := repo.SaveProfileSuggestion(ProfileSuggestionRecord{
		TenantID: tenant, ChatID: chat, FieldName: "tone", SuggestedValue: "casual",
	})
	if id == 0 {
		t.Fatalf("failed to seed suggestion")
	}

	n, err := repo.AutoApplyProfileSuggestions(tenant, chat)
	if err != nil {
		t.Fatalf("auto-apply: %v", err)
	}
	if n != 1 {
		t.Fatalf("applied = %d, want 1", n)
	}
	if p, _ := repo.ContactProfile(tenant, chat); p.Tone != "casual" {
		t.Fatalf("tone = %q, want casual", p.Tone)
	}
	if pend := repo.PendingSuggestions(tenant, chat); len(pend) != 0 {
		t.Fatalf("pending after auto-apply = %d, want 0", len(pend))
	}

	// Idempotent: a second pass applies nothing.
	if n2, _ := repo.AutoApplyProfileSuggestions(tenant, chat); n2 != 0 {
		t.Fatalf("second auto-apply = %d, want 0", n2)
	}

	// Surfaced as revertible with the auto_applied marker.
	rev := repo.RevertableSuggestions(tenant, chat)
	if len(rev) != 1 || rev[0].ID != id || !rev[0].AutoApplied {
		t.Fatalf("revertable = %+v, want the auto-applied row", rev)
	}

	if err := repo.RevertSuggestion(tenant, id); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if p, _ := repo.ContactProfile(tenant, chat); p.Tone != "neutral" {
		t.Fatalf("tone after revert = %q, want neutral (restored)", p.Tone)
	}
	if rev := repo.RevertableSuggestions(tenant, chat); len(rev) != 0 {
		t.Fatalf("revertable after revert = %d, want 0", len(rev))
	}
}

// TestAutoApplyRestoresUnsetFieldOnRevert covers the case where the field had no
// prior value: reverting must clear it back to empty, not leave the new value.
func TestAutoApplyRestoresUnsetFieldOnRevert(t *testing.T) {
	const tenant, chat = "tenant:user:7", "bot:1:99"
	repo := NewRepository()

	id := repo.SaveProfileSuggestion(ProfileSuggestionRecord{
		TenantID: tenant, ChatID: chat, FieldName: "language", SuggestedValue: "ru",
	})
	if _, err := repo.AutoApplyProfileSuggestions(tenant, chat); err != nil {
		t.Fatalf("auto-apply: %v", err)
	}
	if p, _ := repo.ContactProfile(tenant, chat); p.Language != "ru" {
		t.Fatalf("language = %q, want ru", p.Language)
	}
	if err := repo.RevertSuggestion(tenant, id); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if p, _ := repo.ContactProfile(tenant, chat); p.Language != "" {
		t.Fatalf("language after revert = %q, want empty (previously unset)", p.Language)
	}
}

// TestRevertRejectsManuallyAcceptedSuggestion ensures only auto-applied rows are
// revertible — an owner-accepted suggestion must not be undoable via revert.
func TestRevertRejectsManuallyAcceptedSuggestion(t *testing.T) {
	const tenant, chat = "tenant:user:7", "bot:1:7"
	repo := NewRepository()
	id := repo.SaveProfileSuggestion(ProfileSuggestionRecord{
		TenantID: tenant, ChatID: chat, FieldName: "gender", SuggestedValue: "female",
	})
	if err := repo.ResolveSuggestion(tenant, id, true); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := repo.RevertSuggestion(tenant, id); err == nil {
		t.Fatalf("revert of a manually-accepted suggestion should fail")
	}
}

// TestAIRoleAutoLearnProfileRoundTrips verifies the role flag the auto-apply gate
// reads persists and reloads (default true when constructed without setting it).
func TestAIRoleAutoLearnProfileRoundTrips(t *testing.T) {
	repo := NewRepository()
	saved := repo.SaveAIRole(AIRoleRecord{
		TenantID: "tenant:user:7", Name: "manual-role", Description: "d",
		AutoLearnProfile: false,
	})
	got, ok := repo.AIRoleByID("tenant:user:7", saved.ID)
	if !ok {
		t.Fatalf("role not found")
	}
	if got.AutoLearnProfile {
		t.Fatalf("AutoLearnProfile = true, want false (round-trip)")
	}
}

// contactProfileWith builds a minimal profile with a single field set, for tests.
func contactProfileWith(tenant, chat, field, value string) contracts.ContactProfile {
	p := contracts.ContactProfile{TenantID: tenant, ChatID: chat}
	setProfileFieldValue(&p, field, value)
	return p
}
