package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestVolatileCapabilitiesPreserveCredentialAndMembershipBoundaries(t *testing.T) {
	st := NewVolatileBackend()
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "one")
	first := config.FirstOperatorIdentity{OrganizationName: "Test", Email: "owner@example.test", DisplayName: "Owner"}
	if _, err := st.BootstrapIdentity(ctx, first, "fixture-token"); err != nil {
		t.Fatal(err)
	}
	if seeded, err := st.BootstrapIdentity(ctx, first, "fixture-token"); err != nil || seeded {
		t.Fatalf("repeat bootstrap: %v %v", seeded, err)
	}
	owner, err := st.VerifyPersonalAccessToken(ctx, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithCredential(ctx, core.AuthenticatedCredential{ID: "bootstrap", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	ctx = WithActor(ctx, Actor{ID: UserActorID(owner.ID), Role: core.ActorUser})
	cfg := &config.Config{Workspace: "one", Repos: []config.Repo{{Name: "repo", Base: "main"}}}
	if _, err := st.CreateWorkspace(ctx, "one", "One", cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeWorkspaceRole(ctx, owner.ID, "one"); !errors.Is(err, ErrLastWorkspaceOperator) {
		t.Fatalf("sole operator revoked: %v", err)
	}
	if _, err := st.GetCallerIdentity(ctx, owner.ID, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbound identity exposed: %v", err)
	}
	if _, err := st.GrantWorkspaceRole(ctx, "invitee@example.test", "one", core.WorkspaceRoleExecutor); err != nil {
		t.Fatal(err)
	}
	link, err := st.IssueSignInLink(ctx, "invitee@example.test")
	if err != nil {
		t.Fatal(err)
	}
	session, invitee, err := st.RedeemSignInLink(ctx, link.Value)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := st.AuthorizeWorkspace(ctx, invitee.ID, "one", core.CapabilityClaimWork); err != nil || !allowed {
		t.Fatalf("invitation binding: %v %v", allowed, err)
	}
	if err := st.SetOwnPassword(ctx, invitee.ID, session.ID, "", "a long fixture password"); err != nil {
		t.Fatal(err)
	}
	passwordSession, _, err := st.SignInWithPassword(ctx, invitee.Email, "a long fixture password")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetOwnPassword(ctx, invitee.ID, passwordSession.ID, "wrong", "a changed fixture password"); !errors.Is(err, ErrInvalidCurrentPassword) {
		t.Fatalf("password proof bypassed: %v", err)
	}
	token, err := st.IssueOwnPersonalAccessToken(ctx, invitee.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeOwnPersonalAccessToken(ctx, owner.ID, token.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner's token was addressable: %v", err)
	}
	agent, err := st.IssueAgentCredential(ctx, owner.ID, "test agent")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := st.VerifyCredential(ctx, agent.Value)
	if err != nil || auth.Scope != core.CredentialScopeUser || auth.Kind != core.CredentialAgent {
		t.Fatalf("agent scope: %+v %v", auth, err)
	}
	if err := st.RevokeWorkspaceRole(ctx, invitee.ID, "one"); err != nil {
		t.Fatal(err)
	}
	if allowed, err := st.AuthorizeWorkspace(ctx, invitee.ID, "one", core.CapabilityClaimWork); err != nil || allowed {
		t.Fatalf("revoked membership: %v %v", allowed, err)
	}
}

func TestMemoryConstructorRetainsExistingCapabilities(t *testing.T) {
	if _, ok := NewMemory().(Backend); ok {
		t.Fatal("base memory fixtures unexpectedly expose deployment capabilities")
	}
}

func TestMemoryDispatchJobConflict(t *testing.T) {
	st := NewMemory()
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "test")
	if err := st.CreateTask(ctx, core.Task{ID: "task", Workspace: "test", Branch: "task-branch", State: core.TaskQueued}); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "job", TaskID: "task", State: core.JobRunning}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); !errors.Is(err, ErrDispatchJobConflict) {
		t.Fatalf("duplicate job: %v", err)
	}
}

// TestAppendEventRequiresActor proves the volatile backend refuses a write
// whose context carries no actor before it changes any state: events,
// lineage, tasks, and workers stay unchanged, and an actor named on the event
// envelope does not bypass the check (component-persistence, Actor context).
func TestAppendEventRequiresActor(t *testing.T) {
	st := NewVolatileBackend()
	systemCtx := WithWorkspace(WithActor(t.Context(), SystemActor()), "one")
	if _, err := st.BootstrapWorkspaceConfig(systemCtx, &config.Config{Workspace: "one"}); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "task-actor", Workspace: "one", Title: "actor", State: core.TaskQueued, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(systemCtx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorker(systemCtx, core.Worker{ID: "worker-actor", Workspace: "one", OwnerUserID: "usr-1", Name: "w", CredentialHash: "hash", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	memory := st.(*volatileMemory)
	snapshot := func() (int, int, int) {
		memory.mu.RLock()
		defer memory.mu.RUnlock()
		events := 0
		for _, items := range memory.events {
			events += len(items)
		}
		return events, len(memory.lineage), len(memory.tasks)
	}
	beforeEvents, beforeLineage, beforeTasks := snapshot()
	bare := WithWorkspace(t.Context(), "one")
	partial := WithActor(bare, Actor{ID: "user:usr-1"})
	for name, ctx := range map[string]context.Context{"absent": bare, "partial": partial} {
		if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "test.event", ActorID: "user:usr-1", ActorRole: core.ActorUser}); !errors.Is(err, ErrMissingActor) {
			t.Fatalf("%s AppendEvent: %v", name, err)
		}
		if err := st.CreateTask(ctx, core.Task{ID: "task-refused-" + name, Workspace: "one", Title: "refused", State: core.TaskQueued, CreatedAt: time.Now().UTC()}); !errors.Is(err, ErrMissingActor) {
			t.Fatalf("%s CreateTask: %v", name, err)
		}
		if _, err := st.SetTaskHold(ctx, task.ID, true); !errors.Is(err, ErrMissingActor) {
			t.Fatalf("%s SetTaskHold: %v", name, err)
		}
		if err := st.RevokeWorker(ctx, "worker-actor"); !errors.Is(err, ErrMissingActor) {
			t.Fatalf("%s RevokeWorker: %v", name, err)
		}
	}
	if events, lineage, tasks := snapshot(); events != beforeEvents || lineage != beforeLineage || tasks != beforeTasks {
		t.Fatalf("refused writes changed state: events %d->%d lineage %d->%d tasks %d->%d", beforeEvents, events, beforeLineage, lineage, beforeTasks, tasks)
	}
	if got, err := st.GetTask(systemCtx, task.ID); err != nil || got.Hold {
		t.Fatalf("refused hold changed task: %+v %v", got, err)
	}
	workers, err := st.ListWorkers(systemCtx)
	if err != nil || len(workers) != 1 || !workers[0].RevokedAt.IsZero() {
		t.Fatalf("refused revocation changed worker: %+v %v", workers, err)
	}
	userCtx := WithActor(bare, Actor{ID: UserActorID("usr-1"), Role: core.ActorUser})
	if err := st.AppendEvent(userCtx, core.Event{TaskID: task.ID, Kind: "test.event"}); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(systemCtx, task.ID)
	if err != nil || len(events) == 0 || events[len(events)-1].ActorID != "user:usr-1" || events[len(events)-1].ActorRole != core.ActorUser {
		t.Fatalf("explicit actor attribution lost: %+v %v", events, err)
	}
}
