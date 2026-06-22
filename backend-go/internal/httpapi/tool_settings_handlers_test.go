package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/store"
)

func TestToolCatalogAndRoleToolSettingsAPI(t *testing.T) {
	repo := store.NewRepository()
	server := NewServer(core.NewService(repo, nil, nil))

	res := miniappRequest(t, server, http.MethodGet, "/api/v1/tool-catalog", "")
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"key":"memory.search_facts"`) ||
		!strings.Contains(res.Body.String(), `"category":"read"`) {
		t.Fatalf("tool catalog status=%d body=%s", res.Code, res.Body.String())
	}

	res = miniappRequest(t, server, http.MethodPost, "/api/v1/roles",
		`{"name":"Менеджер","description":"Помогает","tool_settings":{"memory.search_facts":false}}`)
	if res.Code != http.StatusOK {
		t.Fatalf("create role status=%d body=%s", res.Code, res.Body.String())
	}
	var created roleJSON
	decodeBody(t, res, &created)
	if created.ToolSettings["memory.search_facts"] {
		t.Fatalf("created tool settings=%+v", created.ToolSettings)
	}

	res = miniappRequest(t, server, http.MethodPut, fmt.Sprintf("/api/v1/roles/%d/tools", created.ID),
		`{"tool_settings":{"memory.save_fact":false}}`)
	if res.Code != http.StatusOK {
		t.Fatalf("save tools status=%d body=%s", res.Code, res.Body.String())
	}
	role, ok := repo.AIRoleByID(testTenant, created.ID)
	if !ok || role.Name != "Менеджер" || len(role.ToolSettings) != 1 || role.ToolSettings["memory.save_fact"] {
		t.Fatalf("dedicated tools update changed wrong fields: %+v ok=%v", role, ok)
	}

	res = miniappRequest(t, server, http.MethodPut, fmt.Sprintf("/api/v1/roles/%d/tools", created.ID),
		`{"tool_settings":{"unknown.tool":true}}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("unknown PUT tool key status=%d body=%s", res.Code, res.Body.String())
	}
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/roles",
		`{"name":"Bad","description":"Bad","tool_settings":{"unknown.tool":true}}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("unknown create tool key status=%d body=%s", res.Code, res.Body.String())
	}
	res = miniappRequest(t, server, http.MethodPut, "/api/v1/roles/999/tools",
		`{"tool_settings":{}}`)
	if res.Code != http.StatusNotFound {
		t.Fatalf("missing role tools status=%d body=%s", res.Code, res.Body.String())
	}
}
