package core

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

// contextTokenBudget caps the assembled memory context size (design §8.1 / MEM-9).
// Tokens are estimated as len(text)/3 — coarse but dependency-free for ru/en mix.
const contextTokenBudget = 4000

// estimateTokens returns a rough token count for budget accounting (§8.1).
func estimateTokens(lines []string) int {
	total := 0
	for _, l := range lines {
		total += len(l) / 3
	}
	return total
}

// conversationContext assembles the full memory context slice for the AI.
// See conversationContextAndMessages; this wrapper discards the raw messages.
func (s *Service) conversationContext(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string, limit int) []string {
	parts, _ := s.conversationContextAndMessages(ctx, event, ownerID, limit)
	return parts
}

// conversationContextAndMessages assembles the memory context for the AI and
// also returns the raw recent messages so callers (extraction window) reuse the
// same fetch instead of issuing a second query (ARCH-8).
//
// Independent reads run concurrently: the embedding call (~2-5 s) overlaps the
// DB reads instead of preceding them, and the vector-dependent retrieval runs
// once the embedding is ready. The result is budget-trimmed per §8.1: messages
// → episodic summaries → chat facts → pending drafts, in that order; runtime
// lines, commitments, and hard-include facts (taboo/health) are never trimmed.
func (s *Service) conversationContextAndMessages(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string, limit int) ([]string, []store.MessageRecord) {
	now := s.now().UTC()
	roleContextID, roleScoped := s.memoryRoleContextForEvent(event)

	// Phase 1 — independent reads in parallel (ARCH-8). The embedding API call
	// dominates (~2-5 s); everything else is a DB roundtrip that overlaps it.
	var (
		queryVector  []float64
		records      []store.MessageRecord
		greeted      bool
		pausedHours  float64
		commitments  []store.SemanticFactRecord
		episode      store.ActiveEpisodeRecord
		hasEpisode   bool
		drafts       []store.DraftRecord
		bm25         []store.SemanticFactRecord
		userSettings contracts.UserSettings
	)
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	run(func() {
		// Embed the incoming message for similarity-based retrieval (design §8).
		// Bounded so a slow provider never blocks the reply path; 5 s covers
		// typical OpenRouter embedding latency (~2–3 s) with margin.
		if embedder, ok := s.ai.(aiclient.TextEmbedder); ok && event.MessageText != "" {
			embedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			resp, err := embedder.EmbedTexts(embedCtx, aiclient.EmbedTextsRequest{Texts: []string{event.MessageText}})
			if err == nil && len(resp.Embeddings) > 0 {
				queryVector = resp.Embeddings[0]
			}
		}
	})
	run(func() {
		if roleScoped {
			records = s.repository.RecentMessagesForRoleContext(event.TenantID, event.ChatID, limit, roleContextID)
			return
		}
		records = s.repository.RecentMessages(event.TenantID, event.ChatID, limit)
	})
	run(func() { greeted = s.repository.GreetedToday(event.TenantID, event.ChatID, ownerID) })
	run(func() { pausedHours = s.repository.ChatPausedHours(event.TenantID, event.ChatID) })
	run(func() {
		if roleScoped {
			commitments = s.repository.OpenCommitmentsForRoleContext(event.TenantID, event.ChatID, roleContextID)
			return
		}
		commitments = s.repository.OpenCommitments(event.TenantID, event.ChatID)
	})
	run(func() {
		if roleScoped {
			episode, hasEpisode = s.repository.ActiveEpisodeByChatForRoleContext(event.TenantID, event.ChatID, roleContextID)
			return
		}
		episode, hasEpisode = s.repository.ActiveEpisodeByChat(event.TenantID, event.ChatID)
	})
	run(func() {
		if roleScoped {
			drafts = s.repository.DraftsForChatForRoleContext(event.TenantID, event.ChatID, roleContextID)
			return
		}
		drafts = s.repository.DraftsForChat(event.TenantID, event.ChatID)
	})
	run(func() {
		if event.MessageText != "" {
			if roleScoped {
				bm25 = s.repository.SemanticFactsByBM25ForRoleContext(event.TenantID, event.ChatID, event.MessageText, 20, roleContextID)
				return
			}
			bm25 = s.repository.SemanticFactsByBM25(event.TenantID, event.ChatID, event.MessageText, 20)
		}
	})
	run(func() { userSettings, _ = s.repository.UserSettings(event.TenantID) })
	wg.Wait()

	// B: single timezone source for this whole context block (header + transcript).
	// Empty/invalid IANA name means the owner's zone is UNKNOWN (A1 invariant) —
	// never silently treated as UTC; rendered with an explicit UTC marker instead.
	ownerLoc, ownerTZKnown := ownerTimeLocation(userSettings.Timezone)

	// Phase 2 — vector-dependent retrieval.
	// Memory is strictly per-chat: only facts observed in THIS chat are recalled.
	var chatVector []store.SemanticFactRecord
	if roleScoped {
		chatVector = s.repository.SemanticFactsByVectorForRoleContext(event.TenantID, event.ChatID, queryVector, 20, roleContextID)
	} else {
		chatVector = s.repository.SemanticFactsByVector(event.TenantID, event.ChatID, queryVector, 20)
	}
	chatFacts := chatVector
	if len(bm25) > 0 {
		// RRF fusion: merge vector + BM25 ranked lists into a single ordered set,
		// then resolve fused IDs back to records (preserves RRF order).
		fusedChatIDs := rrfFuse(factIDs(chatVector), factIDs(bm25), 60)
		if roleScoped {
			chatFacts = s.repository.SemanticFactsByIDsForRoleContext(event.TenantID, fusedChatIDs, roleContextID)
		} else {
			chatFacts = s.repository.SemanticFactsByIDs(event.TenantID, fusedChatIDs)
		}
	}
	// Final ordering (ARCH-5): deterministic cosine reranking in SQL replaces the
	// listwise LLM reranker; hard-include facts (taboo/health, §8.2) always survive.
	if roleScoped {
		chatFacts = s.finalizeFactOrderForRoleContext(event.TenantID, chatFacts, queryVector, 8, roleContextID)
	} else {
		chatFacts = s.finalizeFactOrder(event.TenantID, chatFacts, queryVector, 8)
	}
	var episodeSummaries []store.EpisodeSummaryRecord
	if roleScoped {
		episodeSummaries = s.repository.EpisodeSummariesByVectorForRoleContext(event.TenantID, event.ChatID, queryVector, 3, roleContextID)
	} else {
		episodeSummaries = s.repository.EpisodeSummariesByVector(event.TenantID, event.ChatID, queryVector, 3)
	}

	// --- Section assembly (then budget trimming per §8.1) ---

	// Header: dates, runtime flags, commitments — never trimmed.
	header := make([]string, 0, len(commitments)+6)
	if ownerTZKnown {
		header = append(header, fmt.Sprintf(
			"[CURRENT DATE: %s (%s)]", now.In(ownerLoc).Format("2006-01-02T15:04:05Z07:00"), userSettings.Timezone,
		))
	} else {
		header = append(header, fmt.Sprintf(
			"[CURRENT DATE: %s (UTC, пояс владельца неизвестен)]", now.Format("2006-01-02T15:04:05Z07:00"),
		))
	}
	// Authoritative greeting state from the DB, always emitted (true AND false)
	// so the Python prompt builder can trust this flag and skip re-deriving it.
	header = append(header, fmt.Sprintf("[RUNTIME: greeting_sent_today=%t]", greeted))
	// Pause length from view_chat_paused_hours — the authoritative gap signal
	// for the Python greeting rule (MEM-9).
	header = append(header, fmt.Sprintf("[RUNTIME: paused_hours=%.1f]", pausedHours))
	for _, c := range commitments {
		header = append(header, fmt.Sprintf("[ОБЯЗАТЕЛЬСТВО: %s]", c.FactText))
	}
	if hasEpisode {
		if episode.CurrentTopic != "" {
			header = append(header, fmt.Sprintf("[RUNTIME: тема разговора] %s", episode.CurrentTopic))
		}
		if episode.PendingOwnerAction != "" {
			header = append(header, fmt.Sprintf("[RUNTIME: ожидается действие владельца] %s", episode.PendingOwnerAction))
		}
		// Unanswered contact question (§6.2, MEM-5): surface its text so the
		// draft can address it instead of only the newest message.
		if episode.OpenQuestionMsgID != "" && episode.OpenQuestionMsgID != event.SourceMessageID {
			if msg, ok := s.repository.Message(event.TenantID, event.ChatID, episode.OpenQuestionMsgID); ok {
				if !roleScoped || msg.RoleContextID == roleContextID {
					if text := strings.TrimSpace(msg.Event.MessageText); text != "" {
						header = append(header, fmt.Sprintf("[RUNTIME: незакрытый вопрос] %s", text))
					}
				}
			}
		}
	}

	// Facts: hard-include (taboo/health) lines are exempt from trimming.
	hardFactLines := make([]string, 0, 2)
	softFactLines := make([]string, 0, len(chatFacts))
	for _, fact := range chatFacts {
		// Owner-subject facts stay chat-scoped but keep a distinct marker so the
		// prompt can separate "кто пишет" background from contact facts.
		line := fmt.Sprintf("[ФАКТ: %s] %s", fact.FactType, fact.FactText)
		if fact.Subject == "owner" {
			line = fmt.Sprintf("[ФАКТ О ВЛАДЕЛЬЦЕ: %s] %s", fact.FactType, fact.FactText)
		}
		if fact.FactType == "taboo" || fact.FactType == "health" {
			hardFactLines = append(hardFactLines, line)
		} else {
			softFactLines = append(softFactLines, line)
		}
	}

	// Episodic layer: compressed summaries of past closed episodes, oldest first (§7.5).
	summaryLines := make([]string, 0, len(episodeSummaries))
	for i := len(episodeSummaries) - 1; i >= 0; i-- {
		sm := episodeSummaries[i]
		summaryLines = append(summaryLines, fmt.Sprintf("[СВОДКА %s] %s",
			sm.PeriodEnd.UTC().Format("2006-01-02"), sm.Content))
	}

	// Working layer: recent raw messages from this chat (oldest first).
	// Transport is chat-fixed; resolve it once so OWNER/CONTACT labelling matches
	// the bot-vs-business identity rule (see isOwnerSideMessage). On bot transport
	// the human's messages — even the owner's — are CONTACT, and only the bot's
	// own replies ("out:" events) are OWNER.
	transport := s.repository.ChatTransport(event.TenantID, event.ChatID)
	messageLines := make([]string, 0, len(records))
	for _, record := range records {
		if record.Event.SourceMessageID == event.SourceMessageID {
			continue
		}
		text := strings.TrimSpace(record.Event.MessageText)
		if text == "" {
			continue
		}
		role := "CONTACT"
		if isOwnerSideMessage(record.Event, ownerID, transport) {
			role = "OWNER"
		}
		var msgDate string
		if ownerTZKnown {
			msgDate = record.Event.CreatedAt.In(ownerLoc).Format("2006-01-02 15:04 MST")
		} else {
			msgDate = record.Event.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		messageLines = append(messageLines, fmt.Sprintf("[%s] %s: %s", msgDate, role, text))
	}

	// Pending drafts: unanswered obligations awaiting owner approval.
	draftLines := make([]string, 0, 4)
	for _, d := range drafts {
		if d.Status != store.DraftPending {
			continue
		}
		draftLines = append(draftLines, fmt.Sprintf(
			"[ЧЕРНОВИК НЕ ОТПРАВЛЕН] В ответ на: %q\nПредложено (не отправлено): %q",
			d.SourceMessageText, d.ResponseText,
		))
	}

	// Budget trimming, strictly in §8.1 order. Each step only runs while the
	// estimate is still above contextTokenBudget.
	estimate := func() int {
		return estimateTokens(header) + estimateTokens(hardFactLines) + estimateTokens(softFactLines) +
			estimateTokens(summaryLines) + estimateTokens(messageLines) + estimateTokens(draftLines)
	}
	trimmed := []string{}
	if estimate() > contextTokenBudget && len(messageLines) > 10 {
		messageLines = messageLines[len(messageLines)-10:]
		trimmed = append(trimmed, "messages→10")
	}
	if estimate() > contextTokenBudget && len(summaryLines) > 1 {
		// The most relevant summary is the last line (retrieval order was reversed
		// for oldest-first rendering) — keep that one.
		summaryLines = summaryLines[len(summaryLines)-1:]
		trimmed = append(trimmed, "summaries→1")
	}
	if estimate() > contextTokenBudget && len(softFactLines) > 5 {
		softFactLines = softFactLines[:5]
		trimmed = append(trimmed, "facts→5")
	}
	if estimate() > contextTokenBudget && len(draftLines) > 2 {
		draftLines = draftLines[len(draftLines)-2:]
		trimmed = append(trimmed, "drafts→2")
	}
	if len(trimmed) > 0 {
		s.logger.Info("context budget trimming applied",
			"tenant_id", event.TenantID,
			"chat_id", event.ChatID,
			"trimmed", strings.Join(trimmed, ","),
			"final_token_estimate", estimate(),
		)
	}

	// Final assembly preserves the historical section order.
	parts := make([]string, 0, len(header)+len(hardFactLines)+len(softFactLines)+len(summaryLines)+len(messageLines)+len(draftLines)+3)
	parts = append(parts, header...)
	parts = append(parts, hardFactLines...)
	parts = append(parts, softFactLines...)
	if len(summaryLines) > 0 {
		parts = append(parts, "--- ЭПИЗОДИЧЕСКАЯ ПАМЯТЬ (сводки прошлых диалогов) ---")
		parts = append(parts, summaryLines...)
	}
	parts = append(parts, "--- ПАМЯТЬ ТЕКУЩЕГО ДНЯ ---")
	parts = append(parts, messageLines...)
	if len(draftLines) > 0 {
		parts = append(parts, "--- НЕОТПРАВЛЕННЫЕ ЧЕРНОВИКИ (ожидают подтверждения владельца) ---")
		parts = append(parts, draftLines...)
	}
	return parts, records
}

// ownerTimeLocation resolves the owner's IANA timezone for prompt rendering
// (B: unified localized time block). Mirrors the validation in
// apiSaveUserSettings (time.LoadLocation): an empty or unrecognised name
// means the zone is UNKNOWN — callers must label times as UTC explicitly
// rather than silently treating UTC as the owner's true local time (A1).
func ownerTimeLocation(timezone string) (*time.Location, bool) {
	if timezone == "" {
		return nil, false
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, false
	}
	return loc, true
}

// rrfFuse merges two ranked ID lists using Reciprocal Rank Fusion (k=60).
// Returns a deduplicated slice of IDs in descending fused-score order.
// k=60 is the standard constant that balances precision vs recall for short lists.
func rrfFuse(vectorIDs, bm25IDs []int64, k int) []int64 {
	scores := make(map[int64]float64)
	for rank, id := range vectorIDs {
		scores[id] += 1.0 / float64(k+rank+1)
	}
	for rank, id := range bm25IDs {
		scores[id] += 1.0 / float64(k+rank+1)
	}
	// Collect and sort by descending score.
	type scored struct {
		id    int64
		score float64
	}
	list := make([]scored, 0, len(scores))
	for id, sc := range scores {
		list = append(list, scored{id, sc})
	}
	// Tie-break on id so map iteration order cannot make results non-deterministic.
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].id < list[j].id
	})
	result := make([]int64, len(list))
	for i, s := range list {
		result[i] = s.id
	}
	return result
}

