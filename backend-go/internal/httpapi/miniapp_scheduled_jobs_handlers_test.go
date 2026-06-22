package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

func TestScheduledJobsListAndCancelAPI(t *testing.T) {
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{
		TenantID: testTenant, ChatID: "42", Transport: contracts.TransportBot,
	})
	job, err := repo.InsertScheduledJob(store.ScheduledJobRecord{
		TenantID: testTenant, ChatID: "42", Kind: "reminder", Target: "owner",
		FireAt: time.Now().Add(time.Hour), Payload: map[string]any{"text": "Напомнить"},
		CreatedBy: "ai", IdempotencyKey: "api-job-1",
	})
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	server := NewServer(core.NewService(repo, nil, telegram.NoopSender{}))

	response := miniappRequest(t, server, http.MethodGet, "/api/v1/chats/42/scheduled-jobs", "")
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", response.Code, response.Body.String())
	}
	var listed struct {
		Jobs []store.ScheduledJobRecord `json:"scheduled_jobs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	if len(listed.Jobs) != 1 || listed.Jobs[0].ID != job.ID {
		t.Fatalf("listed jobs = %+v", listed.Jobs)
	}

	response = miniappRequest(
		t, server, http.MethodDelete,
		"/api/v1/chats/42/scheduled-jobs/"+intString(job.ID), "",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s", response.Code, response.Body.String())
	}
	jobs := repo.ListJobs(testTenant, "42")
	if len(jobs) != 1 || jobs[0].Status != "cancelled" {
		t.Fatalf("jobs after cancel = %+v", jobs)
	}
}

func intString(value int64) string {
	return strconv.FormatInt(value, 10)
}
