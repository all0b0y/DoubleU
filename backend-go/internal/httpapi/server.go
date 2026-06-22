// Package httpapi exposes the backend HTTP API.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/core"
)

// ServerOption configures an HTTP server.
type ServerOption func(*Server)

// WithHub attaches an SSE hub for real-time Mini App notifications.
func WithHub(hub *SSEHub) ServerOption {
	return func(s *Server) { s.hub = hub }
}

// WithBotToken sets the Telegram bot token used to validate TMA initData signatures.
func WithBotToken(token string) ServerOption {
	return func(s *Server) { s.botToken = token }
}

// WithWebhookSecret sets the secret token registered with Telegram via setWebhook.
// When non-empty, inbound webhook requests must carry a matching
// X-Telegram-Bot-Api-Secret-Token header or they are rejected.
func WithWebhookSecret(secret string) ServerOption {
	return func(s *Server) { s.webhookSecret = secret }
}

// WithMiniAppDir configures the directory of Mini App static files to serve.
func WithMiniAppDir(dir string) ServerOption {
	return func(s *Server) { s.miniappDir = dir }
}

// WithAIBaseURL sets the Python AI service base URL used by Mini App AI calls.
func WithAIBaseURL(url string) ServerOption {
	return func(s *Server) { s.aiBaseURL = url }
}

// Server exposes health, Telegram webhook, approval callback, and Mini App API handlers.
type Server struct {
	service       *core.Service
	mux           *http.ServeMux
	hub           *SSEHub
	bots          BotManager
	botToken      string
	webhookSecret string
	miniappDir    string
	aiBaseURL     string
}

// NewServer creates an HTTP server wrapper.
func NewServer(service *core.Service, opts ...ServerOption) *Server {
	server := &Server{service: service, mux: http.NewServeMux()}
	for _, opt := range opts {
		opt(server)
	}
	server.routes()
	return server
}

// Start launches background goroutines bound to the given context.
// Call this once after NewServer, before accepting requests.
func (s *Server) Start(ctx context.Context) {
	s.service.StartWebhookWorkers(ctx)
	go s.service.StartDailySummaryScheduler(ctx)
	go s.service.StartEpisodeCloser(ctx)
}

