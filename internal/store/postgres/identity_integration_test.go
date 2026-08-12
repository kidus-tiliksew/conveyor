package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func TestIdentityBootstrapAndPersonalAccessTokenLifecycleIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	identity := config.FirstOperatorIdentity{OrganizationName: "Example Org", Email: "owner@example.test", DisplayName: "Example Owner"}
	legacy := "legacy-token-value"
	seeded, err := st.BootstrapIdentity(t.Context(), identity, legacy)
	if err != nil || !seeded {
		t.Fatalf("bootstrap seeded=%t err=%v", seeded, err)
	}
	var orgCount, userCount, tokenCount int
	if err := st.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM orgs), (SELECT count(*) FROM users), (SELECT count(*) FROM user_tokens)`).Scan(&orgCount, &userCount, &tokenCount); err != nil {
		t.Fatal(err)
	}
	if orgCount != 1 || userCount != 1 || tokenCount != 1 {
		t.Fatalf("bootstrap counts org=%d user=%d token=%d, want 1/1/1", orgCount, userCount, tokenCount)
	}
	principal, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil || principal.Email != identity.Email || principal.Status != "active" {
		t.Fatalf("legacy verification principal=%+v err=%v", principal, err)
	}
	workspace := "identity-rotation-" + core.NewTaskID()
	if seededWorkspace, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(t.Context(), workspace), isolationConfig(workspace)); err != nil || !seededWorkspace {
		t.Fatalf("bootstrap rotation workspace seeded=%t err=%v", seededWorkspace, err)
	}
	var storedHash []byte
	if err := st.pool.QueryRow(t.Context(), `SELECT token_hash FROM user_tokens WHERE label='legacy API token'`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte(legacy))
	if string(storedHash) != string(wantHash[:]) || string(storedHash) == legacy {
		t.Fatal("legacy token was not stored exclusively as its SHA-256 hash")
	}

	seeded, err = st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{OrganizationName: "Replacement", Email: "other@example.test", DisplayName: "Other"}, "replacement-token")
	if err != nil || !seeded {
		t.Fatalf("rotation bootstrap changed=%t err=%v, want remap", seeded, err)
	}
	var orgName, email string
	if err := st.pool.QueryRow(t.Context(), `SELECT name FROM orgs`).Scan(&orgName); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT email FROM users`).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if orgName != identity.OrganizationName || email != identity.Email {
		t.Fatalf("restart overwrote identity: org=%q email=%q", orgName, email)
	}
	if _, err := st.VerifyPersonalAccessToken(t.Context(), legacy); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("old token verification err=%v, want invalid", err)
	}
	if rotated, err := st.VerifyPersonalAccessToken(t.Context(), "replacement-token"); err != nil || rotated.ID != principal.ID {
		t.Fatalf("replacement token principal=%+v err=%v", rotated, err)
	}
	var rotationEvents int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE workspace_id=$1 AND kind='identity.legacy_token_rotated'
		AND payload_json ? 'credential_id' AND payload_json::text NOT LIKE '%replacement-token%'`, workspace).Scan(&rotationEvents); err != nil || rotationEvents != 1 {
		t.Fatalf("rotation audit count=%d err=%v", rotationEvents, err)
	}
	seeded, err = st.BootstrapIdentity(t.Context(), identity, "replacement-token")
	if err != nil || seeded {
		t.Fatalf("healthy restart bootstrap changed=%t err=%v, want no-op", seeded, err)
	}

	issued, err := st.IssuePersonalAccessToken(t.Context(), principal.ID, "CLI")
	if err != nil {
		t.Fatal(err)
	}
	if issued.Value == "" || !strings.HasPrefix(issued.Value, "cv_pat_") || strings.Count(issued.Value, ".") == 2 {
		t.Fatal("issued token does not have the opaque non-JWT format")
	}
	if verified, err := st.VerifyPersonalAccessToken(t.Context(), issued.Value); err != nil || verified.ID != principal.ID {
		t.Fatalf("issued token verification principal=%+v err=%v", verified, err)
	}
	agent, err := st.IssueAgentCredential(t.Context(), principal.ID, "Codex")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := st.VerifyCredential(t.Context(), agent.Value)
	if err != nil || resolved.ID != agent.ID || resolved.OwnerUserID != principal.ID || resolved.Kind != core.CredentialAgent || resolved.Scope != core.CredentialScopeUser {
		t.Fatalf("resolved agent credential=%+v err=%v", resolved, err)
	}
	if _, err = st.VerifyPersonalAccessToken(t.Context(), agent.Value); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("agent credential authenticated as PAT: %v", err)
	}
	if _, err = st.RevokePersonalAccessToken(t.Context(), agent.ID); err == nil {
		t.Fatal("personal-token revocation accepted an agent credential")
	}
	if resolved, err = st.VerifyCredential(t.Context(), agent.Value); err != nil || resolved.Kind != core.CredentialAgent {
		t.Fatalf("agent credential changed by PAT revocation: credential=%+v err=%v", resolved, err)
	}
	if revoked, err := st.RevokePersonalAccessToken(t.Context(), issued.ID); err != nil || revoked.RevokedAt == nil {
		t.Fatalf("revoke token=%+v err=%v", revoked, err)
	}
	if _, err := st.VerifyPersonalAccessToken(t.Context(), issued.Value); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("revoked verification err=%v, want invalid", err)
	}

	second, err := st.IssuePersonalAccessToken(t.Context(), principal.ID, "second")
	if err != nil {
		t.Fatal(err)
	}
	if user, err := st.DeactivateIdentityUser(t.Context(), principal.ID); err != nil || user.Status != "deactivated" {
		t.Fatalf("deactivate user=%+v err=%v", user, err)
	}
	if _, err := st.VerifyPersonalAccessToken(t.Context(), second.Value); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("deactivated-user verification err=%v, want invalid", err)
	}
	if _, err := st.IssuePersonalAccessToken(t.Context(), principal.ID, "forbidden"); err == nil {
		t.Fatal("issued token for deactivated user")
	}
}

func TestHTTPMutationDerivesLegacyUserAndRejectsAgentCredentialIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "legacy-http-token"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{OrganizationName: "HTTP Org", Email: "owner@example.test", DisplayName: "Owner"}, legacy); err != nil {
		t.Fatal(err)
	}
	principal, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.IssueAgentCredential(t.Context(), principal.ID, "integration agent")
	if err != nil {
		t.Fatal(err)
	}
	workspace := "identity-http-" + core.NewTaskID()
	operatorCtx := store.WithActor(t.Context(), store.Actor{ID: store.UserActorID(principal.ID), Role: core.ActorUser})
	if _, err = st.CreateWorkspace(operatorCtx, workspace, workspace, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(operatorCtx, workspace)
	taskID := "identity-event-" + core.NewTaskID()
	if err = st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Source: "test", Title: "Credential actor", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	server := httpapi.NewServer(st)
	server.Workspace = workspace
	handler := server.Handler()
	call := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/close?workspace_id="+workspace, strings.NewReader(`{"reason":"integration"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Conveyor-Actor", "attacker")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := call(agent.Value); response.Code != http.StatusUnauthorized {
		t.Fatalf("agent mutation status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(legacy); response.Code != http.StatusAccepted {
		t.Fatalf("legacy mutation status=%d body=%s", response.Code, response.Body.String())
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "task.cancelled" {
			found = event.ActorID == store.UserActorID(principal.ID) && event.ActorRole == core.ActorUser
			if strings.Contains(string(event.Payload), legacy) || strings.Contains(string(event.Payload), agent.Value) || strings.Contains(string(event.Payload), "attacker") {
				t.Fatalf("credential or asserted actor leaked into event: %s", event.Payload)
			}
		}
	}
	if !found {
		t.Fatalf("credential-derived user actor missing from events: %+v", events)
	}
}

