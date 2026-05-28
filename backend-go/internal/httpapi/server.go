// Package httpapi exposes the backend HTTP API.
package httpapi

import (
	"context"
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

// WithMiniAppDir configures the directory of Mini App static files to serve.
func WithMiniAppDir(dir string) ServerOption {
	return func(s *Server) { s.miniappDir = dir }
}

// Server exposes health, Telegram webhook, approval callback, and Mini App API handlers.
type Server struct {
	service    *core.Service
	mux        *http.ServeMux
	hub        *SSEHub
	botToken   string
	miniappDir string
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
	s.mux.HandleFunc("GET /api/v1/chats/{chat_id}/settings", s.tmaAuth(s.apiGetChatSettings))
	s.mux.HandleFunc("PATCH /api/v1/chats/{chat_id}/settings", s.tmaAuth(s.apiUpdateChatSettings))
	s.mux.HandleFunc("GET /api/v1/contacts", s.tmaAuth(s.apiContacts))
	s.mux.HandleFunc("GET /api/v1/chats/{chat_id}/profile", s.tmaAuth(s.apiGetProfile))
	s.mux.HandleFunc("PUT /api/v1/chats/{chat_id}/profile", s.tmaAuth(s.apiSaveProfile))
	s.mux.HandleFunc("GET /api/v1/chats/{chat_id}/avatar", s.tmaAuth(s.apiGetChatAvatar))

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
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	tenantHint := r.Header.Get("X-Tenant-ID")
	// Ack immediately so Telegram does not retry within the 60s window.
	// The duplicate-receive gate in HandleTelegramWebhook ensures any concurrent
	// re-delivery is dropped once processing has started.
	w.WriteHeader(http.StatusOK)
	go func() {
		_, _ = s.service.HandleTelegramWebhook(context.Background(), raw, tenantHint)
	}()
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
