package httpapi

import (
	"net/http"
	"strconv"
)

func (s *Server) apiScheduledJobs(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	if _, ok := s.service.Repository().Chat(tenantID, chatID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chat not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scheduled_jobs": s.service.Repository().ListJobs(tenantID, chatID),
	})
}

func (s *Server) apiCancelScheduledJob(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	chatID := r.PathValue("chat_id")
	jobID, err := strconv.ParseInt(r.PathValue("job_id"), 10, 64)
	if err != nil || jobID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid job id"})
		return
	}
	if !s.service.Repository().CancelJob(tenantID, chatID, jobID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "pending job not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": jobID, "status": "cancelled"})
}
