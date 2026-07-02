package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// apiCheckMCPServer proxies a stored MCP server to the Python /ai/mcp-check probe
// (Э1.3) so the owner UI can list a server's tools before granting a role access.
// The decrypted auth header travels only over this internal Go→Python channel —
// exactly like the per-request MCPServerSpec — and is never returned to the UI.
func (s *Server) apiCheckMCPServer(w http.ResponseWriter, r *http.Request) {
	if s.aiBaseURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ai service not configured"})
		return
	}
	tenantID := tenantFromContext(r.Context())
	id, err := strconv.ParseInt(r.PathValue("server_id"), 10, 64)
	if err != nil || id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server_id"})
		return
	}
	transport, url, authHeader, ok := s.service.Repository().MCPServerForCheck(tenantID, id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
		return
	}
	body, err := json.Marshal(map[string]string{"url": url, "transport": transport, "auth_header": authHeader})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.aiBaseURL+"/ai/mcp-check", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "ai service unavailable"})
		return
	}
	defer resp.Body.Close()
	// The Python probe already strips secrets/traces from its {ok,error} body, so
	// forward it verbatim.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
