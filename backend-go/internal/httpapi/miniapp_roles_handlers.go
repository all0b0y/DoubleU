package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"doubleu/backend-go/internal/bot"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/store"
)

// roleContextMaxRunes caps owner-supplied role context so the draft prompt
// stays within the AI service token budget.
const roleContextMaxRunes = 20000

// roleCustomFieldMaxRunes caps a single custom field's context, mirroring the
// whole-role context cap so the prompt stays within budget.
const roleCustomFieldMaxRunes = 5000

// roleJSON is the wire shape of an AI role in the Mini App API.
type roleJSON struct {
	ID            int64                        `json:"id"`
	Name          string                       `json:"name"`
	Description   string                       `json:"description"`
	SpeakingStyle string                       `json:"speaking_style"`
	Goals         string                       `json:"goals"`
	Constraints   string                       `json:"constraints"`
	Context       string                       `json:"context"`
	CustomFields  []contracts.AIRoleCustomField `json:"custom_fields"`
}

func roleToJSON(role store.AIRoleRecord) roleJSON {
	fields := role.CustomFields
	if fields == nil {
		// Always emit an array so the Mini App can iterate without a nil guard.
		fields = []contracts.AIRoleCustomField{}
	}
	return roleJSON{
		ID:            role.ID,
		Name:          role.Name,
		Description:   role.Description,
		SpeakingStyle: role.SpeakingStyle,
		Goals:         role.Goals,
		Constraints:   role.Constraints,
		Context:       role.Context,
		CustomFields:  fields,
	}
}

// roleRequest is the create/update payload; field trimming happens here so the
// store always receives normalised values.
type roleRequest struct {
	Name          string                        `json:"name"`
	Description   string                        `json:"description"`
	SpeakingStyle string                        `json:"speaking_style"`
	Goals         string                        `json:"goals"`
	Constraints   string                        `json:"constraints"`
	Context       string                        `json:"context"`
	CustomFields  []contracts.AIRoleCustomField `json:"custom_fields"`
}

func (req *roleRequest) normalize() {
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
	req.SpeakingStyle = strings.TrimSpace(req.SpeakingStyle)
	req.Goals = strings.TrimSpace(req.Goals)
	req.Constraints = strings.TrimSpace(req.Constraints)
	req.Context = strings.TrimSpace(req.Context)
	// Drop fully-empty rows (the UI may leave a trailing blank one) and trim the rest.
	kept := req.CustomFields[:0]
	for _, f := range req.CustomFields {
		f.Title = strings.TrimSpace(f.Title)
		f.Description = strings.TrimSpace(f.Description)
		f.Context = strings.TrimSpace(f.Context)
		if f.Title == "" && f.Description == "" && f.Context == "" {
			continue
		}
		kept = append(kept, f)
	}
	req.CustomFields = kept
}

func (req *roleRequest) validate() string {
	if req.Name == "" {
		return "name is required"
	}
	if len([]rune(req.Name)) > 64 {
		return "name must be at most 64 characters"
	}
	if req.Description == "" {
		return "description is required"
	}
	if len([]rune(req.Context)) > roleContextMaxRunes {
		return "context is too long (max 20000 characters)"
	}
	for _, f := range req.CustomFields {
		// Each custom field needs a title and a "why it exists" description;
		// context is optional. Mirrors the Mini App role-edit form rules.
		if f.Title == "" || f.Description == "" {
			return "each custom field needs a title and a description"
		}
		if len([]rune(f.Title)) > 64 {
			return "custom field title must be at most 64 characters"
		}
		if len([]rune(f.Context)) > roleCustomFieldMaxRunes {
			return "custom field context is too long (max 5000 characters)"
		}
	}
	return ""
}

