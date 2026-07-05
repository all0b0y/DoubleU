// Mini App settings endpoints for bot-transport ingestion.
//
// The bot is the communication and feedback channel; the Mini App UI is the
// configuration surface. These endpoints let the owner register their bot
// token and switch the ingestion mode (bot / personal / full).
//
// Security invariant: the bot token is accepted on input only and never
// echoed back — responses carry bot_id / bot_username instead.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

// BotManager is the bot-ingestion control surface used by the settings API
// (implemented by botservice.Manager).
type BotManager interface {
	RegisterBot(ctx context.Context, tenantID, token string) (store.UserBotRecord, error)
	DisconnectBot(tenantID string, botID int64)
	Refresh(tenantID string)
}

// WithBotManager wires the bot ingestion manager into the settings API.
func WithBotManager(manager BotManager) ServerOption {
	return func(s *Server) { s.bots = manager }
}

// botRolePayload is the bot's governing role (nil when none) — every
// bot-transport chat answers in this role.
type botRolePayload struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// botStatusPayload is the owner-facing view of a bot registration (no token).
type botStatusPayload struct {
	Connected             bool            `json:"connected"`
	BotID                 int64           `json:"bot_id,omitempty"`
	BotUsername           string          `json:"bot_username,omitempty"`
	Status                string          `json:"status,omitempty"`
	AIRole                *botRolePayload `json:"ai_role,omitempty"`
	RichFormattingEnabled *bool           `json:"rich_formatting_enabled"`
	StreamingEnabled      *bool           `json:"streaming_enabled"`
	// PersonalMode: when true the bot answers only the owner + allowlist.
	PersonalMode bool `json:"personal_mode"`
}

func (s *Server) botStatusFor(record store.UserBotRecord, connected bool) botStatusPayload {
	if !connected {
		return botStatusPayload{}
	}
	payload := botStatusPayload{
		Connected:             true,
		BotID:                 record.BotID,
		BotUsername:           record.BotUsername,
		Status:                record.Status,
		RichFormattingEnabled: cloneBoolPtr(record.RichFormattingEnabled),
		StreamingEnabled:      cloneBoolPtr(record.StreamingEnabled),
		PersonalMode:          record.PersonalMode,
	}
	if record.AIRoleID != 0 {
		if role, ok := s.service.Repository().AIRoleByID(record.TenantID, record.AIRoleID); ok {
			payload.AIRole = &botRolePayload{ID: role.ID, Name: role.Name}
		}
	}
	return payload
}

// apiListBots returns all of the tenant's bot registrations.
func (s *Server) apiListBots(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	records := s.service.Repository().UserBotsByTenant(tenantID)
	bots := make([]botStatusPayload, 0, len(records))
	for _, rec := range records {
		bots = append(bots, s.botStatusFor(rec, true))
	}
	writeJSON(w, http.StatusOK, map[string]any{"bots": bots})
}

