package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/store"
)

// Owner-supplied skill field limits, validated at the boundary so the API
// returns a readable 400 rather than relying on the DB CHECK constraints (Э4.7).
const (
	skillNameMaxRunes = 64
	skillDescMaxRunes = 500
	skillBodyMaxRunes = 20000
)

// skillJSON is the wire shape of one skill for the owner UI. Global (first-party)
// skills are flagged Global so the UI can render them read-only.
type skillJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Enabled     bool   `json:"enabled"`
	Global      bool   `json:"global"`
}

func skillToJSON(rec store.SkillRecord) skillJSON {
	return skillJSON{
		ID: rec.ID, Name: rec.Name, Description: rec.Description,
		Body: rec.Body, Enabled: rec.Enabled, Global: rec.Global,
	}
}

type skillRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Enabled     *bool  `json:"enabled"`
}

// validateSkillInput enforces the boundary limits. Returns "" when valid.
func validateSkillInput(name, description, body string) string {
	if n := utf8.RuneCountInString(name); n < 1 || n > skillNameMaxRunes {
		return "name must be 1..64 characters"
	}
	if n := utf8.RuneCountInString(description); n < 1 || n > skillDescMaxRunes {
		return "description must be 1..500 characters"
	}
	if utf8.RuneCountInString(body) > skillBodyMaxRunes {
		return "body is too long (max 20000 characters)"
	}
	return ""
}

func (s *Server) apiListSkills(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	recs := s.service.Repository().SkillsForTenant(tenantID)
	out := make([]skillJSON, 0, len(recs))
	for _, rec := range recs {
		out = append(out, skillToJSON(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

// skillDraftRequest is the authoring payload: exactly one source — free text,
// a voice recording, or a chat to learn from.
type skillDraftRequest struct {
	Text        string `json:"text"`
	AudioBase64 string `json:"audio_base64"`
	MimeType    string `json:"mime_type"`
	ChatID      string `json:"chat_id"` // "learn a skill from this chat"
}

// apiDraftSkill generates an editable skill draft from a description (text or
// voice) or from a chat's history, without persisting anything. Only the
// existing create endpoint saves the final skill. Mirrors apiDraftRole.
func (s *Server) apiDraftSkill(w http.ResponseWriter, r *http.Request) {
	if s.aiBaseURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ai service not configured"})
		return
	}
	tenantID := tenantFromContext(r.Context())
	defer r.Body.Close()
	var req skillDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	req.AudioBase64 = strings.TrimSpace(req.AudioBase64)
	req.MimeType = strings.TrimSpace(req.MimeType)
	req.ChatID = strings.TrimSpace(req.ChatID)

	// Exactly one source of truth.
	provided := 0
	for _, present := range []bool{req.Text != "", req.AudioBase64 != "", req.ChatID != ""} {
		if present {
			provided++
		}
	}
	if provided != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide exactly one of text, audio_base64 or chat_id"})
		return
	}
	if len([]rune(req.Text)) > roleDraftTextMaxRunes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text is too long (max 8000 characters)"})
		return
	}
	if len(req.AudioBase64) > roleDraftAudioMaxB64 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "audio is too large"})
		return
	}
	if req.AudioBase64 != "" {
		if req.MimeType == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mime_type is required for audio"})
			return
		}
		if _, err := base64.StdEncoding.DecodeString(req.AudioBase64); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "audio_base64 is invalid"})
			return
		}
	}

	client := aiclient.NewHTTPClient(s.aiBaseURL, &http.Client{Timeout: roleDraftHTTPTimeout})
	source := req.Text
	switch {
	case req.AudioBase64 != "":
		extracted, err := client.ExtractMedia(r.Context(), aiclient.ExtractMediaRequest{
			Kind: "voice", DataBase64: req.AudioBase64, MimeType: req.MimeType,
		})
		if err != nil {
			slog.WarnContext(r.Context(), "skill audio transcription failed",
				"tenant_id", tenantID, "mime_type", req.MimeType, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "audio transcription failed"})
			return
		}
		source = strings.TrimSpace(extracted.Text)
		if source == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "audio contains no recognizable speech"})
			return
		}
	case req.ChatID != "":
		source = s.chatSkillSource(tenantID, req.ChatID)
		if source == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "not enough chat history to learn a skill"})
			return
		}
	}

	draft, err := client.DraftSkill(r.Context(), aiclient.DraftSkillRequest{Text: source})
	if err != nil {
		slog.WarnContext(r.Context(), "skill drafting failed", "tenant_id", tenantID, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "skill drafting failed"})
		return
	}
	if msg := validateSkillInput(draft.Name, draft.Description, draft.Body); msg != "" {
		slog.WarnContext(r.Context(), "skill drafting returned invalid data", "tenant_id", tenantID, "reason", msg)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "ai returned an invalid skill draft"})
		return
	}
	writeJSON(w, http.StatusOK, skillJSON{
		Name: draft.Name, Description: draft.Description, Body: draft.Body, Enabled: true,
	})
}

// chatSkillSource assembles a compact digest of a chat's recent episode
// summaries as authoring source for "learn a skill from this chat". Capped to
// the draft text limit so the AI request stays within bounds.
func (s *Server) chatSkillSource(tenantID, chatID string) string {
	summaries := s.service.Repository().EpisodeSummaries(tenantID, chatID, 10)
	var b strings.Builder
	for _, su := range summaries {
		content := strings.TrimSpace(su.Content)
		if content == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(content)
		b.WriteString("\n")
	}
	digest := strings.TrimSpace(b.String())
	if runes := []rune(digest); len(runes) > roleDraftTextMaxRunes {
		digest = string(runes[:roleDraftTextMaxRunes])
	}
	return digest
}

