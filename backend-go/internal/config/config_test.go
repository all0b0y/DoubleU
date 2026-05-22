package config

import (
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	t.Setenv("BACKEND_HTTP_ADDR", ":9090")
	t.Setenv("AI_BASE_URL", "http://ai")
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("BACKEND_TIMEOUT_SECONDS", "3")
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("PUBLIC_WEBHOOK_URL", "https://example.com/telegram/webhook")
	t.Setenv("TELEGRAM_WEBHOOK_SECRET", "secret")
	t.Setenv("TELEGRAM_AUTO_SETUP", "true")

	cfg := Load()
	if cfg.HTTPAddr != ":9090" || cfg.AIBaseURL != "http://ai" || cfg.Timeout != 3*time.Second {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.DatabaseURL != "postgres://example" {
		t.Fatalf("database url = %q", cfg.DatabaseURL)
	}
	if cfg.TelegramBotToken != "token" || cfg.PublicWebhookURL == "" || cfg.TelegramWebhookSecret != "secret" || !cfg.TelegramAutoSetup {
		t.Fatalf("telegram config = %+v", cfg)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("BACKEND_HTTP_ADDR", "")
	t.Setenv("AI_BASE_URL", "")
	t.Setenv("BACKEND_TIMEOUT_SECONDS", "bad")

	cfg := Load()
	if cfg.HTTPAddr != ":8080" || cfg.Timeout != 10*time.Second {
		t.Fatalf("default config = %+v", cfg)
	}
}
