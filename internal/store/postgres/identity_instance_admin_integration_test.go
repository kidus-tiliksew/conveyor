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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// Instance administration and membership preservation on PostgreSQL
// (req-accounts-and-membership AC-1.3, AC-4.3; DEC-63;
// component-identity-membership, Verification). Every case uses its own
// schema through newIdentityIntegrationStore.

var instanceOwnerIdentity = config.FirstOperatorIdentity{OrganizationName: "Instance Org", Email: "instance-owner@example.test", DisplayName: "Instance Owner"}

func instanceUserContext(base context.Context, userID string) context.Context {
	ctx := store.WithCredential(base, core.AuthenticatedCredential{ID: "fixture-" + userID, OwnerUserID: userID, Kind: core.CredentialUser, Scope: core.CredentialScopeUser})
	return store.WithActor(ctx, store.Actor{ID: store.UserActorID(userID), Role: core.ActorUser})
}

func instanceSystemContext(base context.Context) context.Context {
	return store.WithActor(base, store.Actor{ID: "system", Role: core.ActorSystem})
}

func instanceRole(t *testing.T, st *Store, userID, workspaceID string) string {
	t.Helper()
	var role string
	err := st.pool.QueryRow(t.Context(), `SELECT role FROM workspace_role_bindings WHERE user_id=$1 AND workspace_id=$2`, userID, workspaceID).Scan(&role)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return role
}

