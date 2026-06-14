// Package config loads runtime configuration from the environment.
package config

import (
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
		HTTPAddr:              valueOrDefault(os.Getenv("BACKEND_HTTP_ADDR"), ":8080"),
		AIBaseURL:             os.Getenv("AI_BASE_URL"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		Timeout:               timeout,
		AITimeout:             aiTimeout,
		TelegramBotToken:      os.Getenv("TELEGRAM_BOT_TOKEN"),
		PublicWebhookURL:      os.Getenv("PUBLIC_WEBHOOK_URL"),
		TelegramWebhookSecret: os.Getenv("TELEGRAM_WEBHOOK_SECRET"),
		TelegramAutoSetup:     boolValue(os.Getenv("TELEGRAM_AUTO_SETUP")),
		ReplyDelay:            replyDelay,
		MiniAppDir:            valueOrDefault(os.Getenv("MINIAPP_DIR"), "./miniapp"),
		IngestionModeDefault:  valueOrDefault(os.Getenv("INGESTION_MODE_DEFAULT"), "bot"),
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