// ServeHTTP dispatches HTTP requests to API handlers.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// Health probes (no auth required)
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /readyz", s.ready)

	// Legacy webhook and admin endpoints (no auth required — secured by Telegram secret)
	s.mux.HandleFunc("GET /audit", s.audit)
	s.mux.HandleFunc("POST /chat-settings", s.chatSettings)
	s.mux.HandleFunc("POST /telegram/webhook", s.telegramWebhook)
	s.mux.HandleFunc("POST /approval/callback", s.approvalCallback)

	// Mini App API — all routes require TMA auth
	s.mux.HandleFunc("GET /api/v1/me", s.tmaAuth(s.apiMe))
	s.mux.HandleFunc("GET /api/v1/events", s.tmaAuth(s.serveSSE))
	s.mux.HandleFunc("GET /api/v1/chats", s.tmaAuth(s.apiChats))
	s.mux.HandleFunc("POST /api/v1/chats", s.tmaAuth(s.apiAddChats))
	s.mux.HandleFunc("GET /api/v1/chats/{chat_id}/messages", s.tmaAuth(s.apiMessages))
	s.mux.HandleFunc("GET /api/v1/chats/{chat_id}/drafts", s.tmaAuth(s.apiDrafts))
	s.mux.HandleFunc("POST /api/v1/chats/{chat_id}/drafts/{draft_id}/action", s.tmaAuth(s.apiDraftAction))
	s.mux.HandleFunc("POST /api/v1/chats/{chat_id}/send", s.tmaAuth(s.apiSendMessage))
	s.mux.HandleFunc("GET /api/v1/chats/{chat_id}/settings", s.tmaAuth(s.apiGetChatSettings))
	s.mux.HandleFunc("PATCH /api/v1/chats/{chat_id}/settings", s.tmaAuth(s.apiUpdateChatSettings))
	s.mux.HandleFunc("GET /api/v1/contacts", s.tmaAuth(s.apiContacts))
	s.mux.HandleFunc("GET /api/v1/chats/{chat_id}/profile", s.tmaAuth(s.apiGetProfile))
	s.mux.HandleFunc("PUT /api/v1/chats/{chat_id}/profile", s.tmaAuth(s.apiSaveProfile))
	s.mux.HandleFunc("GET /api/v1/chats/{chat_id}/avatar", s.tmaAuth(s.apiGetChatAvatar))

	// AI roles (personas) — CRUD plus per-chat assignment.
	s.mux.HandleFunc("GET /api/v1/tool-catalog", s.tmaAuth(s.apiToolCatalog))
	s.mux.HandleFunc("GET /api/v1/roles", s.tmaAuth(s.apiRoles))
	s.mux.HandleFunc("POST /api/v1/roles/draft", s.tmaAuth(s.apiDraftRole))
	s.mux.HandleFunc("POST /api/v1/roles", s.tmaAuth(s.apiCreateRole))
	s.mux.HandleFunc("PUT /api/v1/roles/{role_id}", s.tmaAuth(s.apiUpdateRole))
	s.mux.HandleFunc("PUT /api/v1/roles/{role_id}/tools", s.tmaAuth(s.apiUpdateRoleTools))
	s.mux.HandleFunc("DELETE /api/v1/roles/{role_id}", s.tmaAuth(s.apiDeleteRole))
	s.mux.HandleFunc("PUT /api/v1/chats/{chat_id}/role", s.tmaAuth(s.apiAssignChatRole))

	// AI profile suggestions: tenant-wide inbox and per-chat list.
	s.mux.HandleFunc("GET /api/v1/suggestions", s.tmaAuth(s.apiSuggestions))
	s.mux.HandleFunc("GET /api/v1/chats/{chat_id}/suggestions", s.tmaAuth(s.apiSuggestions))
	s.mux.HandleFunc("POST /api/v1/suggestions/{suggestion_id}/action", s.tmaAuth(s.apiSuggestionAction))

	// Memory: on-demand summary, facts, episode summaries, erasure.
	s.mux.HandleFunc("POST /api/v1/chats/{chat_id}/summary", s.tmaAuth(s.apiChatSummary))
	s.mux.HandleFunc("GET /api/v1/chats/{chat_id}/facts", s.tmaAuth(s.apiFacts))
	s.mux.HandleFunc("DELETE /api/v1/facts/{fact_id}", s.tmaAuth(s.apiDeleteFact))
	s.mux.HandleFunc("GET /api/v1/chats/{chat_id}/episodes", s.tmaAuth(s.apiEpisodes))
	s.mux.HandleFunc("POST /api/v1/chats/{chat_id}/forget", s.tmaAuth(s.apiForgetChat))
	s.mux.HandleFunc("POST /api/v1/forget-all", s.tmaAuth(s.apiForgetAll))

	// Owner-level settings ("Я" screen).
	s.mux.HandleFunc("GET /api/v1/settings/user", s.tmaAuth(s.apiGetUserSettings))
	s.mux.HandleFunc("PUT /api/v1/settings/user", s.tmaAuth(s.apiSaveUserSettings))

	// Bot ingestion settings (UI is the configuration surface; the bot is the
	// communication channel).
	s.mux.HandleFunc("GET /api/v1/bots", s.tmaAuth(s.apiListBots))
	s.mux.HandleFunc("POST /api/v1/bots", s.tmaAuth(s.apiRegisterBot))
	s.mux.HandleFunc("PUT /api/v1/bots/{bot_id}/role", s.tmaAuth(s.apiSetBotRole))
	s.mux.HandleFunc("DELETE /api/v1/bots/{bot_id}", s.tmaAuth(s.apiDisconnectBot))
	s.mux.HandleFunc("GET /api/v1/settings/ingestion-mode", s.tmaAuth(s.apiGetIngestionMode))
	s.mux.HandleFunc("PUT /api/v1/settings/ingestion-mode", s.tmaAuth(s.apiSetIngestionMode))

	// Mini App static files
	if s.miniappDir != "" {
		fs := http.FileServer(http.Dir(s.miniappDir))
		s.mux.Handle("GET /miniapp/", http.StripPrefix("/miniapp", fs))
		s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				http.Redirect(w, r, "/miniapp/", http.StatusMovedPermanently)
				return
			}
			http.NotFound(w, r)
		})
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenant_id")
	chatID := r.URL.Query().Get("chat_id")
	events := s.service.Repository().Audits()
	filtered := make([]contracts.AuditEvent, 0, len(events))
	for _, event := range events {
		if tenantID != "" && event.TenantID != tenantID {
			continue
		}
		if chatID != "" && event.ChatID != chatID {
			continue
		}
		filtered = append(filtered, event)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": filtered})
}

func (s *Server) chatSettings(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var settings contracts.ChatSettings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	settings = settings.WithDefaults()
	if settings.TenantID == "" || settings.ChatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id and chat_id are required"})
		return
	}
	if !settings.Mode.IsValid() || !settings.PrivacyFlags.IsValid() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid chat settings"})
		return
	}
	s.service.Repository().SaveChatSettings(settings)
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) telegramWebhook(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	// Authenticate the caller as Telegram: the secret token registered via
	// setWebhook is echoed back in this header on every legitimate delivery.
	// Without this check the endpoint accepts forged updates from anyone who
	// knows the public URL. Skipped only when no secret is configured (dev).
	if s.webhookSecret != "" {
		got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.webhookSecret)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid webhook secret"})
			return
		}
	}
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	tenantHint := r.Header.Get("X-Tenant-ID")
	// Ack immediately so Telegram does not retry within the 60s window.
	// Processing goes through the per-chat ordered worker pool (ARCH-7); the
	// duplicate-receive gate inside ensures re-deliveries are dropped. When the
	// shard and overflow queues are both saturated the update is shed with 429
	// so Telegram backs off and retries instead of us growing goroutines.
	if s.service.EnqueueTelegramWebhook(raw, tenantHint) {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusTooManyRequests)
}

func (s *Server) approvalCallback(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var payload contracts.ApprovalCallbackPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	result, err := s.service.HandleApprovalCallback(r.Context(), payload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
