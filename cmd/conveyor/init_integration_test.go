package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	postgresstore "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
)

func TestFreshStoreInitServesAPIAndCreatesFirstTaskIntegration(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("CONVEYOR_TEST_DATABASE_URL"))
	if baseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || !strings.HasSuffix(strings.TrimPrefix(parsed.Path, "/"), "_test") {
		t.Fatalf("refusing non-test database URL %q", baseURL)
	}
	admin, err := pgxpool.New(t.Context(), baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "cli_init_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	databaseURL := parsed.String()
	t.Setenv("CONVEYOR_DATABASE_URL", databaseURL)
	t.Setenv("CONVEYOR_API_TOKEN", "fresh-init-operator-token")
	t.Setenv(config.LLMAPIKeyEnv, "fresh-init-api-key")
	answers := initAnswers{
		Organization: "Fresh Org", OperatorName: "Fresh Operator", OperatorEmail: "owner@example.test",
		WorkspaceID: "fresh", WorkspaceName: "Fresh", RepositoryName: "app",
		RepositoryURL: "https://github.com/example/app", BaseBranch: "main", ClonePath: t.TempDir(),
	}
	configPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	var output strings.Builder
	if err = initializeDeployment(t.Context(), &output, configPath, answers); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(configPath); err != nil {
		t.Fatalf("generated config: %v", err)
	}
	if err = initializeDeployment(t.Context(), &output, configPath, answers); err != nil {
		t.Fatalf("safe rerun: %v", err)
	}
	if !strings.Contains(output.String(), "already initialized; no changes were made") {
		t.Fatalf("output=%q", output.String())
	}

	st, err := postgresstore.Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.VerifyPersonalAccessToken(t.Context(), "fresh-init-operator-token"); err != nil {
		t.Fatalf("operator token: %v", err)
	}
	workspaces, err := st.ListWorkspaces(t.Context())
	if err != nil || len(workspaces) != 1 || workspaces[0].ID != "fresh" {
		t.Fatalf("workspaces=%+v err=%v", workspaces, err)
	}
	deployment, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	server := httpapi.NewServer(st)
	server.BearerToken, server.Workspace, server.Repos = "fresh-init-operator-token", "fresh", []string{"app"}
	server.Workspaces, server.ConfigStore = st, st
	server.ConfigProvider = func(ctx context.Context) (*config.Config, error) { return st.RuntimeConfig(ctx, deployment) }
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "First task", nil }
	handler := server.Handler()
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d", health.Code)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks?workspace_id=fresh", strings.NewReader(`{"body":"First task from init","repo":"app","hold":true}`))
	request.Header.Set("Authorization", "Bearer fresh-init-operator-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create task status=%d body=%s", response.Code, response.Body.String())
	}
}
