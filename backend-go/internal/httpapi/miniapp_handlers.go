package httpapi

import (
	"encoding/json"
	"io"
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
	userID := strings.TrimPrefix(tenantID, "tenant:user:")
	conn, _ := s.service.Repository().BusinessConnectionByUser(userID)
	stagingTenant := "staging:" + conn.ConnectionID

	claimed := make([]string, 0, len(req.ChatIDs))
	for _, chatID := range req.ChatIDs {
		// Look for the chat in the staging tenant first, then fall back to global lookup.
		chat, ok := s.service.Repository().ChatBySelector(stagingTenant, chatID)
		if !ok {
			chat, ok = s.service.Repository().ChatBySelector("", chatID)
		}
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

	ownerUserID := strings.TrimPrefix(tenantID, "tenant:user:")
	chat, _ := s.service.Repository().ChatBySelector(tenantID, chatID)
	contactName := chat.DisplayName
	if contactName == "" {
		contactName = chatID
	}

	msgs := s.service.Repository().RecentMessages(tenantID, chatID, 50)
	type msgItem struct {
		ID          string `json:"id"`
		Text        string `json:"text"`
		MessageType string `json:"message_type"`
		SenderID    string `json:"sender_id"`
		SenderName  string `json:"sender_name"`
		Direction   string `json:"direction"`
		Label       string `json:"label,omitempty"` // set only for DoubleU-sent messages
		CreatedAt   string `json:"created_at"`
	}
	items := make([]msgItem, 0, len(msgs))
	for _, m := range msgs {
		direction := "incoming"
		senderName := contactName
		label := ""
		if m.Event.SenderID == ownerUserID {
			direction = "outgoing"
			senderName = "Вы"
			// Look up whether this message was sent through DoubleU.
			if draft, ok := s.service.Repository().DraftBySentMessageID(tenantID, chatID, m.Event.SourceMessageID); ok {
				label = doubleULabel(draft.ActionHistory)
			}
		}
		items = append(items, msgItem{
			ID:          m.Event.SourceMessageID,
			Text:        m.Event.MessageText,
			MessageType: m.Event.MessageType,
			SenderID:    m.Event.SenderID,
			SenderName:  senderName,
			Direction:   direction,
			Label:       label,
			CreatedAt:   m.Event.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
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
		"context_score":   s.service.Repository().ContextScoreForChat(tenantID, chatID),
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
	userID := strings.TrimPrefix(tenantID, "tenant:user:")

	// Staging chats are stored under "staging:<connectionID>" by the webhook handler.
	conn, ok := s.service.Repository().BusinessConnectionByUser(userID)
	type contactItem struct {
		ChatID      string `json:"chat_id"`
		DisplayName string `json:"display_name"`
		LastSeenAt  string `json:"last_seen_at,omitempty"`
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"contacts": []contactItem{}})
		return
	}

	stagingChats := s.service.Repository().StagingChatsForConnection(conn.ConnectionID)
	items := make([]contactItem, 0, len(stagingChats))
	for _, c := range stagingChats {
		items = append(items, contactItem{
			ChatID:      c.ChatID,
			DisplayName: c.DisplayName,
			LastSeenAt:  c.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"contacts": items})
}

func (s *Server) apiGetProfile(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}
	profile, _ := s.service.Repository().ContactProfile(tenantID, chatID)
	writeJSON(w, http.StatusOK, map[string]any{
		"chat_id":           chatID,
		"name":              profile.Name,
		"gender":            profile.Gender,
		"role":              profile.Role,
		"relationship_type": profile.RelationshipType,
		"notes":             profile.Notes,
	})
}

func (s *Server) apiSaveProfile(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}
	defer r.Body.Close()
	var req struct {
		Name             string `json:"name"`
		Gender           string `json:"gender"`
		Role             string `json:"role"`
		RelationshipType string `json:"relationship_type"`
		Notes            string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	profile := contracts.ContactProfile{
		TenantID:         tenantID,
		ChatID:           chatID,
		Name:             req.Name,
		Gender:           req.Gender,
		Role:             req.Role,
		RelationshipType: req.RelationshipType,
		Notes:            req.Notes,
	}
	s.service.Repository().SaveContactProfile(profile)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// doubleULabel returns the label shown on DoubleU-sent messages.
func doubleULabel(_ []string) string {
	return "DoubleU"
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

// apiGetChatAvatar proxies the contact's Telegram profile photo.
// Fetches the smallest available photo via getUserProfilePhotos + getFile and streams it back.
// Returns 204 No Content when the bot token is absent or the user has no photo.
func (s *Server) apiGetChatAvatar(w http.ResponseWriter, r *http.Request) {
	if s.botToken == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}

	// Step 1: get profile photos (limit=1, smallest size is index 0 of first set).
	photosURL := "https://api.telegram.org/bot" + s.botToken +
		"/getUserProfilePhotos?user_id=" + chatID + "&limit=1"
	resp, err := http.Get(photosURL) //nolint:noctx // avatar fetch is best-effort
	if err != nil || resp.StatusCode != http.StatusOK {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer resp.Body.Close()

	var photosResult struct {
		OK     bool `json:"ok"`
		Result struct {
			Photos [][]struct {
				FileID string `json:"file_id"`
			} `json:"photos"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&photosResult); err != nil ||
		!photosResult.OK || len(photosResult.Result.Photos) == 0 || len(photosResult.Result.Photos[0]) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	fileID := photosResult.Result.Photos[0][0].FileID

	// Step 2: resolve file_id → file_path.
	fileURL := "https://api.telegram.org/bot" + s.botToken + "/getFile?file_id=" + fileID
	resp2, err := http.Get(fileURL) //nolint:noctx
	if err != nil || resp2.StatusCode != http.StatusOK {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer resp2.Body.Close()

	var fileResult struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&fileResult); err != nil ||
		!fileResult.OK || fileResult.Result.FilePath == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Step 3: proxy the image bytes — never expose the bot token to the client.
	imgURL := "https://api.telegram.org/file/bot" + s.botToken + "/" + fileResult.Result.FilePath
	resp3, err := http.Get(imgURL) //nolint:noctx
	if err != nil || resp3.StatusCode != http.StatusOK {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer resp3.Body.Close()

	w.Header().Set("Content-Type", resp3.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp3.Body)
}