func (s *Server) apiRoles(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	roles := s.service.Repository().AIRoles(tenantID)
	items := make([]roleJSON, 0, len(roles))
	for _, role := range roles {
		items = append(items, roleToJSON(role))
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": items})
}

func (s *Server) apiCreateRole(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	defer r.Body.Close()
	var req roleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	req.normalize()
	if msg := req.validate(); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	if _, exists := s.service.Repository().AIRoleByName(tenantID, req.Name); exists {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "role with this name already exists"})
		return
	}
	role := s.service.Repository().SaveAIRole(store.AIRoleRecord{
		TenantID:      tenantID,
		Name:          req.Name,
		Description:   req.Description,
		SpeakingStyle: req.SpeakingStyle,
		Goals:         req.Goals,
		Constraints:   req.Constraints,
		Context:       req.Context,
		CustomFields:  req.CustomFields,
	})
	if role.ID == 0 {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save role"})
		return
	}
	writeJSON(w, http.StatusOK, roleToJSON(role))
}

func (s *Server) apiUpdateRole(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	roleID, err := strconv.ParseInt(r.PathValue("role_id"), 10, 64)
	if err != nil || roleID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role_id"})
		return
	}
	defer r.Body.Close()
	var req roleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	req.normalize()
	if msg := req.validate(); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	if _, ok := s.service.Repository().AIRoleByID(tenantID, roleID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role not found"})
		return
	}
	// A rename must not collide with another existing role.
	if other, exists := s.service.Repository().AIRoleByName(tenantID, req.Name); exists && other.ID != roleID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "role with this name already exists"})
		return
	}
	role := store.AIRoleRecord{
		ID:            roleID,
		TenantID:      tenantID,
		Name:          req.Name,
		Description:   req.Description,
		SpeakingStyle: req.SpeakingStyle,
		Goals:         req.Goals,
		Constraints:   req.Constraints,
		Context:       req.Context,
		CustomFields:  req.CustomFields,
	}
	if !s.service.Repository().UpdateAIRole(role) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update role"})
		return
	}
	writeJSON(w, http.StatusOK, roleToJSON(role))
}

