package httpapi

import (
	"io"
	"net/http"
)

// apiModelConfig proxies GET /ai/model-config from the Python AI service so the
// Mini App can display real LLM model names next to each tier option in the
// role-edit UI. The Python endpoint reads MODEL_TIER_* env vars and returns a
// plain JSON object — we forward it verbatim without re-encoding.
func (s *Server) apiModelConfig(w http.ResponseWriter, r *http.Request) {
	if s.aiBaseURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ai service not configured"})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.aiBaseURL+"/ai/model-config", nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "ai service unavailable"})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
