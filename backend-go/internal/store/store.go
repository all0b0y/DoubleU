// Package store provides the repository used by the DoubleU backend.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"

	"doubleu/backend-go/internal/contracts"
)

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
		pendingInputs:       make(map[string]contracts.PendingInput),
		contactProfiles:     make(map[string]contracts.ContactProfile),
		messageCounts:       make(map[string]int),
		activeEpisodes:      make(map[string]ActiveEpisodeRecord),
		userSettings:        make(map[string]contracts.UserSettings),
		aiRoles:             make(map[int64]AIRoleRecord),
		chatAIRoleIDs:       make(map[string]int64),
		scheduledJobs:       make(map[int64]ScheduledJobRecord),
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

// vectorOrderExpr returns the cosine-distance ORDER BY expression for the
// given embedding column and query-vector placeholder. With halfvec available
// the expression matches the migration-015 HNSW indexes so the planner uses
// ANN; otherwise it stays on the plain vector operator (seq scan, MEM-7).
func (r *Repository) vectorOrderExpr(column, placeholder string) string {
	if r.halfvecOK {
		return fmt.Sprintf("(%s::halfvec(4096) <=> %s::halfvec(4096))", column, placeholder)
	}
	return fmt.Sprintf("(%s <=> %s::vector)", column, placeholder)
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

// ============================================================
// Contact Profile (Per-Chat Identity Layer)
// ============================================================

// SaveContactProfile stores the owner-supplied contact profile for a chat.
// It maps both the legacy fields (Name, Notes) and the new architecture fields
// (DisplayNames, StyleNotes, ChatContext, Tone, EmojiUsage, AddressForm, …) to
// the contact_profiles table.
func (r *Repository) SaveContactProfile(profile contracts.ContactProfile) {
	r.invalidateContextScore(profile.TenantID, profile.ChatID)
	// Synchronise Name ↔ DisplayNames[0] so both representations stay consistent.
	if profile.Name != "" && len(profile.DisplayNames) == 0 {
		profile.DisplayNames = []string{profile.Name}
	} else if len(profile.DisplayNames) > 0 && profile.Name == "" {
		profile.Name = profile.DisplayNames[0]
	}

	// Parse the legacy notes JSON field into the new dedicated columns.
	if profile.Notes != "" {
		var notes struct {
			MeStyle string `json:"me_style"`
			ChatCtx string `json:"chat_context"`
		}
		if err := json.Unmarshal([]byte(profile.Notes), &notes); err == nil {
			if notes.MeStyle != "" && profile.StyleNotes == "" {
				profile.StyleNotes = notes.MeStyle
			}
			if notes.ChatCtx != "" && profile.ChatContext == "" {
				profile.ChatContext = notes.ChatCtx
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.contactProfiles[chatKey(profile.TenantID, profile.ChatID)] = profile
	if r.db != nil {
		_, _ = r.db.Exec(
			`INSERT INTO contact_profiles (
			   tenant_id, chat_id, name, gender, role, relationship_type,
			   display_names, preferred_address, language, contact_timezone,
			   tone, emoji_usage, address_form, style_notes,
			   topics, chat_context, do_not_discuss,
			   notes, updated_at
			 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,now())
			 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
			   name              = EXCLUDED.name,
			   gender            = EXCLUDED.gender,
			   role              = EXCLUDED.role,
			   relationship_type = EXCLUDED.relationship_type,
			   display_names     = EXCLUDED.display_names,
			   preferred_address = EXCLUDED.preferred_address,
			   language          = EXCLUDED.language,
			   contact_timezone  = EXCLUDED.contact_timezone,
			   tone              = EXCLUDED.tone,
			   emoji_usage       = EXCLUDED.emoji_usage,
			   address_form      = EXCLUDED.address_form,
			   style_notes       = EXCLUDED.style_notes,
			   topics            = EXCLUDED.topics,
			   chat_context      = EXCLUDED.chat_context,
			   do_not_discuss    = EXCLUDED.do_not_discuss,
			   notes             = EXCLUDED.notes,
			   updated_at        = now()`,
			profile.TenantID, profile.ChatID,
			profile.Name, profile.Gender, profile.Role, profile.RelationshipType,
			pq.Array(nilToEmpty(profile.DisplayNames)),
			nullableString(profile.PreferredAddress),
			nullableString(profile.Language),
			nullableString(profile.ContactTimezone),
			nullableString(profile.Tone),
			nullableString(profile.EmojiUsage),
			nullableString(profile.AddressForm),
			nullableString(profile.StyleNotes),
			pq.Array(nilToEmpty(profile.Topics)),
			nullableString(profile.ChatContext),
			pq.Array(nilToEmpty(profile.DoNotDiscuss)),
			buildLegacyNotes(profile),
		)
	}
}

// buildLegacyNotes serialises the new style fields back to the legacy JSON notes format
// so the existing Mini App UI (which reads notes.me_style and notes.chat_context) continues
// to work without changes.
func buildLegacyNotes(p contracts.ContactProfile) string {
	type notesPayload struct {
		MeStyle string `json:"me_style,omitempty"`
		ChatCtx string `json:"chat_context,omitempty"`
	}
	payload := notesPayload{MeStyle: p.StyleNotes, ChatCtx: p.ChatContext}
	b, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ContactProfile returns the contact profile for a chat.
// It populates both legacy fields and new architecture fields from the DB row.
func (r *Repository) ContactProfile(tenantID, chatID string) (contracts.ContactProfile, bool) {
	if r.db != nil {
		var p contracts.ContactProfile
		p.TenantID = tenantID
		p.ChatID = chatID
		var displayNames pq.StringArray
		var topics pq.StringArray
		var doNotDiscuss pq.StringArray
		var (
			preferredAddress sql.NullString
			language         sql.NullString
			contactTimezone  sql.NullString
			tone             sql.NullString
			emojiUsage       sql.NullString
			addressForm      sql.NullString
			styleNotes       sql.NullString
			chatContext      sql.NullString
			notes            sql.NullString
		)
		err := r.db.QueryRow(
			`SELECT COALESCE(name,''), COALESCE(gender,''), COALESCE(role,''),
			        COALESCE(relationship_type,''),
			        COALESCE(display_names, ARRAY[]::text[]),
			        preferred_address, language, contact_timezone,
			        tone, emoji_usage, address_form, style_notes,
			        COALESCE(topics, ARRAY[]::text[]),
			        chat_context,
			        COALESCE(do_not_discuss, ARRAY[]::text[]),
			        notes
			   FROM contact_profiles WHERE tenant_id=$1 AND chat_id=$2`,
			tenantID, chatID,
		).Scan(
			&p.Name, &p.Gender, &p.Role, &p.RelationshipType,
			&displayNames,
			&preferredAddress, &language, &contactTimezone,
			&tone, &emojiUsage, &addressForm, &styleNotes,
			&topics,
			&chatContext,
			&doNotDiscuss,
			&notes,
		)
		if err == nil {
			p.DisplayNames = []string(displayNames)
			p.Topics = []string(topics)
			p.DoNotDiscuss = []string(doNotDiscuss)
			p.PreferredAddress = preferredAddress.String
			p.Language = language.String
			p.ContactTimezone = contactTimezone.String
			p.Tone = tone.String
			p.EmojiUsage = emojiUsage.String
			p.AddressForm = addressForm.String
			p.StyleNotes = styleNotes.String
			p.ChatContext = chatContext.String
			p.Notes = notes.String
			// Ensure Name and DisplayNames are consistent.
			if p.Name == "" && len(p.DisplayNames) > 0 {
				p.Name = p.DisplayNames[0]
			}
			// Populate legacy notes from new columns if notes is empty.
			if p.Notes == "" {
				p.Notes = buildLegacyNotes(p)
			}
			return p, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.contactProfiles[chatKey(tenantID, chatID)]
	return p, ok
}

// ContextScoreForChat computes the 0–100 richness score for a chat.
// Results are cached per chat with a short TTL (ARCH-8); writes that change
// the inputs (profile, facts, feedback) invalidate the entry immediately.
func (r *Repository) ContextScoreForChat(tenantID, chatID string) int {
	key := tenantID + "/" + chatID
	if v, ok := r.ctxScoreCache.Load(key); ok {
		if e, ok := v.(ctxScoreEntry); ok && time.Since(e.at) < ctxScoreTTL {
			return e.score
		}
	}
	profile, _ := r.ContactProfile(tenantID, chatID)
	facts := r.SemanticFacts(tenantID, chatID, 20)
	feedback := r.RecentFeedback(tenantID, chatID, 10)
	score := contracts.ContextScore(profile, facts, feedback)
	r.ctxScoreCache.Store(key, ctxScoreEntry{score: score, at: time.Now()})
	return score
}

// invalidateContextScore drops the cached score after a write that affects it.
func (r *Repository) invalidateContextScore(tenantID, chatID string) {
	r.ctxScoreCache.Delete(tenantID + "/" + chatID)
}

// AllActiveTenantIDs returns all non-staging tenant IDs that have at least one chat.
func (r *Repository) AllActiveTenantIDs() []string {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT DISTINCT tenant_id FROM chats WHERE tenant_id NOT LIKE 'staging:%'`,
		)
		if err == nil {
			defer rows.Close()
			result := make([]string, 0)
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					result = append(result, id)
				}
			}
			return result
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, chat := range r.chats {
		if !strings.HasPrefix(chat.TenantID, "staging:") {
			if _, ok := seen[chat.TenantID]; !ok {
				seen[chat.TenantID] = struct{}{}
				result = append(result, chat.TenantID)
			}
		}
	}
	return result
}

// FactPair is one pair of similar facts (an edge for reflection clustering).
type FactPair struct {
	IDA int64
	IDB int64
}

// ReflectionCandidateChats returns chat ids for a tenant that have at least
// minFacts active facts created since `since` and do NOT carry the no_memory
// privacy flag — the chats worth running sleep-time reflection on (#5).
// Read-only; returns nil on error or without a DB.
func (r *Repository) ReflectionCandidateChats(tenantID string, since time.Time, minFacts int) []string {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT sf.chat_id
		   FROM semantic_facts sf
		   LEFT JOIN chat_settings cs
		     ON cs.tenant_id = sf.tenant_id AND cs.chat_id = sf.chat_id
		  WHERE sf.tenant_id = $1
		    AND sf.scope_kind = 'chat'
		    AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
		    AND sf.created_at >= $2
		    AND NOT ('no_memory' = ANY(COALESCE(cs.privacy_flags, ARRAY[]::public.privacy_flag[])::text[]))
		  GROUP BY sf.chat_id
		 HAVING count(*) >= $3`,
		tenantID, since, minFacts,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var chats []string
	for rows.Next() {
		var chatID string
		if rows.Scan(&chatID) == nil {
			chats = append(chats, chatID)
		}
	}
	return chats
}

// SimilarFactPairs returns pairs of a chat's active recent facts whose embeddings
// are within (1-minSim) cosine distance — the edges the reflection clusterer
// groups (#5). Only facts that have embeddings participate. Read-only.
func (r *Repository) SimilarFactPairs(tenantID, chatID string, since time.Time, minSim float64) []FactPair {
	if r.db == nil {
		return nil
	}
	maxDist := 1.0 - minSim
	rows, err := r.db.Query(
		`SELECT a.fact_id, b.fact_id
		   FROM fact_embeddings a
		   JOIN fact_embeddings b
		     ON b.tenant_id = a.tenant_id AND b.chat_id = a.chat_id AND a.fact_id < b.fact_id
		   JOIN semantic_facts sfa ON sfa.id = a.fact_id
		     AND sfa.deleted_at IS NULL AND sfa.superseded_by_id IS NULL AND sfa.created_at >= $3
		   JOIN semantic_facts sfb ON sfb.id = b.fact_id
		     AND sfb.deleted_at IS NULL AND sfb.superseded_by_id IS NULL AND sfb.created_at >= $3
		  WHERE a.tenant_id = $1 AND a.chat_id = $2
		    AND (a.embedding <=> b.embedding) < $4`,
		tenantID, chatID, since, maxDist,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var pairs []FactPair
	for rows.Next() {
		var p FactPair
		if rows.Scan(&p.IDA, &p.IDB) == nil {
			pairs = append(pairs, p)
		}
	}
	return pairs
}

// SemanticFactTextsByIDs returns id→fact_text for the given fact ids of a tenant,
// scoped defensively to the tenant. Used to materialise reflection clusters (#5).
func (r *Repository) SemanticFactTextsByIDs(tenantID string, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))
	if r.db == nil || len(ids) == 0 {
		return out
	}
	rows, err := r.db.Query(
		`SELECT id, fact_text FROM semantic_facts
		  WHERE tenant_id = $1 AND id = ANY($2)`,
		tenantID, pq.Array(ids),
	)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var text string
		if rows.Scan(&id, &text) == nil {
			out[id] = text
		}
	}
	return out
}

// ============================================================
// Semantic Layer (semantic_facts table)
// ============================================================

// normalizeFactKey approximates the DB normalize_fact_text hash for the
// in-memory fallback: lowercase, collapsed whitespace.
func normalizeFactKey(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// SaveSemanticFact stores a fact in the in-memory fallback only. Every DB
// write goes through the upsert_semantic_fact stored procedure (MEM-11): a
// direct INSERT path here would bypass dedup and the advisory lock.
// Returns the synthetic in-memory ID, or 0 when an exact duplicate exists.
func (r *Repository) SaveSemanticFact(fact SemanticFactRecord) int64 {
	if r.db != nil {
		return r.UpsertSemanticFact(fact, nil).FactID
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := normalizeFactKey(fact.FactText)
	for _, existing := range r.semanticFacts {
		if existing.TenantID == fact.TenantID && existing.ChatID == fact.ChatID &&
			existing.FactType == fact.FactType && existing.Subject == fact.Subject &&
			normalizeFactKey(existing.FactText) == key {
			return 0
		}
	}
	r.aiRoleSeq++ // reuse the in-memory sequence; IDs only need to be unique per process
	fact.ID = r.aiRoleSeq
	r.semanticFacts = append(r.semanticFacts, fact)
	return fact.ID
}

// UpsertSemanticFact deduplicates a fact via the upsert_semantic_fact v3 stored
// procedure when a DB is configured (MEM-1): exact-hash tier always runs; the
// semantic tiers run only when the candidate embedding is provided. The proc
// returns (action, fact_id, superseded_fact_id) so no follow-up query is needed.
// In-memory mode falls back to hash-style dedup on normalised text.
//
// The in-memory semanticFacts slice is NOT appended on the DB path (ARCH-13):
// it exists solely as the no-DB fallback and previously grew unboundedly for
// the lifetime of the process.
func (r *Repository) UpsertSemanticFact(fact SemanticFactRecord, embedding []float64) SemanticFactUpsertResult {
	if r.db != nil {
		var embeddingArg any
		if len(embedding) > 0 {
			embeddingArg = float64SliceToVector(embedding)
		}
		var result SemanticFactUpsertResult
		var superseded *int64
		err := r.db.QueryRow(
			`SELECT action, fact_id, superseded_fact_id
			   FROM upsert_semantic_fact($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			fact.TenantID,
			fact.ScopeKind,
			nullableString(fact.ChatID),
			fact.FactText,
			fact.FactType,
			fact.Subject,
			fact.Confidence,
			fact.Source,
			fact.SourceMessageID,
			nullableTime(fact.ExpiresAt),
			embeddingArg,
			"qwen/qwen3-embedding-8b",
		).Scan(&result.Action, &result.FactID, &superseded)
		if err != nil {
			r.logWriteError("upsert_semantic_fact", fact.TenantID, fact.ChatID, err)
			return SemanticFactUpsertResult{Action: "rejected"}
		}
		if superseded != nil {
			result.SupersededFactID = *superseded
		}
		r.invalidateContextScore(fact.TenantID, fact.ChatID)
		return result
	}
	id := r.SaveSemanticFact(fact)
	r.invalidateContextScore(fact.TenantID, fact.ChatID)
	if id == 0 {
		return SemanticFactUpsertResult{Action: "occurrence_added"}
	}
	return SemanticFactUpsertResult{FactID: id, Action: "inserted"}
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

// SemanticFacts returns active (non-deleted, non-superseded) chat-scoped semantic facts.
func (r *Repository) SemanticFacts(tenantID, chatID string, limit int) []SemanticFactRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT id, tenant_id, scope_kind, COALESCE(chat_id,''), fact_text, fact_type,
			        subject, confidence, source, COALESCE(source_message_id,''), expires_at, created_at
			   FROM semantic_facts
			  WHERE tenant_id = $1 AND chat_id = $2
			    AND scope_kind = 'chat'
			    AND deleted_at IS NULL AND superseded_by_id IS NULL
			  ORDER BY created_at DESC
			  LIMIT $3`,
			tenantID, chatID, limit,
		)
		if err == nil {
			defer rows.Close()
			return scanSemanticFacts(rows)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]SemanticFactRecord, 0)
	for i := len(r.semanticFacts) - 1; i >= 0 && len(result) < limit; i-- {
		f := r.semanticFacts[i]
		if f.TenantID == tenantID && f.ChatID == chatID && f.ScopeKind == "chat" {
			result = append(result, f)
		}
	}
	return result
}

func scanSemanticFacts(rows *sql.Rows) []SemanticFactRecord {
	result := make([]SemanticFactRecord, 0)
	for rows.Next() {
		var f SemanticFactRecord
		var expiresAt sql.NullTime
		if rows.Scan(
			&f.ID, &f.TenantID, &f.ScopeKind, &f.ChatID, &f.FactText,
			&f.FactType, &f.Subject, &f.Confidence, &f.Source,
			&f.SourceMessageID, &expiresAt, &f.CreatedAt,
		) == nil {
			if expiresAt.Valid {
				f.ExpiresAt = &expiresAt.Time
			}
			result = append(result, f)
		}
	}
	return result
}

// ============================================================
// Runtime State Layer (active_episodes table)
// ============================================================

// UpsertActiveEpisode creates or updates the active episode for a chat.
// On first message: inserts a new episode. On subsequent messages: updates last_message_at
// and message_count. The episode is closed (deleted) externally when silence > 6h or
// message_count > 40.
func (r *Repository) UpsertActiveEpisode(episode ActiveEpisodeRecord) {
	r.mu.Lock()
	r.activeEpisodes[chatKey(episode.TenantID, episode.ChatID)] = episode
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`INSERT INTO active_episodes (
		   tenant_id, chat_id, started_at, last_message_at, message_count,
		   current_topic, open_question_message_id, pending_owner_action, updated_at
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
		   last_message_at         = EXCLUDED.last_message_at,
		   message_count           = active_episodes.message_count + 1,
		   current_topic           = COALESCE(EXCLUDED.current_topic, active_episodes.current_topic),
		   open_question_message_id = COALESCE(EXCLUDED.open_question_message_id, active_episodes.open_question_message_id),
		   pending_owner_action    = COALESCE(EXCLUDED.pending_owner_action, active_episodes.pending_owner_action),
		   updated_at              = now()`,
		episode.TenantID, episode.ChatID,
		episode.StartedAt, episode.LastMessageAt, episode.MessageCount,
		nullableString(episode.CurrentTopic),
		nullableString(episode.OpenQuestionMsgID),
		nullableString(episode.PendingOwnerAction),
	)
	r.logWriteError("upsert_active_episode", episode.TenantID, episode.ChatID, err)
}

// UpdateEpisodeRuntime sets runtime-state fields on the active episode without
// touching message_count (MEM-5): UpsertActiveEpisode increments the counter on
// conflict, so using it for post-AI runtime writes would inflate the count.
// Empty arguments leave the corresponding field unchanged.
func (r *Repository) UpdateEpisodeRuntime(tenantID, chatID, topic, openQuestionMsgID, pendingOwnerAction string) {
	r.mu.Lock()
	key := chatKey(tenantID, chatID)
	if ep, ok := r.activeEpisodes[key]; ok {
		if topic != "" {
			ep.CurrentTopic = topic
		}
		if openQuestionMsgID != "" {
			ep.OpenQuestionMsgID = openQuestionMsgID
		}
		if pendingOwnerAction != "" {
			ep.PendingOwnerAction = pendingOwnerAction
		}
		r.activeEpisodes[key] = ep
	}
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`UPDATE active_episodes
		    SET current_topic            = COALESCE(NULLIF($3,''), current_topic),
		        open_question_message_id = COALESCE(NULLIF($4,''), open_question_message_id),
		        pending_owner_action     = COALESCE(NULLIF($5,''), pending_owner_action),
		        updated_at               = now()
		  WHERE tenant_id = $1 AND chat_id = $2`,
		tenantID, chatID, topic, openQuestionMsgID, pendingOwnerAction,
	)
	r.logWriteError("update_episode_runtime", tenantID, chatID, err)
}

// ClearEpisodeRuntimeState nulls open_question_message_id and
// pending_owner_action on the active episode (MEM-5). A separate method is
// required because UpsertActiveEpisode COALESCEs those columns and therefore
// cannot reset them.
func (r *Repository) ClearEpisodeRuntimeState(tenantID, chatID string) {
	r.mu.Lock()
	key := chatKey(tenantID, chatID)
	if ep, ok := r.activeEpisodes[key]; ok {
		ep.OpenQuestionMsgID = ""
		ep.PendingOwnerAction = ""
		r.activeEpisodes[key] = ep
	}
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`UPDATE active_episodes
		    SET open_question_message_id = NULL,
		        pending_owner_action = NULL,
		        updated_at = now()
		  WHERE tenant_id = $1 AND chat_id = $2`,
		tenantID, chatID,
	)
	r.logWriteError("clear_episode_runtime_state", tenantID, chatID, err)
}

// ActiveEpisodeByChat returns the current open episode for a chat, if one exists.
func (r *Repository) ActiveEpisodeByChat(tenantID, chatID string) (ActiveEpisodeRecord, bool) {
	if r.db != nil {
		var ep ActiveEpisodeRecord
		var currentTopic sql.NullString
		var openQuestion sql.NullString
		var pendingAction sql.NullString
		err := r.db.QueryRow(
			`SELECT tenant_id, chat_id, started_at, last_message_at, message_count,
			        current_topic, open_question_message_id, pending_owner_action
			   FROM active_episodes
			  WHERE tenant_id = $1 AND chat_id = $2`,
			tenantID, chatID,
		).Scan(
			&ep.TenantID, &ep.ChatID,
			&ep.StartedAt, &ep.LastMessageAt, &ep.MessageCount,
			&currentTopic, &openQuestion, &pendingAction,
		)
		if err == nil {
			ep.CurrentTopic = currentTopic.String
			ep.OpenQuestionMsgID = openQuestion.String
			ep.PendingOwnerAction = pendingAction.String
			return ep, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ep, ok := r.activeEpisodes[chatKey(tenantID, chatID)]
	return ep, ok
}

// CloseActiveEpisode deletes the active episode record, signalling that it has been
// materialised into episode_summaries.
func (r *Repository) CloseActiveEpisode(tenantID, chatID string) {
	r.mu.Lock()
	delete(r.activeEpisodes, chatKey(tenantID, chatID))
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`DELETE FROM active_episodes WHERE tenant_id = $1 AND chat_id = $2`,
		tenantID, chatID,
	)
	r.logWriteError("close_active_episode", tenantID, chatID, err)
}

// LogExtraction records one async fact-extraction attempt to the extraction_log table.
// Best-effort: a failure must never break the extraction path, but it is logged
// (MEM-10) so a broken diagnostics table is itself observable.
// No-op when running without a DB (unit tests use the in-memory repository).
func (r *Repository) LogExtraction(rec ExtractionLogRecord) {
	if r.db == nil {
		return
	}
	rawOutput := rec.RawOutput
	if len(rawOutput) > 4000 {
		rawOutput = rawOutput[:4000]
	}
	_, err := r.db.Exec(
		`INSERT INTO extraction_log (
		   tenant_id, chat_id, input_message_ids, model_used, raw_output,
		   facts_created, facts_skipped, facts_merged, error, duration_ms
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rec.TenantID, rec.ChatID,
		pq.Array(nilToEmpty(rec.InputMessageIDs)),
		rec.ModelUsed,
		// raw_output is JSONB; store the verbatim text as a JSON string value.
		rawOutputJSON(rawOutput),
		pq.Array(nilToEmptyInt64(rec.FactsCreated)),
		rec.FactsSkipped, rec.FactsMerged,
		nullableString(rec.Error),
		rec.DurationMS,
	)
	r.logWriteError("log_extraction", rec.TenantID, rec.ChatID, err)
}

// rawOutputJSON wraps arbitrary extractor text as a valid JSON value for the
// JSONB raw_output column. The LLM output is usually a JSON array already, but it
// can be an error string or malformed; encoding as a JSON string is always valid.
func rawOutputJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return "\"\""
	}
	return string(b)
}

// ============================================================
// Episodic Layer (episode_summaries table)
// ============================================================

// SaveEpisodeSummary stores a compressed summary of a closed conversation episode.
// Returns the inserted row ID (0 in memory-only mode), used by the embedding worker.
func (r *Repository) SaveEpisodeSummary(summary EpisodeSummaryRecord) int64 {
	r.mu.Lock()
	r.episodeSummaries = append(r.episodeSummaries, summary)
	r.mu.Unlock()
	if r.db == nil {
		return 0
	}
	var id int64
	err := r.db.QueryRow(
		`INSERT INTO episode_summaries (
		   tenant_id, chat_id, period_start, period_end, content,
		   message_count, closure_reason, source_message_ids
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id`,
		summary.TenantID, summary.ChatID,
		summary.PeriodStart, summary.PeriodEnd,
		summary.Content, summary.MessageCount,
		valueOrDefault(summary.ClosureReason, "manual"),
		pq.Array(nilToEmpty(summary.SourceMessageIDs)),
	).Scan(&id)
	// id=0 already signals failure to callers; the log makes the cause visible
	// (a CHECK-constraint violation here once silently dropped all summaries — MEM-10).
	r.logWriteError("save_episode_summary", summary.TenantID, summary.ChatID, err)
	return id
}

// SaveEpisodeSummaryEmbedding persists the embedding vector for an episode summary row.
// Called by the embedding worker after the Python service generates the vector.
func (r *Repository) SaveEpisodeSummaryEmbedding(id int64, tenantID string, embedding []float64, model string) {
	if r.db == nil || len(embedding) == 0 {
		return
	}
	vec := float64SliceToVector(embedding)
	_, err := r.db.Exec(
		`UPDATE episode_summaries
		    SET embedding = $1::vector, embedding_model = $2
		  WHERE id = $3 AND tenant_id = $4`,
		vec, model, id, tenantID,
	)
	// A silent failure here once made the worker re-embed the same rows forever
	// (missing column, fixed in migration 012) — keep it observable (MEM-10).
	r.logWriteError("save_episode_summary_embedding", tenantID, "", err)
}

// EpisodeSummariesWithoutEmbeddings returns up to limit episode summary rows that
// have no embedding vector yet. Used by the embedding worker for batch embedding.
func (r *Repository) EpisodeSummariesWithoutEmbeddings(limit int) []EpisodeSummaryRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT id, tenant_id, chat_id, period_start, period_end,
		        content, closure_reason, message_count, created_at
		   FROM episode_summaries
		  WHERE embedding IS NULL
		    AND deleted_at IS NULL
		  LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]EpisodeSummaryRecord, 0, limit)
	for rows.Next() {
		var s EpisodeSummaryRecord
		if rows.Scan(
			&s.ID, &s.TenantID, &s.ChatID,
			&s.PeriodStart, &s.PeriodEnd,
			&s.Content, &s.ClosureReason, &s.MessageCount, &s.CreatedAt,
		) == nil {
			result = append(result, s)
		}
	}
	return result
}

// EpisodeSummaries returns recent episode summaries for a chat, newest first.
func (r *Repository) EpisodeSummaries(tenantID, chatID string, limit int) []EpisodeSummaryRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT id, tenant_id, chat_id, period_start, period_end,
			        content, closure_reason, message_count, created_at
			   FROM episode_summaries
			  WHERE tenant_id = $1 AND chat_id = $2
			    AND deleted_at IS NULL
			  ORDER BY period_end DESC
			  LIMIT $3`,
			tenantID, chatID, limit,
		)
		if err == nil {
			defer rows.Close()
			result := make([]EpisodeSummaryRecord, 0)
			for rows.Next() {
				var s EpisodeSummaryRecord
				if rows.Scan(
					&s.ID, &s.TenantID, &s.ChatID,
					&s.PeriodStart, &s.PeriodEnd,
					&s.Content, &s.ClosureReason, &s.MessageCount, &s.CreatedAt,
				) == nil {
					result = append(result, s)
				}
			}
			return result
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]EpisodeSummaryRecord, 0)
	for i := len(r.episodeSummaries) - 1; i >= 0 && len(result) < limit; i-- {
		s := r.episodeSummaries[i]
		if s.TenantID == tenantID && s.ChatID == chatID {
			result = append(result, s)
		}
	}
	return result
}

// float64SliceToVector converts a Go float64 slice to a pgvector literal string.
// Format: "[0.1, 0.2, ...]" — distinct from the PostgreSQL array literal "{0.1, ...}".
// Used to pass query vectors to ORDER BY embedding <=> $n::vector clauses.
func float64SliceToVector(v []float64) string {
	if len(v) == 0 {
		return "[]"
	}
	b := make([]byte, 0, len(v)*12+2)
	b = append(b, '[')
	for i, f := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, f, 'f', -1, 64)
	}
	b = append(b, ']')
	return string(b)
}

// hardIncludeFactsByScope returns taboo and health facts for a given scope unconditionally.
// Design §8.2: taboo and health facts must always appear in context regardless of similarity.
func (r *Repository) hardIncludeChatFacts(tenantID, chatID string) []SemanticFactRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT id, tenant_id, scope_kind, COALESCE(chat_id,''), fact_text, fact_type,
		        subject, confidence, source, COALESCE(source_message_id,''), expires_at, created_at
		   FROM semantic_facts
		  WHERE tenant_id = $1 AND chat_id = $2
		    AND scope_kind = 'chat'
		    AND fact_type IN ('taboo', 'health')
		    AND deleted_at IS NULL AND superseded_by_id IS NULL
		  ORDER BY created_at DESC`,
		tenantID, chatID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanSemanticFacts(rows)
}

// SemanticFactsByVector returns chat-scoped facts ranked by cosine similarity to queryVector.
// Taboo and health facts are always included regardless of similarity (design §8.2).
// The remaining slots are filled by top-K similarity-ranked facts with commitment (÷1.5) and
// preference (÷1.2) effective-distance boosts. Falls back to SemanticFacts when queryVector
// is empty or the fact_embeddings join yields no rows (pgvector unavailable).
func (r *Repository) SemanticFactsByVector(tenantID, chatID string, queryVector []float64, limit int) []SemanticFactRecord {
	if r.db == nil || len(queryVector) == 0 {
		return r.SemanticFacts(tenantID, chatID, limit)
	}

	// Hard-include: taboo + health always in context (design §8.2).
	hardFacts := r.hardIncludeChatFacts(tenantID, chatID)

	// Top-K by cosine similarity, excluding hard-include types (they are already in).
	remaining := limit - len(hardFacts)
	if remaining < 1 {
		remaining = 1
	}
	vec := float64SliceToVector(queryVector)
	// Two-stage ranking (MEM-7): the inner ANN query orders purely by cosine
	// distance — the only shape the HNSW expression index can serve — over an
	// oversampled K×3 candidate set; the type-boost re-sort happens outside.
	// A boost-multiplied ORDER BY on the index expression would force a seq scan.
	rows, err := r.db.Query(fmt.Sprintf(
		`SELECT id, tenant_id, scope_kind, chat_id, fact_text, fact_type,
		        subject, confidence, source, source_message_id, expires_at, created_at
		   FROM (
		     SELECT sf.id, sf.tenant_id, sf.scope_kind, COALESCE(sf.chat_id,'') AS chat_id,
		            sf.fact_text, sf.fact_type, sf.subject, sf.confidence, sf.source,
		            COALESCE(sf.source_message_id,'') AS source_message_id, sf.expires_at, sf.created_at,
		            %s AS dist
		       FROM semantic_facts sf
		       JOIN fact_embeddings fe ON fe.fact_id = sf.id
		      WHERE sf.tenant_id = $1 AND sf.chat_id = $2
		        AND sf.scope_kind = 'chat'
		        AND sf.fact_type NOT IN ('taboo', 'health')
		        AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
		        AND fe.embedding IS NOT NULL
		      ORDER BY %s ASC
		      LIMIT $4 * 3
		   ) candidates
		  ORDER BY dist * CASE fact_type
		             WHEN 'commitment' THEN 0.667
		             WHEN 'preference' THEN 0.833
		             ELSE 1.0 END ASC
		  LIMIT $4`,
		r.vectorOrderExpr("fe.embedding", "$3"),
		r.vectorOrderExpr("fe.embedding", "$3"),
	),
		tenantID, chatID, vec, remaining,
	)
	if err != nil {
		// pgvector unavailable — fall back to recency-ordered query.
		return r.SemanticFacts(tenantID, chatID, limit)
	}
	defer rows.Close()
	simFacts := scanSemanticFacts(rows)
	return append(hardFacts, simFacts...)
}

// SemanticFactsByBM25 returns chat-scoped facts ranked by full-text (BM25) relevance to query.
// Uses the 'simple' text-search configuration so it handles Russian and English without
// a language-specific dictionary. Returns nil when the fact_tsv column is absent (pre-migration).
func (r *Repository) SemanticFactsByBM25(tenantID, chatID, query string, limit int) []SemanticFactRecord {
	if r.db == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT id, tenant_id, scope_kind, COALESCE(chat_id,''), fact_text, fact_type,
		        subject, confidence, source, COALESCE(source_message_id,''), expires_at, created_at
		   FROM semantic_facts
		  WHERE tenant_id = $1 AND chat_id = $2
		    AND scope_kind = 'chat'
		    AND fact_tsv @@ plainto_tsquery('simple', $3)
		    AND deleted_at IS NULL AND superseded_by_id IS NULL
		  ORDER BY ts_rank_cd(fact_tsv, plainto_tsquery('simple', $3)) DESC
		  LIMIT $4`,
		tenantID, chatID, query, limit,
	)
	if err != nil {
		return nil // column absent or query error — caller falls back to vector-only
	}
	defer rows.Close()
	return scanSemanticFacts(rows)
}

// SemanticFactsByIDs returns facts with the given IDs in the order the IDs are provided.
// Used by the reranking path to assemble the final fact list from reranker-ordered IDs.
func (r *Repository) SemanticFactsByIDs(tenantID string, ids []int64) []SemanticFactRecord {
	if r.db == nil || len(ids) == 0 {
		return nil
	}
	// Fetch all requested facts in one round-trip; preserve requested order below.
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, tenantID)
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	query := fmt.Sprintf(
		`SELECT id, tenant_id, scope_kind, COALESCE(chat_id,''), fact_text, fact_type,
		        subject, confidence, source, COALESCE(source_message_id,''), expires_at, created_at
		   FROM semantic_facts
		  WHERE tenant_id = $1 AND id IN (%s)
		    AND deleted_at IS NULL`,
		strings.Join(placeholders, ","),
	)
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	byID := make(map[int64]SemanticFactRecord)
	for _, f := range scanSemanticFacts(rows) {
		byID[f.ID] = f
	}
	// Return in the caller-specified order (reranker-sorted).
	result := make([]SemanticFactRecord, 0, len(ids))
	for _, id := range ids {
		if f, ok := byID[id]; ok {
			result = append(result, f)
		}
	}
	return result
}

