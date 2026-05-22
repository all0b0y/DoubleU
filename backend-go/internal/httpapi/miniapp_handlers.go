package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

type connectionInfo struct {
	Status   string `json:"status"` // "connected", "revoked", "not_found"
	CanReply bool   `json:"can_reply"`
}

type meResponse struct {
	UserID     string         `json:"user_id"`
	TenantID   string         `json:"tenant_id"`
	Connection connectionInfo `json:"connection"`
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	userID := strings.TrimPrefix(tenantID, "tenant:user:")
	conn, ok := s.service.Repository().BusinessConnectionByUser(userID)
	info := connectionInfo{Status: "not_found"}
	if ok {
		if conn.IsEnabled {
			info.Status = "connected"
		} else {
			info.Status = "revoked"
		}
		info.CanReply = conn.CanReply
	}
	writeJSON(w, http.StatusOK, meResponse{
		UserID:     userID,
		TenantID:   tenantID,
		Connection: info,
	})
}

type chatListItem struct {
	ChatID       string   `json:"chat_id"`
	DisplayName  string   `json:"display_name"`
	Mode         string   `json:"mode"`
	PrivacyFlags []string `json:"privacy_flags"`
	DraftCount   int      `json:"draft_count"`
	LastMessage  string   `json:"last_message,omitempty"`
	LastSeenAt   string   `json:"last_seen_at,omitempty"`
}

