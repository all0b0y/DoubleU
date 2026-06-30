// Package store provides the repository used by the DoubleU backend.
package store

import (
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/lib/pq"

	"doubleu/backend-go/internal/contracts"
)

// FormAnswer pairs one questionnaire question with the contact's answer to it.
type FormAnswer struct {
	Prompt string `json:"prompt"`
	Answer string `json:"answer"`
}

// InteractionFormRecord is the persisted state of a step-by-step questionnaire
// (interaction.ask_questions). Questions are asked one at a time; Answers grows
// as the contact replies (by button tap or free text) and len(Answers) equals
// CurrentIndex. Exactly one form per chat may be active at a time.
type InteractionFormRecord struct {
	ID              int64                `json:"id"`
	TenantID        string               `json:"tenant_id"`
	ChatID          string               `json:"chat_id"`
	BotID           string               `json:"bot_id,omitempty"`
	Questions       []contracts.Question `json:"questions"`
	CurrentIndex    int                  `json:"current_index"`
	Answers         []FormAnswer         `json:"answers"`
	Status          string               `json:"status"`
	SourceMessageID string               `json:"source_message_id,omitempty"`
	CreatedAt       time.Time            `json:"created_at"`
	ExpiresAt       time.Time            `json:"expires_at"`
}

// Repository stores state — in memory when no DB is configured, in Postgres otherwise.
type Repository struct {
	mu           sync.Mutex
	db           *sql.DB
	chatSettings map[string]contracts.ChatSettings
	receiveKeys  map[string]struct{}
	sendKeys     map[string]SendRecord
	callbackKeys map[string]struct{}
	messages     map[string]MessageRecord
	// chatMessageKeys indexes message keys per chat in insertion order so the
	// in-memory RecentMessages path avoids an O(all messages) scan (ARCH-13).
	chatMessageKeys map[string][]string
	// profileSuggestions is the in-memory fallback for contact_profile_suggestions (MEM-4).
	profileSuggestions  []ProfileSuggestionRecord
	audits              []contracts.AuditEvent
	drafts              map[string]DraftRecord
	draftsByMsg         map[string]string
	businessConnections map[string]BusinessConnectionRecord
	pendingInputs       map[string]contracts.PendingInput
	draftFeedbacks      []contracts.DraftFeedback
	contactProfiles     map[string]contracts.ContactProfile
	messageCounts       map[string]int
	connectionByUser    map[string]string
	connectionTenants   map[string]string
	chats               map[string]ChatRecord
	unclaimedChats      map[string]UnclaimedChatRecord
	// userBots and tenantModes back bot-transport ingestion in memory mode
	// (user_bots table / tenants.ingestion_mode column in Postgres mode).
	userBots    map[string]UserBotRecord
	tenantModes map[string]contracts.IngestionMode
	// roleContexts are reusable bot memory contexts keyed by bot and nullable role.
	roleContexts   map[int64]RoleContextRecord
	roleContextSeq int64
	// New memory architecture fields
	semanticFacts    []SemanticFactRecord
	activeEpisodes   map[string]ActiveEpisodeRecord
	episodeSummaries []EpisodeSummaryRecord
	userSettings     map[string]contracts.UserSettings // keyed by tenantID
	// AI roles (in-memory mode): role records by ID plus per-chat assignment.
	aiRoles       map[int64]AIRoleRecord
	aiRoleSeq     int64
	chatAIRoleIDs map[string]int64 // chatKey -> ai_roles.id
	// scheduledJobs is the in-memory fallback for migration 114.
	scheduledJobs   map[int64]ScheduledJobRecord
	scheduledJobSeq int64
	// interactionForms is the in-memory fallback for migration 116.
	interactionForms   map[int64]InteractionFormRecord
	interactionFormSeq int64
	// ctxScoreCache memoises ContextScoreForChat with a short TTL (ARCH-8):
	// the score gates mode selection and was recomputed (3 queries) per check.
	// Writes that affect the score delete the entry so reads stay fresh.
	ctxScoreCache sync.Map // chatKey -> ctxScoreEntry
	// logger reports memory-layer write failures (MEM-10). Defaults to
	// slog.Default(); override via SetLogger.
	logger *slog.Logger
	// halfvecOK is true when pgvector ≥ 0.7 provides the halfvec type, so
	// vector queries can target the HNSW expression indexes (MEM-7).
	halfvecOK bool
}

