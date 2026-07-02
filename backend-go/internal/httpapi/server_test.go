package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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

func TestMiniAppStaticRequiresRevalidation(t *testing.T) {
	dir := t.TempDir()
	apiDir := filepath.Join(dir, "api")
	if err := os.MkdirAll(apiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(apiDir, "client.js"), []byte("export const ok = true;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := NewServer(core.NewService(store.NewRepository(), nil, nil), WithMiniAppDir(dir))
	req := httptest.NewRequest(http.MethodGet, "/miniapp/api/client.js", nil)
	res := httptest.NewRecorder()

	server.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Cache-Control"); got != "no-cache, max-age=0, must-revalidate" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := res.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q", got)
	}
}

// TestMiniAppStaticStampsModuleGraph proves the cache-busting fix end to end:
// the HTML entrypoint and every JS module it pulls in are rewritten so their
// same-origin asset URLs carry a content version, and a request for a
// version-stamped URL still serves the underlying file. This is what forces a
// Telegram WebView pinned to a stale module bundle to re-fetch after a deploy.
func TestMiniAppStaticStampsModuleGraph(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("index.html", `<!DOCTYPE html><html><head>`+
		`<link rel="stylesheet" href="/miniapp/styles/main.css">`+
		`</head><body>`+
		`<script type="module" src="/miniapp/api/client.js"></script>`+
		`</body></html>`)
	mustWrite("api/client.js", `import { isBotRoleContextPolicy } from '/miniapp/domain/bot-role-context-policy.js';`+"\n"+
		`export const api = { route: () => navigate('/chats') };`+"\n")
	mustWrite("domain/bot-role-context-policy.js", "export const ok = true;\n")
	mustWrite("styles/main.css", "body { margin: 0 }\n")

	server := NewServer(core.NewService(store.NewRepository(), nil, nil), WithMiniAppDir(dir))

	get := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		res := httptest.NewRecorder()
		server.ServeHTTP(res, req)
		return res
	}

	// The HTML <script src>/<link href> are stamped...
	html := get("/miniapp/")
	if html.Code != http.StatusOK {
		t.Fatalf("index status = %d", html.Code)
	}
	verRe := regexp.MustCompile(`/miniapp/api/client\.js\?v=([0-9a-f]{12})`)
	m := verRe.FindStringSubmatch(html.Body.String())
	if m == nil {
		t.Fatalf("index.html script src not stamped:\n%s", html.Body.String())
	}
	version := m[1]
	if !strings.Contains(html.Body.String(), "/miniapp/styles/main.css?v="+version) {
		t.Fatalf("stylesheet href not stamped with %s:\n%s", version, html.Body.String())
	}

	// ...and so are the transitive imports inside the served JS, with the SAME
	// version — otherwise an unversioned transitive import would resolve to a
	// separately-cached (stale) module instance.
	js := get("/miniapp/api/client.js?v=" + version)
	if js.Code != http.StatusOK {
		t.Fatalf("versioned client.js status = %d", js.Code)
	}
	if !strings.Contains(js.Body.String(), "/miniapp/domain/bot-role-context-policy.js?v="+version) {
		t.Fatalf("transitive import not stamped with %s:\n%s", version, js.Body.String())
	}
	// Route strings and non-asset literals must be left untouched.
	if !strings.Contains(js.Body.String(), "navigate('/chats')") {
		t.Fatalf("unrelated string literal was rewritten:\n%s", js.Body.String())
	}

	// A different content version must be produced when an asset changes, so a
	// deploy actually busts the cache.
	if v2 := miniAppAssetVersion(dir); v2 != version {
		t.Fatalf("version unstable without change: %s vs %s", version, v2)
	}
	mustWrite("domain/bot-role-context-policy.js", "export const ok = false; // changed\n")
	if v3 := miniAppAssetVersion(dir); v3 == version {
		t.Fatalf("version did not change after asset edit: still %s", version)
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