// factIDs extracts the IDs from a slice of SemanticFactRecords, preserving order.
func factIDs(facts []store.SemanticFactRecord) []int64 {
	ids := make([]int64, len(facts))
	for i, f := range facts {
		ids[i] = f.ID
	}
	return ids
}

// finalizeFactOrder reduces RRF-fused recall candidates to the top `limit`
// facts for the prompt. Hard-include facts (taboo/health) always survive;
// the rest are reordered by cosine distance to the query vector in SQL when
// possible, with the incoming RRF order as the deterministic fallback for
// facts that have no embedding yet (the async worker fills those later).
// When the candidate set already fits the limit no ranking work is done at all.
func (s *Service) finalizeFactOrder(tenantID string, chatFacts []store.SemanticFactRecord, queryVector []float64, limit int) []store.SemanticFactRecord {
	return s.finalizeFactOrderScoped(tenantID, chatFacts, queryVector, limit, 0, false)
}

func (s *Service) finalizeFactOrderForRoleContext(tenantID string, chatFacts []store.SemanticFactRecord, queryVector []float64, limit int, roleContextID int64) []store.SemanticFactRecord {
	return s.finalizeFactOrderScoped(tenantID, chatFacts, queryVector, limit, roleContextID, true)
}

func (s *Service) finalizeFactOrderScoped(tenantID string, chatFacts []store.SemanticFactRecord, queryVector []float64, limit int, roleContextID int64, roleScoped bool) []store.SemanticFactRecord {
	hard := make([]store.SemanticFactRecord, 0, 2)
	rest := make([]store.SemanticFactRecord, 0, len(chatFacts))
	for _, f := range chatFacts {
		if f.FactType == "taboo" || f.FactType == "health" {
			hard = append(hard, f)
		} else {
			rest = append(rest, f)
		}
	}
	slots := limit - len(hard)
	if slots < 0 {
		slots = 0
	}
	if len(rest) > slots {
		if len(queryVector) > 0 {
			var ranked []store.SemanticFactRecord
			if roleScoped {
				ranked = s.repository.RankFactsByCosineForRoleContext(tenantID, factIDs(rest), queryVector, slots, roleContextID)
			} else {
				ranked = s.repository.RankFactsByCosine(tenantID, factIDs(rest), queryVector, slots)
			}
			// Backfill facts without embeddings in their RRF order.
			if len(ranked) < slots {
				seen := make(map[int64]bool, len(ranked))
				for _, f := range ranked {
					seen[f.ID] = true
				}
				for _, f := range rest {
					if len(ranked) >= slots {
						break
					}
					if !seen[f.ID] {
						ranked = append(ranked, f)
					}
				}
			}
			if len(ranked) > 0 {
				rest = ranked
			} else {
				rest = rest[:slots]
			}
		} else {
			rest = rest[:slots]
		}
	}
	return append(hard, rest...)
}