// apiImproveSkill rewrites a skill from its usage/feedback signal and returns
// the proposal for owner review — it never persists. The owner saves via the
// existing update path (which snapshots a version).
func (s *Server) apiImproveSkill(w http.ResponseWriter, r *http.Request) {
	if s.aiBaseURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ai service not configured"})
		return
	}
	tenantID := tenantFromContext(r.Context())
	id, err := strconv.ParseInt(r.PathValue("skill_id"), 10, 64)
	if err != nil || id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid skill_id"})
		return
	}
	skill, ok := s.service.Repository().Skill(tenantID, id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "skill not found"})
		return
	}
	sig := s.service.Repository().SkillSignal(id)
	signalText := fmt.Sprintf(
		"Загрузок навыка: %d. Ответы понравились владельцу: %d. Не понравились: %d.",
		sig.Loads, sig.Positive, sig.Negative,
	)
	client := aiclient.NewHTTPClient(s.aiBaseURL, &http.Client{Timeout: roleDraftHTTPTimeout})
	draft, err := client.ImproveSkill(r.Context(), aiclient.ImproveSkillRequest{
		Name: skill.Name, Description: skill.Description, Body: skill.Body, Signal: signalText,
	})
	if err != nil {
		slog.WarnContext(r.Context(), "skill improvement failed", "tenant_id", tenantID, "skill_id", id, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "skill improvement failed"})
		return
	}
	if msg := validateSkillInput(draft.Name, draft.Description, draft.Body); msg != "" {
		slog.WarnContext(r.Context(), "skill improvement returned invalid data", "tenant_id", tenantID, "reason", msg)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "ai returned an invalid skill"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"proposal": skillJSON{ID: id, Name: draft.Name, Description: draft.Description, Body: draft.Body, Enabled: skill.Enabled, Global: skill.Global},
		"signal":   map[string]int{"loads": sig.Loads, "positive": sig.Positive, "negative": sig.Negative},
	})
}

// apiSkillVersions lists a skill's body-version history (newest first).
func (s *Server) apiSkillVersions(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	id, err := strconv.ParseInt(r.PathValue("skill_id"), 10, 64)
	if err != nil || id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid skill_id"})
		return
	}
	if _, ok := s.service.Repository().Skill(tenantID, id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "skill not found"})
		return
	}
	type versionJSON struct {
		ID        int64  `json:"id"`
		Author    string `json:"author"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	}
	recs := s.service.Repository().SkillVersions(id)
	out := make([]versionJSON, 0, len(recs))
	for _, v := range recs {
		out = append(out, versionJSON{
			ID: v.ID, Author: v.Author, Body: v.Body,
			CreatedAt: v.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": out})
}

// apiRevertSkill restores a tenant-owned skill's body to a prior version.
func (s *Server) apiRevertSkill(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	id, err := strconv.ParseInt(r.PathValue("skill_id"), 10, 64)
	if err != nil || id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid skill_id"})
		return
	}
	defer r.Body.Close()
	var req struct {
		VersionID int64 `json:"version_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.VersionID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version_id is required"})
		return
	}
	if err := s.service.Repository().RevertSkillToVersion(tenantID, id, req.VersionID); err != nil {
		if errors.Is(err, store.ErrSkillNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "skill or version not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to revert skill"})
		return
	}
	skill, _ := s.service.Repository().Skill(tenantID, id)
	writeJSON(w, http.StatusOK, skillToJSON(skill))
}

func (s *Server) apiCreateSkill(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	defer r.Body.Close()
	var req skillRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if msg := validateSkillInput(req.Name, req.Description, req.Body); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	id, err := s.service.Repository().SaveSkill(tenantID, store.SkillRecord{
		Name: req.Name, Description: req.Description, Body: req.Body, Enabled: enabled,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save skill"})
		return
	}
	writeJSON(w, http.StatusOK, skillJSON{
		ID: id, Name: req.Name, Description: req.Description, Body: req.Body, Enabled: enabled,
	})
}

func (s *Server) apiUpdateSkill(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	id, err := strconv.ParseInt(r.PathValue("skill_id"), 10, 64)
	if err != nil || id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid skill_id"})
		return
	}
	defer r.Body.Close()
	var req skillRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if msg := validateSkillInput(req.Name, req.Description, req.Body); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	err = s.service.Repository().UpdateSkill(tenantID, id, req.Name, req.Description, req.Body, enabled)
	if errors.Is(err, store.ErrSkillNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "skill not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update skill"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) apiDeleteSkill(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	id, err := strconv.ParseInt(r.PathValue("skill_id"), 10, 64)
	if err != nil || id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid skill_id"})
		return
	}
	if err := s.service.Repository().DeleteSkill(tenantID, id); err != nil {
		if errors.Is(err, store.ErrSkillNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "skill not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete skill"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) apiSetRoleSkills(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	roleID, err := strconv.ParseInt(r.PathValue("role_id"), 10, 64)
	if err != nil || roleID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role_id"})
		return
	}
	defer r.Body.Close()
	var req struct {
		SkillIDs []int64 `json:"skill_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, ok := s.service.Repository().AIRoleByID(tenantID, roleID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role not found"})
		return
	}
	// Every skill must be visible to this tenant (its own or a global one).
	visible := map[int64]bool{}
	for _, rec := range s.service.Repository().SkillsForTenant(tenantID) {
		visible[rec.ID] = true
	}
	for _, sid := range req.SkillIDs {
		if !visible[sid] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "skill_id is not visible to tenant"})
			return
		}
	}
	if err := s.service.Repository().SetRoleSkills(roleID, req.SkillIDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update role skills"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
