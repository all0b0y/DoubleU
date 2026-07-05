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

func TestHTTPClientDraftRoleContract(t *testing.T) {
	client := NewHTTPClient("http://ai-service", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/ai/draft-role" {
			return responseWithStatus(http.StatusNotFound, "not found"), nil
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request["text"] != "Нужен менеджер" || len(request) != 1 {
			t.Fatalf("draft-role request contract = %#v", request)
		}
		var body bytes.Buffer
		_ = json.NewEncoder(&body).Encode(DraftRoleResponse{
			Name:          "Менеджер",
			Description:   "Помогает клиентам",
			SpeakingStyle: "Кратко",
			Goals:         "Решать вопросы",
			Constraints:   "Не выдумывать факты",
			CustomFields: []contracts.AIRoleCustomField{{
				Title: "Эскалация", Description: "Когда звать человека",
			}},
		})
		return responseWithStatus(http.StatusOK, body.String()), nil
	})})

	response, err := client.DraftRole(
		context.Background(), DraftRoleRequest{Text: "Нужен менеджер"},
	)
	if err != nil {
		t.Fatalf("draft role: %v", err)
	}
	if response.Name != "Менеджер" || len(response.CustomFields) != 1 {
		t.Fatalf("draft-role response contract = %+v", response)
	}
}

func TestHTTPClientDraftSkillContract(t *testing.T) {
	client := NewHTTPClient("http://ai-service", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/ai/draft-skill" {
			return responseWithStatus(http.StatusNotFound, "not found"), nil
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request["text"] != "Как отвечать на возражения" || len(request) != 1 {
			t.Fatalf("draft-skill request contract = %#v", request)
		}
		var body bytes.Buffer
		_ = json.NewEncoder(&body).Encode(DraftSkillResponse{
			Name:        "работа-с-возражениями",
			Description: "Как отвечать на ценовые возражения.",
			Body:        "## Когда применять\n...",
		})
		return responseWithStatus(http.StatusOK, body.String()), nil
	})})

	response, err := client.DraftSkill(
		context.Background(), DraftSkillRequest{Text: "Как отвечать на возражения"},
	)
	if err != nil {
		t.Fatalf("draft skill: %v", err)
	}
	if response.Name != "работа-с-возражениями" || response.Body == "" {
		t.Fatalf("draft-skill response contract = %+v", response)
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