// aiRoleFor resolves the chat's assigned AI role into the contract shape.
// Returns nil when no role is assigned so the Python service falls back to the
// default assistant persona.
func (s *Service) aiRoleFor(tenantID, chatID string) *contracts.AIRole {
	role, ok := s.resolveRole(tenantID, chatID)
	if !ok {
		return nil
	}
	return &contracts.AIRole{
		Name:          role.Name,
		Description:   role.Description,
		SpeakingStyle: role.SpeakingStyle,
		Goals:         role.Goals,
		Constraints:   role.Constraints,
		Context:       role.Context,
		CustomFields:  role.CustomFields,
		MCPServers:    s.repository.RoleMCPServers(role.ID),
		SpeakAsOwner:  role.SpeakAsOwner,
		ModelTier:     role.ModelTier,
		ModelName:     role.ModelName,
		ToolSettings:  role.ToolSettings,
	}
}

// personaSpeaksAsOwner reports whether the AI should present owner identity.
// True for the default assistant (nil role) and any role with SpeakAsOwner set.
// False for bot-persona roles — those get a stripped UserSettings and no FeedbackContext.
func personaSpeaksAsOwner(role *contracts.AIRole) bool {
	return role == nil || role.SpeakAsOwner
}

// botPersonaEffectiveSettings makes an open bot persona conversational by
// default. Bot chats are otherwise born in conservative manual mode, which
// would save every generated answer as an owner draft and leave the Telegram
// user without a reply. Explicit disabled/silent/semi-auto/auto settings,
// owner-speaking roles, business chats and all risk/privacy gates are preserved.
func botPersonaEffectiveSettings(
	event contracts.NormalizedTelegramEvent,
	settings contracts.ChatSettings,
	role *contracts.AIRole,
) (contracts.ChatSettings, bool) {
	if event.Transport != contracts.TransportBot || role == nil || role.SpeakAsOwner ||
		settings.Mode != contracts.ChatModeManual {
		return settings, false
	}
	settings.Mode = contracts.ChatModeAuto
	return settings, true
}