// apiSetBotRole updates one bot's governing role and nullable delivery settings.
// role_id 0 clears the role. When role_id is present, the caller must also
// choose which role-scoped memory context to activate; delivery-only patches
// keep their current role and do not need a memory policy.
func (s *Server) apiSetBotRole(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	tenantID := tenantFromContext(r.Context())
	botID, err := strconv.ParseInt(r.PathValue("bot_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bot_id"})
		return
	}
	var payload struct {
		RoleID                optionalInt64Patch `json:"role_id"`
		RoleContextPolicy     string             `json:"role_context_policy"`
		RichFormattingEnabled optionalBoolPatch  `json:"rich_formatting_enabled"`
		StreamingEnabled      optionalBoolPatch  `json:"streaming_enabled"`
		PersonalMode          optionalBoolPatch  `json:"personal_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	record, ok := s.service.Repository().UserBotByID(tenantID, botID)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no such bot"})
		return
	}
	var roleContextPolicy store.RoleContextPolicy
	if payload.RoleID.Present {
		rawPolicy := strings.TrimSpace(payload.RoleContextPolicy)
		if rawPolicy == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role_context_policy is required when role_id is present"})
			return
		}
		roleContextPolicy = store.RoleContextPolicy(rawPolicy)
		if !roleContextPolicy.Valid() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role_context_policy must be one of: reuse_role_context, new_role_context"})
			return
		}
	}
	if payload.RoleID.Present && payload.RoleID.Value != 0 {
		if _, ok := s.service.Repository().AIRoleByID(tenantID, payload.RoleID.Value); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role not found"})
			return
		}
	}
	if payload.RoleID.Present {
		roleContext, err := s.service.Repository().SetUserBotRoleWithContextPolicy(tenantID, botID, payload.RoleID.Value, roleContextPolicy)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		record.AIRoleID = payload.RoleID.Value
		record.ActiveRoleContextID = roleContext.ID
	}
	if payload.RichFormattingEnabled.Present || payload.StreamingEnabled.Present {
		record.RichFormattingEnabled = payload.RichFormattingEnabled.Apply(record.RichFormattingEnabled)
		record.StreamingEnabled = payload.StreamingEnabled.Apply(record.StreamingEnabled)
		s.service.Repository().SetUserBotDeliverySettings(
			tenantID, botID, record.RichFormattingEnabled, record.StreamingEnabled,
		)
	}
	if payload.PersonalMode.Present {
		record.PersonalMode = boolOrDefault(payload.PersonalMode.Value, record.PersonalMode)
		s.service.Repository().SetUserBotPersonalMode(tenantID, botID, record.PersonalMode)
	}
	if s.hub != nil {
		// Bot role governs every chat of that bot — refresh open chat lists/settings.
		s.hub.Broadcast(tenantID, "chats_updated", map[string]any{"reason": "bot_role_changed"})
	}
	writeJSON(w, http.StatusOK, s.botStatusFor(record, true))
}

// apiRegisterBot validates and stores the owner's bot token, then starts polling.
func (s *Server) apiRegisterBot(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if s.bots == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "bot ingestion is not configured"})
		return
	}
	tenantID := tenantFromContext(r.Context())
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(payload.Token) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token is required"})
		return
	}
	record, err := s.bots.RegisterBot(r.Context(), tenantID, payload.Token)
	if err != nil {
		// Registration errors are token-redacted by the manager; safe to return.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.botStatusFor(record, true))
}

// apiDisconnectBot stops polling and removes one specific bot of the tenant.
func (s *Server) apiDisconnectBot(w http.ResponseWriter, r *http.Request) {
	if s.bots == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "bot ingestion is not configured"})
		return
	}
	tenantID := tenantFromContext(r.Context())
	botID, err := strconv.ParseInt(r.PathValue("bot_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bot_id"})
		return
	}
	s.bots.DisconnectBot(tenantID, botID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// apiListBotAllowlist returns the Telegram user ids a personal bot also serves
// (besides the owner, who is always served and never listed here).
func (s *Server) apiListBotAllowlist(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	botID, err := strconv.ParseInt(r.PathValue("bot_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bot_id"})
		return
	}
	if _, ok := s.service.Repository().UserBotByID(tenantID, botID); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no such bot"})
		return
	}
	users := s.service.Repository().BotAllowedUsers(tenantID, botID)
	if users == nil {
		users = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"allowed_users": users})
}

// apiAddBotAllowlist adds one Telegram user id to a personal bot's allowlist.
func (s *Server) apiAddBotAllowlist(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	tenantID := tenantFromContext(r.Context())
	botID, err := strconv.ParseInt(r.PathValue("bot_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bot_id"})
		return
	}
	if _, ok := s.service.Repository().UserBotByID(tenantID, botID); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no such bot"})
		return
	}
	var payload struct {
		TelegramUserID string `json:"telegram_user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	userID := strings.TrimSpace(payload.TelegramUserID)
	// Telegram user ids are numeric; reject anything else so a username or typo
	// never silently lands in the allowlist and fails to match event.SenderID.
	if _, err := strconv.ParseInt(userID, 10, 64); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "telegram_user_id must be a numeric Telegram user id"})
		return
	}
	s.service.Repository().AddBotAllowedUser(tenantID, botID, userID)
	users := s.service.Repository().BotAllowedUsers(tenantID, botID)
	if users == nil {
		users = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"allowed_users": users})
}

// apiRemoveBotAllowlist drops one Telegram user id from a personal bot's allowlist.
func (s *Server) apiRemoveBotAllowlist(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	botID, err := strconv.ParseInt(r.PathValue("bot_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bot_id"})
		return
	}
	userID := strings.TrimSpace(r.PathValue("user_id"))
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id is required"})
		return
	}
	s.service.Repository().RemoveBotAllowedUser(tenantID, botID, userID)
	users := s.service.Repository().BotAllowedUsers(tenantID, botID)
	if users == nil {
		users = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"allowed_users": users})
}

// apiGetIngestionMode returns the tenant's effective ingestion mode.
func (s *Server) apiGetIngestionMode(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	mode := s.service.Repository().TenantIngestionMode(tenantID)
	writeJSON(w, http.StatusOK, map[string]string{"mode": string(mode)})
}

// apiSetIngestionMode switches the tenant between bot / personal / full and
// starts or stops the bot poller accordingly.
func (s *Server) apiSetIngestionMode(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	tenantID := tenantFromContext(r.Context())
	var payload struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	mode, ok := contracts.ParseIngestionMode(payload.Mode)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode must be one of: bot, personal, full"})
		return
	}
	s.service.Repository().SetTenantIngestionMode(tenantID, mode)
	if s.bots != nil {
		s.bots.Refresh(tenantID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"mode": string(mode)})
}
