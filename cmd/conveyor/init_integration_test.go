package main

import (
	"context"
	"errors"
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
	"github.com/kidus-tiliksew/conveyor/internal/store/backend"
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
	assertGeneratedConfigHoldsBackendOnly(t, configPath, "postgres", databaseURL)
	assertNoDatabaseSecret(t, "fresh init output", output.String(), databaseURL)
	var rerunOutput strings.Builder
	if err = initializeDeployment(store.WithActor(t.Context(), store.SystemActor()), &rerunOutput, configPath, answers); err != nil {
		t.Fatalf("safe rerun: %v", err)
	}
	secondInitToken := signInTokenFromOutput(t, rerunOutput.String())
	if !strings.Contains(rerunOutput.String(), "already initialized; issuing a fresh") || secondInitToken == "" || secondInitToken == firstInitToken {
		t.Fatalf("rerun output=%q", rerunOutput.String())
	}
	assertInitRerunToleratesPaddedURL(t, databaseURL, configPath, answers)
	// Every answer flag reruns init through the command without reading
	// standard input (req-agent-guided-setup AC-5.1).
	flagConfigPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	var flagOutput strings.Builder
	flagCommand := initCmd()
	flagCommand.SetIn(failingReader{t})
	flagCommand.SetOut(&flagOutput)
	flagCommand.SetArgs([]string{
		"--organization", answers.Organization, "--operator-name", answers.OperatorName, "--operator-email", answers.OperatorEmail,
		"--workspace-id", answers.WorkspaceID, "--workspace-name", answers.WorkspaceName, "--repository-name", answers.RepositoryName,
		"--repository-url", answers.RepositoryURL, "--default-branch", answers.BaseBranch, "--config", flagConfigPath,
	})
	if err = flagCommand.Execute(); err != nil {
		t.Fatalf("answer-flag init: %v", err)
	}
	if _, err = os.Stat(flagConfigPath); err != nil || strings.Contains(flagOutput.String(), "Organization name") || signInTokenFromOutput(t, flagOutput.String()) == "" {
		t.Fatalf("answer-flag init config err=%v output=%q", err, flagOutput.String())
	}
	assertGeneratedConfigHoldsBackendOnly(t, flagConfigPath, "postgres", databaseURL)
	// A file in the pre-DEC-56 init shape is accepted on rerun and left
	// unchanged (component-identity-membership).
	legacyPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	legacy := legacyExecutorInitConfig(t, databaseURL, answers)
	if err = os.WriteFile(legacyPath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	var legacyOutput strings.Builder
	if err = initializeDeployment(store.WithActor(t.Context(), store.SystemActor()), &legacyOutput, legacyPath, answers); err != nil {
		t.Fatalf("rerun over a legacy executor config: %v", err)
	}
	if after, readErr := os.ReadFile(legacyPath); readErr != nil || string(after) != string(legacy) {
		t.Fatalf("legacy config was rewritten: %v", readErr)
	}
	legacyToken := signInTokenFromOutput(t, legacyOutput.String())
	if legacyToken == "" {
		t.Fatal("legacy rerun printed no sign-in link")
	}
	assertLegacyURLNotice(t, legacyOutput.String(), legacyPath, databaseURL)
	assertInitCollisionsRefused(t, databaseURL, "postgres", answers, legacyToken)

	st, err := postgresstore.Open(store.WithActor(t.Context(), store.SystemActor()), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.VerifyPersonalAccessToken(store.WithActor(t.Context(), store.SystemActor()), "fresh-init-operator-token"); err != nil {
		t.Fatalf("operator token: %v", err)
	}
	if _, _, err = st.RedeemSignInLink(store.WithActor(t.Context(), store.SystemActor()), firstInitToken); err == nil {
		t.Fatal("init rerun did not invalidate the prior unredeemed link")
	}

	owner, err := st.VerifyPersonalAccessToken(store.WithActor(t.Context(), store.SystemActor()), "fresh-init-operator-token")
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
	if _, _, err = st.RedeemSignInLink(store.WithActor(t.Context(), store.SystemActor()), firstActiveToken); err == nil {
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
	if err = st.Pool().QueryRow(store.WithActor(t.Context(), store.SystemActor()), `SELECT
		count(*) FILTER (WHERE actor_id=$1 AND actor_role='system' AND kind IN ('identity.signin_link_issued','identity.invitation_delivery_fallback')),
		count(*) FILTER (WHERE payload_json::text LIKE '%cv_signin_%')
		FROM deployment_events`, hostLocalSignInLinkActorID).Scan(&cliEvents, &leakedSecrets); err != nil {
		t.Fatal(err)
	}
	if cliEvents < 8 || leakedSecrets != 0 {
		t.Fatalf("CLI audit events=%d leaked secret payloads=%d", cliEvents, leakedSecrets)
	}
	workspaces, err := st.ListWorkspaces(store.WithActor(t.Context(), store.SystemActor()))
	if err != nil || len(workspaces) != 1 || workspaces[0].ID != "fresh" {
		t.Fatalf("workspaces=%+v err=%v", workspaces, err)
	}
	deployment, err := config.LoadDeployment(configPath, nil)
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
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks?workspace_id=fresh", strings.NewReader(`{"body":"First task from init","repo":"app"}`))
	request.Header.Set("Authorization", "Bearer fresh-init-operator-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create task status=%d body=%s", response.Code, response.Body.String())
	}
}

// assertNoDatabaseSecret fails when text holds the connection string or its
// password, and names only the label. A CI password that is also a word the
// product prints, such as the fixture's "conveyor", can't be told apart in text,
// so the check keeps the connection string for it.
func assertNoDatabaseSecret(t *testing.T, label, text, databaseURL string) {
	t.Helper()
	for _, value := range databaseURLSecrets(databaseURL) {
		if value != "" && !strings.Contains("conveyor init conveyord", value) && strings.Contains(text, value) {
			t.Errorf("%s holds the database URL or its password", label)
		}
	}
}

// assertGeneratedConfigHoldsBackendOnly reads a file init published on a durable
// backend (req-security-boundaries REQ-2; component-identity-membership invariant 14).
func assertGeneratedConfigHoldsBackendOnly(t *testing.T, path, backendName, databaseURL string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertDatabaseNodeIsBackendOnly(t, data, backendName)
	assertNoDatabaseSecret(t, "generated config", string(data), databaseURL)
	loaded, err := config.LoadDeployment(path, nil)
	if err != nil {
		t.Fatal("generated config does not load")
	}
	if loaded.Database.Backend != backendName || loaded.Database.URL != os.Getenv("CONVEYOR_DATABASE_URL") {
		t.Fatalf("loaded backend=%q took the URL from the environment=%t", loaded.Database.Backend, loaded.Database.URL == os.Getenv("CONVEYOR_DATABASE_URL"))
	}
}

// assertInitRerunToleratesPaddedURL reruns over a generated file with spaces and
// a newline around the environment value. The file stays byte-equal.
func assertInitRerunToleratesPaddedURL(t *testing.T, databaseURL, path string, answers initAnswers) {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONVEYOR_DATABASE_URL", "  "+databaseURL+" \n")
	var output strings.Builder
	err = initializeDeployment(store.WithActor(t.Context(), store.SystemActor()), &output, path, answers)
	t.Setenv("CONVEYOR_DATABASE_URL", databaseURL)
	if err != nil {
		t.Fatal("rerun with a padded environment value was refused: ", err)
	}
	assertNoDatabaseSecret(t, "padded rerun output", output.String(), databaseURL)
	if signInTokenFromOutput(t, output.String()) == "" {
		t.Fatal("padded rerun printed no sign-in link")
	}
	if after, readErr := os.ReadFile(path); readErr != nil || string(after) != string(before) {
		t.Fatalf("padded rerun rewrote the config (read err %v)", readErr)
	}
}

func assertLegacyURLNotice(t *testing.T, output, path, databaseURL string) {
	t.Helper()
	if !strings.Contains(output, "Notice: "+path+" holds a database URL in database.url") {
		t.Fatal("legacy rerun printed no notice that the file holds a URL")
	}
	assertNoDatabaseSecret(t, "legacy rerun output", output, databaseURL)
}

// assertInitCollisionsRefused runs `conveyor init` as a process against a store
// that already holds answers' workspace. priorToken is the store's latest
// unredeemed sign-in link. A new link invalidates it, so redeeming it after the
// refusals proves they issued none (req-deployment-and-releases REQ-2).
func assertInitCollisionsRefused(t *testing.T, databaseURL, backendName string, answers initAnswers, priorToken string) {
	t.Helper()
	ctx := store.WithActor(t.Context(), store.SystemActor())
	st, err := backend.Open(ctx, config.DatabaseForURL(databaseURL))
	if err != nil {
		t.Fatalf("open %s store: %v", backendName, scrubInitError(err, databaseURL, ""))
	}
	defer st.Close()
	before, err := st.ListWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	takenName, foldedName, otherName := answers, answers, answers
	takenName.WorkspaceID = "collision-new-id"
	foldedName.WorkspaceID, foldedName.WorkspaceName = "collision-folded-id", strings.ToUpper(answers.WorkspaceName)
	otherName.WorkspaceName = answers.WorkspaceName + " Renamed"
	for _, tc := range []struct {
		name    string
		answers initAnswers
		want    string
	}{
		{"workspace_collision/taken_name_with_new_ID", takenName, "is taken by another workspace"},
		{"workspace_collision/taken_name_in_another_case_with_new_ID", foldedName, "is taken by another workspace"},
		{"workspace_collision/same_ID_with_different_name", otherName, "already exists with the name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			status, stdout, stderr := runInitProcess(t, filepath.Join(directory, "conveyor.yaml"), tc.answers)
			assertNoDatabaseSecret(t, "collision stdout", stdout, databaseURL)
			assertNoDatabaseSecret(t, "collision stderr", stderr, databaseURL)
			if status == 0 {
				t.Fatal("collision exited zero")
			}
			if clean := scrubInitError(errors.New(stderr), databaseURL, os.Getenv("CONVEYOR_API_TOKEN")).Error(); !strings.Contains(clean, tc.want) {
				t.Fatalf("stderr lacks %q: %s", tc.want, clean)
			}
			if strings.Contains(stdout, "Initialized") || strings.Contains(stdout, "already initialized") || strings.Contains(stdout, "Sign-in") || signInTokenFromOutput(t, stdout) != "" {
				t.Fatal("collision reported success or printed a sign-in link")
			}
			assertOnlyDirectoryEntries(t, directory)
		})
	}
	t.Run("workspace_collision/created_no_workspace_and_no_sign_in_link", func(t *testing.T) {
		after, listErr := st.ListWorkspaces(ctx)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(after) != len(before) {
			t.Fatalf("workspaces went from %d to %d", len(before), len(after))
		}
		if _, _, redeemErr := st.RedeemSignInLink(ctx, priorToken); redeemErr != nil {
			t.Fatalf("a refusal invalidated the prior sign-in link: %v", redeemErr)
		}
	})
	t.Run("workspace_collision/same_ID_and_name_stays_safe", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, "conveyor.yaml")
		status, stdout, stderr := runInitProcess(t, path, answers)
		assertNoDatabaseSecret(t, "safe rerun stdout", stdout, databaseURL)
		assertNoDatabaseSecret(t, "safe rerun stderr", stderr, databaseURL)
		if status != 0 {
			t.Fatalf("safe rerun exited %d: %s", status, scrubInitError(errors.New(stderr), databaseURL, os.Getenv("CONVEYOR_API_TOKEN")))
		}
		if !strings.Contains(stdout, "already initialized; issuing a fresh") || signInTokenFromOutput(t, stdout) == "" {
			t.Fatal("safe rerun did not report the completed deployment and a fresh link")
		}
		assertGeneratedConfigHoldsBackendOnly(t, path, backendName, databaseURL)
		assertOnlyDirectoryEntries(t, directory, "conveyor.yaml")
	})
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