// SetLogger overrides the repository logger (MEM-10 write-path observability).
func (r *Repository) SetLogger(logger *slog.Logger) {
	if logger != nil {
		r.logger = logger
	}
}

// logWriteError records a failed memory-layer write. Memory writes are
// fire-and-forget by design, but the two silent-failure bugs found in the
// 2026-06-11 audit (lost summaries, never-saved embeddings) lived undetected
// precisely because these errors were discarded (MEM-10).
func (r *Repository) logWriteError(op, tenantID, chatID string, err error) {
	if err == nil {
		return
	}
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	// TODO(observability): also increment memory_write_errors_total{op} once a
	// counters mechanism exists in the observability package.
	logger.Warn("memory write failed", "op", op, "tenant_id", tenantID, "chat_id", chatID, "error", err)
}

// ctxScoreEntry is one cached context-score value with its computation time.
type ctxScoreEntry struct {
	score int
	at    time.Time
}

// ctxScoreTTL bounds context-score staleness for reads not covered by
// write-path invalidation (e.g. fact decay).
const ctxScoreTTL = 30 * time.Second

// NewRepository creates an empty repository.
func NewRepository() *Repository {
	return &Repository{
		chatSettings:        make(map[string]contracts.ChatSettings),
		receiveKeys:         make(map[string]struct{}),
		sendKeys:            make(map[string]SendRecord),
		callbackKeys:        make(map[string]struct{}),
		messages:            make(map[string]MessageRecord),
		chatMessageKeys:     make(map[string][]string),
		drafts:              make(map[string]DraftRecord),
		draftsByMsg:         make(map[string]string),
		businessConnections: make(map[string]BusinessConnectionRecord),
		connectionByUser:    make(map[string]string),
		connectionTenants:   make(map[string]string),
		chats:               make(map[string]ChatRecord),
		unclaimedChats:      make(map[string]UnclaimedChatRecord),
		userBots:            make(map[string]UserBotRecord),
		tenantModes:         make(map[string]contracts.IngestionMode),
		roleContexts:        make(map[int64]RoleContextRecord),
		pendingInputs:       make(map[string]contracts.PendingInput),
		contactProfiles:     make(map[string]contracts.ContactProfile),
		messageCounts:       make(map[string]int),
		activeEpisodes:      make(map[string]ActiveEpisodeRecord),
		userSettings:        make(map[string]contracts.UserSettings),
		aiRoles:             make(map[int64]AIRoleRecord),
		chatAIRoleIDs:       make(map[string]int64),
		scheduledJobs:       make(map[int64]ScheduledJobRecord),
		interactionForms:    make(map[int64]InteractionFormRecord),
	}
}

// NewPostgresRepository creates a repository backed by Postgres plus an in-memory cache.
// Connection pool is sized for moderate concurrent load: up to 25 open connections,
// 5 idle connections kept alive, with a 1-hour max lifetime to handle DB-side timeouts.
func NewPostgresRepository(databaseURL string, migrationPaths ...string) (*Repository, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	repository := NewRepository()
	repository.db = db
	if err := repository.applyMigrations(migrationPaths...); err != nil {
		_ = db.Close()
		return nil, err
	}
	// halfvec (pgvector ≥ 0.7) enables the HNSW expression indexes from
	// migration 015; queries emit halfvec casts only when the type exists.
	_ = db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'halfvec')`,
	).Scan(&repository.halfvecOK)
	return repository, nil
}

// Close releases database resources held by the repository.
func (r *Repository) Close() error {
	if r.db == nil {
		return nil
	}
	return r.db.Close()
}

func (r *Repository) ensureTenant(tenantID, telegramUserID string) {
	if r.db == nil || tenantID == "" {
		return
	}
	var ownerUserID any
	if telegramUserID != "" {
		userID := "user:" + telegramUserID
		_, _ = r.db.Exec(
			`INSERT INTO users (id, telegram_user_id)
			 VALUES ($1, $2)
			 ON CONFLICT (id) DO UPDATE SET telegram_user_id = EXCLUDED.telegram_user_id, updated_at = now()`,
			userID,
			telegramUserID,
		)
		ownerUserID = userID
	}
	_, _ = r.db.Exec(
		`INSERT INTO tenants (id, owner_user_id, name)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (id) DO UPDATE SET owner_user_id = COALESCE(EXCLUDED.owner_user_id, tenants.owner_user_id), updated_at = now()`,
		tenantID,
		ownerUserID,
		tenantID,
	)
}

// SetPendingInput stores a pending-input state for an owner chat (TTL enforced at read time).
func (r *Repository) SetPendingInput(tenantID, chatID string, input contracts.PendingInput) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingInputs[chatKey(tenantID, chatID)] = input
}

// PendingInput returns the pending input state for an owner chat, if any and not expired.
func (r *Repository) PendingInput(tenantID, chatID string) (contracts.PendingInput, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	input, ok := r.pendingInputs[chatKey(tenantID, chatID)]
	if !ok {
		return contracts.PendingInput{}, false
	}
	if !input.ExpiresAt.IsZero() && time.Now().After(input.ExpiresAt) {
		delete(r.pendingInputs, chatKey(tenantID, chatID))
		return contracts.PendingInput{}, false
	}
	return input, true
}

// ClearPendingInput removes any pending input state for an owner chat.
func (r *Repository) ClearPendingInput(tenantID, chatID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pendingInputs, chatKey(tenantID, chatID))
}

// RoleMCPServers returns the enabled MCP servers a role may call, with each
// server's per-role tool allow-list (#7). Read-only; nil without a DB.
func (r *Repository) RoleMCPServers(roleID int64) []contracts.MCPServerSpec {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT s.name, s.transport, s.url, COALESCE(s.auth_header,''), a.allowed_tools
		   FROM role_mcp_access a
		   JOIN mcp_servers s ON s.id = a.mcp_server_id
		  WHERE a.role_id = $1 AND s.enabled`,
		roleID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var specs []contracts.MCPServerSpec
	for rows.Next() {
		var spec contracts.MCPServerSpec
		var allowed pq.StringArray
		if rows.Scan(&spec.Name, &spec.Transport, &spec.URL, &spec.AuthHeader, &allowed) == nil {
			spec.AllowedTools = []string(allowed)
			specs = append(specs, spec)
		}
	}
	return specs
}

