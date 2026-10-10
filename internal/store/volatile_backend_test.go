package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
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

// volatileEventSinks are the volatile ledger writers. Each records an event
// under the actor bound to the context it receives.
var volatileEventSinks = map[string]bool{
	"appendEventLocked": true, "recordEventLocked": true, "deploymentEventLocked": true,
	"workspaceEventLocked": true, "userEventLocked": true,
}

// volatileDerivedActorEntryPoints record events only under an actor the
// operation itself establishes from a verified credential or link, never the
// caller's context actor, so they need no context actor check.
var volatileDerivedActorEntryPoints = map[string]string{
	"RedeemSignInLink":             "records the redeemed user's actor (userEventLocked) and the invitation grant actor (recordEventLocked)",
	"SignInWithPassword":           "records the verified user's actor (userEventLocked)",
	"SetOwnPassword":               "records the session owner's actor (userEventLocked)",
	"RevokeDashboardSession":       "records the session owner's actor (userEventLocked)",
	"BootstrapIdentity":            "records SystemActor(\"system\") for legacy token lifecycle (recordEventLocked) and grant actors for redeemed invitations",
	"ProvisionIdentityUser":        "records the inviting or redeeming user's actor for redeemed invitations (recordEventLocked)",
	"IssuePersonalAccessToken":     "records the credential-derived user's actor (personalTokenEventLocked)",
	"IssueOwnPersonalAccessToken":  "delegates to IssuePersonalAccessToken",
	"RevokePersonalAccessToken":    "records the credential-derived user's actor (personalTokenEventLocked)",
	"RevokeOwnPersonalAccessToken": "records the credential-derived user's actor (personalTokenEventLocked)",
}

// contextActorWriters are the functions through which a volatile write records
// the caller's context actor. A derived-actor entry point must reach none of
// them; explicitActorWriters take the actor as an argument and are not
// traversed.
var (
	contextActorWriters  = map[string]bool{"appendEventLocked": true, "deploymentEventLocked": true, "workspaceEventLocked": true, "ActorFromContext": true}
	explicitActorWriters = map[string]bool{"recordEventLocked": true, "userEventLocked": true}
)

type volatileMutationBoundary struct {
	name, file      string
	guarded, exempt bool
	reason          string
}

