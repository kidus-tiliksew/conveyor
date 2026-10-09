package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
	"gopkg.in/yaml.v3"
)

// composedRuntimeDeployment carries deployment control-plane values that
// appear nowhere in workspace policy, so any of them in a workspace read is a
// leak of the runtime composition (component-runtime; DEC-56).
const composedRuntimeDeployment = `workspace: demo
database: {url: "postgres://db.invalid/conveyor"}
execution_settings:
  control_plane:
    triage: {model: leak-triage-model, effort: low, timeout: 7m}
    planning: {model: leak-planning-model, effort: high, timeout: 9m, exploration_output_tokens: 1234}
planning_models: [leak-planning-model, leak-alternate-model]
repos: []
`

var composedRuntimeLeaks = []string{"leak-triage-model", "leak-planning-model", "leak-alternate-model", "7m", "9m", "1234"}

// composedRuntimeServer serves a policy-only workspace stored in a volatile
// backend through the real RuntimeConfig composition, exactly as conveyord
// wires a durable store.
func composedRuntimeServer(t *testing.T) (*Server, store.Backend, *config.Config, context.Context) {
	t.Helper()
	deployment, err := config.ParseDeployment([]byte(composedRuntimeDeployment), "composed-runtime.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	backend := store.NewVolatileBackend()
	t.Cleanup(backend.Close)
	ctx := store.WithWorkspace(t.Context(), "demo")
	workspace := &config.Config{Workspace: "demo", Database: config.Database{Backend: "postgres"}, MaxBounces: 4, Repos: []config.Repo{{Name: "api", URL: "https://github.com/example/api", Base: "main"}}}
	if _, err = backend.BootstrapWorkspaceConfig(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	runtime, err := backend.RuntimeConfig(ctx, deployment)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ExecutionSettings == nil || runtime.ExecutionSettings.ControlPlane.Planning.Model != "leak-planning-model" || runtime.Routing.Stages["triage"].Model != "leak-triage-model" || len(runtime.PlanningModels) != 2 {
		t.Fatalf("fixture runtime is not composed: settings=%+v triage=%+v allowlist=%v", runtime.ExecutionSettings, runtime.Routing.Stages["triage"], runtime.PlanningModels)
	}
	provider := func(ctx context.Context) (*config.Config, error) { return backend.RuntimeConfig(ctx, deployment) }
	server := NewServer(backend)
	server.Workspace = "demo"
	server.BearerToken = "token"
	server.Deployment = deployment
	server.ConfigProvider = provider
	server.WorkOrders = &workorder.Service{Store: backend, ConfigProvider: provider}
	server.Workers = &workerservice.Service{Store: backend, WorkOrders: server.WorkOrders, ConfigProvider: provider}
	return server, backend, deployment, ctx
}

// assertPolicyOnlyExport fails when a workspace read carries any composed
// control-plane value or any execution-detail key at any depth.
func assertPolicyOnlyExport(t *testing.T, name string, body []byte) {
	t.Helper()
	for _, leaked := range composedRuntimeLeaks {
		if strings.Contains(string(body), leaked) {
			t.Fatalf("%s carries composed control-plane value %q: %s", name, leaked, body)
		}
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{"execution_settings": true, "planning_models": true, "routing": true, "harnesses": true, "setups": true, "default_setup": true, "model": true, "effort": true, "control_plane": true, "triage": true, "planning": true}
	var walk func(any, string)
	walk = func(value any, path string) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if forbidden[key] {
					t.Fatalf("%s carries execution-detail key %s.%s: %s", name, path, key, body)
				}
				walk(child, path+"."+key)
			}
		case []any:
			for _, child := range typed {
				walk(child, path+"[]")
			}
		}
	}
	walk(decoded, name)
}

func requireUnchangedComposition(t *testing.T, backend store.Backend, deployment *config.Config, ctx context.Context, before []byte, version int64) {
	t.Helper()
	after, err := yaml.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("workspace read mutated the deployment\nbefore:\n%s\nafter:\n%s", before, after)
	}
	stored, err := backend.WorkspaceConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != version || stored.Document.ExecutionSettings != nil || len(stored.Document.PlanningModels) != 0 {
		t.Fatalf("workspace read changed the stored configuration: version %d -> %d document=%+v", version, stored.Version, stored.Document)
	}
}

func TestWorkspaceInfoHTTPServesNoComposedControlPlane(t *testing.T) {
	composed, backend, deployment, ctx := composedRuntimeServer(t)
	// The deployment bearer token authenticates against the identity store;
	// the workspace read comes from the composed runtime provider.
	server := NewServer(store.NewMemory())
	server.BearerToken = "token"
	server.Deployment = deployment
	server.ConfigProvider = composed.ConfigProvider
	before, err := yaml.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := backend.WorkspaceConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"runtime provider", "deployment value"} {
		if mode == "deployment value" {
			// A server without a durable provider serves the deployment value.
			server.ConfigProvider = nil
			server.WorkspaceInfo = NewWorkspaceInfo(deployment)
		}
		request := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", mode, response.Code, response.Body.String())
		}
		assertPolicyOnlyExport(t, "workspace info ("+mode+")", response.Body.Bytes())
		var info map[string]json.RawMessage
		if err = json.Unmarshal(response.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		if len(info) != 4 {
			t.Fatalf("%s: workspace info fields = %s", mode, response.Body.String())
		}
		for _, key := range []string{"workspace", "max_bounces", "database", "repos"} {
			if _, ok := info[key]; !ok {
				t.Fatalf("%s: workspace info omits %q: %s", mode, key, response.Body.String())
			}
		}
	}
	requireUnchangedComposition(t, backend, deployment, ctx, before, stored.Version)
}
