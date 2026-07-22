package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestChangeTaskSetupHTTPIsAuthenticatedAndSupportsApplyLatest(t *testing.T) {
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	old := config.ExecutionSetup{Name: "current", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "codex", Model: "old", ModelPolicy: config.ModelPolicyExplicit, TimeoutText: "1h"}}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "old", Harness: "codex"}}}}
	latest := old
	latest.ExecutionSettings.Implementation.Model = "new"
	task := core.Task{ID: "apply-latest", Workspace: "demo", Repo: "app", State: core.TaskQueued, SetupName: old.Name, SetupContract: old, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo", Setups: []config.ExecutionSetup{latest}, DefaultSetup: latest.Name}
	server := NewServer(st)
	server.Workspace = "demo"
	server.BearerToken = "token"
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	server.ConfigProvider = provider
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	request := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/setup", strings.NewReader(`{"apply_latest":true,"reason":"refresh corrected model","request_id":"latest-1"}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	if response := request(""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized=%d body=%s", response.Code, response.Body.String())
	}
	response := request("token")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"setup":"current"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	updated, _ := st.GetTask(ctx, task.ID)
	if updated.SetupContract.ExecutionSettings.Implementation.Model != "new" {
		t.Fatalf("updated=%+v", updated.SetupContract)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	var changes int
	for _, event := range events {
		if event.Kind == "task.setup.changed" {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("changes=%d", changes)
	}
}