// volatileEventMutationBoundaries parses the volatile backend's source and
// returns every exported memory or volatileMemory method that reaches a
// ledger writer, directly or through other package functions, with whether a
// RequireActor call precedes the method's first mutation boundary: a mutex
// acquisition, a lock helper, or a call into another event-writing function.
func volatileEventMutationBoundaries(t *testing.T) []volatileMutationBoundary {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	type function struct {
		decl    *ast.FuncDecl
		file    string
		callees map[string]bool
	}
	byName := map[string][]*function{}
	var all []*function
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if fd.Recv != nil {
				recv := fd.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); !ok || (id.Name != "memory" && id.Name != "volatileMemory") {
					continue
				}
			}
			f := &function{decl: fd, file: path, callees: map[string]bool{}}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					f.callees[calleeName(call)] = true
				}
				return true
			})
			byName[fd.Name.Name] = append(byName[fd.Name.Name], f)
			all = append(all, f)
		}
	}
	writes := map[string]bool{}
	for name := range volatileEventSinks {
		writes[name] = true
	}
	for changed := true; changed; {
		changed = false
		for _, f := range all {
			if writes[f.decl.Name.Name] {
				continue
			}
			for callee := range f.callees {
				if writes[callee] {
					writes[f.decl.Name.Name] = true
					changed = true
					break
				}
			}
		}
	}
	// reachesContextActor reports whether name reaches a context-actor writer
	// without passing through an explicit-actor writer.
	var reachesContextActor func(name string, seen map[string]bool) bool
	reachesContextActor = func(name string, seen map[string]bool) bool {
		if contextActorWriters[name] {
			return true
		}
		if explicitActorWriters[name] || seen[name] {
			return false
		}
		seen[name] = true
		for _, g := range byName[name] {
			for callee := range g.callees {
				if reachesContextActor(callee, seen) {
					return true
				}
			}
		}
		return false
	}
	var result []volatileMutationBoundary
	for _, f := range all {
		fd := f.decl
		if fd.Recv == nil || !fd.Name.IsExported() || !writes[fd.Name.Name] {
			continue
		}
		guard, boundary := token.NoPos, token.NoPos
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calleeName(call)
			switch {
			case name == "RequireActor":
				if guard == token.NoPos || call.Pos() < guard {
					guard = call.Pos()
				}
			case name == "Lock" || name == "RLock" || name == "lock" || strings.HasSuffix(name, "Lock") || (writes[name] && name != fd.Name.Name):
				if boundary == token.NoPos || call.Pos() < boundary {
					boundary = call.Pos()
				}
			}
			return true
		})
		reason, exempt := volatileDerivedActorEntryPoints[fd.Name.Name]
		if exempt {
			seen := map[string]bool{fd.Name.Name: true}
			for callee := range f.callees {
				if reachesContextActor(callee, seen) {
					exempt, reason = false, ""
					t.Errorf("%s: derived-actor entry point %s reaches a context-actor writer through %s", f.file, fd.Name.Name, callee)
				}
			}
		}
		result = append(result, volatileMutationBoundary{
			name: fd.Name.Name, file: f.file, exempt: exempt, reason: reason,
			guarded: guard != token.NoPos && (boundary == token.NoPos || guard < boundary),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].file+result[i].name < result[j].file+result[j].name })
	return result
}

func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name
	case *ast.Ident:
		return fun.Name
	case *ast.IndexExpr:
		if id, ok := fun.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// TestVolatileEventWritersCheckActorBeforeMutation is the per-boundary actor
// check: every exported volatile method that can record an event checks
// RequireActor before its first lock or delegated write, so a missing actor is
// refused before any projection changes. Methods that record only an actor
// they derive themselves are listed with their reason. The inventory prints
// with -v for the actor audit (component-persistence, Actor context).
func TestVolatileEventWritersCheckActorBeforeMutation(t *testing.T) {
	boundaries := volatileEventMutationBoundaries(t)
	if len(boundaries) < 100 {
		t.Fatalf("found only %d event-writing volatile entry points; the scan is broken", len(boundaries))
	}
	seenExempt := map[string]bool{}
	for _, b := range boundaries {
		switch {
		case b.exempt:
			seenExempt[b.name] = true
			t.Logf("derived-actor %s.%s: %s", strings.TrimSuffix(b.file, ".go"), b.name, b.reason)
		case b.guarded:
			t.Logf("guarded %s.%s", strings.TrimSuffix(b.file, ".go"), b.name)
		default:
			t.Errorf("%s: %s records an event without a RequireActor check before its first mutation boundary", b.file, b.name)
		}
	}
	for name := range volatileDerivedActorEntryPoints {
		if !seenExempt[name] {
			t.Errorf("derived-actor exemption %s names no event-writing entry point", name)
		}
	}
}

// volatileSnapshot captures every projection a refused write could touch.
type volatileSnapshot struct {
	events, lineage, sessions, orders, jobs, tasks string
}

func snapshotVolatile(t *testing.T, st Store) volatileSnapshot {
	t.Helper()
	m := st.(*memory)
	m.mu.RLock()
	defer m.mu.RUnlock()
	encode := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	lineage := make([]string, 0, len(m.lineage))
	for key := range m.lineage {
		lineage = append(lineage, fmt.Sprint(key))
	}
	sort.Strings(lineage)
	sessions := map[string]core.PlanningSession{}
	for key, session := range m.planningSessions {
		sessions[key.workspace+"/"+key.id] = session
	}
	return volatileSnapshot{
		events: encode(m.events), lineage: encode(lineage), sessions: encode(sessions),
		orders: encode(m.workOrders), jobs: encode(m.jobs), tasks: encode(m.tasks),
	}
}