func (s *Server) apiDeleteRole(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	roleID, err := strconv.ParseInt(r.PathValue("role_id"), 10, 64)
	if err != nil || roleID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role_id"})
		return
	}
	if err := s.service.Repository().DeleteAIRole(tenantID, roleID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if s.hub != nil {
		s.hub.Broadcast(tenantID, "chats_updated", map[string]any{"reason": "role_deleted"})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) apiAssignChatRole(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}
	defer r.Body.Close()
	var req struct {
		RoleID int64 `json:"role_id"` // 0 clears the assignment
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	roleName := ""
	if req.RoleID != 0 {
		role, ok := s.service.Repository().AIRoleByID(tenantID, req.RoleID)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role not found"})
			return
		}
		roleName = role.Name
	}
	s.service.Repository().AssignAIRoleToChat(tenantID, chatID, req.RoleID)
	if s.hub != nil {
		s.hub.Broadcast(tenantID, "chat_role_changed", map[string]any{
			"chat_id":   chatID,
			"role_id":   req.RoleID,
			"role_name": roleName,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chat_id":   chatID,
		"role_id":   req.RoleID,
		"role_name": roleName,
	})
}

// apiSuggestions serves both the tenant-wide inbox (no chat_id path value) and
// the per-chat list — one handler registered on two routes.
func (s *Server) apiSuggestions(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	pending := s.service.Repository().PendingSuggestions(tenantID, chatID)
	type suggestionItem struct {
		ID              int64  `json:"id"`
		ChatID          string `json:"chat_id"`
		ChatDisplayName string `json:"chat_display_name"`
		FieldName       string `json:"field_name"`
		FieldLabel      string `json:"field_label"`
		SuggestedValue  string `json:"suggested_value"`
		Rationale       string `json:"rationale,omitempty"`
		CreatedAt       string `json:"created_at"`
	}
	items := make([]suggestionItem, 0, len(pending))
	for _, sg := range pending {
		chatName := sg.ChatID
		if chat, ok := s.service.Repository().ChatBySelector(tenantID, sg.ChatID); ok && chat.DisplayName != "" {
			chatName = chat.DisplayName
		}
		label := bot.SuggestionFieldLabels[sg.FieldName]
		if label == "" {
			label = sg.FieldName
		}
		items = append(items, suggestionItem{
			ID:              sg.ID,
			ChatID:          sg.ChatID,
			ChatDisplayName: chatName,
			FieldName:       sg.FieldName,
			FieldLabel:      label,
			SuggestedValue:  sg.SuggestedValue,
			Rationale:       sg.Rationale,
			CreatedAt:       sg.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": items})
}

func (s *Server) apiSuggestionAction(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	id, err := strconv.ParseInt(r.PathValue("suggestion_id"), 10, 64)
	if err != nil || id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid suggestion_id"})
		return
	}
	defer r.Body.Close()
	var req struct {
		Action string `json:"action"` // "accept" | "reject"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Action != "accept" && req.Action != "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action must be accept or reject"})
		return
	}
	if err := s.service.Repository().ResolveSuggestion(tenantID, id, req.Action == "accept"); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": req.Action + "ed"})
}

func (s *Server) apiChatSummary(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}
	summ, msgCount, err := s.service.SummarizeChat(r.Context(), tenantID, chatID)
	switch {
	case errors.Is(err, core.ErrSummaryBlockedByPrivacy):
		writeJSON(w, http.StatusOK, map[string]any{"blocked_by_privacy": true})
		return
	case errors.Is(err, core.ErrSummaryNoMessages):
		writeJSON(w, http.StatusOK, map[string]any{"no_messages": true})
		return
	case err != nil || summ.Summary == "":
		writeJSON(w, http.StatusOK, map[string]any{
			"unavailable":   true,
			"message_count": msgCount,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"summary":         summ.Summary,
		"action_items":    summ.ActionItems,
		"recommendations": summ.Recommendations,
		"message_count":   msgCount,
	})
}

func (s *Server) apiFacts(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}
	facts := s.service.Repository().SemanticFacts(tenantID, chatID, 100)
	type factItem struct {
		ID         int64   `json:"id"`
		FactText   string  `json:"fact_text"`
		FactType   string  `json:"fact_type"`
		Subject    string  `json:"subject"`
		Confidence float64 `json:"confidence"`
		Source     string  `json:"source"`
		CreatedAt  string  `json:"created_at"`
	}
	items := make([]factItem, 0, len(facts))
	for _, f := range facts {
		items = append(items, factItem{
			ID:         f.ID,
			FactText:   f.FactText,
			FactType:   f.FactType,
			Subject:    f.Subject,
			Confidence: f.Confidence,
			Source:     f.Source,
			CreatedAt:  f.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"facts": items})
}

func (s *Server) apiDeleteFact(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	factID, err := strconv.ParseInt(r.PathValue("fact_id"), 10, 64)
	if err != nil || factID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid fact_id"})
		return
	}
	if err := s.service.Repository().UnlearnFact(tenantID, factID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) apiEpisodes(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}
	summaries := s.service.Repository().EpisodeSummaries(tenantID, chatID, 20)
	type episodeItem struct {
		ID            int64  `json:"id"`
		Content       string `json:"content"`
		ClosureReason string `json:"closure_reason"`
		MessageCount  int    `json:"message_count"`
		PeriodStart   string `json:"period_start"`
		PeriodEnd     string `json:"period_end"`
	}
	items := make([]episodeItem, 0, len(summaries))
	for _, e := range summaries {
		items = append(items, episodeItem{
			ID:            e.ID,
			Content:       e.Content,
			ClosureReason: e.ClosureReason,
			MessageCount:  e.MessageCount,
			PeriodStart:   e.PeriodStart.UTC().Format("2006-01-02T15:04:05Z"),
			PeriodEnd:     e.PeriodEnd.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"episodes": items})
}

func (s *Server) apiForgetChat(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing chat_id"})
		return
	}
	if err := s.service.Repository().ForgetChat(tenantID, chatID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if s.hub != nil {
		s.hub.Broadcast(tenantID, "chats_updated", map[string]any{"reason": "chat_forgotten", "chat_id": chatID})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "forgotten"})
}

func (s *Server) apiForgetAll(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	defer r.Body.Close()
	var req struct {
		ConfirmPhrase string `json:"confirm_phrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Same phrase contract as the bot flow — a typed confirmation, not a button.
	if strings.TrimSpace(req.ConfirmPhrase) != contracts.ForgetAllConfirmationPhrase {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "confirmation phrase does not match"})
		return
	}
	if err := s.service.Repository().ForgetAll(tenantID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "erased"})
}

// userSettingsModes whitelists default_mode_for_new_chats values (the DB CHECK
// constraint uses the same list; validating here keeps Postgres errors out of
// the API surface).
var userSettingsModes = map[string]bool{
	"off": true, "observe": true, "manual": true, "semi_auto": true, "auto": true,
}

func (s *Server) apiGetUserSettings(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	settings, ok := s.service.Repository().UserSettings(tenantID)
	if !ok {
		settings = contracts.UserSettings{
			UserID:                 strings.TrimPrefix(tenantID, "tenant:user:"),
			TenantID:               tenantID,
			PrimaryLanguage:        "ru",
			DefaultModeForNewChats: "manual",
			LearningEnabled:        true,
			RichFormattingEnabled:  true,
			LanguageForDrafts:      "auto",
		}
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) apiSaveUserSettings(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	defer r.Body.Close()
	var req struct {
		DisplayName            string `json:"display_name"`
		Bio                    string `json:"bio"`
		Timezone               string `json:"timezone"`
		PrimaryLanguage        string `json:"primary_language"`
		DefaultModeForNewChats string `json:"default_mode_for_new_chats"`
		LearningEnabled        *bool  `json:"learning_enabled"`
		RichFormattingEnabled  *bool  `json:"rich_formatting_enabled"`
		LanguageForDrafts      string `json:"language_for_drafts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	settings, ok := s.service.Repository().UserSettings(tenantID)
	if !ok {
		settings = contracts.UserSettings{
			TenantID:               tenantID,
			PrimaryLanguage:        "ru",
			DefaultModeForNewChats: "manual",
			LearningEnabled:        true,
			RichFormattingEnabled:  true,
			LanguageForDrafts:      "auto",
		}
	}
	settings.UserID = strings.TrimPrefix(tenantID, "tenant:user:")
	settings.DisplayName = strings.TrimSpace(req.DisplayName)
	settings.Bio = strings.TrimSpace(req.Bio)
	if req.Timezone != "" {
		settings.Timezone = req.Timezone
	}
	if req.PrimaryLanguage != "" {
		settings.PrimaryLanguage = req.PrimaryLanguage
	}
	if req.DefaultModeForNewChats != "" {
		if !userSettingsModes[req.DefaultModeForNewChats] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid default_mode_for_new_chats"})
			return
		}
		settings.DefaultModeForNewChats = req.DefaultModeForNewChats
	}
	if req.LearningEnabled != nil {
		settings.LearningEnabled = *req.LearningEnabled
	}
	if req.RichFormattingEnabled != nil {
		settings.RichFormattingEnabled = *req.RichFormattingEnabled
	}
	if req.LanguageForDrafts != "" {
		if req.LanguageForDrafts != "auto" && req.LanguageForDrafts != "primary" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid language_for_drafts"})
			return
		}
		settings.LanguageForDrafts = req.LanguageForDrafts
	}
	s.service.Repository().SaveUserSettings(settings)
	writeJSON(w, http.StatusOK, settings)
}
