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
