package aiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"doubleu/backend-go/internal/contracts"
	tracepkg "doubleu/backend-go/internal/trace"
)

// HTTPClient calls the Python AI service over HTTP.
type HTTPClient struct {
	baseURL string
	client  *http.Client
}

// NewHTTPClient creates an HTTP-backed Python AI client.
func NewHTTPClient(baseURL string, client *http.Client) *HTTPClient {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

// ProcessMessage sends a normalized message to the Python AI process-message endpoint.
func (c *HTTPClient) ProcessMessage(ctx context.Context, request contracts.AIProcessMessageRequest) (contracts.AIProcessMessageResponse, error) {
	var response contracts.AIProcessMessageResponse
	if c.baseURL == "" {
		return response, fmt.Errorf("ai base url is empty")
	}
	if err := c.postJSON(ctx, "/ai/process-message", request, &response); err != nil {
		return response, err
	}
	return response, nil
}

// ReviseDraft sends a draft revision request to the Python AI draft revision endpoint.
func (c *HTTPClient) ReviseDraft(ctx context.Context, request DraftRevisionRequest) (DraftRevisionResponse, error) {
	var response DraftRevisionResponse
	if c.baseURL == "" {
		return response, fmt.Errorf("ai base url is empty")
	}
	if err := c.postJSON(ctx, "/ai/revise-draft", request, &response); err != nil {
		return response, err
	}
	return response, nil
}

// SummarizeChat sends a chat history to the Python AI summarize-chat endpoint.
func (c *HTTPClient) SummarizeChat(ctx context.Context, request contracts.ChatSummaryRequest) (contracts.ChatSummaryResponse, error) {
	var response contracts.ChatSummaryResponse
	if c.baseURL == "" {
		return response, fmt.Errorf("ai base url is empty")
	}
	if err := c.postJSON(ctx, "/ai/summarize-chat", request, &response); err != nil {
		return response, err
	}
	return response, nil
}

// ExtractMemory calls the Python AI /ai/extract-memory endpoint to extract facts from a message.
// This is designed for async use: callers should fire-and-forget via a goroutine.
func (c *HTTPClient) ExtractMemory(ctx context.Context, request MemoryExtractionRequest) (MemoryExtractionResponse, error) {
	var response MemoryExtractionResponse
	if c.baseURL == "" {
		return response, fmt.Errorf("ai base url is empty")
	}
	if err := c.postJSON(ctx, "/ai/extract-memory", request, &response); err != nil {
		return response, err
	}
	return response, nil
}

// ExtractFactsTyped calls the Python AI /ai/extract-facts-typed endpoint.
// Returns structured TypedFact objects with fact_type, subject, and scope_kind
// so the Go backend can route facts without hard-coded defaults.
// Designed for async use; callers should fire-and-forget via a goroutine.
func (c *HTTPClient) ExtractFactsTyped(ctx context.Context, request TypedExtractionRequest) (TypedExtractionResponse, error) {
	var response TypedExtractionResponse
	if c.baseURL == "" {
		return response, fmt.Errorf("ai base url is empty")
	}
	if err := c.postJSON(ctx, "/ai/extract-facts-typed", request, &response); err != nil {
		return response, err
	}
	return response, nil
}

// embedTimeout is the per-call deadline for /ai/embed-texts.
// Embedding a batch via a remote provider (OpenRouter/qwen) typically takes
// 5–20 seconds; the default http.Client timeout of 10 s is too tight and
// causes Broken-pipe errors when the provider is slow.
const embedTimeout = 60 * time.Second

// EmbedTexts calls the Python AI /ai/embed-texts endpoint to generate embeddings.
// The Go embedding worker calls this in batches for facts without embeddings.
func (c *HTTPClient) EmbedTexts(ctx context.Context, request EmbedTextsRequest) (EmbedTextsResponse, error) {
	var response EmbedTextsResponse
	if c.baseURL == "" {
		return response, fmt.Errorf("ai base url is empty")
	}
	// Override the shared client timeout: embedding a batch through a remote
	// provider takes much longer than a chat-completion request.
	embedCtx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	if err := c.postJSON(embedCtx, "/ai/embed-texts", request, &response); err != nil {
		return response, err
	}
	return response, nil
}

func (c *HTTPClient) postJSON(ctx context.Context, path string, input any, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Propagate the W3C trace-id end-to-end so Python pipelines and Langfuse
	// land on the same trace.  Legacy X-DoubleU-Trace-ID is sent for one
	// release as a transitional fallback for any consumer not yet on W3C.
	if traceID := tracepkg.FromContext(ctx); traceID != "" {
		req.Header.Set("traceparent", tracepkg.FormatTraceparent(traceID))
		req.Header.Set("X-DoubleU-Trace-ID", traceID)
	}
	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("ai service status %d: %s", res.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.NewDecoder(res.Body).Decode(output)
}
