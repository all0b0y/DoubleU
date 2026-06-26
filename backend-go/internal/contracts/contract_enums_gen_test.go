package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// contractJSON mirrors the subset of contracts/contracts.json that this test
// validates.  Keeping the struct local (instead of importing a JSON-parsing
// utility) keeps the test free of dependencies and explicit about what fields
// participate in the contract.
type contractJSON struct {
	Version              string             `json:"version"`
	ToolCategories       []string           `json:"tool_categories"`
	Decisions            []string           `json:"decisions"`
	ChatModes            []string           `json:"chat_modes"`
	PrivacyFlags         []string           `json:"privacy_flags"`
	RiskLevels           []string           `json:"risk_levels"`
	ApprovalActions      []string           `json:"approval_actions"`
	MessageStates        []string           `json:"message_states"`
	ConfidenceThresholds map[string]float64 `json:"confidence_thresholds"`
}

func loadContract(t *testing.T) contractJSON {
	t.Helper()
	// The package lives at backend-go/internal/contracts; contracts.json is
	// four directories up.
	path := filepath.Join("..", "..", "..", "contracts", "contracts.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read contracts.json: %v", err)
	}
	var c contractJSON
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("parse contracts.json: %v", err)
	}
	return c
}

func sliceToSet[T ~string](in []T) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, v := range in {
		out[string(v)] = struct{}{}
	}
	return out
}

func assertSameSet(t *testing.T, name string, got map[string]struct{}, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: count mismatch — generated=%d contract=%d", name, len(got), len(want))
	}
	for _, v := range want {
		if _, ok := got[v]; !ok {
			t.Errorf("%s: contract value %q missing from generated code", name, v)
		}
	}
}

// TestGeneratedEnumsMatchContractJSON guards against drift between
// contracts/contracts.json and the generated Go enums.  If this test fails,
// run `python3 scripts/generate_contracts.py` and commit the result.
func TestGeneratedEnumsMatchContractJSON(t *testing.T) {
	contract := loadContract(t)
	if contract.Version != ContractVersion {
		t.Errorf("contract version drift: generated=%q contract=%q", ContractVersion, contract.Version)
	}
	assertSameSet(t, "decisions", sliceToSet(AllDecisions), contract.Decisions)
	assertSameSet(t, "tool_categories", sliceToSet(AllToolCategories), contract.ToolCategories)
	assertSameSet(t, "chat_modes", sliceToSet(AllChatModes), contract.ChatModes)
	assertSameSet(t, "privacy_flags", sliceToSet(AllPrivacyFlags), contract.PrivacyFlags)
	assertSameSet(t, "risk_levels", sliceToSet(AllRiskLevels), contract.RiskLevels)
	assertSameSet(t, "approval_actions", sliceToSet(AllApprovalActions), contract.ApprovalActions)
	assertSameSet(t, "message_states", sliceToSet(AllMessageStates), contract.MessageStates)
	if got, want := SemiAutoMinConfidence, contract.ConfidenceThresholds["semi_auto_min"]; got != want {
		t.Errorf("semi_auto_min: generated=%v contract=%v", got, want)
	}
	if got, want := AutoMinConfidence, contract.ConfidenceThresholds["auto_min"]; got != want {
		t.Errorf("auto_min: generated=%v contract=%v", got, want)
	}
}