// resolveRole picks the persona for a chat. Bot-transport chats answer in the
// bot's role and nothing else — the bot role overrides any per-chat assignment
// ("и никак иначе"); a bot without a role falls through to the default persona.
// Non-bot chats keep the legacy per-chat role assignment.
func (s *Service) resolveRole(tenantID, chatID string) (store.AIRoleRecord, bool) {
	if s.repository.ChatTransport(tenantID, chatID) == contracts.TransportBot {
		// The chat id is namespaced by bot, so the governing bot — and its role —
		// is recoverable from the chat alone (multi-bot: each bot its own role).
		bot, ok := s.botForChat(tenantID, chatID)
		if !ok || bot.AIRoleID == 0 {
			return store.AIRoleRecord{}, false
		}
		return s.repository.AIRoleByID(tenantID, bot.AIRoleID)
	}
	return s.repository.AIRoleForChat(tenantID, chatID)
}

// botForChat resolves the bot that owns a bot-transport chat from the bot id
// embedded in its namespaced chat id.
func (s *Service) botForChat(tenantID, chatID string) (store.UserBotRecord, bool) {
	botIDStr, _, ok := contracts.ParseBotChatID(chatID)
	if !ok {
		return store.UserBotRecord{}, false
	}
	botID, err := strconv.ParseInt(botIDStr, 10, 64)
	if err != nil {
		return store.UserBotRecord{}, false
	}
	return s.repository.UserBotByID(tenantID, botID)
}

