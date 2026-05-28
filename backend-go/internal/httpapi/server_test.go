package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/store"
)

func TestHealthEndpoints(t *testing.T) {
	server := NewServer(core.NewService(store.NewRepository(), nil, nil))

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		res := httptest.NewRecorder()
		server.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, res.Code)
		}
	}
}

func TestTelegramWebhookRoute(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	server := NewServer(core.NewService(repository, routeAI{}, nil))
	body := `{"update_id":1,"business_message":{"message_id":2,"business_connection_id":"bc","chat":{"id":42},"from":{"id":7},"text":"hi","date":1700000000}}`
	req := httptest.NewRequest(http.MethodPost, "/telegram/webhook", strings.NewReader(body))
	req.Header.Set("X-Tenant-ID", "tenant-1")
	res := httptest.NewRecorder()

	server.ServeHTTP(res, req)
	// Webhook ACKs immediately with 200 and processes in a background goroutine.
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
}

func TestChatSettingsAndAuditRoutes(t *testing.T) {
	repository := store.NewRepository()
	server := NewServer(core.NewService(repository, routeAI{}, nil))
	settingsBody := `{"tenant_id":"tenant-1","chat_id":"42","mode":"auto","privacy_flags":["no_training"]}`
	settingsReq := httptest.NewRequest(http.MethodPost, "/chat-settings", strings.NewReader(settingsBody))
	settingsRes := httptest.NewRecorder()

	server.ServeHTTP(settingsRes, settingsReq)
	if settingsRes.Code != http.StatusOK {
		t.Fatalf("settings status = %d body=%s", settingsRes.Code, settingsRes.Body.String())
	}
	if got := repository.ChatSettings("tenant-1", "42"); got.Mode != contracts.ChatModeAuto {
		t.Fatalf("mode = %s, want auto", got.Mode)
	}

	body := `{"update_id":3,"business_message":{"message_id":4,"business_connection_id":"bc","chat":{"id":42},"from":{"id":7},"text":"hi","date":1700000000}}`
	webhookReq := httptest.NewRequest(http.MethodPost, "/telegram/webhook", strings.NewReader(body))
	webhookReq.Header.Set("X-Tenant-ID", "tenant-1")
	webhookRes := httptest.NewRecorder()
	server.ServeHTTP(webhookRes, webhookReq)
	if webhookRes.Code != http.StatusOK {
		t.Fatalf("webhook status = %d", webhookRes.Code)
	}

	auditReq := httptest.NewRequest(http.MethodGet, "/audit?tenant_id=tenant-1&chat_id=42", nil)
	auditRes := httptest.NewRecorder()
	server.ServeHTTP(auditRes, auditReq)
	if auditRes.Code != http.StatusOK {
		t.Fatalf("audit status = %d", auditRes.Code)
	}
	if !strings.Contains(auditRes.Body.String(), `"events"`) {
		t.Fatalf("audit body = %s", auditRes.Body.String())
	}
}

func TestApprovalCallbackRouteBadRequest(t *testing.T) {
	server := NewServer(core.NewService(store.NewRepository(), nil, nil))
	req := httptest.NewRequest(http.MethodPost, "/approval/callback", strings.NewReader(`{"action":"approve"}`))
	res := httptest.NewRecorder()

	server.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", res.Code)
	}
}

type routeAI struct{}

func (routeAI) ProcessMessage(ctx context.Context, request contracts.AIProcessMessageRequest) (contracts.AIProcessMessageResponse, error) {
	return contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "draft",
		Confidence:   0.8,
		RiskLevel:    contracts.RiskLow,
	}, nil
}
