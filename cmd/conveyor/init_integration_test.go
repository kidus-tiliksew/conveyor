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
	"github.com/kidus-tiliksew/conveyor/internal/store"
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
	t.Setenv(config.PublicURLEnv, "https://conveyor.example/")
	t.Setenv("PATH", t.TempDir())
	answers := initAnswers{
		Organization: "Fresh Org", OperatorName: "Fresh Operator", OperatorEmail: "owner@example.test",
		WorkspaceID: "fresh", WorkspaceName: "Fresh", RepositoryName: "app",
		RepositoryURL: "https://github.com/example/app", BaseBranch: "main",
	}
	configPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	var output strings.Builder
	command := initCmd()
	command.SetIn(strings.NewReader("Fresh Org\nFresh Operator\nowner@example.test\nfresh\nFresh\napp\nhttps://github.com/example/app\nmain\n"))
	command.SetOut(&output)
	command.SetArgs([]string{"--config", configPath})
	if err = command.Execute(); err != nil {
		t.Fatal(err)
	}
	firstInitToken := signInTokenFromOutput(t, output.String())
	if firstInitToken == "" {
		t.Fatalf("fresh init did not print first operator sign-in link: %q", output.String())
	}
	if _, err = os.Stat(configPath); err != nil {
		t.Fatalf("generated config: %v", err)
	}
	var rerunOutput strings.Builder
	if err = initializeDeployment(t.Context(), &rerunOutput, configPath, answers); err != nil {
		t.Fatalf("safe rerun: %v", err)
	}
	secondInitToken := signInTokenFromOutput(t, rerunOutput.String())
	if !strings.Contains(rerunOutput.String(), "already initialized; issuing a fresh") || secondInitToken == "" || secondInitToken == firstInitToken {
		t.Fatalf("rerun output=%q", rerunOutput.String())
	}

	st, err := postgresstore.Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.VerifyPersonalAccessToken(t.Context(), "fresh-init-operator-token"); err != nil {
		t.Fatalf("operator token: %v", err)
	}
	if _, _, err = st.RedeemSignInLink(t.Context(), firstInitToken); err == nil {
		t.Fatal("init rerun did not invalidate the prior unredeemed link")
	}

	owner, err := st.VerifyPersonalAccessToken(t.Context(), "fresh-init-operator-token")
	if err != nil {
		t.Fatal(err)
	}
	credential := core.AuthenticatedCredential{ID: "bootstrap", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}
	operatorCtx := store.WithWorkspace(store.WithActor(store.WithCredential(t.Context(), credential), store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser}), "fresh")
	if _, err = st.GrantWorkspaceRole(operatorCtx, "pending@example.test", "fresh", core.WorkspaceRoleViewer); err != nil {
		t.Fatalf("create pending invitation: %v", err)
	}

	issue := func(email string) (string, error) {
		command := userCmd()
		var commandOutput strings.Builder
		command.SetOut(&commandOutput)
		command.SetArgs([]string{"issue-link", email})
		if commandErr := command.Execute(); commandErr != nil {
			return "", commandErr
		}
		return signInTokenFromOutput(t, commandOutput.String()), nil
	}
	firstActiveToken, err := issue(" OWNER@example.test ")
	if err != nil {
		t.Fatalf("active account issue-link: %v", err)
	}
	secondActiveToken, err := issue("owner@example.test")
	if err != nil {
		t.Fatalf("active account reissue: %v", err)
	}
	if firstActiveToken == secondActiveToken {
		t.Fatal("reissue returned the prior token")
	}
	if _, _, err = st.RedeemSignInLink(t.Context(), firstActiveToken); err == nil {
		t.Fatal("reissue did not invalidate prior active-account link")
	}
	if pendingToken, pendingErr := issue("pending@example.test"); pendingErr != nil || pendingToken == "" {
		t.Fatalf("pending invitation issue-link token=%q err=%v", pendingToken, pendingErr)
	}
	if _, unknownErr := issue("unknown@example.test"); unknownErr == nil || !strings.Contains(unknownErr.Error(), "no active account or pending invitation") {
		t.Fatalf("unknown email error=%v", unknownErr)
	}
	deactivated, err := st.ProvisionIdentityUser(operatorCtx, "deactivated@example.test", "Deactivated User")
	if err != nil {
		t.Fatalf("provision deactivated test account: %v", err)
	}
	if _, err = st.DeactivateIdentityUser(operatorCtx, deactivated.ID); err != nil {
		t.Fatalf("deactivate test account: %v", err)
	}
	if _, deactivatedErr := issue("deactivated@example.test"); deactivatedErr == nil || !strings.Contains(deactivatedErr.Error(), "no active account or pending invitation") {
		t.Fatalf("deactivated email error=%v", deactivatedErr)
	}
	var cliEvents, leakedSecrets int
	if err = st.Pool().QueryRow(t.Context(), `SELECT
		count(*) FILTER (WHERE actor_id=$1 AND actor_role='system' AND kind IN ('identity.signin_link_issued','identity.invitation_delivery_fallback')),
		count(*) FILTER (WHERE payload_json::text LIKE '%cv_signin_%')
		FROM deployment_events`, hostLocalSignInLinkActorID).Scan(&cliEvents, &leakedSecrets); err != nil {
		t.Fatal(err)
	}
	if cliEvents < 8 || leakedSecrets != 0 {
		t.Fatalf("CLI audit events=%d leaked secret payloads=%d", cliEvents, leakedSecrets)
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

func signInTokenFromOutput(t *testing.T, output string) string {
	t.Helper()
	const marker = "/sign-in#token="
	index := strings.LastIndex(output, marker)
	if index < 0 {
		return ""
	}
	encoded := output[index+len(marker):]
	if newline := strings.IndexByte(encoded, '\n'); newline >= 0 {
		encoded = encoded[:newline]
	}
	token, err := url.QueryUnescape(strings.TrimSpace(encoded))
	if err != nil {
		t.Fatalf("decode sign-in token from output: %v", err)
	}
	return token
}
