package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"unicode/utf8"

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
