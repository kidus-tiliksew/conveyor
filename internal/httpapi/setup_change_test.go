package httpapi

import (
	"context"
	"encoding/json"
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
	t.Skip("server execution configuration retired by DEC-23")
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
	request := func(token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/setup", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	presentReasonBody := `{"apply_latest":true,"reason":"refresh corrected model","request_id":"latest-1"}`
	if response := request("", presentReasonBody); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized=%d body=%s", response.Code, response.Body.String())
	}
	response := request("token", presentReasonBody)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"setup":"current"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for name, body := range map[string]string{
		"absent":     `{"apply_latest":true,"request_id":"latest-absent"}`,
		"empty":      `{"apply_latest":true,"reason":"","request_id":"latest-empty"}`,
		"whitespace": `{"apply_latest":true,"reason":"  \t ","request_id":"latest-whitespace"}`,
	} {
		t.Run(name+" reason", func(t *testing.T) {
			response := request("token", body)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"setup":"current"`) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	updated, _ := st.GetTask(ctx, task.ID)
	if updated.SetupContract.ExecutionSettings.Implementation.Model != "new" {
		t.Fatalf("updated=%+v", updated.SetupContract)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	var changes, emptyReasons, presentReasons int
	for _, event := range events {
		if event.Kind == "task.setup.changed" {
			changes++
			var payload struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Reason == "" {
				emptyReasons++
			} else if payload.Reason == "refresh corrected model" {
				presentReasons++
			}
		}
	}
	if changes != 4 || emptyReasons != 3 || presentReasons != 1 {
		t.Fatalf("changes=%d empty_reasons=%d present_reasons=%d", changes, emptyReasons, presentReasons)
	}
}
