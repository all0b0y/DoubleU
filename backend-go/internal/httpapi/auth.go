// Package httpapi exposes the backend HTTP API.
package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type contextKey string

const tenantIDKey contextKey = "tenantID"

// tmaAuthMaxAge is how long an initData token stays valid.
const tmaAuthMaxAge = 24 * time.Hour

func contextWithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey, tenantID)
}

func tenantFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tenantIDKey).(string); ok {
		return v
	}
	return ""
}

type tmaUserPayload struct {
	ID int64 `json:"id"`
}

// validateInitData parses and validates Telegram Mini App initData with HMAC-SHA256.
// When botToken is empty, validation is skipped (development mode).
func validateInitData(initData, botToken string) (url.Values, error) {
	if initData == "" {
		return nil, fmt.Errorf("empty initData")
	}
	values, err := url.ParseQuery(initData)
	if err != nil {
		return nil, fmt.Errorf("malformed initData: %w", err)
	}
	if botToken == "" {
		return values, nil
	}
	hash := values.Get("hash")
	if hash == "" {
		return nil, fmt.Errorf("missing hash")
	}
	var pairs []string
	for key, vals := range values {
		if key == "hash" || len(vals) == 0 {
			continue
		}
		pairs = append(pairs, key+"="+vals[0])
	}
	sort.Strings(pairs)
	dataCheckString := strings.Join(pairs, "\n")

	// secret_key = HMAC-SHA256("WebAppData", bot_token)
	sk := hmac.New(sha256.New, []byte("WebAppData"))
	sk.Write([]byte(botToken))

	// signature = HMAC-SHA256(data_check_string, secret_key)
	mac := hmac.New(sha256.New, sk.Sum(nil))
	mac.Write([]byte(dataCheckString))
	computed := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(computed), []byte(hash)) {
		return nil, fmt.Errorf("signature mismatch")
	}
	if raw := values.Get("auth_date"); raw != "" {
		ts, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr == nil && time.Since(time.Unix(ts, 0)) > tmaAuthMaxAge {
			return nil, fmt.Errorf("initData expired")
		}
	}
	return values, nil
}

// userIDFromValues extracts the Telegram user ID from parsed initData values.
func userIDFromValues(values url.Values) string {
	userJSON := values.Get("user")
	if userJSON == "" {
		// Fallback: check query param directly (for dev/test usage)
		return values.Get("user_id")
	}
	var u tmaUserPayload
	if err := json.Unmarshal([]byte(userJSON), &u); err != nil || u.ID == 0 {
		return ""
	}
	return strconv.FormatInt(u.ID, 10)
}

// tmaAuth validates TMA initData and injects tenantID into the request context.
func (s *Server) tmaAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		initData := extractInitData(r)
		if initData == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing authorization"})
			return
		}
		values, err := validateInitData(initData, s.botToken)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
		userID := userIDFromValues(values)
		tenantID := "tenant:user:" + userID
		next(w, r.WithContext(contextWithTenant(r.Context(), tenantID)))
	}
}

func extractInitData(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "tma ") {
		return strings.TrimPrefix(auth, "tma ")
	}
	// Allow auth via query param for SSE (EventSource doesn't support custom headers).
	return r.URL.Query().Get("auth")
}