// refusesWithoutPanic runs call and fails the test if it panics or does not
// return an error wrapping ErrMissingActor.
func refusesWithoutPanic(t *testing.T, name string, call func() error) {
	t.Helper()
	var recovered any
	var err error
	func() {
		defer func() { recovered = recover() }()
		err = call()
	}()
	if recovered != nil {
		t.Fatalf("%s panicked: %v", name, recovered)
	}
	if !errors.Is(err, ErrMissingActor) {
		t.Fatalf("%s error=%v, want ErrMissingActor", name, err)
	}
}

// TestAbandonPlanningSessionRequiresActorBeforeMutation reproduces review
// round 1: an actorless or partial-actor abandonment is refused with
// ErrMissingActor, without a panic, and leaves the session, events, and
// lineage unchanged; an explicit actor still abandons with attribution.
func TestAbandonPlanningSessionRequiresActorBeforeMutation(t *testing.T) {
	bare := WithWorkspace(context.Background(), "probe")
	explicit := WithActor(bare, SystemActor())
	st := NewMemory()
	session, err := st.CreatePlanningSession(explicit, core.PlanningSession{ID: "probe-session"})
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotVolatile(t, st)
	for name, ctx := range map[string]context.Context{"missing": bare, "partial": WithActor(bare, Actor{ID: "user:usr-1"})} {
		refusesWithoutPanic(t, name+" AbandonPlanningSession", func() error {
			_, err := st.AbandonPlanningSession(ctx, session.ID, "probe")
			return err
		})
		if after := snapshotVolatile(t, st); after != before {
			t.Fatalf("%s actor changed state: before=%+v after=%+v", name, before, after)
		}
	}
	current, err := st.GetPlanningSession(explicit, session.ID)
	if err != nil || current.Status != session.Status {
		t.Fatalf("session=%+v err=%v, want status %s", current, err, session.Status)
	}
	user := WithActor(bare, Actor{ID: UserActorID("usr-1"), Role: core.ActorUser})
	abandoned, err := st.AbandonPlanningSession(user, session.ID, "done")
	if err != nil || abandoned.Status != core.PlanningSessionAbandoned {
		t.Fatalf("abandoned=%+v err=%v", abandoned, err)
	}
	events, err := st.ListEvents(explicit, "")
	if err != nil || len(events) == 0 || events[len(events)-1].Kind != "planning_session.abandoned" || events[len(events)-1].ActorID != UserActorID("usr-1") {
		t.Fatalf("abandon events=%+v err=%v", events, err)
	}
}