// RankFactsByCosine reorders the given fact IDs by cosine distance to queryVector
// (closest first), truncated to limit. Facts without an embedding row are absent
// from the result — callers backfill them from their pre-ranked order. Replaces
// the listwise LLM reranker in the hot retrieval path (ARCH-5): the embeddings
// already exist in fact_embeddings, so the final ordering is one cheap SQL query.
func (r *Repository) RankFactsByCosine(tenantID string, ids []int64, queryVector []float64, limit int) []SemanticFactRecord {
	if r.db == nil || len(ids) == 0 || len(queryVector) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+3)
	args = append(args, tenantID, float64SliceToVector(queryVector))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+3)
		args = append(args, id)
	}
	args = append(args, limit)
	query := fmt.Sprintf(
		`SELECT sf.id, sf.tenant_id, sf.scope_kind, COALESCE(sf.chat_id,''), sf.fact_text, sf.fact_type,
		        sf.subject, sf.confidence, sf.source, COALESCE(sf.source_message_id,''), sf.expires_at, sf.created_at
		   FROM semantic_facts sf
		   JOIN fact_embeddings fe ON fe.fact_id = sf.id
		  WHERE sf.tenant_id = $1 AND sf.id IN (%s)
		    AND sf.deleted_at IS NULL
		  ORDER BY %s ASC
		  LIMIT $%d`,
		strings.Join(placeholders, ","), r.vectorOrderExpr("fe.embedding", "$2"), len(ids)+3,
	)
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanSemanticFacts(rows)
}