// activeRoleContextIDForEvent returns the reusable bot memory context currently
// active for this event. Non-bot chats deliberately keep the legacy zero/NULL
// context.
func (s *Service) activeRoleContextIDForEvent(event contracts.NormalizedTelegramEvent) int64 {
	if s == nil || s.repository == nil {
		return 0
	}
	transport := event.Transport
	if transport == "" {
		transport = s.repository.ChatTransport(event.TenantID, event.ChatID)
	}
	if transport != contracts.TransportBot {
		return 0
	}
	bot, ok := s.botForChat(event.TenantID, event.ChatID)
	if !ok {
		return 0
	}
	if bot.ActiveRoleContextID != 0 {
		return bot.ActiveRoleContextID
	}
	ctx, ok := s.repository.ActiveBotRoleContext(event.TenantID, bot.BotID)
	if !ok {
		return 0
	}
	return ctx.ID
}

func (s *Service) memoryRoleContextForEvent(event contracts.NormalizedTelegramEvent) (int64, bool) {
	if s == nil || s.repository == nil {
		return 0, false
	}
	transport := event.Transport
	if transport == "" {
		transport = s.repository.ChatTransport(event.TenantID, event.ChatID)
	}
	if transport != contracts.TransportBot {
		return 0, false
	}
	// Role-scoping only applies once a concrete bot role context is active. A bot
	// whose role was bound before the role-context feature (or with no role at
	// all) has active_role_context_id=0, and every write for it lands at
	// role_context_id=0. The role-scoped readers intentionally return nothing for
	// a zero context, so claiming roleScoped here would blank that bot's entire
	// memory even though its history exists. Fall back to the broad (legacy)
	// readers instead — symmetric with the zero-context writes — so isolation
	// engages only when a real context exists, never erases memory in its absence.
	roleContextID := s.activeRoleContextIDForEvent(event)
	if roleContextID == 0 {
		return 0, false
	}
	return roleContextID, true
}

