package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/config"
	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/httpapi"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	// Use a short timeout for Telegram API calls; AI/LLM calls get a longer separate timeout.
	telegramHTTPClient := &http.Client{Timeout: cfg.Timeout}
	aiHTTPClient := &http.Client{Timeout: cfg.AITimeout}
	ai := aiclient.NewHTTPClient(cfg.AIBaseURL, aiHTTPClient)
	sender := telegram.Sender(telegram.NoopSender{})
	if cfg.TelegramBotToken != "" {
		botSender := telegram.NewBotSender(cfg.TelegramBotToken, telegramHTTPClient)
		sender = botSender
		if cfg.TelegramAutoSetup && cfg.PublicWebhookURL != "" {
			configureTelegramBot(logger, botSender, cfg.PublicWebhookURL, cfg.TelegramWebhookSecret)
		}
	}
	repository := store.NewRepository()
	storage := "memory"
	if cfg.DatabaseURL != "" {
		postgresRepository, err := store.NewPostgresRepository(cfg.DatabaseURL,
			"/migrations/001_core_schema.sql",
			"../infra/migrations/001_core_schema.sql",
			"/migrations/002_drop_audit_fks.sql",
			"../infra/migrations/002_drop_audit_fks.sql",
		)
		if err != nil {
			logger.Error("postgres connection failed — DATABASE_URL is set but database is unreachable", "error", err)
			os.Exit(1)
		}
		repository = postgresRepository
		defer func() {
			if err := repository.Close(); err != nil {
				logger.Error("postgres repository close failed", "error", err)
			}
		}()
		storage = "postgres"
	} else {
		logger.Warn("DATABASE_URL not set — using in-memory storage, all state will be lost on restart")
	}
	hub := httpapi.NewSSEHub()
	service := core.NewService(repository, ai, sender,
		core.WithLogger(logger),
		core.WithReplyDelay(cfg.ReplyDelay),
		core.WithNotifier(hub.Broadcast),
	)
	server := httpapi.NewServer(service,
		httpapi.WithHub(hub),
		httpapi.WithBotToken(cfg.TelegramBotToken),
		httpapi.WithMiniAppDir(cfg.MiniAppDir),
	)
	logger.Info("backend storage configured", "storage", storage, "database_url_configured", cfg.DatabaseURL != "")
	logger.Info("backend listening", "addr", cfg.HTTPAddr, "ai_base_url", cfg.AIBaseURL)
	if err := http.ListenAndServe(cfg.HTTPAddr, server); err != nil {
		logger.Error("backend stopped", "error", err)
		os.Exit(1)
	}
}

func configureTelegramBot(logger *slog.Logger, bot *telegram.BotSender, webhookURL string, secret string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := bot.SetCommands(ctx, []telegram.BotCommand{
		{Command: "start", Description: "Первичная настройка помощника"},
		{Command: "status", Description: "Статус подключения, чаты и режимы"},
		{Command: "chats", Description: "Список чатов и управление режимами"},
		{Command: "summary", Description: "Сводка выбранного чата"},
		{Command: "profile", Description: "Профиль контакта и прогресс контекста"},
		{Command: "profile_set", Description: "Заполнить профиль контакта"},
		{Command: "help", Description: "Список команд"},
		{Command: "mode_silent", Description: "Только наблюдение"},
		{Command: "mode_manual", Description: "Черновики на одобрение"},
		{Command: "mode_semi_auto", Description: "Умные автоответы"},
		{Command: "mode_auto", Description: "Полная автономия"},
	}); err != nil {
		logger.Error("telegram set commands failed", "error", err)
	} else {
		logger.Info("telegram commands configured")
	}
	if err := bot.SetWebhook(ctx, webhookURL, secret); err != nil {
		logger.Error("telegram set webhook failed", "error", err)
		return
	}
	logger.Info("telegram webhook configured", "url", webhookURL)
}
