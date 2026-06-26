package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // embed the IANA zone database — the alpine runtime image ships none

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/botservice"
	"doubleu/backend-go/internal/config"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/httpapi"
	"doubleu/backend-go/internal/observability"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

// migrationSearchRoots lists the directories where migration files may live.
// The first directory that exists wins; that lets the same binary work in the
// docker image (/migrations) and from the repo checkout (../infra/migrations).
var migrationSearchRoots = []string{
	"/migrations",
	"../infra/migrations",
	"infra/migrations",
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		runMigrateCommand(os.Args[2:])
		return
	}
	cfg := config.Load()
	flags := config.LoadFlags()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	logger.Info("feature flags loaded",
		"safety_input_enabled", flags.SafetyInputEnabled,
		"safety_output_enabled", flags.SafetyOutputEnabled,
		"llm_gateway", string(flags.LLMGateway),
		"observability_sink", string(flags.ObservabilitySink),
		"shadow_mode_only", flags.ShadowModeOnly,
		"cutover", flags.Cutover,
	)
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
	repository.SetLogger(logger)
	storage := "memory"
	if cfg.DatabaseURL != "" {
		migrationPaths := discoverMigrationPaths(logger)
		postgresRepository, err := store.NewPostgresRepository(cfg.DatabaseURL, migrationPaths...)
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
	obsShutdown, err := observability.Init(context.Background(), logger, flags)
	if err != nil {
		logger.Error("observability sink init failed", "error", err)
	}
	defer func() {
		_ = obsShutdown(context.Background())
	}()
	hub := httpapi.NewSSEHub()
	ingestionDefault, ok := contracts.ParseIngestionMode(cfg.IngestionModeDefault)
	if !ok {
		logger.Warn("invalid INGESTION_MODE_DEFAULT, falling back to bot", "value", cfg.IngestionModeDefault)
		ingestionDefault = contracts.IngestionModeBot
	}
	// The bot manager and the core service reference each other (manager feeds
	// events in, service resolves outbound senders through the manager), so the
	// handler closes over the service variable assigned right below.
	var service *core.Service
	botManager := botservice.NewManager(repository,
		botservice.HandlerFuncs{Message: botservice.HandlerFunc(func(ctx context.Context, event contracts.NormalizedTelegramEvent, media *telegram.MediaDescriptor) error {
			// Turn any voice/photo/document into text before AI processing so the
			// agent stays text-only (media -> text). Best-effort: never blocks the
			// message on extraction failure.
			event = service.EnrichBotMedia(ctx, event, media)
			_, err := service.HandleBotMessage(ctx, event)
			return err
		}), Callback: func(ctx context.Context, callback telegram.CallbackQueryUpdate, tenantID, botID string) error {
			_, err := service.HandleBotCallbackQuery(ctx, callback, tenantID, botID)
			return err
		}},
		telegramHTTPClient,
		botservice.WithLogger(logger),
		botservice.WithDefaultIngestionMode(ingestionDefault),
	)
	mediaCfg := core.DefaultMediaConfig()
	mediaCfg.Enabled = cfg.MediaExtractionEnabled
	service = core.NewService(repository, ai, sender,
		core.WithLogger(logger),
		core.WithReplyDelay(cfg.ReplyDelay),
		core.WithNotifier(hub.Broadcast),
		core.WithBotSenderProvider(botManager),
		core.WithDefaultIngestionMode(ingestionDefault),
		core.WithAIMemorySource(string(flags.AIMemorySource)),
		core.WithReflectionMode(string(flags.ReflectionMode)),
		core.WithMediaConfig(mediaCfg),
	)
	server := httpapi.NewServer(service,
		httpapi.WithHub(hub),
		httpapi.WithBotToken(cfg.TelegramBotToken),
		httpapi.WithWebhookSecret(cfg.TelegramWebhookSecret),
		httpapi.WithMiniAppDir(cfg.MiniAppDir),
		httpapi.WithBotManager(botManager),
		httpapi.WithAIBaseURL(cfg.AIBaseURL),
	)
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	server.Start(serverCtx)
	go botManager.Run(serverCtx)
	go service.StartEmbeddingWorker(serverCtx)
	go service.StartEpisodeCloser(serverCtx)
	go service.StartDailySummaryScheduler(serverCtx)
	go service.StartScheduler(serverCtx)
	go service.StartConsolidationWorker(serverCtx)
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
		{Command: "forget", Description: "Стереть память чата (безвозвратно)"},
		{Command: "forget_all", Description: "Стереть все данные (безвозвратно)"},
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

// discoverMigrationPaths returns every *.sql up-migration found under the
// first existing search root, sorted by their numeric version prefix.
// *.down.sql files are excluded — they are picked up by name during rollback.
func discoverMigrationPaths(logger *slog.Logger) []string {
	root := pickMigrationRoot()
	if root == "" {
		logger.Warn("no migration directory found", "searched", migrationSearchRoots)
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		logger.Error("read migration directory failed", "root", root, "error", err)
		return nil
	}
	type versioned struct {
		path    string
		version int
	}
	out := make([]versioned, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") || strings.HasSuffix(name, ".down.sql") {
			continue
		}
		v := versionFromFilename(name)
		out = append(out, versioned{path: filepath.Join(root, name), version: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].version != out[j].version {
			return out[i].version < out[j].version
		}
		return out[i].path < out[j].path
	})
	paths := make([]string, len(out))
	for i, v := range out {
		paths[i] = v.path
	}
	return paths
}

