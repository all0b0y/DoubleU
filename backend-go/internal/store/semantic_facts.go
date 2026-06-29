package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"doubleu/backend-go/internal/contracts"
)

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
