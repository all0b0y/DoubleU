package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"unicode/utf8"

	"doubleu/backend-go/internal/store"
)

// Owner-supplied MCP field limits, validated at the boundary so the API returns
// a human-readable 400 rather than relying on the DB CHECK constraints (Э1.2).
const (
	mcpNameMaxRunes = 64
	mcpURLMaxLen    = 2048
)

// mcpServerJSON is the wire shape of one MCP server for the owner UI. It carries
// only HasAuth, never the auth header value.
type mcpServerJSON struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
	URL       string `json:"url"`
	HasAuth   bool   `json:"has_auth"`
	Enabled   bool   `json:"enabled"`
}

func mcpServerToJSON(rec store.MCPServerRecord) mcpServerJSON {
	return mcpServerJSON{
		ID: rec.ID, Name: rec.Name, Transport: rec.Transport,
		URL: rec.URL, HasAuth: rec.HasAuth, Enabled: rec.Enabled,
	}
}

// mcpServerRequest is the create/update body. AuthHeader is write-only: it is
// accepted here but never echoed back. An empty AuthHeader on update leaves the
// stored secret unchanged.
type mcpServerRequest struct {
	Name       string `json:"name"`
	Transport  string `json:"transport"`
	URL        string `json:"url"`
	AuthHeader string `json:"auth_header"`
	Enabled    *bool  `json:"enabled"`
}

// validateMCPServerInput enforces the boundary limits and the SSRF guard (Э0.1).
// Returns "" when valid, else a human-readable message.
func validateMCPServerInput(name, transport, url string) string {
	if name == "" || utf8.RuneCountInString(name) > mcpNameMaxRunes {
		return "name must be 1..64 characters"
	}
	if transport != "http" && transport != "sse" {
		return "transport must be http or sse"
	}
	if len(url) > mcpURLMaxLen {
		return "url is too long"
	}
	if err := ValidateMCPServerURL(url); err != nil {
		return err.Error()
	}
	return ""
}

func (s *Server) apiListMCPServers(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	recs := s.service.Repository().MCPServersForTenant(tenantID)
	out := make([]mcpServerJSON, 0, len(recs))
	for _, rec := range recs {
		out = append(out, mcpServerToJSON(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

func (s *Server) apiCreateMCPServer(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	defer r.Body.Close()
	var req mcpServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Transport == "" {
		req.Transport = "http"
	}
	if msg := validateMCPServerInput(req.Name, req.Transport, req.URL); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	id, err := s.service.Repository().SaveMCPServer(tenantID, req.Name, req.Transport, req.URL, req.AuthHeader)
	if err != nil {
		if errors.Is(err, store.ErrMissingSecretsKey) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "server encryption key is not configured; cannot store an auth header"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save mcp server"})
		return
	}
	writeJSON(w, http.StatusOK, mcpServerJSON{
		ID: id, Name: req.Name, Transport: req.Transport, URL: req.URL,
		HasAuth: req.AuthHeader != "", Enabled: true,
	})
}

func (s *Server) apiUpdateMCPServer(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	id, err := strconv.ParseInt(r.PathValue("server_id"), 10, 64)
	if err != nil || id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server_id"})
		return
	}
	defer r.Body.Close()
	var req mcpServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Transport == "" {
		req.Transport = "http"
	}
	if msg := validateMCPServerInput(req.Name, req.Transport, req.URL); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	err = s.service.Repository().UpdateMCPServer(tenantID, id, req.Name, req.Transport, req.URL, req.AuthHeader, enabled)
	switch {
	case errors.Is(err, store.ErrMCPServerNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
	case errors.Is(err, store.ErrMissingSecretsKey):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "server encryption key is not configured; cannot store an auth header"})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update mcp server"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
	}
}

func (s *Server) apiDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	id, err := strconv.ParseInt(r.PathValue("server_id"), 10, 64)
	if err != nil || id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server_id"})
		return
	}
	if err := s.service.Repository().DeleteMCPServer(tenantID, id); err != nil {
		if errors.Is(err, store.ErrMCPServerNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete mcp server"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// roleMCPGrantJSON is one role→server grant with its tool allow-list. A null
// allowed_tools means every tool the server exposes is permitted.
type roleMCPGrantJSON struct {
	ServerID     int64    `json:"server_id"`
	AllowedTools []string `json:"allowed_tools"`
}

func (s *Server) apiGetRoleMCP(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	roleID, err := strconv.ParseInt(r.PathValue("role_id"), 10, 64)
	if err != nil || roleID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role_id"})
		return
	}
	if _, ok := s.service.Repository().AIRoleByID(tenantID, roleID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role not found"})
		return
	}
	access := s.service.Repository().RoleMCPAccessByRole(roleID)
	out := make([]roleMCPGrantJSON, 0, len(access))
	for _, a := range access {
		out = append(out, roleMCPGrantJSON{ServerID: a.ServerID, AllowedTools: a.AllowedTools})
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

func (s *Server) apiSetRoleMCP(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	roleID, err := strconv.ParseInt(r.PathValue("role_id"), 10, 64)
	if err != nil || roleID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role_id"})
		return
	}
	defer r.Body.Close()
	var req struct {
		Servers []roleMCPGrantJSON `json:"servers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, ok := s.service.Repository().AIRoleByID(tenantID, roleID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role not found"})
		return
	}
	// Every referenced server must belong to this tenant — no cross-tenant grants.
	owned := map[int64]bool{}
	for _, rec := range s.service.Repository().MCPServersForTenant(tenantID) {
		owned[rec.ID] = true
	}
	grants := make([]store.RoleMCPAccessRecord, 0, len(req.Servers))
	for _, g := range req.Servers {
		if !owned[g.ServerID] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "server_id does not belong to tenant"})
			return
		}
		grants = append(grants, store.RoleMCPAccessRecord{ServerID: g.ServerID, AllowedTools: g.AllowedTools})
	}
	if err := s.service.Repository().SetRoleMCPServers(roleID, grants); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update role mcp access"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