// recentMessagesForMemoryEvent keeps bot lifecycle reads inside the active role
// memory context while preserving legacy broad reads for non-bot chats.
func (s *Service) recentMessagesForMemoryEvent(event contracts.NormalizedTelegramEvent, limit int, roleContextID int64) []store.MessageRecord {
	if _, roleScoped := s.memoryRoleContextForEvent(event); roleScoped {
		return s.repository.RecentMessagesForRoleContext(event.TenantID, event.ChatID, limit, roleContextID)
	}
	return s.repository.RecentMessages(event.TenantID, event.ChatID, limit)
}

// activeEpisodeForMemoryEvent mirrors prompt-context isolation for bot lifecycle
// reads and for the structured ActiveEpisode field sent beside MemoryContext.
func (s *Service) activeEpisodeForMemoryEvent(event contracts.NormalizedTelegramEvent, roleContextID int64) (store.ActiveEpisodeRecord, bool) {
	if _, roleScoped := s.memoryRoleContextForEvent(event); roleScoped {
		return s.repository.ActiveEpisodeByChatForRoleContext(event.TenantID, event.ChatID, roleContextID)
	}
	return s.repository.ActiveEpisodeByChat(event.TenantID, event.ChatID)
}

// feedbackContext returns recent draft feedback for this chat to send to the AI.
func (s *Service) feedbackContext(tenantID, chatID string, limit int) []contracts.FeedbackItem {
	feedbacks := s.repository.RecentFeedback(tenantID, chatID, limit)
	items := make([]contracts.FeedbackItem, 0, len(feedbacks))
	for _, fb := range feedbacks {
		items = append(items, contracts.FeedbackItem{
			Rating:       fb.Rating,
			ResponseText: fb.ResponseText,
			FeedbackText: fb.FeedbackText,
		})
	}
	return items
}
