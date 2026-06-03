// Package aiclient contains the Python AI service boundary.
package aiclient

import (
	"context"

	"doubleu/backend-go/internal/contracts"
)

// Client processes normalized messages through the Python AI layer.
type Client interface {
	ProcessMessage(ctx context.Context, request contracts.AIProcessMessageRequest) (contracts.AIProcessMessageResponse, error)
}

// DraftRevisionRequest asks the Python AI layer to revise an existing draft.
type DraftRevisionRequest struct {
	Action contracts.ApprovalAction `json:"action"`
	Text   string                   `json:"text"`
}

// DraftRevisionResponse contains a revised draft.
type DraftRevisionResponse struct {
	Text string `json:"text"`
}

// DraftReviser is optionally implemented by AI clients that can revise drafts.
type DraftReviser interface {
	ReviseDraft(ctx context.Context, request DraftRevisionRequest) (DraftRevisionResponse, error)
}

// ChatSummarizer is optionally implemented by AI clients that can summarize chats.
type ChatSummarizer interface {
	SummarizeChat(ctx context.Context, request contracts.ChatSummaryRequest) (contracts.ChatSummaryResponse, error)
}

// MemoryExtractionRequest is sent to the Python AI service for async fact extraction.
type MemoryExtractionRequest struct {
	MessageText  string   `json:"message_text"`
	TenantID     string   `json:"tenant_id"`
	ChatID       string   `json:"chat_id"`
	PrivacyFlags []string `json:"privacy_flags"`
	TraceID      string   `json:"trace_id,omitempty"`
}

// MemoryExtractionResponse is returned by the Python AI service for async fact extraction.
type MemoryExtractionResponse struct {
	Memories         []string `json:"memories"`
	BlockedByPrivacy bool     `json:"blocked_by_privacy"`
}

// MemoryExtractor is optionally implemented by AI clients that can extract facts from messages.
// It is called asynchronously after the draft is returned so it never blocks the reply path.
type MemoryExtractor interface {
	ExtractMemory(ctx context.Context, request MemoryExtractionRequest) (MemoryExtractionResponse, error)
}

// TypedExtractionRequest is sent to the Python AI service for typed, scoped fact extraction.
type TypedExtractionRequest struct {
	MessageText    string   `json:"message_text"`
	TenantID       string   `json:"tenant_id"`
	ChatID         string   `json:"chat_id"`
	PrivacyFlags   []string `json:"privacy_flags"`
	TraceID        string   `json:"trace_id,omitempty"`
	// RecentMessages contains the last few messages in the chat (oldest first) so the
	// Python extractor can detect multi-turn facts spread across several messages.
	RecentMessages []string `json:"recent_messages,omitempty"`
}

// TypedFact is a single typed, atomic fact extracted from a message.
// fact_type and subject determine scope routing on the Go side per design §5.4.
type TypedFact struct {
	FactText   string  `json:"fact_text"`
	FactType   string  `json:"fact_type"`
	Subject    string  `json:"subject"`
	ScopeKind  string  `json:"scope_kind"`
	Confidence float64 `json:"confidence"`
}

// TypedExtractionResponse is returned by the Python AI service for typed fact extraction.
// The diagnostic fields back the extraction_log row so a returned empty Facts slice is
// distinguishable from a silent failure (design: extraction observability).
type TypedExtractionResponse struct {
	Facts                   []TypedFact `json:"facts"`
	BlockedByPrivacy        bool        `json:"blocked_by_privacy"`
	ModelUsed               string      `json:"model_used"`
	RawOutput               string      `json:"raw_output"`
	LLMFactsCount           int         `json:"llm_facts_count"`
	DeterministicFactsCount int         `json:"deterministic_facts_count"`
}

// TypedMemoryExtractor is optionally implemented by AI clients that support typed extraction.
// Called asynchronously after the draft is dispatched; never blocks the reply path.
type TypedMemoryExtractor interface {
	ExtractFactsTyped(ctx context.Context, request TypedExtractionRequest) (TypedExtractionResponse, error)
}

// EmbedTextsRequest is sent to the Python AI service to generate text embeddings.
type EmbedTextsRequest struct {
	Texts   []string `json:"texts"`
	Model   string   `json:"model,omitempty"`
	TraceID string   `json:"trace_id,omitempty"`
}

// EmbedTextsResponse contains one embedding vector per input text.
type EmbedTextsResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
	ModelUsed  string      `json:"model_used"`
}

// TextEmbedder is optionally implemented by AI clients that can generate embeddings.
// The Go embedding worker calls this to populate fact_embeddings rows.
type TextEmbedder interface {
	EmbedTexts(ctx context.Context, request EmbedTextsRequest) (EmbedTextsResponse, error)
}

// TopicTaggingRequest is sent to the Python AI service to label the current episode topic.
type TopicTaggingRequest struct {
	MessageText    string   `json:"message_text"`
	RecentMessages []string `json:"recent_messages,omitempty"`
	TenantID       string   `json:"tenant_id,omitempty"`
	ChatID         string   `json:"chat_id,omitempty"`
}

// TopicTaggingResponse contains the short topic label for the active episode.
type TopicTaggingResponse struct {
	Topic     string `json:"topic"`
	ModelUsed string `json:"model_used"`
}

// TopicTagger is optionally implemented by AI clients that can tag conversation topics.
// Called asynchronously after each message to populate active_episodes.current_topic.
type TopicTagger interface {
	TagTopic(ctx context.Context, request TopicTaggingRequest) (TopicTaggingResponse, error)
}

// RerankFactsRequest is sent to the Python AI service to rerank candidate facts.
type RerankFactsRequest struct {
	Query string       `json:"query"`
	Facts []RerankFact `json:"facts"`
	TopK  int          `json:"top_k"`
}

// RerankFact is a single candidate fact with its database ID and text.
type RerankFact struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
}

// RerankFactsResponse contains fact IDs in order of descending relevance.
type RerankFactsResponse struct {
	RankedIDs []int64 `json:"ranked_ids"`
	ModelUsed string  `json:"model_used"`
}

// FactReranker is optionally implemented by AI clients that support cross-encoder reranking.
// Called during conversationContext construction to reorder broad recall results.
type FactReranker interface {
	RerankFacts(ctx context.Context, request RerankFactsRequest) (RerankFactsResponse, error)
}
