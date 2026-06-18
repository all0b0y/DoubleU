package aiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"doubleu/backend-go/internal/contracts"
)

func TestHTTPClientProcessMessageAndReviseDraft(t *testing.T) {
	client := NewHTTPClient("http://ai-service", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body bytes.Buffer
		switch r.URL.Path {
		case "/ai/process-message":
			_ = json.NewEncoder(&body).Encode(contracts.AIProcessMessageResponse{
				Decision:     contracts.DecisionDraft,
				ResponseText: "draft",
				TextFormat:   contracts.TextFormatRich,
				Confidence:   0.8,
				RiskLevel:    contracts.RiskLow,
				Usage:        contracts.TokenUsage{},
				MemoryUsed:   []string{},
			})
		case "/ai/revise-draft":
			_ = json.NewEncoder(&body).Encode(DraftRevisionResponse{Text: "revised"})
		default:
			return responseWithStatus(http.StatusNotFound, "not found"), nil
		}
		return responseWithStatus(http.StatusOK, body.String()), nil
	})})

	response, err := client.ProcessMessage(context.Background(), contracts.AIProcessMessageRequest{RequestID: "r1"})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if response.ResponseText != "draft" {
		t.Fatalf("response = %+v", response)
	}
	if response.TextFormat != contracts.TextFormatRich {
		t.Fatalf("text_format = %q", response.TextFormat)
	}
	revised, err := client.ReviseDraft(context.Background(), DraftRevisionRequest{Text: "draft"})
	if err != nil {
		t.Fatalf("revise: %v", err)
	}
	if revised.Text != "revised" {
		t.Fatalf("revised = %+v", revised)
	}
}

func TestHTTPClientErrors(t *testing.T) {
	client := NewHTTPClient("", nil)
	if _, err := client.ProcessMessage(context.Background(), contracts.AIProcessMessageRequest{}); err == nil {
		t.Fatalf("expected empty base url process error")
	}
	if _, err := client.ReviseDraft(context.Background(), DraftRevisionRequest{}); err == nil {
		t.Fatalf("expected empty base url revise error")
	}

	client = NewHTTPClient("http://ai-service", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return responseWithStatus(http.StatusBadGateway, "bad"), nil
	})})
	if _, err := client.ProcessMessage(context.Background(), contracts.AIProcessMessageRequest{}); err == nil {
		t.Fatalf("expected status error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func responseWithStatus(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
