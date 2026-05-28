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
	MessageText  string   `json:"message_text"`
	TenantID     string   `json:"tenant_id"`
	ChatID       string   `json:"chat_id"`
	PrivacyFlags []string `json:"privacy_flags"`
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
type TypedExtractionResponse struct {
	Facts            []TypedFact `json:"facts"`
	BlockedByPrivacy bool        `json:"blocked_by_privacy"`
}

// TypedMemoryExtractor is optionally implemented by AI clients that support typed extraction.
// Called asynchronously after the draft is dispatched; never blocks the reply path.
type TypedMemoryExtractor interface {
	ExtractFactsTyped(ctx context.Context, request TypedExtractionRequest) (TypedExtractionResponse, error)
}

// EmbedTextsRequest is sent to the Python AI service to generate text embeddings.
type EmbedTextsRequest struct {
	Texts []string `json:"texts"`
	Model string   `json:"model,omitempty"`
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
