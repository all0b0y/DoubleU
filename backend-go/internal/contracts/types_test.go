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
			"interaction.present_buttons": true,
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

func TestAIButtonCallbackDataRoundTrip(t *testing.T) {
	draftID := "draft:9002:bot:555:8"
	data := CallbackDataEncodeAIButton(draftID, 1)
	option, decodedDraft, idem, ok := CallbackDataDecodeAIButton(data)
	if !ok || option != 1 || decodedDraft != draftID || idem == "" {
		t.Fatalf("decode %q = option=%d draft=%q idem=%q ok=%v", data, option, decodedDraft, idem, ok)
	}
	if len(data) > 64 {
		t.Fatalf("callback_data exceeds Telegram limit: %d", len(data))
	}
	if _, _, _, ok := CallbackDataDecodeAIButton(data + "tampered"); ok {
		t.Fatal("tampered callback data accepted")
	}
}

func TestAIProcessMessageResponsePlanJSONRoundTrip(t *testing.T) {
	want := AIProcessMessageResponse{
		Decision: DecisionDraft, ResponseText: "Выберите время",
		ResponsePlan: &ResponsePlan{Shape: "buttons", Options: []string{"Сегодня", "Завтра"}},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal response plan: %v", err)
	}
	var got AIProcessMessageResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal response plan: %v", err)
	}
	if got.ResponsePlan == nil || got.ResponsePlan.Shape != "buttons" || len(got.ResponsePlan.Options) != 2 {
		t.Fatalf("response plan round-trip = %+v", got.ResponsePlan)
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