func pickMigrationRoot() string {
	for _, root := range migrationSearchRoots {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return root
		}
	}
	return ""
}

func versionFromFilename(name string) int {
	for i, ch := range name {
		if ch < '0' || ch > '9' {
			if i == 0 {
				return 0
			}
			v, _ := strconv.Atoi(name[:i])
			return v
		}
	}
	v, _ := strconv.Atoi(name)
	return v
}

// runMigrateCommand handles `backend migrate {up|down|status} [args]`.
func runMigrateCommand(args []string) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(args) == 0 {
		printMigrateUsage()
		os.Exit(2)
	}
	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL must be set for migrate commands")
		os.Exit(2)
	}
	paths := discoverMigrationPaths(logger)
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "no migration files discovered")
		os.Exit(2)
	}
	switch args[0] {
	case "up":
		repo, err := store.NewPostgresRepository(cfg.DatabaseURL, paths...)
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrate up failed:", err)
			os.Exit(1)
		}
		_ = repo.Close()
		fmt.Println("migrate up: complete")
	case "down":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: backend migrate down <target_version>")
			os.Exit(2)
		}
		target, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "target_version must be an integer:", err)
			os.Exit(2)
		}
		// Connect WITHOUT running up-migrations first.
		repo, err := store.NewPostgresRepository(cfg.DatabaseURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrate down failed:", err)
			os.Exit(1)
		}
		defer repo.Close()
		if err := repo.MigrateDown(target, paths...); err != nil {
			fmt.Fprintln(os.Stderr, "migrate down failed:", err)
			os.Exit(1)
		}
		fmt.Printf("migrate down: rolled back to version %d\n", target)
	case "status":
		repo, err := store.NewPostgresRepository(cfg.DatabaseURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrate status failed:", err)
			os.Exit(1)
		}
		defer repo.Close()
		statuses, err := repo.MigrationStatuses(paths...)
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrate status failed:", err)
			os.Exit(1)
		}
		fmt.Printf("%-6s %-40s %-9s %s\n", "VER", "FILENAME", "APPLIED", "FLAGS")
		for _, s := range statuses {
			flag := ""
			if s.Drifted {
				flag = "DRIFT"
			}
			fmt.Printf("%-6d %-40s %-9t %s\n", s.Version, s.Filename, s.Applied, flag)
		}
	default:
		printMigrateUsage()
		os.Exit(2)
	}
}

func printMigrateUsage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  backend migrate up                    # apply pending migrations")
	fmt.Fprintln(os.Stderr, "  backend migrate down <target_version> # roll back to target version (0 = all)")
	fmt.Fprintln(os.Stderr, "  backend migrate status                # show applied/pending migrations")
}