func (s *Server) apiChats(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chats := s.service.Repository().ChatsForTenant(tenantID)
	items := make([]chatListItem, 0, len(chats))
	for _, chat := range chats {
		settings := s.service.Repository().ChatSettings(tenantID, chat.ChatID)
		drafts := s.service.Repository().DraftsForChat(tenantID, chat.ChatID)
		pending := 0
		for _, d := range drafts {
			if d.Status == store.DraftPending {
				pending++
			}
		}
		lastMsg := ""
		if msgs := s.service.Repository().RecentMessages(tenantID, chat.ChatID, 1); len(msgs) > 0 {
			lastMsg = truncateText(msgs[0].Event.MessageText, 80)
		}
		flags := privacyFlagsToStrings(settings.PrivacyFlags)
		items = append(items, chatListItem{
			ChatID:       chat.ChatID,
			DisplayName:  chat.DisplayName,
			Mode:         string(settings.Mode),
			PrivacyFlags: flags,
			DraftCount:   pending,
			LastMessage:  lastMsg,
			LastSeenAt:   chat.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": items})
}

func (s *Server) apiAddChats(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	defer r.Body.Close()
	var req struct {
		ChatIDs []string `json:"chat_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	claimed := make([]string, 0, len(req.ChatIDs))
	for _, chatID := range req.ChatIDs {
		chat, ok := s.service.Repository().ChatBySelector("", chatID)
		if !ok {
			continue
		}
		s.service.Repository().ClaimChatForTenant(tenantID, chat)
		claimed = append(claimed, chatID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"claimed": claimed})
}

func (s *Server) apiMessages(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}
	msgs := s.service.Repository().RecentMessages(tenantID, chatID, 50)
	type msgItem struct {
		ID        string `json:"id"`
		Text      string `json:"text"`
		SenderID  string `json:"sender_id"`
		Type      string `json:"type"`
		CreatedAt string `json:"created_at"`
	}
	items := make([]msgItem, 0, len(msgs))
	for _, m := range msgs {
		items = append(items, msgItem{
			ID:        m.Event.SourceMessageID,
			Text:      m.Event.MessageText,
			SenderID:  m.Event.SenderID,
			Type:      m.Event.MessageType,
			CreatedAt: m.Event.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": items})
}

func (s *Server) apiDrafts(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}
	drafts := s.service.Repository().DraftsForChat(tenantID, chatID)
	type draftItem struct {
		DraftID         string `json:"draft_id"`
		SourceMessageID string `json:"source_message_id"`
		SourceText      string `json:"source_text"`
		ResponseText    string `json:"response_text"`
		Status          string `json:"status"`
		CreatedAt       string `json:"created_at"`
	}
	items := make([]draftItem, 0, len(drafts))
	for _, d := range drafts {
		items = append(items, draftItem{
			DraftID:         d.DraftID,
			SourceMessageID: d.SourceMessageID,
			SourceText:      d.SourceMessageText,
			ResponseText:    d.ResponseText,
			Status:          string(d.Status),
			CreatedAt:       d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafts": items})
}

func (s *Server) apiDraftAction(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	draftID := r.PathValue("draft_id")
	if chatID == "" || draftID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing path parameters"})
		return
	}
	defer r.Body.Close()
	var req struct {
		Action     string `json:"action"`
		EditedText string `json:"edited_text,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	action := contracts.ApprovalAction(req.Action)
	if !action.IsValid() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid action"})
		return
	}
	draft, ok := s.service.Repository().Draft(draftID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "draft not found"})
		return
	}
	payload := contracts.ApprovalCallbackPayload{
		Action:          action,
		TenantID:        tenantID,
		ChatID:          chatID,
		SourceMessageID: draft.SourceMessageID,
		DraftID:         draftID,
		DecisionID:      draft.DecisionID,
		IdempotencyKey:  draftID + ":" + req.Action + ":miniapp",
		EditedText:      req.EditedText,
	}
	result, err := s.service.HandleApprovalCallback(r.Context(), payload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) apiGetChatSettings(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}
	settings := s.service.Repository().ChatSettings(tenantID, chatID)
	modes := s.service.Repository().AvailableModesForChat(tenantID, chatID)
	modeStrs := make([]string, 0, len(modes))
	for _, m := range modes {
		modeStrs = append(modeStrs, string(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chat_id":         chatID,
		"mode":            string(settings.Mode),
		"privacy_flags":   privacyFlagsToStrings(settings.PrivacyFlags),
		"available_modes": modeStrs,
	})
}

func (s *Server) apiUpdateChatSettings(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}
	defer r.Body.Close()
	var req struct {
		Mode         string   `json:"mode,omitempty"`
		PrivacyFlags []string `json:"privacy_flags,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	settings := s.service.Repository().ChatSettings(tenantID, chatID)
	if req.Mode != "" {
		mode := contracts.ChatMode(req.Mode)
		if !mode.IsValid() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid mode"})
			return
		}
		settings.Mode = mode
	}
	if req.PrivacyFlags != nil {
		flags := make(contracts.PrivacyFlags, 0, len(req.PrivacyFlags))
		for _, f := range req.PrivacyFlags {
			flag := contracts.PrivacyFlag(f)
			if !flag.IsValid() {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid privacy flag: " + f})
				return
			}
			flags = append(flags, flag)
		}
		settings.PrivacyFlags = flags
	}
	s.service.Repository().SaveChatSettings(settings)
	if s.hub != nil {
		s.hub.Broadcast(tenantID, "chat_mode_changed", map[string]string{
			"chat_id": chatID,
			"mode":    string(settings.Mode),
		})
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) apiContacts(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	claimed := s.service.Repository().ChatsForTenant(tenantID)
	claimedIDs := make(map[string]struct{}, len(claimed))
	for _, c := range claimed {
		claimedIDs[c.ChatID] = struct{}{}
	}
	// Unclaimed business chats (observed but not yet assigned to any tenant).
	all := s.service.Repository().ChatsForTenant("")
	type contactItem struct {
		ChatID      string `json:"chat_id"`
		DisplayName string `json:"display_name"`
		LastSeenAt  string `json:"last_seen_at,omitempty"`
	}
	items := make([]contactItem, 0)
	for _, c := range all {
		if _, ok := claimedIDs[c.ChatID]; ok {
			continue
		}
		items = append(items, contactItem{
			ChatID:      c.ChatID,
			DisplayName: c.DisplayName,
			LastSeenAt:  c.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"contacts": items})
}

func privacyFlagsToStrings(flags contracts.PrivacyFlags) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		out = append(out, string(f))
	}
	return out
}

func truncateText(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
