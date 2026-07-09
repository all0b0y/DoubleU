// Package store provides the repository used by the DoubleU backend.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/lib/pq"

	"doubleu/backend-go/internal/contracts"
)

// ErrMissingSecretsKey is returned when a caller tries to persist a non-empty
// secret but no encryption key was injected — encryption is mandatory, so we
// fail closed rather than fall back to plaintext.
var ErrMissingSecretsKey = errors.New("cannot store secret: MCP_SECRETS_KEY is not configured")

// ErrMCPServerNotFound signals that an update/delete matched no row for the
// caller's tenant (missing id or cross-tenant attempt).
var ErrMCPServerNotFound = errors.New("mcp server not found")

// encryptSecret seals plaintext with AES-256-GCM and returns nonce||ciphertext.
// A fresh random nonce is prepended to every value (crypto/rand), so the same
// header encrypts to a different blob each time.
func encryptSecret(key []byte, plaintext string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	// Seal appends the ciphertext to nonce, yielding nonce||ciphertext||tag.
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// decryptSecret reverses encryptSecret. Returns an error (never a partial or
// plaintext value) if the key is wrong or the blob is malformed/truncated.
func decryptSecret(key, blob []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(blob) < gcm.NonceSize() {
		return "", fmt.Errorf("mcp auth ciphertext is too short")
	}
	nonce, ciphertext := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

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
	// skill self-improvement in-memory fallbacks (migration 124): per-load usage
	// telemetry and immutable body versions.
	skillUsage    []SkillUsageRecord
	skillVersions []SkillVersionRecord
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
	// personal-bot mode in-memory fallbacks (migration 122): the per-bot
	// allowlist of Telegram user ids, and the set of contacts already shown the
	// stub. botAllowedUsers is keyed by botStoreKey → set of telegram ids;
	// personalStubShown by tenantID+"#"+chatID.
	botAllowedUsers   map[string]map[string]struct{}
	personalStubShown map[string]struct{}
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
	// secretsKey is the 32-byte AES-256 key encrypting MCP auth headers at rest
	// (Э0.3). Nil disables encryption; SaveMCPServer then refuses to store a
	// non-empty header (fail closed). Injected via SetSecretsKey, never global.
	secretsKey []byte
}

// SetLogger overrides the repository logger (MEM-10 write-path observability).
func (r *Repository) SetLogger(logger *slog.Logger) {
	if logger != nil {
		r.logger = logger
	}
}

// SetSecretsKey injects the AES-256 key used to encrypt/decrypt MCP auth headers
// at rest (Э0.3). Mirrors SetLogger's post-construction injection so the key is
// an instance dependency, not package-global state (charter). A nil key leaves
// encryption disabled.
func (r *Repository) SetSecretsKey(key []byte) {
	r.secretsKey = key
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
		botAllowedUsers:     make(map[string]map[string]struct{}),
		personalStubShown:   make(map[string]struct{}),
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
		`SELECT s.name, s.transport, s.url, COALESCE(s.auth_header,''), s.auth_header_enc, a.allowed_tools
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
		var plaintext string
		var enc []byte
		var allowed pq.StringArray
		if rows.Scan(&spec.Name, &spec.Transport, &spec.URL, &plaintext, &enc, &allowed) != nil {
			continue
		}
		// Prefer the encrypted header; fall back to a legacy plaintext row that
		// predates encryption. A decrypt failure yields an empty header (the
		// server degrades to unauthenticated) rather than a leaked/garbled value.
		switch {
		case len(enc) > 0 && len(r.secretsKey) > 0:
			if header, derr := decryptSecret(r.secretsKey, enc); derr == nil {
				spec.AuthHeader = header
			} else {
				r.logWriteError("decrypt_mcp_auth", "", "", derr)
			}
		default:
			spec.AuthHeader = plaintext
		}
		spec.AllowedTools = []string(allowed)
		specs = append(specs, spec)
	}
	return specs
}

// SaveMCPServer upserts a tenant's MCP server (by tenant+name) and returns its
// id. The auth header, when present, is encrypted at rest (Э0.3): it is written
// only to auth_header_enc and the legacy plaintext column is cleared. Fails
// closed — a non-empty header with no configured key is an error, never a
// plaintext write.
func (r *Repository) SaveMCPServer(tenantID, name, transport, url, authHeader string) (int64, error) {
	if r.db == nil {
		return 0, nil
	}
	var enc []byte
	if authHeader != "" {
		if len(r.secretsKey) == 0 {
			return 0, ErrMissingSecretsKey
		}
		var err error
		if enc, err = encryptSecret(r.secretsKey, authHeader); err != nil {
			return 0, err
		}
	}
	var id int64
	// New writes never populate the plaintext auth_header column; enc holds the
	// only copy. Legacy plaintext rows keep working via RoleMCPServers' COALESCE.
	err := r.db.QueryRow(
		`INSERT INTO mcp_servers (tenant_id, name, transport, url, auth_header, auth_header_enc)
		 VALUES ($1,$2,$3,$4, NULL, $5)
		 ON CONFLICT (tenant_id, name)
		 DO UPDATE SET transport = EXCLUDED.transport, url = EXCLUDED.url,
		               auth_header = NULL, auth_header_enc = EXCLUDED.auth_header_enc,
		               enabled = true
		 RETURNING id`,
		tenantID, name, transport, url, enc,
	).Scan(&id)
	if err != nil {
		r.logWriteError("save_mcp_server", tenantID, "", err)
		return 0, err
	}
	return id, nil
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

// MCPServerRecord is the owner-facing view of one configured MCP server. The
// auth header value NEVER leaves the store in this shape — only HasAuth, the
// fact of its presence, so the UI can show "заголовок задан" without exposure.
type MCPServerRecord struct {
	ID        int64
	Name      string
	Transport string
	URL       string
	HasAuth   bool
	Enabled   bool
}

// MCPServersForTenant lists a tenant's MCP servers for the owner UI (Э1.1). One
// query; the auth header value is never selected, only its presence (HasAuth).
func (r *Repository) MCPServersForTenant(tenantID string) []MCPServerRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT id, name, transport, url,
		        (auth_header IS NOT NULL OR auth_header_enc IS NOT NULL) AS has_auth, enabled
		   FROM mcp_servers WHERE tenant_id = $1 ORDER BY name`,
		tenantID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []MCPServerRecord
	for rows.Next() {
		var rec MCPServerRecord
		if rows.Scan(&rec.ID, &rec.Name, &rec.Transport, &rec.URL, &rec.HasAuth, &rec.Enabled) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// UpdateMCPServer updates a tenant's server in place (Э1.1). An empty authHeader
// leaves the stored secret untouched; a non-empty one is re-encrypted (fail
// closed without a key), clearing any legacy plaintext. Scoped by tenant_id;
// returns an error when no row matches (not found or wrong tenant).
func (r *Repository) UpdateMCPServer(tenantID string, id int64, name, transport, url, authHeader string, enabled bool) error {
	if r.db == nil {
		return nil
	}
	var (
		res sql.Result
		err error
	)
	if authHeader == "" {
		res, err = r.db.Exec(
			`UPDATE mcp_servers SET name=$1, transport=$2, url=$3, enabled=$4
			  WHERE id=$5 AND tenant_id=$6`,
			name, transport, url, enabled, id, tenantID,
		)
	} else {
		if len(r.secretsKey) == 0 {
			return ErrMissingSecretsKey
		}
		enc, encErr := encryptSecret(r.secretsKey, authHeader)
		if encErr != nil {
			return encErr
		}
		res, err = r.db.Exec(
			`UPDATE mcp_servers SET name=$1, transport=$2, url=$3, enabled=$4,
			        auth_header=NULL, auth_header_enc=$5
			  WHERE id=$6 AND tenant_id=$7`,
			name, transport, url, enabled, enc, id, tenantID,
		)
	}
	if err != nil {
		r.logWriteError("update_mcp_server", tenantID, "", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMCPServerNotFound
	}
	return nil
}

// DeleteMCPServer removes a tenant's server (Э1.1). Scoped by tenant_id so one
// tenant cannot delete another's row; an unmatched id/tenant is an error.
func (r *Repository) DeleteMCPServer(tenantID string, id int64) error {
	if r.db == nil {
		return nil
	}
	res, err := r.db.Exec(`DELETE FROM mcp_servers WHERE id=$1 AND tenant_id=$2`, id, tenantID)
	if err != nil {
		r.logWriteError("delete_mcp_server", tenantID, "", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMCPServerNotFound
	}
	return nil
}

// RoleMCPAccessRecord is one role→server grant with its tool allow-list
// (nil = all tools), for rendering the per-role MCP toggles (Э1.1).
type RoleMCPAccessRecord struct {
	ServerID     int64
	AllowedTools []string
}

// RoleMCPAccessByRole returns every server a role is granted, with its allow-list
// (Э1.1). One query; nil AllowedTools means all tools are permitted.
func (r *Repository) RoleMCPAccessByRole(roleID int64) []RoleMCPAccessRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT mcp_server_id, allowed_tools FROM role_mcp_access WHERE role_id = $1`,
		roleID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []RoleMCPAccessRecord
	for rows.Next() {
		var rec RoleMCPAccessRecord
		var allowed pq.StringArray
		if rows.Scan(&rec.ServerID, &allowed) == nil {
			rec.AllowedTools = []string(allowed)
			out = append(out, rec)
		}
	}
	return out
}

// SetRoleMCPServers replaces a role's entire MCP grant set in one transaction
// (Э1.2): the toggle UI sends the full desired set, so de-selected servers must
// lose their grant. DELETE-then-INSERT keeps the read side (RoleMCPServers) in
// sync with exactly what the owner chose. A nil/empty AllowedTools means all
// tools. Caller is responsible for validating server ownership.
func (r *Repository) SetRoleMCPServers(roleID int64, grants []RoleMCPAccessRecord) error {
	if r.db == nil {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err := tx.Exec(`DELETE FROM role_mcp_access WHERE role_id = $1`, roleID); err != nil {
		return err
	}
	for _, g := range grants {
		var allowed any
		if len(g.AllowedTools) > 0 {
			allowed = pq.Array(g.AllowedTools)
		}
		if _, err := tx.Exec(
			`INSERT INTO role_mcp_access (role_id, mcp_server_id, allowed_tools) VALUES ($1,$2,$3)`,
			roleID, g.ServerID, allowed,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MCPServerForCheck returns one server's transport/url and DECRYPTED auth header
// for the internal Go→Python /ai/mcp-check probe (Э1.3). The header travels only
// over that internal channel — exactly like the per-request MCPServerSpec today —
// and is never returned to the owner UI. ok is false when no row matches the
// tenant (missing id or cross-tenant attempt).
func (r *Repository) MCPServerForCheck(tenantID string, id int64) (transport, url, authHeader string, ok bool) {
	if r.db == nil {
		return "", "", "", false
	}
	var plaintext string
	var enc []byte
	if err := r.db.QueryRow(
		`SELECT transport, url, COALESCE(auth_header,''), auth_header_enc
		   FROM mcp_servers WHERE id = $1 AND tenant_id = $2`,
		id, tenantID,
	).Scan(&transport, &url, &plaintext, &enc); err != nil {
		return "", "", "", false
	}
	authHeader = plaintext
	if len(enc) > 0 && len(r.secretsKey) > 0 {
		if h, derr := decryptSecret(r.secretsKey, enc); derr == nil {
			authHeader = h
		} else {
			r.logWriteError("decrypt_mcp_auth", tenantID, "", derr)
		}
	}
	return transport, url, authHeader, true
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
