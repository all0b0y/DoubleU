package core

import (
	"testing"
	"time"
)

// TestSkillCurationConfigDefaultsAndEnv verifies the curator tuning is env-driven
// with conservative defaults (no hardcoded policy).
func TestSkillCurationConfigDefaultsAndEnv(t *testing.T) {
	interval, minLoads, minNeg := skillCurationConfig()
	if interval != 30*time.Minute || minLoads != 15 || minNeg != 3 {
		t.Fatalf("defaults = %v/%d/%d, want 30m/15/3", interval, minLoads, minNeg)
	}

	t.Setenv("SKILL_CURATION_INTERVAL", "10m")
	t.Setenv("SKILL_CURATION_MIN_LOADS", "40")
	t.Setenv("SKILL_CURATION_MIN_NEGATIVE", "7")
	interval, minLoads, minNeg = skillCurationConfig()
	if interval != 10*time.Minute || minLoads != 40 || minNeg != 7 {
		t.Fatalf("env override = %v/%d/%d, want 10m/40/7", interval, minLoads, minNeg)
	}

	// Invalid / non-positive values fall back to the default.
	t.Setenv("SKILL_CURATION_MIN_LOADS", "0")
	t.Setenv("SKILL_CURATION_INTERVAL", "nonsense")
	interval, minLoads, _ = skillCurationConfig()
	if interval != 30*time.Minute || minLoads != 15 {
		t.Fatalf("invalid env not defaulted: %v/%d", interval, minLoads)
	}
}