// SaveMCPServer upserts a tenant's MCP server (by tenant+name) and returns its
// id. Minimal seeding helper; owner-facing registration is deferred (#7).
func (r *Repository) SaveMCPServer(tenantID, name, transport, url, authHeader string) int64 {
	if r.db == nil {
		return 0
	}
	var id int64
	err := r.db.QueryRow(
		`INSERT INTO mcp_servers (tenant_id, name, transport, url, auth_header)
		 VALUES ($1,$2,$3,$4, NULLIF($5,''))
		 ON CONFLICT (tenant_id, name)
		 DO UPDATE SET transport = EXCLUDED.transport, url = EXCLUDED.url,
		               auth_header = EXCLUDED.auth_header, enabled = true
		 RETURNING id`,
		tenantID, name, transport, url, authHeader,
	).Scan(&id)
	if err != nil {
		r.logWriteError("save_mcp_server", tenantID, "", err)
		return 0
	}
	return id
}

// SetRoleMCPAccess grants a role access to a server with an optional tool
// allow-list (nil/empty = all tools). Minimal seeding helper (#7).
func (r *Repository) SetRoleMCPAccess(roleID, serverID int64, allowedTools []string) {
	if r.db == nil {
		return
	}
	var allowed any
	if len(allowedTools) > 0 {
		allowed = pq.Array(allowedTools)
	}
	_, err := r.db.Exec(
		`INSERT INTO role_mcp_access (role_id, mcp_server_id, allowed_tools)
		 VALUES ($1,$2,$3)
		 ON CONFLICT (role_id, mcp_server_id) DO UPDATE SET allowed_tools = EXCLUDED.allowed_tools`,
		roleID, serverID, allowed,
	)
	if err != nil {
		r.logWriteError("set_role_mcp_access", "", "", err)
	}
}

// ============================================================
// Helpers
// ============================================================

func boolPtrFromNull(value sql.NullBool) *bool {
	if !value.Valid {
		return nil
	}
	v := value.Bool
	return &v
}

func cloneBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

func nullableBoolArg(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// nilToEmpty normalises a nil string slice to an empty (non-nil) slice so that
// pq.Array serialises it as `'{}'::text[]` instead of SQL NULL.  Required for
// columns declared `TEXT[] NOT NULL DEFAULT '{}'`, because `pq.Array(nil)` emits
// NULL and overrides the column DEFAULT, causing NOT NULL constraint failures.
func nilToEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nilToEmptyInt64(s []int64) []int64 {
	if s == nil {
		return []int64{}
	}
	return s
}

func chatKey(tenantID, chatID string) string {
	return tenantID + ":" + chatID
}
