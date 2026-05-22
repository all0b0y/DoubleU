package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// SSEHub manages per-tenant server-sent event streams.
type SSEHub struct {
	mu      sync.Mutex
	clients map[string][]chan []byte
}

// NewSSEHub creates an empty SSE hub.
func NewSSEHub() *SSEHub {
	return &SSEHub{clients: make(map[string][]chan []byte)}
}

// Broadcast delivers a named event to all connected clients for a tenant.
// Slow clients are skipped (non-blocking send).
func (h *SSEHub) Broadcast(tenantID, eventType string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	msg := fmt.Appendf(nil, "event: %s\ndata: %s\n\n", eventType, data)
	h.mu.Lock()
	clients := append([]chan []byte(nil), h.clients[tenantID]...)
	h.mu.Unlock()
	for _, ch := range clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (h *SSEHub) subscribe(tenantID string) chan []byte {
	ch := make(chan []byte, 32)
	h.mu.Lock()
	h.clients[tenantID] = append(h.clients[tenantID], ch)
	h.mu.Unlock()
	return ch
}

func (h *SSEHub) unsubscribe(tenantID string, ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.clients[tenantID]
	for i, c := range list {
		if c == ch {
			h.clients[tenantID] = append(list[:i], list[i+1:]...)
			close(ch)
			return
		}
	}
}

// serveSSE handles a persistent SSE connection for an authenticated owner.
func (s *Server) serveSSE(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprintf(w, ": connected tenant=%s\n\n", tenantID)
	flusher.Flush()
	ch := s.hub.subscribe(tenantID)
	defer s.hub.unsubscribe(tenantID, ch)
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, open := <-ch:
			if !open {
				return
			}
			w.Write(msg)
			flusher.Flush()
		}
	}
}