// SaveFactRelation persists a directed temporal edge between two semantic_facts rows.
// Idempotent: ON CONFLICT DO NOTHING so duplicate calls are safe.
func (r *Repository) SaveFactRelation(rel FactRelationRecord) {
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`INSERT INTO fact_relations (tenant_id, fact_id_from, fact_id_to, relation_type, confidence)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (fact_id_from, fact_id_to, relation_type) DO NOTHING`,
		rel.TenantID, rel.FactIDFrom, rel.FactIDTo, rel.RelationType, rel.Confidence,
	)
	r.logWriteError("save_fact_relation", rel.TenantID, "", err)
}

// ============================================================
// Contact profile suggestions (§4.5, MEM-4)
// ============================================================

// ProfileSuggestionRecord maps to contact_profile_suggestions: an AI-proposed
// profile field update awaiting owner review. The AI never writes the profile
// directly — accepted suggestions are applied by ResolveSuggestion (§15).
type ProfileSuggestionRecord struct {
	ID             int64
	TenantID       string
	ChatID         string
	FieldName      string
	SuggestedValue string
	Rationale      string
	Status         string // pending | accepted | rejected | expired
	CreatedAt      time.Time
}

// suggestionProfileFields whitelists the contact_profiles columns a suggestion
// may target; ResolveSuggestion refuses anything else (defence against a
// poisoned field_name reaching the SQL identifier position).
var suggestionProfileFields = map[string]bool{
	"relationship_type": true,
	"preferred_address": true,
	"language":          true,
	"tone":              true,
	"gender":            true,
}