func countRows(t *testing.T, st *Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := st.pool.QueryRow(t.Context(), query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestInstanceAdministrationHTTPIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	ctx := instanceSystemContext(t.Context())
	if _, err := st.BootstrapIdentity(ctx, instanceOwnerIdentity, "instance-owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(ctx, "instance-owner-token")
	if err != nil {
		t.Fatal(err)
	}
	workspace := "instance-" + core.NewTaskID()
	if _, err := st.CreateWorkspace(instanceSystemContext(store.WithWorkspace(ctx, workspace)), workspace, "Instance Workspace", isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	if countRows(t, st, `SELECT count(*) FROM workspace_role_bindings WHERE user_id=$1`, owner.ID) != 0 {
		t.Fatal("owner must start without bindings")
	}
	nonowner, err := st.ProvisionIdentityUser(ctx, "instance-operator@example.test", "Instance Operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GrantWorkspaceRole(instanceUserContext(ctx, owner.ID), nonowner.Email, workspace, core.WorkspaceRoleOperator); err != nil {
		t.Fatal(err)
	}
	nonownerPAT, err := st.IssuePersonalAccessToken(ctx, nonowner.ID, "nonowner")
	if err != nil {
		t.Fatal(err)
	}
	ownerPAT, err := st.IssuePersonalAccessToken(ctx, owner.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	server := httpapi.NewServer(st)
	server.Workspaces, server.Deployment = st, &config.Config{}
	handler := server.Handler()
	call := func(token, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	notFound := "{\"error\":\"workspace_not_found\",\"message\":\"workspace not found\"}\n"
	for _, request := range []struct{ path, body string }{
		{"/v1/users", `{"email":"refused@example.test","display_name":"Refused"}`},
		{"/v1/users", `{"email":"instance-owner@example.test","display_name":"Existing"}`},
		{"/v1/workspaces", `{"id":"refused-workspace","name":"Refused"}`},
		{"/v1/workspaces", `{"id":"` + workspace + `","name":"Collision"}`},
	} {
		if response := call(nonownerPAT.Value, request.path, request.body); response.Code != http.StatusNotFound || response.Body.String() != notFound {
			t.Fatalf("nonowner operator %s %s status=%d body=%q", request.path, request.body, response.Code, response.Body.String())
		}
	}
	if countRows(t, st, `SELECT count(*) FROM users WHERE email='refused@example.test'`) != 0 || countRows(t, st, `SELECT count(*) FROM workspaces WHERE id='refused-workspace'`) != 0 {
		t.Fatal("refused requests wrote rows")
	}

	if _, err := st.pool.Exec(ctx, `INSERT INTO users(id,email,display_name,status) VALUES('usr_inactive_instance','inactive-instance@example.test','Inactive','deactivated')`); err != nil {
		t.Fatal(err)
	}
	ack := "{\"accepted\":true}\n"
	for _, body := range []string{
		`{"email":"fresh-instance@example.test","display_name":"Fresh"}`,
		`{"email":"INSTANCE-OPERATOR@example.test","display_name":"Renamed"}`,
		`{"email":"inactive-instance@example.test","display_name":"Reactivate"}`,
	} {
		if response := call(ownerPAT.Value, "/v1/users", body); response.Code != http.StatusOK || response.Body.String() != ack {
			t.Fatalf("owner provisioning %s status=%d body=%q", body, response.Code, response.Body.String())
		}
	}
	var name, status string
	if err := st.pool.QueryRow(ctx, `SELECT display_name,status FROM users WHERE id=$1`, nonowner.ID).Scan(&name, &status); err != nil || name != "Instance Operator" || status != "active" {
		t.Fatalf("existing account changed name=%q status=%q err=%v", name, status, err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT status FROM users WHERE id='usr_inactive_instance'`).Scan(&status); err != nil || status != "deactivated" {
		t.Fatalf("deactivated account status=%q err=%v", status, err)
	}

	created := "created-" + core.NewTaskID()
	if response := call(ownerPAT.Value, "/v1/workspaces", `{"id":"`+created+`","name":"Created Workspace"}`); response.Code != http.StatusCreated {
		t.Fatalf("owner creation status=%d body=%s", response.Code, response.Body.String())
	}
	if role := instanceRole(t, st, owner.ID, created); role != "operator" {
		t.Fatalf("creator binding=%q", role)
	}
	var actorID string
	if err := st.pool.QueryRow(ctx, `SELECT actor_id FROM events WHERE workspace_id=$1 AND kind='workspace.created'`, created).Scan(&actorID); err != nil || actorID != store.UserActorID(owner.ID) {
		t.Fatalf("creation actor=%q err=%v", actorID, err)
	}
	conflict := "{\"error\":\"workspace_conflict\",\"message\":\"workspace could not be created\"}\n"
	for _, body := range []string{`{"id":"` + created + `","name":"Other"}`, `{"id":"other-` + core.NewTaskID() + `","name":"  CREATED WORKSPACE "}`, `{"id":"` + workspace + `","name":"Hidden"}`} {
		if response := call(ownerPAT.Value, "/v1/workspaces", body); response.Code != http.StatusConflict || response.Body.String() != conflict {
			t.Fatalf("collision %s status=%d body=%q", body, response.Code, response.Body.String())
		}
	}
}

// reopenIdentityStore opens a new pool on the same schema, as a restarted
// daemon would.
func reopenIdentityStore(t *testing.T, st *Store) *Store {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(context.Background(), st.pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return newStore(pool)
}

func TestBootstrapMembershipPersistenceIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	ctx := instanceSystemContext(t.Context())
	if _, err := st.BootstrapIdentity(ctx, instanceOwnerIdentity, "persist-one"); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(ctx, "persist-one")
	if err != nil {
		t.Fatal(err)
	}
	first := "persist-first-" + core.NewTaskID()
	if seeded, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(ctx, first), isolationConfig(first)); err != nil || !seeded {
		t.Fatalf("first workspace seeded=%t err=%v", seeded, err)
	}
	second, err := st.ProvisionIdentityUser(ctx, "persist-second@example.test", "Second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GrantWorkspaceRole(instanceUserContext(ctx, owner.ID), second.Email, first, core.WorkspaceRoleOperator); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GrantWorkspaceRole(instanceUserContext(ctx, second.ID), owner.Email, first, core.WorkspaceRoleViewer); err != nil {
		t.Fatal(err)
	}
	excluded := "persist-excluded-" + core.NewTaskID()
	if _, err := st.CreateWorkspace(instanceSystemContext(store.WithWorkspace(ctx, excluded)), excluded, excluded, isolationConfig(excluded)); err != nil {
		t.Fatal(err)
	}
	revoked := "persist-revoked-" + core.NewTaskID()
	if _, err := st.CreateWorkspace(instanceUserContext(store.WithWorkspace(ctx, revoked), owner.ID), revoked, revoked, isolationConfig(revoked)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GrantWorkspaceRole(instanceUserContext(ctx, owner.ID), second.Email, revoked, core.WorkspaceRoleOperator); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeWorkspaceRole(instanceUserContext(store.WithWorkspace(ctx, revoked), second.ID), owner.ID, revoked); err != nil {
		t.Fatal(err)
	}
	check := func(step string, current *Store) {
		t.Helper()
		if role := instanceRole(t, current, owner.ID, first); role != "viewer" {
			t.Fatalf("%s: demoted owner role=%q", step, role)
		}
		for _, workspace := range []string{excluded, revoked} {
			if role := instanceRole(t, current, owner.ID, workspace); role != "" {
				t.Fatalf("%s: owner restored as %s in %s", step, role, workspace)
			}
		}
		if healed := countRows(t, current, `SELECT count(*) FROM deployment_events WHERE kind='identity.legacy_bindings_healed'`); healed != 0 {
			t.Fatalf("%s: healing events=%d", step, healed)
		}
	}
	check("before restart", st)
	reopened := reopenIdentityStore(t, st)
	if changed, err := reopened.BootstrapIdentity(ctx, instanceOwnerIdentity, "persist-one"); err != nil || changed {
		t.Fatalf("unchanged restart changed=%t err=%v", changed, err)
	}
	if _, err := reopened.BootstrapWorkspaceConfig(store.WithWorkspace(ctx, first), isolationConfig(first)); err != nil {
		t.Fatal(err)
	}
	check("unchanged restart", reopened)
	rotatedStore := reopenIdentityStore(t, reopened)
	if changed, err := rotatedStore.BootstrapIdentity(ctx, instanceOwnerIdentity, "persist-two"); err != nil || !changed {
		t.Fatalf("rotated restart changed=%t err=%v", changed, err)
	}
	check("rotated restart", rotatedStore)
	if current, err := rotatedStore.VerifyPersonalAccessToken(ctx, "persist-two"); err != nil || current.ID != owner.ID {
		t.Fatalf("rotation owner=%+v err=%v", current, err)
	}
	// Workspace events keep their scope and credential-derived or system
	// actors: the demotion by the second operator, the system creation.
	var demotionActor, creationActor string
	if err := rotatedStore.pool.QueryRow(ctx, `SELECT actor_id FROM events WHERE workspace_id=$1 AND kind='workspace.membership_granted' AND payload_json->>'role'='viewer'`, first).Scan(&demotionActor); err != nil || demotionActor != store.UserActorID(second.ID) {
		t.Fatalf("demotion actor=%q err=%v", demotionActor, err)
	}
	if err := rotatedStore.pool.QueryRow(ctx, `SELECT actor_id FROM events WHERE workspace_id=$1 AND kind='workspace.created'`, excluded).Scan(&creationActor); err != nil || creationActor != "system" {
		t.Fatalf("system creation actor=%q err=%v", creationActor, err)
	}
	if rotations := countRows(t, rotatedStore, `SELECT count(*) FROM deployment_events WHERE kind='identity.legacy_token_rotated' AND actor_id='system' AND actor_role='system'`); rotations != 1 {
		t.Fatalf("rotation audit rows=%d", rotations)
	}
}

func TestBootstrapMembershipSerializationIntegration(t *testing.T) {
	t.Run("ChangeCommitsWhileBootstrapHolds", func(t *testing.T) {
		st := newIdentityIntegrationStore(t, 0)
		ctx := instanceSystemContext(t.Context())
		if _, err := st.BootstrapIdentity(ctx, instanceOwnerIdentity, "serial-zero"); err != nil {
			t.Fatal(err)
		}
		owner, err := st.VerifyPersonalAccessToken(ctx, "serial-zero")
		if err != nil {
			t.Fatal(err)
		}
		workspace := "serial-" + core.NewTaskID()
		if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(ctx, workspace), isolationConfig(workspace)); err != nil {
			t.Fatal(err)
		}
		second, err := st.ProvisionIdentityUser(ctx, "serial-second@example.test", "Second")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.GrantWorkspaceRole(instanceUserContext(ctx, owner.ID), second.Email, workspace, core.WorkspaceRoleOperator); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			stage, token, want string
			change             func() error
		}{
			{store.IdentityHookBootstrapLocked, "serial-one", "viewer", func() error {
				_, err := st.GrantWorkspaceRole(instanceUserContext(ctx, second.ID), owner.Email, workspace, core.WorkspaceRoleViewer)
				return err
			}},
			{store.IdentityHookBootstrapBeforeCommit, "serial-two", "", func() error {
				return st.RevokeWorkspaceRole(instanceUserContext(store.WithWorkspace(ctx, workspace), second.ID), owner.ID, workspace)
			}},
		} {
			// Membership writes do not take the bootstrap lock, so the hook
			// waits until the change has committed before bootstrap resumes.
			hooked := store.WithIdentityTestHook(ctx, func(stage string) error {
				if stage == test.stage {
					return test.change()
				}
				return nil
			})
			if changed, err := st.BootstrapIdentity(hooked, instanceOwnerIdentity, test.token); err != nil || !changed {
				t.Fatalf("%s: bootstrap changed=%t err=%v", test.stage, changed, err)
			}
			if role := instanceRole(t, st, owner.ID, workspace); role != test.want {
				t.Fatalf("%s: owner role=%q want %q", test.stage, role, test.want)
			}
			if _, err := st.BootstrapIdentity(ctx, instanceOwnerIdentity, test.token); err != nil {
				t.Fatal(err)
			}
			if role := instanceRole(t, st, owner.ID, workspace); role != test.want {
				t.Fatalf("%s: later restart role=%q want %q", test.stage, role, test.want)
			}
		}
	})
	t.Run("ConcurrentFirstStarts", func(t *testing.T) {
		st := newIdentityIntegrationStore(t, 0)
		start := make(chan struct{})
		var wait sync.WaitGroup
		errs := make(chan error, 2)
		for range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, err := st.BootstrapIdentity(instanceSystemContext(t.Context()), instanceOwnerIdentity, "concurrent-first")
				errs <- err
			}()
		}
		close(start)
		wait.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if users, markers := countRows(t, st, `SELECT count(*) FROM users`), countRows(t, st, `SELECT count(*) FROM user_tokens WHERE deployment_credential`); users != 1 || markers != 1 {
			t.Fatalf("concurrent first starts users=%d markers=%d", users, markers)
		}
	})
	t.Run("InjectedFailureRollsBackRotation", func(t *testing.T) {
		st := newIdentityIntegrationStore(t, 0)
		ctx := instanceSystemContext(t.Context())
		if _, err := st.BootstrapIdentity(ctx, instanceOwnerIdentity, "rollback-one"); err != nil {
			t.Fatal(err)
		}
		before := countRows(t, st, `SELECT count(*) FROM deployment_events`)
		injected := errors.New("injected before commit")
		failing := store.WithIdentityTestHook(ctx, func(stage string) error {
			if stage == store.IdentityHookBootstrapBeforeCommit {
				return injected
			}
			return nil
		})
		if _, err := st.BootstrapIdentity(failing, instanceOwnerIdentity, "rollback-two"); !errors.Is(err, injected) {
			t.Fatalf("injected err=%v", err)
		}
		var hash []byte
		if err := st.pool.QueryRow(ctx, `SELECT token_hash FROM user_tokens WHERE deployment_credential`).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256([]byte("rollback-one"))
		if string(hash) != string(want[:]) {
			t.Fatal("rolled-back rotation changed the credential hash")
		}
		if after := countRows(t, st, `SELECT count(*) FROM deployment_events`); after != before {
			t.Fatalf("rolled-back rotation wrote events before=%d after=%d", before, after)
		}
	})
}

func TestFreshDeploymentBootstrapIntegration(t *testing.T) {
	storetest.RunFreshDeploymentBootstrap(t, storetest.FreshDeployment{
		Open: func(t *testing.T) store.Backend { return newIdentityIntegrationStore(t, 0) },
		Deactivate: func(t *testing.T, backend store.Backend, userID string) {
			if _, err := backend.(*Store).pool.Exec(t.Context(), `UPDATE users SET status='deactivated' WHERE id=$1`, userID); err != nil {
				t.Fatal(err)
			}
		},
		DeploymentEvents: func(t *testing.T, backend store.Backend, kind string) int {
			return countRows(t, backend.(*Store), `SELECT count(*) FROM deployment_events WHERE kind=$1`, kind)
		},
	})
}
