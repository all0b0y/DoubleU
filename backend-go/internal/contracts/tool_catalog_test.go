package contracts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestToolCatalogMatchesCanonicalPythonCatalog(t *testing.T) {
	aiPythonDir := filepath.Join("..", "..", "..", "ai-python")
	command := exec.Command(
		"python3", "-c",
		`import ast, json, pathlib
tree = ast.parse(pathlib.Path("ai_agent/agents/tool_catalog.py").read_text())
entries = []
for node in ast.walk(tree):
    if isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id == "ToolCatalogEntry":
        fields = {item.arg: item.value for item in node.keywords}
        entries.append({"key": ast.literal_eval(fields["key"]), "category": fields["category"].attr.lower()})
print(json.dumps(entries))`,
	)
	command.Dir = aiPythonDir
	command.Env = append(os.Environ(), "PYTHONPATH=.")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("load canonical Python tool catalog: %v: %s", err, output)
	}
	var canonical []struct {
		Key      string       `json:"key"`
		Category ToolCategory `json:"category"`
	}
	if err := json.Unmarshal(output, &canonical); err != nil {
		t.Fatalf("decode canonical Python tool catalog: %v", err)
	}
	goCatalog := ToolCatalog()
	if len(goCatalog) != len(canonical) {
		t.Fatalf("catalog length: Go=%d Python=%d", len(goCatalog), len(canonical))
	}
	for index, want := range canonical {
		got := goCatalog[index]
		if got.Key != want.Key || got.Category != want.Category {
			t.Errorf("catalog[%d]: Go=(%q,%q) Python=(%q,%q)", index, got.Key, got.Category, want.Key, want.Category)
		}
	}
}

func TestInteractionToolDefaultsDisabled(t *testing.T) {
	if ToolEnabled(nil, "interaction.ask_questions") {
		t.Fatal("interaction tool must require explicit role enablement")
	}
	if !ToolEnabled(map[string]bool{"interaction.ask_questions": true}, "interaction.ask_questions") {
		t.Fatal("explicit interaction enablement ignored")
	}
}
