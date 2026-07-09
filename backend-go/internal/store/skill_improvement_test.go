package store

import "testing"

// TestSkillUsageAttributionAndSignal verifies that a turn's loads get the owner's
// rating attributed to every skill loaded that turn (idempotently), and that the
// per-skill signal aggregates loads and up/down tallies.
func TestSkillUsageAttributionAndSignal(t *testing.T) {
	const tenant, chat, msg = "tenant:user:1", "bot:1:5", "m-42"
	repo := NewRepository()
	repo.RecordSkillLoad(tenant, 7, chat, msg)
	repo.RecordSkillLoad(tenant, 8, chat, msg) // two skills, same turn
	repo.RecordSkillLoad(tenant, 7, chat, "m-99")

	if n := repo.RecordSkillFeedbackForTurn(tenant, chat, msg, "down"); n != 2 {
		t.Fatalf("stamped = %d, want 2 (both skills of turn m-42)", n)
	}
	// Idempotent: already-rated rows are not re-stamped.
	if n := repo.RecordSkillFeedbackForTurn(tenant, chat, msg, "up"); n != 0 {
		t.Fatalf("re-stamp = %d, want 0", n)
	}
	if s := repo.SkillSignal(7); s.Loads != 2 || s.Negative != 1 || s.Positive != 0 {
		t.Fatalf("signal(7) = %+v, want loads=2 negative=1", s)
	}
	if s := repo.SkillSignal(8); s.Loads != 1 || s.Negative != 1 {
		t.Fatalf("signal(8) = %+v, want loads=1 negative=1", s)
	}
}

// TestApplyCuratedSkillRecordsCuratorVersion verifies the autonomous-curator
// apply path snapshots the change as a 'curator'-authored version.
func TestApplyCuratedSkillRecordsCuratorVersion(t *testing.T) {
	repo := NewRepository()
	if err := repo.ApplyCuratedSkill("tenant:user:1", 5, "improved body"); err != nil {
		t.Fatalf("apply curated: %v", err)
	}
	vers := repo.SkillVersions(5)
	if len(vers) != 1 || vers[0].Author != "curator" || vers[0].Body != "improved body" {
		t.Fatalf("versions = %+v, want one curator version", vers)
	}
}

// TestAIRoleAutoImproveSkillsRoundTrips checks the curator opt-in flag persists.
func TestAIRoleAutoImproveSkillsRoundTrips(t *testing.T) {
	repo := NewRepository()
	saved := repo.SaveAIRole(AIRoleRecord{
		TenantID: "tenant:user:1", Name: "curated-role", Description: "d",
		AutoImproveSkills: true,
	})
	got, ok := repo.AIRoleByID("tenant:user:1", saved.ID)
	if !ok || !got.AutoImproveSkills {
		t.Fatalf("AutoImproveSkills round-trip failed: ok=%v got=%+v", ok, got)
	}
}

// TestSkillVersionsSaveDedupAndList covers snapshot dedup and newest-first order.
func TestSkillVersionsSaveDedupAndList(t *testing.T) {
	repo := NewRepository()
	if v1 := repo.SaveSkillVersion(3, "body one", "owner"); v1 == 0 {
		t.Fatalf("first version not saved")
	}
	if dup := repo.SaveSkillVersion(3, "body one", "owner"); dup != 0 {
		t.Fatalf("identical body should be skipped, got %d", dup)
	}
	if v2 := repo.SaveSkillVersion(3, "body two", "curator"); v2 == 0 {
		t.Fatalf("changed body not saved")
	}
	vers := repo.SkillVersions(3)
	if len(vers) != 2 || vers[0].Body != "body two" || vers[0].Author != "curator" {
		t.Fatalf("versions = %+v, want newest-first [body two(curator), body one(owner)]", vers)
	}
}
