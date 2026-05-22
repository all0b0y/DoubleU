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