// TestRequestPlanRevisionRequiresActorBeforeMutation reproduces review round
// 1: a task-run claim (no worker) contesting its plan from an actorless or
// partial-actor context is refused with ErrMissingActor, without a panic, and
// leaves the order, job, task, session, events, and lineage unchanged. A
// worker claim keeps its internally derived worker actor, and a run claim
// with its credential-derived actor succeeds.
func TestRequestPlanRevisionRequiresActorBeforeMutation(t *testing.T) {
	bare := WithWorkspace(context.Background(), "probe")
	explicit := WithActor(bare, SystemActor())
	setup := func(t *testing.T, id string, claim core.WorkOrderClaim) (Store, core.Task, core.WorkOrder) {
		t.Helper()
		st := NewMemory()
		now := time.Now().UTC()
		task := core.Task{ID: id, Workspace: "probe", Repo: "app", Branch: "conveyor/" + id, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
		if err := st.CreateTask(explicit, task); err != nil {
			t.Fatal(err)
		}
		spec, err := st.CreateSpecVersion(explicit, core.SpecVersion{TaskID: task.ID, Content: "approved plan", CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		if err = st.ApproveSpecVersion(explicit, task.ID, spec.Version); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: id + "-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
		if err = st.CreateJob(explicit, job); err != nil {
			t.Fatal(err)
		}
		order := core.WorkOrder{ID: id + "-order", TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
		if err = storetestFor(st).CreateWorkOrder(explicit, order); err != nil {
			t.Fatal(err)
		}
		claimed, err := storetestFor(st).ClaimWorkOrder(explicit, order.ID, claim)
		if err != nil {
			t.Fatal(err)
		}
		return st, task, claimed
	}
	request := func(ctx context.Context, st Store, order core.WorkOrder) (PlanRevisionRequestResult, error) {
		identity := core.WorkOrderClaimIdentity{WorkerID: order.WorkerID, ClaimantID: order.ClaimantID, SessionID: order.SessionID}
		return taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdRequestPlanRevision, func(lease taskops.TaskLease) (PlanRevisionRequestResult, error) {
			return st.RequestPlanRevisionCommand(ctx, lease, order.ID, identity, "the plan conflicts with the API")
		})
	}
	runClaim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("usr-probe"), OwnerUserID: "usr-probe", SessionID: "run-session", ClientToken: "run-client", Agent: "codex", Lease: time.Minute, ExecutionTimeout: time.Hour}

	t.Run("run claim without actor", func(t *testing.T) {
		st, task, order := setup(t, "probe-run", runClaim)
		before := snapshotVolatile(t, st)
		for name, ctx := range map[string]context.Context{"missing": bare, "partial": WithActor(bare, Actor{Role: core.ActorAgent})} {
			refusesWithoutPanic(t, name+" RequestPlanRevisionCommand", func() error {
				_, err := request(ctx, st, order)
				return err
			})
			if after := snapshotVolatile(t, st); after != before {
				t.Fatalf("%s actor changed state: before=%+v after=%+v", name, before, after)
			}
		}
		current, err := st.GetWorkOrder(explicit, order.ID)
		if err != nil || current.State != core.WorkOrderClaimed || current.SessionID != order.SessionID {
			t.Fatalf("order=%+v err=%v", current, err)
		}
		if refreshed, err := st.GetTask(explicit, task.ID); err != nil || refreshed.State != core.TaskRunning {
			t.Fatalf("task=%+v err=%v", refreshed, err)
		}
		agent := WithActor(bare, Actor{ID: AgentActorID("run-agent"), Role: core.ActorAgent})
		result, err := request(agent, st, order)
		if err != nil || result.WorkOrder.State != core.WorkOrderQueued || result.Task.State != core.TaskAwaiting {
			t.Fatalf("explicit request result=%+v err=%v", result, err)
		}
		requireEventActor(t, st, explicit, task.ID, "work_order.plan_revision_requested", AgentActorID("run-agent"))
	})

	t.Run("worker claim derives its actor", func(t *testing.T) {
		workerClaim := runClaim
		workerClaim.ClaimantID, workerClaim.WorkerID, workerClaim.OwnerUserID, workerClaim.SessionID = "probe-worker", "probe-worker", "", "worker-session"
		st, task, order := setup(t, "probe-worker-task", workerClaim)
		result, err := request(bare, st, order)
		if err != nil || result.WorkOrder.State != core.WorkOrderQueued || result.Task.State != core.TaskAwaiting {
			t.Fatalf("worker request result=%+v err=%v", result, err)
		}
		requireEventActor(t, st, explicit, task.ID, "work_order.plan_revision_requested", WorkerActorID("probe-worker"))
	})
}

func requireEventActor(t *testing.T, st Store, ctx context.Context, taskID, kind, actorID string) {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == kind {
			if event.ActorID != actorID {
				t.Fatalf("%s actor=%s, want %s", kind, event.ActorID, actorID)
			}
			return
		}
	}
	t.Fatalf("no %s event", kind)
}
