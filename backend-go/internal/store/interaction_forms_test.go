package store

import (
	"testing"

	"doubleu/backend-go/internal/contracts"
)

func sampleForm() InteractionFormRecord {
	return InteractionFormRecord{
		TenantID: "tenant:user:1",
		ChatID:   "bot:5:42",
		BotID:    "5",
		Questions: []contracts.Question{
			{Prompt: "Когда?", Options: []string{"Сегодня", "Завтра"}},
			{Prompt: "Бюджет?"},
		},
		SourceMessageID: "7",
	}
}

func TestInteractionFormLifecycle(t *testing.T) {
	repo := NewRepository()
	repo.SaveChat(ChatRecord{TenantID: "tenant:user:1", ChatID: "bot:5:42"})

	form, err := repo.CreateInteractionForm(sampleForm())
	if err != nil {
		t.Fatalf("create form: %v", err)
	}
	if form.ID == 0 || form.Status != "active" || form.CurrentIndex != 0 {
		t.Fatalf("new form = %+v", form)
	}

	active, ok := repo.ActiveInteractionForm("tenant:user:1", "bot:5:42")
	if !ok || active.ID != form.ID {
		t.Fatalf("active form lookup = %+v ok=%v", active, ok)
	}

	updated, ok := repo.RecordFormAnswer(form.ID, "Завтра")
	if !ok || updated.CurrentIndex != 1 || len(updated.Answers) != 1 {
		t.Fatalf("first answer = %+v ok=%v", updated, ok)
	}
	if updated.Answers[0].Prompt != "Когда?" || updated.Answers[0].Answer != "Завтра" {
		t.Fatalf("answer pairing = %+v", updated.Answers[0])
	}
	if !updated.QuestionsRemaining() {
		t.Fatal("one question should remain after the first answer")
	}

	final, ok := repo.RecordFormAnswer(form.ID, "До 100к")
	if !ok || final.QuestionsRemaining() {
		t.Fatalf("second answer should exhaust the pool: %+v ok=%v", final, ok)
	}
	if !repo.CompleteForm(form.ID) {
		t.Fatal("complete form")
	}
	if _, stillActive := repo.ActiveInteractionForm("tenant:user:1", "bot:5:42"); stillActive {
		t.Fatal("completed form must not be active")
	}
}

func TestInteractionFormOneActivePerChat(t *testing.T) {
	repo := NewRepository()
	repo.SaveChat(ChatRecord{TenantID: "tenant:user:1", ChatID: "bot:5:42"})

	first, err := repo.CreateInteractionForm(sampleForm())
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := repo.CreateInteractionForm(sampleForm())
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	// Creating a second form supersedes the first so only one stays active.
	active, ok := repo.ActiveInteractionForm("tenant:user:1", "bot:5:42")
	if !ok || active.ID != second.ID {
		t.Fatalf("active form = %+v, want id %d", active, second.ID)
	}
	if _, ok := repo.RecordFormAnswer(first.ID, "x"); ok {
		t.Fatal("superseded form must reject answers")
	}
}

func TestCancelFormStopsAnswers(t *testing.T) {
	repo := NewRepository()
	repo.SaveChat(ChatRecord{TenantID: "tenant:user:1", ChatID: "bot:5:42"})
	form, err := repo.CreateInteractionForm(sampleForm())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !repo.CancelForm(form.ID) {
		t.Fatal("cancel form")
	}
	if _, ok := repo.ActiveInteractionForm("tenant:user:1", "bot:5:42"); ok {
		t.Fatal("cancelled form must not be active")
	}
	if _, ok := repo.RecordFormAnswer(form.ID, "x"); ok {
		t.Fatal("cancelled form must reject answers")
	}
}