// SaveProfileSuggestion stages a suggestion for owner review. Returns the new
// row ID, or 0 when an identical pending suggestion for the field exists
// (unique partial index from migration 016).
func (r *Repository) SaveProfileSuggestion(rec ProfileSuggestionRecord) int64 {
	if !suggestionProfileFields[rec.FieldName] {
		return 0
	}
	if r.db == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, existing := range r.profileSuggestions {
			if existing.TenantID == rec.TenantID && existing.ChatID == rec.ChatID &&
				existing.FieldName == rec.FieldName && existing.Status == "pending" {
				return 0
			}
		}
		r.aiRoleSeq++
		rec.ID = r.aiRoleSeq
		rec.Status = "pending"
		rec.CreatedAt = time.Now()
		r.profileSuggestions = append(r.profileSuggestions, rec)
		return rec.ID
	}
	var id int64
	err := r.db.QueryRow(
		`INSERT INTO contact_profile_suggestions (tenant_id, chat_id, field_name, suggested_value, rationale)
		 VALUES ($1, $2, $3, to_jsonb($4::text), NULLIF($5,''))
		 ON CONFLICT (tenant_id, chat_id, field_name) WHERE status = 'pending'
		 DO NOTHING
		 RETURNING id`,
		rec.TenantID, rec.ChatID, rec.FieldName, rec.SuggestedValue, rec.Rationale,
	).Scan(&id)
	if err != nil {
		if err != sql.ErrNoRows {
			r.logWriteError("save_profile_suggestion", rec.TenantID, rec.ChatID, err)
		}
		return 0
	}
	return id
}