func TestIdentityBootstrapConcurrentStartsConvergeIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	identity := config.FirstOperatorIdentity{OrganizationName: "Concurrent Org", Email: "owner@example.test", DisplayName: "Owner"}
	var wait sync.WaitGroup
	wait.Add(2)
	results := make(chan bool, 2)
	errors := make(chan error, 2)
	for range 2 {
		go func() {
			defer wait.Done()
			seeded, err := st.BootstrapIdentity(t.Context(), identity, "shared-legacy-token")
			results <- seeded
			errors <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errors)
	seededCount := 0
	for seeded := range results {
		if seeded {
			seededCount++
		}
	}
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if seededCount != 1 {
		t.Fatalf("concurrent bootstrap seeded count=%d, want 1", seededCount)
	}
	var users, tokens int
	if err := st.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM user_tokens)`).Scan(&users, &tokens); err != nil {
		t.Fatal(err)
	}
	if users != 1 || tokens != 1 {
		t.Fatalf("concurrent bootstrap rows users=%d tokens=%d, want 1/1", users, tokens)
	}
}

func TestIdentityMigrationsUpgradeExistingWorkspaceIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 80)
	workspace := "identity-upgrade-" + core.NewTaskID()
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO workspaces (id,name,config_yaml) VALUES ($1,$1,'')`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO users(id,identity_provider_ref,role) VALUES('phase2-user','legacy-subject','operator')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateControlPlaneToVersion(t.Context(), st.pool, 0); err != nil {
		t.Fatalf("upgrade from migration 080: %v", err)
	}
	identity := config.FirstOperatorIdentity{OrganizationName: "Upgraded Org", Email: "upgrade-owner@example.test", DisplayName: "Upgrade Owner"}
	if seeded, err := st.BootstrapIdentity(t.Context(), identity, "upgrade-legacy-token"); err != nil || !seeded {
		t.Fatalf("upgrade bootstrap seeded=%t err=%v", seeded, err)
	}
	principal, err := st.VerifyPersonalAccessToken(t.Context(), "upgrade-legacy-token")
	if err != nil || principal.Email != identity.Email {
		t.Fatalf("upgrade legacy token principal=%+v err=%v", principal, err)
	}
	var legacyUsers, orgs int
	if err := st.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM users WHERE email LIKE '%@legacy.invalid'),
		(SELECT count(*) FROM orgs)`).Scan(&legacyUsers, &orgs); err != nil || legacyUsers != 1 || orgs != 1 {
		t.Fatalf("upgrade preserved users=%d orgs=%d err=%v", legacyUsers, orgs, err)
	}
	if seeded, err := st.BootstrapIdentity(t.Context(), identity, "upgrade-legacy-token"); err != nil || seeded {
		t.Fatalf("upgrade healthy restart seeded=%t err=%v", seeded, err)
	}
	var orgID string
	if err := st.pool.QueryRow(t.Context(), `SELECT org_id FROM workspaces WHERE id=$1`, workspace).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if orgID != "deployment" {
		t.Fatalf("upgraded workspace org_id=%q, want deployment", orgID)
	}
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO orgs (id,name) VALUES ('second','Second')`); err == nil {
		t.Fatal("singleton organization constraint accepted a second row")
	}
	if _, err := st.pool.Exec(t.Context(), `DELETE FROM orgs WHERE id='deployment'`); err == nil {
		t.Fatal("singleton organization could be deleted")
	}
	if _, err := st.pool.Exec(t.Context(), `UPDATE workspaces SET org_id='missing' WHERE id=$1`, workspace); err == nil {
		t.Fatal("workspace organization foreign key accepted a missing organization")
	}
}

func newIdentityIntegrationStore(t *testing.T, maxVersion int) *Store {
	t.Helper()
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "identity_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrateControlPlaneToVersion(t.Context(), pool, maxVersion); err != nil {
		t.Fatalf("migrate identity fixture to %d: %v", maxVersion, err)
	}
	return &Store{pool: pool, queries: db.New(pool)}
}
