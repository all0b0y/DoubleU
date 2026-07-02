// Package config loads runtime configuration from the environment.
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config contains backend runtime settings.
type Config struct {
	HTTPAddr              string
	AIBaseURL             string
	DatabaseURL           string
	Timeout               time.Duration // outbound HTTP timeout for Telegram API calls
	AITimeout             time.Duration // outbound HTTP timeout for AI service calls (LLM can be slow)
	TelegramBotToken      string
	PublicWebhookURL      string
	TelegramWebhookSecret string
	TelegramAutoSetup     bool
	ReplyDelay            time.Duration // typing debounce: wait before calling AI
	MiniAppDir            string        // directory of Mini App static files to serve
	// IngestionModeDefault applies to tenants without an explicit ingestion_mode:
	// "bot" (user-supplied bot, default while the Business path is shelved),
	// "personal" (Telegram Business webhook) or "full" (both).
	IngestionModeDefault string
	// MediaExtractionEnabled is the global kill switch for the media -> text step
	// (voice/photo/document). Default true; set MEDIA_EXTRACTION_ENABLED=false to
	// disable all media provider calls (cost control).
	MediaExtractionEnabled bool
	// MCPSecretsKey is the 32-byte AES-256 key used to encrypt MCP auth headers
	// at rest. Nil when MCP_SECRETS_KEY is unset (encryption disabled). Populated
	// by the serve path via ParseMCPSecretsKey — never decoded silently, so an
	// invalid key fails startup rather than corrupting stored secrets.
	MCPSecretsKey []byte
}

// ParseMCPSecretsKey decodes the base64 MCP_SECRETS_KEY into a 32-byte AES-256
// key. An empty value yields (nil, nil) — encryption stays off. A malformed or
// wrong-length value is a hard error so startup fails loudly; the error never
// includes the key material itself.
func ParseMCPSecretsKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("MCP_SECRETS_KEY is not valid base64")
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("MCP_SECRETS_KEY must decode to exactly 32 bytes, got %d", len(key))
	}
	return key, nil
}

// Load reads configuration from environment variables.
func Load() Config {
	timeout := 10 * time.Second
	if raw := os.Getenv("BACKEND_TIMEOUT_SECONDS"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			timeout = time.Duration(seconds) * time.Second
		}
	}
	// AI calls hit an LLM which can take 30-60s; give them a separate, longer timeout.
	aiTimeout := 90 * time.Second
	if raw := os.Getenv("AI_TIMEOUT_SECONDS"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			aiTimeout = time.Duration(seconds) * time.Second
		}
	}
	replyDelay := 3 * time.Second // default: wait 3s before AI call
	if raw := os.Getenv("REPLY_DELAY_SECONDS"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
			replyDelay = time.Duration(seconds) * time.Second
		}
	}
	return Config{
		HTTPAddr:               valueOrDefault(os.Getenv("BACKEND_HTTP_ADDR"), ":8080"),
		AIBaseURL:              os.Getenv("AI_BASE_URL"),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		Timeout:                timeout,
		AITimeout:              aiTimeout,
		TelegramBotToken:       os.Getenv("TELEGRAM_BOT_TOKEN"),
		PublicWebhookURL:       os.Getenv("PUBLIC_WEBHOOK_URL"),
		TelegramWebhookSecret:  os.Getenv("TELEGRAM_WEBHOOK_SECRET"),
		TelegramAutoSetup:      boolValue(os.Getenv("TELEGRAM_AUTO_SETUP")),
		ReplyDelay:             replyDelay,
		MiniAppDir:             valueOrDefault(os.Getenv("MINIAPP_DIR"), "./miniapp"),
		IngestionModeDefault:   valueOrDefault(os.Getenv("INGESTION_MODE_DEFAULT"), "bot"),
		MediaExtractionEnabled: boolWithDefault(os.Getenv("MEDIA_EXTRACTION_ENABLED"), true),
	}
}

// boolWithDefault parses a boolean env value, returning def when unset so a
// default-on flag stays on unless explicitly disabled.
func boolWithDefault(value string, def bool) bool {
	switch value {
	case "":
		return def
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	default:
		return false
	}
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func boolValue(value string) bool {
	switch value {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	default:
		return false
	}
}
