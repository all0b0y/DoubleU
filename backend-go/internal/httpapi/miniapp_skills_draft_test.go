package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/store"
)

// TestDraftSkillFromTextDoesNotPersist verifies the authoring endpoint returns
// an editable draft (name/description/body) from free text and never saves it.
func TestDraftSkillFromTextDoesNotPersist(t *testing.T) {
	var mu sync.Mutex
	requestedText := ""
	aiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ai/draft-skill" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		mu.Lock()
		requestedText = request.Text
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"name": "работа-с-возражениями", "description": "Как отвечать на ценовые возражения.",
			"body": "## Когда применять\nКлиент сомневается.\n\n## Процедура\n1. Уточни.",
		})
	}))
	defer aiServer.Close()

	repo := store.NewRepository()
	server := NewServer(core.NewService(repo, nil, nil), WithAIBaseURL(aiServer.URL))
	res := miniappRequest(t, server, http.MethodPost, "/api/v1/skills/draft",
		`{"text":"Как отвечать на возражения по цене"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("draft status = %d body=%s", res.Code, res.Body.String())
	}
	var draft skillJSON
	decodeBody(t, res, &draft)
	if draft.ID != 0 || draft.Name != "работа-с-возражениями" || draft.Body == "" || !draft.Enabled {
		t.Fatalf("draft = %+v", draft)
	}
	mu.Lock()
	gotText := requestedText
	mu.Unlock()
	if gotText != "Как отвечать на возражения по цене" {
		t.Fatalf("AI draft text = %q", gotText)
	}
	// Drafting must not persist a skill.
	if skills := repo.SkillsForTenant(testTenant); len(skills) != 0 {
		t.Fatalf("draft endpoint persisted skills: %+v", skills)
	}
}

// TestDraftSkillRequiresExactlyOneSource rejects requests that give zero or
// multiple sources (text/audio/chat).
func TestDraftSkillRequiresExactlyOneSource(t *testing.T) {
	repo := store.NewRepository()
	server := NewServer(core.NewService(repo, nil, nil), WithAIBaseURL("http://unused"))
	for _, body := range []string{`{}`, `{"text":"a","chat_id":"c1"}`} {
		res := miniappRequest(t, server, http.MethodPost, "/api/v1/skills/draft", body)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("body %s → status %d, want 400", body, res.Code)
		}
	}
}