// PendingSuggestions returns pending suggestions for a chat, or for the whole
// tenant when chatID is empty (the /suggestions command view).
func (r *Repository) PendingSuggestions(tenantID, chatID string) []ProfileSuggestionRecord {
	if r.db == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		result := make([]ProfileSuggestionRecord, 0)
		for _, s := range r.profileSuggestions {
			if s.TenantID == tenantID && s.Status == "pending" && (chatID == "" || s.ChatID == chatID) {
				result = append(result, s)
			}
		}
		return result
	}
	rows, err := r.db.Query(
		`SELECT id, tenant_id, chat_id, field_name, suggested_value #>> '{}', COALESCE(rationale,''), status, created_at
		   FROM contact_profile_suggestions
		  WHERE tenant_id = $1 AND status = 'pending' AND ($2 = '' OR chat_id = $2)
		  ORDER BY created_at ASC`,
		tenantID, chatID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]ProfileSuggestionRecord, 0)
	for rows.Next() {
		var s ProfileSuggestionRecord
		if rows.Scan(&s.ID, &s.TenantID, &s.ChatID, &s.FieldName, &s.SuggestedValue, &s.Rationale, &s.Status, &s.CreatedAt) == nil {
			result = append(result, s)
		}
	}
	return result
}

// ResolveSuggestion marks a pending suggestion accepted or rejected. Accepting
// applies the value to contact_profiles in the same transaction — the only
// path by which AI-derived data reaches the profile (§4.5/§15).
func (r *Repository) ResolveSuggestion(tenantID string, id int64, accept bool) error {
	status := "rejected"
	if accept {
		status = "accepted"
	}
	if r.db == nil {
		r.mu.Lock()
		var resolved *ProfileSuggestionRecord
		for i := range r.profileSuggestions {
			s := &r.profileSuggestions[i]
			if s.TenantID == tenantID && s.ID == id && s.Status == "pending" {
				s.Status = status
				resolved = s
				break
			}
		}
		r.mu.Unlock()
		if resolved == nil {
			return fmt.Errorf("pending suggestion %d not found", id)
		}
		if accept {
			profile, _ := r.ContactProfile(tenantID, resolved.ChatID)
			profile.TenantID, profile.ChatID = tenantID, resolved.ChatID
			switch resolved.FieldName {
			case "relationship_type":
				profile.RelationshipType = resolved.SuggestedValue
			case "preferred_address":
				profile.PreferredAddress = resolved.SuggestedValue
			case "language":
				profile.Language = resolved.SuggestedValue
			case "tone":
				profile.Tone = resolved.SuggestedValue
			case "gender":
				profile.Gender = resolved.SuggestedValue
			}
			r.SaveContactProfile(profile)
		}
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	var chatID, fieldName, value string
	if err := tx.QueryRow(
		`UPDATE contact_profile_suggestions
		    SET status = $3, reviewed_at = now()
		  WHERE tenant_id = $1 AND id = $2 AND status = 'pending'
		  RETURNING chat_id, field_name, suggested_value #>> '{}'`,
		tenantID, id, status,
	).Scan(&chatID, &fieldName, &value); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("pending suggestion %d not found: %w", id, err)
	}
	if accept {
		if !suggestionProfileFields[fieldName] {
			_ = tx.Rollback()
			return fmt.Errorf("suggestion field %q is not allowed", fieldName)
		}
		// Ensure the profile row exists, then set the single whitelisted column.
		if _, err := tx.Exec(
			`INSERT INTO contact_profiles (tenant_id, chat_id)
			 VALUES ($1, $2) ON CONFLICT (tenant_id, chat_id) DO NOTHING`,
			tenantID, chatID,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
		// fieldName is whitelist-validated above — safe in identifier position.
		if _, err := tx.Exec(
			fmt.Sprintf(`UPDATE contact_profiles SET %s = $3, updated_at = now()
			  WHERE tenant_id = $1 AND chat_id = $2`, fieldName),
			tenantID, chatID, value,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if accept {
		r.invalidateContextScore(tenantID, chatID)
		// Refresh the in-memory profile mirror from the DB on next read.
		r.mu.Lock()
		delete(r.contactProfiles, chatKey(tenantID, chatID))
		r.mu.Unlock()
	}
	return nil
}

// ExpireOldSuggestions marks pending suggestions older than maxAge as expired
// (nightly hygiene, §4.5). Returns the number of expired rows.
func (r *Repository) ExpireOldSuggestions(tenantID string, maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	if r.db == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		expired := 0
		for i := range r.profileSuggestions {
			s := &r.profileSuggestions[i]
			if s.TenantID == tenantID && s.Status == "pending" && s.CreatedAt.Before(cutoff) {
				s.Status = "expired"
				expired++
			}
		}
		return expired
	}
	res, err := r.db.Exec(
		`UPDATE contact_profile_suggestions
		    SET status = 'expired', reviewed_at = now()
		  WHERE tenant_id = $1 AND status = 'pending' AND created_at < $2`,
		tenantID, cutoff,
	)
	if err != nil {
		r.logWriteError("expire_suggestions", tenantID, "", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// ============================================================
// Deletion contract (§10.2, MEM-2): hard delete, never soft.
// ============================================================

// insertForgetAuditTx writes the GDPR audit row inside the deletion transaction.
// audit_events carries a NOT NULL FK to messages with ON DELETE CASCADE, so an
// audit row referencing a deleted message would vanish with it; instead a
// synthetic marker message is inserted first and the audit row points at it.
// This is why the audit insert happens after the deletes — atomicity within
// the transaction still guarantees "no deletion without an audit record".
func insertForgetAuditTx(tx *sql.Tx, tenantID, chatID, auditType string) error {
	markerID := fmt.Sprintf("%s:%d", auditType, time.Now().UnixNano())
	if _, err := tx.Exec(
		`INSERT INTO messages (
		   tenant_id, chat_id, source_message_id, event_id, telegram_update_id,
		   business_connection_id, sender_id, message_text, message_type
		 ) VALUES ($1, $2, $3, $3, 0, '', 'system', '', 'system')`,
		tenantID, chatID, markerID,
	); err != nil {
		return err
	}
	_, err := tx.Exec(
		`INSERT INTO audit_events (tenant_id, chat_id, source_message_id, decision, reason)
		 VALUES ($1, $2, $3, 'ignore'::decision, $4)`,
		tenantID, chatID, markerID, auditType,
	)
	return err
}

// forgetChatTx hard-deletes every memory artifact of one chat inside tx.
// Health facts go first as their own statement (§10.3); FK cascades from
// semantic_facts cover fact_embeddings / fact_occurrences / fact_relations,
// and the messages delete cascades message_states + audit_events. The chats
// row and chat_settings survive — the owner forgets the memory, not the chat.
func forgetChatTx(tx *sql.Tx, tenantID, chatID string) error {
	statements := []string{
		// §10.3: health facts deleted by a dedicated first statement.
		`DELETE FROM semantic_facts WHERE tenant_id = $1 AND chat_id = $2 AND fact_type = 'health'`,
		// Cascades: fact_embeddings, fact_occurrences, fact_relations (FK ON DELETE CASCADE).
		`DELETE FROM semantic_facts WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM episode_summaries WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM active_episodes WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM extraction_log WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM contact_profile_suggestions WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM contact_profiles WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM draft_feedback WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM drafts WHERE tenant_id = $1 AND chat_id = $2`,
		// Cascades: message_states, audit_events (FK ON DELETE CASCADE).
		`DELETE FROM messages WHERE tenant_id = $1 AND chat_id = $2`,
	}
	for _, stmt := range statements {
		if _, err := tx.Exec(stmt, tenantID, chatID); err != nil {
			return err
		}
	}
	return insertForgetAuditTx(tx, tenantID, chatID, "memory_forget_chat")
}

// forgetChatMemoryLocked scrubs the in-memory mirrors for one chat. Caller holds r.mu.
func (r *Repository) forgetChatMemoryLocked(tenantID, chatID string) {
	key := chatKey(tenantID, chatID)
	for _, msgKey := range r.chatMessageKeys[key] {
		delete(r.messages, msgKey)
	}
	delete(r.chatMessageKeys, key)
	delete(r.contactProfiles, key)
	delete(r.activeEpisodes, key)
	delete(r.pendingInputs, key)
	delete(r.messageCounts, key)
	for id, d := range r.drafts {
		if d.TenantID == tenantID && d.ChatID == chatID {
			delete(r.drafts, id)
		}
	}
	keepFeedback := r.draftFeedbacks[:0]
	for _, fb := range r.draftFeedbacks {
		if !(fb.TenantID == tenantID && fb.ChatID == chatID) {
			keepFeedback = append(keepFeedback, fb)
		}
	}
	r.draftFeedbacks = keepFeedback
	keepFacts := r.semanticFacts[:0]
	for _, f := range r.semanticFacts {
		if !(f.TenantID == tenantID && f.ChatID == chatID) {
			keepFacts = append(keepFacts, f)
		}
	}
	r.semanticFacts = keepFacts
	keepSummaries := r.episodeSummaries[:0]
	for _, s := range r.episodeSummaries {
		if !(s.TenantID == tenantID && s.ChatID == chatID) {
			keepSummaries = append(keepSummaries, s)
		}
	}
	r.episodeSummaries = keepSummaries
	keepSuggestions := r.profileSuggestions[:0]
	for _, s := range r.profileSuggestions {
		if !(s.TenantID == tenantID && s.ChatID == chatID) {
			keepSuggestions = append(keepSuggestions, s)
		}
	}
	r.profileSuggestions = keepSuggestions
	r.invalidateContextScore(tenantID, chatID)
}

// ForgetChat hard-deletes all memory of one chat (§10.2): facts, embeddings,
// occurrences, relations, episode summaries, active episode, extraction log,
// profile + suggestions, drafts, feedback, messages and their states/audits.
// One transaction; an audit marker row records the operation. The chat row
// itself and its settings remain so the chat keeps working with a clean slate.
func (r *Repository) ForgetChat(tenantID, chatID string) error {
	if tenantID == "" || chatID == "" {
		return fmt.Errorf("tenant_id and chat_id are required")
	}
	r.mu.Lock()
	r.forgetChatMemoryLocked(tenantID, chatID)
	r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if err := forgetChatTx(tx, tenantID, chatID); err != nil {
		_ = tx.Rollback()
		r.logWriteError("forget_chat", tenantID, chatID, err)
		return err
	}
	return tx.Commit()
}

// UnlearnFact hard-deletes a single semantic fact (§10.2). FK cascades remove
// its embeddings, occurrences, and relations; the audit marker records the act.
func (r *Repository) UnlearnFact(tenantID string, factID int64) error {
	if tenantID == "" || factID == 0 {
		return fmt.Errorf("tenant_id and fact_id are required")
	}
	r.mu.Lock()
	chatID := ""
	keep := r.semanticFacts[:0]
	for _, f := range r.semanticFacts {
		if f.TenantID == tenantID && f.ID == factID {
			chatID = f.ChatID
			continue
		}
		keep = append(keep, f)
	}
	r.semanticFacts = keep
	r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if chatID == "" {
		// The in-memory mirror may not know the fact; resolve its chat for the audit row.
		_ = tx.QueryRow(
			`SELECT COALESCE(chat_id,'') FROM semantic_facts WHERE tenant_id = $1 AND id = $2`,
			tenantID, factID,
		).Scan(&chatID)
	}
	if _, err := tx.Exec(
		`DELETE FROM semantic_facts WHERE tenant_id = $1 AND id = $2`,
		tenantID, factID,
	); err != nil {
		_ = tx.Rollback()
		r.logWriteError("unlearn_fact", tenantID, chatID, err)
		return err
	}
	if chatID != "" {
		if err := insertForgetAuditTx(tx, tenantID, chatID, "memory_unlearn_fact"); err != nil {
			_ = tx.Rollback()
			r.logWriteError("unlearn_fact_audit", tenantID, chatID, err)
			return err
		}
	}
	if chatID != "" {
		r.invalidateContextScore(tenantID, chatID)
	}
	return tx.Commit()
}

// ForgetAll hard-deletes every chat-derived artifact of a tenant (§10.2):
// each chat's memory (per ForgetChat), then the chats themselves (cascading
// chat_settings), business connections, unclaimed chats, idempotency records,
// user settings, and AI roles. The tenants/users rows survive together with
// one synthetic audit marker recording the erasure.
func (r *Repository) ForgetAll(tenantID string) error {
	if tenantID == "" {
		return fmt.Errorf("tenant_id is required")
	}
	r.mu.Lock()
	for key, chat := range r.chats {
		if chat.TenantID == tenantID {
			r.forgetChatMemoryLocked(tenantID, chat.ChatID)
			delete(r.chats, key)
		}
	}
	for key, settings := range r.chatSettings {
		if settings.TenantID == tenantID {
			delete(r.chatSettings, key)
		}
	}
	for key, uc := range r.unclaimedChats {
		if uc.OwnerTenantID == tenantID {
			delete(r.unclaimedChats, key)
		}
	}
	for id, conn := range r.businessConnections {
		if conn.TenantID == tenantID {
			delete(r.businessConnections, id)
			delete(r.connectionByUser, conn.UserID)
			delete(r.connectionTenants, id)
		}
	}
	delete(r.userSettings, tenantID)
	for id, role := range r.aiRoles {
		if role.TenantID == tenantID {
			delete(r.aiRoles, id)
		}
	}
	r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	statements := []string{
		// §10.3: tenant-wide health facts first.
		`DELETE FROM semantic_facts WHERE tenant_id = $1 AND fact_type = 'health'`,
		`DELETE FROM semantic_facts WHERE tenant_id = $1`,
		`DELETE FROM extraction_log WHERE tenant_id = $1`,
		// Cascades from chats: chat_settings, messages (→ states, audits), drafts,
		// draft_feedback, contact_profiles, suggestions, episodes, summaries.
		`DELETE FROM chats WHERE tenant_id = $1`,
		`DELETE FROM unclaimed_chats WHERE owner_tenant_id = $1`,
		`DELETE FROM telegram_business_connections WHERE tenant_id = $1`,
		`DELETE FROM idempotency_records WHERE tenant_id = $1`,
		`DELETE FROM user_settings WHERE tenant_id = $1`,
		`DELETE FROM ai_roles WHERE tenant_id = $1`,
	}
	for _, stmt := range statements {
		if _, err := tx.Exec(stmt, tenantID); err != nil {
			_ = tx.Rollback()
			r.logWriteError("forget_all", tenantID, "", err)
			return err
		}
	}
	// Synthetic chat row so the audit marker has a valid FK chain.
	if _, err := tx.Exec(
		`INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, display_name, chat_type, status)
		 VALUES ($1, 'forget_all', 'forget_all', 'GDPR erasure marker', 'system', 'archived')
		 ON CONFLICT DO NOTHING`,
		tenantID,
	); err != nil {
		_ = tx.Rollback()
		r.logWriteError("forget_all_marker", tenantID, "", err)
		return err
	}
	if err := insertForgetAuditTx(tx, tenantID, "forget_all", "memory_forget_all"); err != nil {
		_ = tx.Rollback()
		r.logWriteError("forget_all_audit", tenantID, "", err)
		return err
	}
	return tx.Commit()
}

// FactRelationsByFactID returns all temporal edges where fact_id_from = factID.
func (r *Repository) FactRelationsByFactID(tenantID string, factID int64) []FactRelationRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT id, tenant_id, fact_id_from, fact_id_to, relation_type, confidence
		   FROM fact_relations
		  WHERE tenant_id = $1 AND fact_id_from = $2`,
		tenantID, factID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []FactRelationRecord
	for rows.Next() {
		var rel FactRelationRecord
		if rows.Scan(&rel.ID, &rel.TenantID, &rel.FactIDFrom, &rel.FactIDTo, &rel.RelationType, &rel.Confidence) == nil {
			result = append(result, rel)
		}
	}
	return result
}

// EpisodeSummariesByVector returns episode summaries ranked by cosine similarity to
// queryVector. Falls back to EpisodeSummaries (recency) when queryVector is empty or
// the embedding column has no data (pgvector unavailable or summaries not yet embedded).
func (r *Repository) EpisodeSummariesByVector(tenantID, chatID string, queryVector []float64, limit int) []EpisodeSummaryRecord {
	if r.db == nil || len(queryVector) == 0 {
		return r.EpisodeSummaries(tenantID, chatID, limit)
	}
	vec := float64SliceToVector(queryVector)
	rows, err := r.db.Query(fmt.Sprintf(
		`SELECT id, tenant_id, chat_id, period_start, period_end,
		        content, closure_reason, message_count, created_at
		   FROM episode_summaries
		  WHERE tenant_id = $1 AND chat_id = $2
		    AND embedding IS NOT NULL
		    AND deleted_at IS NULL
		  ORDER BY %s ASC
		  LIMIT $4`,
		r.vectorOrderExpr("embedding", "$3"),
	),
		tenantID, chatID, vec, limit,
	)
	if err != nil {
		return r.EpisodeSummaries(tenantID, chatID, limit)
	}
	defer rows.Close()
	result := make([]EpisodeSummaryRecord, 0, limit)
	for rows.Next() {
		var s EpisodeSummaryRecord
		if rows.Scan(
			&s.ID, &s.TenantID, &s.ChatID,
			&s.PeriodStart, &s.PeriodEnd,
			&s.Content, &s.ClosureReason, &s.MessageCount, &s.CreatedAt,
		) == nil {
			result = append(result, s)
		}
	}
	// If no embedded summaries found, fall back to recency.
	if len(result) == 0 {
		return r.EpisodeSummaries(tenantID, chatID, limit)
	}
	return result
}

// EpisodeSummaryOverlaps reports whether any summary already covers part of
// the [start, end) period. This is the episode-closure idempotency check
// (MEM-6): the previous one-summary-per-day rule silently dropped the second
// episode of a day.
func (r *Repository) EpisodeSummaryOverlaps(tenantID, chatID string, start, end time.Time) bool {
	if r.db != nil {
		var exists bool
		err := r.db.QueryRow(
			`SELECT EXISTS(
			   SELECT 1 FROM episode_summaries
			    WHERE tenant_id = $1 AND chat_id = $2
			      AND period_end > $3 AND period_start < $4
			      AND deleted_at IS NULL
			 )`,
			tenantID, chatID, start, end,
		).Scan(&exists)
		if err == nil {
			return exists
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.episodeSummaries {
		if s.TenantID == tenantID && s.ChatID == chatID &&
			s.PeriodEnd.After(start) && s.PeriodStart.Before(end) {
			return true
		}
	}
	return false
}

// LatestCoveredEnd returns the latest period_end among summaries overlapping
// [start, end), or the zero time when none overlap. The daily fallback uses it
// to summarise only the part of the day no episode summary covered (MEM-6).
func (r *Repository) LatestCoveredEnd(tenantID, chatID string, start, end time.Time) time.Time {
	if r.db != nil {
		var covered sql.NullTime
		err := r.db.QueryRow(
			`SELECT MAX(period_end) FROM episode_summaries
			  WHERE tenant_id = $1 AND chat_id = $2
			    AND period_end > $3 AND period_start < $4
			    AND deleted_at IS NULL`,
			tenantID, chatID, start, end,
		).Scan(&covered)
		if err == nil {
			if covered.Valid {
				return covered.Time
			}
			return time.Time{}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest time.Time
	for _, s := range r.episodeSummaries {
		if s.TenantID == tenantID && s.ChatID == chatID &&
			s.PeriodEnd.After(start) && s.PeriodStart.Before(end) &&
			s.PeriodEnd.After(latest) {
			latest = s.PeriodEnd
		}
	}
	return latest
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

// FactsWithoutEmbeddings returns up to limit semantic fact IDs + texts that have no embedding yet.
// Used by the embedding worker to find facts to batch-embed.
func (r *Repository) FactsWithoutEmbeddings(limit int) []SemanticFactRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT sf.id, sf.tenant_id, sf.scope_kind, COALESCE(sf.chat_id,''), sf.fact_text,
		        sf.fact_type, sf.subject, sf.confidence, sf.source,
		        COALESCE(sf.source_message_id,''), sf.created_at
		   FROM semantic_facts sf
		  WHERE NOT EXISTS (SELECT 1 FROM fact_embeddings fe WHERE fe.fact_id = sf.id)
		    AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
		  ORDER BY sf.created_at ASC
		  LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]SemanticFactRecord, 0)
	for rows.Next() {
		var f SemanticFactRecord
		if rows.Scan(
			&f.ID, &f.TenantID, &f.ScopeKind, &f.ChatID, &f.FactText,
			&f.FactType, &f.Subject, &f.Confidence, &f.Source,
			&f.SourceMessageID, &f.CreatedAt,
		) == nil {
			result = append(result, f)
		}
	}
	return result
}

// SaveFactEmbedding persists an embedding vector for a semantic fact.
func (r *Repository) SaveFactEmbedding(factID int64, tenantID, scopeKind, chatID string, embedding []float64, model string) {
	if r.db == nil {
		return
	}
	// Encode as PostgreSQL array literal for compatibility with both FLOAT8[] and vector columns.
	_, err := r.db.Exec(
		`INSERT INTO fact_embeddings (fact_id, tenant_id, scope_kind, chat_id, embedding_model, embedding)
		 VALUES ($1, $2, $3, $4, $5, $6::float8[])
		 ON CONFLICT (fact_id) DO UPDATE SET
		   embedding       = EXCLUDED.embedding,
		   embedding_model = EXCLUDED.embedding_model`,
		factID, tenantID, scopeKind, nullableString(chatID), model, float64SliceToArray(embedding),
	)
	r.logWriteError("save_fact_embedding", tenantID, chatID, err)
}

// float64SliceToArray converts a Go float64 slice to a PostgreSQL array literal string.
func float64SliceToArray(v []float64) string {
	if len(v) == 0 {
		return "{}"
	}
	b := make([]byte, 0, len(v)*12+2)
	b = append(b, '{')
	for i, f := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, f, 'f', -1, 64)
	}
	b = append(b, '}')
	return string(b)
}

// GreetedToday returns true when the owner has sent a greeting in this chat today.
// When a DB is configured it calls the view_chat_greeted_today SQL function which
// uses the messages table as the authoritative source — consistent with query results.
// The in-memory fallback scans today's messages for greeting words.
func (r *Repository) GreetedToday(tenantID, chatID, ownerID string) bool {
	if r.db != nil {
		var result bool
		err := r.db.QueryRow(
			`SELECT view_chat_greeted_today($1, $2, $3)`,
			tenantID, chatID, ownerID,
		).Scan(&result)
		if err == nil {
			return result
		}
	}
	// In-memory fallback: scan today's messages for owner greeting words.
	if ownerID == "" {
		return false
	}
	todayStart := time.Now().UTC().Truncate(24 * time.Hour)
	greetings := []string{"привет", "здравствуй", "добрый", "hi ", "hello", "hey ", "хай"}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.messages {
		if rec.Event.TenantID != tenantID || rec.Event.ChatID != chatID {
			continue
		}
		if rec.Event.SenderID != ownerID {
			continue
		}
		if rec.Event.CreatedAt.UTC().Before(todayStart) {
			continue
		}
		lower := strings.ToLower(rec.Event.MessageText)
		for _, g := range greetings {
			if strings.Contains(lower, g) {
				return true
			}
		}
	}
	return false
}

// OpenCommitments returns active commitment facts for a chat (non-expired, non-deleted).
// Uses the view_open_commitments SQL function when a DB is configured.
func (r *Repository) OpenCommitments(tenantID, chatID string) []SemanticFactRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT fact_id, fact_text, expires_at FROM view_open_commitments($1, $2)`,
			tenantID, chatID,
		)
		if err == nil {
			defer rows.Close()
			result := make([]SemanticFactRecord, 0)
			for rows.Next() {
				var f SemanticFactRecord
				var expiresAt *time.Time
				if rows.Scan(&f.ID, &f.FactText, &expiresAt) == nil {
					f.TenantID = tenantID
					f.ChatID = chatID
					f.FactType = "commitment"
					f.ExpiresAt = expiresAt
					result = append(result, f)
				}
			}
			return result
		}
	}
	// In-memory fallback: filter semantic facts for non-expired commitments.
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	result := make([]SemanticFactRecord, 0)
	for _, f := range r.semanticFacts {
		if f.TenantID != tenantID || f.ChatID != chatID || f.FactType != "commitment" {
			continue
		}
		if f.ExpiresAt != nil && f.ExpiresAt.Before(now) {
			continue
		}
		result = append(result, f)
	}
	return result
}

// ConsolidateFacts calls the consolidate_facts stored procedure for a tenant.
// Returns the number of facts cleaned up (expired + soft-deleted). Safe to call
// repeatedly; all operations are idempotent.
func (r *Repository) ConsolidateFacts(tenantID string) int {
	if r.db == nil {
		return 0
	}
	var cleaned int
	_ = r.db.QueryRow(`SELECT consolidate_facts($1)`, tenantID).Scan(&cleaned)
	return cleaned
}

func chatKey(tenantID, chatID string) string {
	return tenantID + ":" + chatID
}
