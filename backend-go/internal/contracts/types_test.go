package contracts

import (
	"encoding/json"
	"testing"
	"time"
)

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

func TestAIRoleModelNameJSONRoundTrip(t *testing.T) {
	want := AIRole{
		Name:      "Support",
		ModelTier: "smart",
		ModelName: "deepseek/model-b",
		ToolSettings: map[string]bool{
			"interaction.ask_questions": true,
		},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal ai role: %v", err)
	}
	var got AIRole
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal ai role: %v", err)
	}
	if got.ModelTier != "smart" || got.ModelName != "deepseek/model-b" {
		t.Fatalf("ai role model round-trip = %+v", got)
	}
}

func TestQFormCallbackDataRoundTrip(t *testing.T) {
	data := CallbackDataEncodeQForm(9002, 2, 1)
	formID, qIdx, optIdx, freeText, idem, ok := CallbackDataDecodeQForm(data)
	if !ok || formID != 9002 || qIdx != 2 || optIdx != 1 || freeText || idem == "" {
		t.Fatalf("decode %q = form=%d q=%d opt=%d free=%v idem=%q ok=%v", data, formID, qIdx, optIdx, freeText, idem, ok)
	}
	if len(data) > 64 {
		t.Fatalf("callback_data exceeds Telegram limit: %d", len(data))
	}
	freeData := CallbackDataEncodeQFormFreeText(9002, 2)
	formID, qIdx, optIdx, freeText, idem, ok = CallbackDataDecodeQForm(freeData)
	if !ok || formID != 9002 || qIdx != 2 || optIdx != -1 || !freeText || idem == "" {
		t.Fatalf("decode free %q = form=%d q=%d opt=%d free=%v ok=%v", freeData, formID, qIdx, optIdx, freeText, ok)
	}
	if _, _, _, _, _, ok := CallbackDataDecodeQForm("qf:abc:1:2"); ok {
		t.Fatal("malformed callback data accepted")
	}
}

func TestAIProcessMessageResponsePlanJSONRoundTrip(t *testing.T) {
	want := AIProcessMessageResponse{
		Decision: DecisionDraft, ResponseText: "Уточню детали",
		ResponsePlan: &ResponsePlan{Shape: "questions", Questions: []Question{
			{Prompt: "Когда удобно?", Options: []string{"Сегодня", "Завтра"}},
			{Prompt: "Какой бюджет?"},
		}},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal response plan: %v", err)
	}
	var got AIProcessMessageResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal response plan: %v", err)
	}
	if got.ResponsePlan == nil || got.ResponsePlan.Shape != "questions" || len(got.ResponsePlan.Questions) != 2 {
		t.Fatalf("response plan round-trip = %+v", got.ResponsePlan)
	}
	if got.ResponsePlan.Questions[0].Prompt != "Когда удобно?" || len(got.ResponsePlan.Questions[0].Options) != 2 {
		t.Fatalf("question 0 round-trip = %+v", got.ResponsePlan.Questions[0])
	}
}

func TestAIProcessMessageScheduledJobsJSONRoundTrip(t *testing.T) {
	fireAt := time.Date(2026, 6, 22, 15, 0, 0, 0, time.UTC)
	want := AIProcessMessageResponse{
		Decision: DecisionDraft, ResponseText: "Напоминание поставлено",
		ScheduledJobs: []ScheduledJob{{
			Kind: "reminder", Target: "owner", FireAt: fireAt,
			Payload:   map[string]any{"text": "Позвонить клиенту"},
			CreatedBy: "ai", IdempotencyKey: "scheduler:key",
		}},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal scheduled jobs: %v", err)
	}
	var got AIProcessMessageResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal scheduled jobs: %v", err)
	}
	if len(got.ScheduledJobs) != 1 {
		t.Fatalf("scheduled jobs round-trip = %+v", got.ScheduledJobs)
	}
	job := got.ScheduledJobs[0]
	if job.Kind != "reminder" || job.Target != "owner" || !job.FireAt.Equal(fireAt) ||
		job.Payload["text"] != "Позвонить клиенту" || job.CreatedBy != "ai" ||
		job.IdempotencyKey != "scheduler:key" {
		t.Fatalf("scheduled job round-trip = %+v", job)
	}
}
