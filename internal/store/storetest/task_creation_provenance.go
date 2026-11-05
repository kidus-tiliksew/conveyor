package storetest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runTaskCreationProvenance proves that a task a session files names the person
// whose credential it presents and writes no agent provenance, on every backend
// and inside the creation transaction, and that an older payload's provenance
// still decodes (req-intake-and-triage AC-4.2, AC-4.3; DEC-116(2), (3), (5);
// component-task-lifecycle invariants 21 and 22).
func runTaskCreationProvenance(t *testing.T, x Fixture) {
	st, base := x.Backend, x.Context
	repo := x.Config.Repos[0].Name
	design := createConfirmedDesign(t, st, base, "provenance-design-"+core.NewTaskID(), "internal/store/**")
	person := core.AuthenticatedCredential{ID: "pat_provenance_session", OwnerUserID: "usr_provenance_owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	sessionCtx := store.WithActor(store.WithCredential(base, person), store.Actor{ID: store.UserActorID(person.OwnerUserID), Role: core.ActorUser})
	human := core.AuthenticatedCredential{ID: "pat_provenance", OwnerUserID: "usr_provenance_human", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	humanCtx := store.WithActor(store.WithCredential(base, human), store.Actor{ID: store.UserActorID(human.OwnerUserID), Role: core.ActorUser})

	newTask := func(source, key string) core.Task {
		id := core.NewTaskID()
		return core.Task{
			ID: id, Workspace: x.Workspace, Source: source, IntakeKey: key, Title: "Provenance " + id, Body: "Body kept verbatim\n\n- item",
			Repo: repo, BaseBranch: "main", Branch: "conveyor/task-" + id, SpecApproval: true, MergeApproval: true, PolicyVersion: 1,
			State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC(),
			Context: core.TaskContext{Designs: []core.TaskDesignContext{{ID: design.ID}}},
		}
	}
	created := func(t *testing.T, taskID string) core.Event {
		t.Helper()
		events, err := st.ListEvents(base, taskID)
		requireOK(t, err)
		var found []core.Event
		for _, event := range events {
			if event.Kind == "task.created" {
				found = append(found, event)
			}
		}
		if len(found) != 1 {
			t.Fatalf("task %s has %d task.created events", taskID, len(found))
		}
		var fields map[string]json.RawMessage
		requireOK(t, json.Unmarshal(found[0].Payload, &fields))
		for _, field := range []string{"body", "title", "source", "repo", "dependencies", "children"} {
			if _, copied := fields[field]; copied {
				t.Fatalf("task.created copied stored task field %q", field)
			}
		}
		return found[0]
	}

	// case: BHIST-001
	t.Run("creation history preserves distinct verify policies", func(t *testing.T) {
		required := newTask("api", "history-required-"+core.NewTaskID())
		required.Body = strings.Repeat("Canonical task authority remains in task storage.\n", 20000)
		required.SetupContract.VerifyStage = true
		requireOK(t, st.CreateTaskWithDependenciesAndContext(humanCtx, required, nil, store.TaskContextInput{}))
		recorded := created(t, required.ID)

		notRequired := newTask("api", "history-not-required-"+core.NewTaskID())
		notRequired.SetupContract.VerifyStage = false
		requireOK(t, st.CreateTaskWithDependenciesAndContext(humanCtx, notRequired, nil, store.TaskContextInput{}))

		for _, task := range []core.Task{required, notRequired} {
			event := created(t, task.ID)
			var history store.TaskCreatedEvent
			requireOK(t, json.Unmarshal(event.Payload, &history))
			if history.PolicyContract.VerifyStage != task.SetupContract.VerifyStage {
				t.Fatalf("task %s creation history verify policy=%t, want %t", task.ID, history.PolicyContract.VerifyStage, task.SetupContract.VerifyStage)
			}
			stored, err := st.GetTask(base, task.ID)
			requireOK(t, err)
			if stored.Body != task.Body || stored.SetupContract.VerifyStage != task.SetupContract.VerifyStage {
				t.Fatalf("task %s canonical body or verify policy changed", task.ID)
			}
			if task.ID == required.ID && (event.ID != recorded.ID || string(event.Payload) != string(recorded.Payload)) {
				t.Fatal("creating a task with another policy rewrote earlier task-created history")
			}
		}
	})

	t.Run("session creation names its person", func(t *testing.T) {
		task := newTask("mcp", "provenance-session-"+core.NewTaskID())
		task.Body = strings.Repeat("The complete task body belongs in task storage, not audit events.\n", 20000)
		requireOK(t, st.CreateTaskWithDependenciesAndContext(sessionCtx, task, nil, store.TaskContextInput{DesignIDs: []string{design.ID}}))
		event := created(t, task.ID)
		if event.ActorID != store.UserActorID(person.OwnerUserID) || event.ActorRole != core.ActorUser {
			t.Fatalf("actor=%q role=%q", event.ActorID, event.ActorRole)
		}
		provenance, ok, err := store.TaskCreatedProvenance(event.Payload)
		requireOK(t, err)
		if ok {
			t.Fatalf("session creation wrote agent provenance=%+v payload=%s", provenance, event.Payload)
		}
		var decoded store.TaskCreatedEvent
		requireOK(t, json.Unmarshal(event.Payload, &decoded))
		if decoded.ID != task.ID || !decoded.SpecApproval || !decoded.MergeApproval || decoded.State != core.TaskQueued || decoded.NextStage != core.StageTriage {
			t.Fatalf("creation history=%+v", decoded)
		}
		if len(decoded.Context.Designs) != 1 || decoded.Context.Designs[0].ID != design.ID {
			t.Fatalf("creation context=%+v", decoded.Context)
		}
		persisted, err := st.GetTask(base, task.ID)
		requireOK(t, err)
		if persisted.Body != task.Body || persisted.Title != task.Title || persisted.Source != task.Source {
			t.Fatal("task storage did not preserve the complete intake")
		}
		t.Logf("stored task body=%d bytes; task.created=%d bytes", len(persisted.Body), len(event.Payload))
		if persisted.State != core.TaskQueued || persisted.NextStage != core.StageTriage {
			t.Fatalf("session-filed task did not enter triage: state=%s next=%s", persisted.State, persisted.NextStage)
		}
		// The two durable backends commit the dispatch intent in the same
		// transaction as the task and its creation event.
		if st.IsDurable() {
			_, err := logqueue.Load(base, st.Log(), x.Workspace, logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), task.ID))
			requireOK(t, err)
		}
	})

	t.Run("provenance on an older payload still decodes", func(t *testing.T) {
		// Nothing writes this field now, and a task.created stored before the
		// class was retired keeps naming its credential and owner (DEC-116(3)).
		older := json.RawMessage(`{"id":"task-older","state":"queued","trigger_provenance":{"agent_credential_id":"agt_older","owner_user_id":"usr_older"}}`)
		provenance, ok, err := store.TaskCreatedProvenance(older)
		requireOK(t, err)
		if !ok || provenance.AgentCredentialID != "agt_older" || provenance.OwnerUserID != "usr_older" {
			t.Fatalf("older provenance=%+v ok=%t", provenance, ok)
		}
		var history store.TaskCreatedEvent
		requireOK(t, json.Unmarshal(older, &history))
		if history.TriggerProvenance == nil || history.TriggerProvenance.OwnerUserID != "usr_older" {
			t.Fatalf("older creation history=%+v", history)
		}
	})

	t.Run("human and system creations omit provenance", func(t *testing.T) {
		for _, creator := range []struct {
			name  string
			ctx   context.Context
			actor string
		}{
			{"human", humanCtx, store.UserActorID(human.OwnerUserID)},
			{"system", base, "conveyor"},
		} {
			name, callCtx := creator.name, creator.ctx
			task := newTask("api", "")
			requireOK(t, st.CreateTaskWithDependenciesAndContext(callCtx, task, nil, store.TaskContextInput{}))
			event := created(t, task.ID)
			if event.ActorID != creator.actor {
				t.Fatalf("%s actor=%q", name, event.ActorID)
			}
			if _, ok, err := store.TaskCreatedProvenance(event.Payload); err != nil || ok {
				t.Fatalf("%s creation gained provenance ok=%t err=%v payload=%s", name, ok, err, event.Payload)
			}
			var fields map[string]json.RawMessage
			requireOK(t, json.Unmarshal(event.Payload, &fields))
			if _, present := fields["trigger_provenance"]; present {
				t.Fatalf("%s payload has trigger_provenance: %s", name, event.Payload)
			}
		}
	})

	t.Run("blueprint children reference stored task content and ancestry", func(t *testing.T) {
		parent := newTask("api", "")
		requireOK(t, st.CreateTask(base, parent))
		version := createBlueprintSpec(t, base, st, parent.ID, []blueprintItem{{
			ID: "SUB-1", Repo: repo, Summary: strings.Repeat("Child task content. ", 512),
		}})
		children := approveBlueprint(t, sessionCtx, st, parent.ID, version.Version)
		if len(children) != 1 {
			t.Fatalf("materialized children=%d", len(children))
		}
		child := children[0]
		event := created(t, child.ID)
		var decoded store.TaskCreatedEvent
		requireOK(t, json.Unmarshal(event.Payload, &decoded))
		if decoded.ID != child.ID || decoded.ParentTaskID != parent.ID || decoded.OriginSpecVersion != version.Version {
			t.Fatalf("child ancestry=%+v", decoded)
		}
		persisted, err := st.GetTask(base, child.ID)
		requireOK(t, err)
		if persisted.Body != child.Body {
			t.Fatal("materialized child body was not preserved")
		}
		provenance, ok, err := store.TaskCreatedProvenance(event.Payload)
		requireOK(t, err)
		if ok {
			t.Fatalf("child creation provenance=%+v", provenance)
		}
		if event.ActorID != store.UserActorID(person.OwnerUserID) {
			t.Fatalf("child creation actor=%q", event.ActorID)
		}
	})

	t.Run("planning tasks reference stored task content", func(t *testing.T) {
		session, err := st.CreatePlanningSession(base, core.PlanningSession{ID: "session-" + core.NewTaskID(), Goal: core.PlanningGoalBundle})
		requireOK(t, err)
		pendingDesign, pendingVersion, err := st.CreateSystemDesign(base,
			core.SystemDesign{ID: "bundle-design-" + core.NewTaskID(), Title: "Planning task references", Category: "Architecture"},
			core.SystemDesignVersion{Content: designProposalContent("task references"), Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		body := strings.Repeat("Planning task content.\n", 512) + "End"
		bundle, err := st.CreatePlanningBundle(base, core.PlanningBundle{
			ID: "bundle-" + core.NewTaskID(), SessionID: session.ID, Title: "Task event references",
			Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleSystemDesign, ID: pendingDesign.ID, Version: pendingVersion.Version}},
			Tasks:     []core.PlanningBundleTask{{MemberID: "one", Title: "Planned task", Body: body, Repo: repo}},
		})
		requireOK(t, err)
		approved, err := st.ApprovePlanningBundle(sessionCtx, bundle.ID)
		requireOK(t, err)
		event := created(t, approved.Tasks[0].CreatedTaskID)
		var decoded store.TaskCreatedEvent
		requireOK(t, json.Unmarshal(event.Payload, &decoded))
		if decoded.ID != approved.Tasks[0].CreatedTaskID {
			t.Fatalf("planned task identity=%q", decoded.ID)
		}
		persisted, err := st.GetTask(base, decoded.ID)
		requireOK(t, err)
		if persisted.Body != body {
			t.Fatal("planned task body was not preserved")
		}
		provenance, ok, err := store.TaskCreatedProvenance(event.Payload)
		requireOK(t, err)
		if ok {
			t.Fatalf("planning task creation provenance=%+v", provenance)
		}
		if event.ActorID != store.UserActorID(person.OwnerUserID) {
			t.Fatalf("planning task creation actor=%q", event.ActorID)
		}
	})

	t.Run("incomplete identity and client source invent nothing", func(t *testing.T) {
		// A context naming the retired agent class writes no provenance, with or
		// without an owner: nothing derives it from a credential any more.
		for _, owner := range []string{"", person.OwnerUserID} {
			retired := store.WithActor(store.WithCredential(base, core.AuthenticatedCredential{ID: "agt_retired", OwnerUserID: owner, Kind: core.CredentialKind("agent")}), store.Actor{ID: store.AgentActorID("agt_retired"), Role: core.ActorAgent})
			task := newTask("mcp:agent=agt_spoofed;owner=usr_spoofed", "")
			requireOK(t, st.CreateTaskWithDependenciesAndContext(retired, task, nil, store.TaskContextInput{}))
			event := created(t, task.ID)
			if _, ok, err := store.TaskCreatedProvenance(event.Payload); err != nil || ok {
				t.Fatalf("retired agent context owner=%q produced provenance ok=%t err=%v", owner, ok, err)
			}
		}
		// A session's own source text never replaces the verified identity.
		spoof := newTask("mcp:agent=agt_spoofed;owner=usr_spoofed", "")
		requireOK(t, st.CreateTaskWithDependenciesAndContext(sessionCtx, spoof, nil, store.TaskContextInput{}))
		event := created(t, spoof.ID)
		if provenance, ok, err := store.TaskCreatedProvenance(event.Payload); err != nil || ok {
			t.Fatalf("provenance followed client source: %+v ok=%t err=%v", provenance, ok, err)
		}
		if event.ActorID != store.UserActorID(person.OwnerUserID) {
			t.Fatalf("actor followed client source: %q", event.ActorID)
		}
	})

	t.Run("refused creation leaves no task or event", func(t *testing.T) {
		badDependency := newTask("mcp", "provenance-bad-dependency-"+core.NewTaskID())
		if err := st.CreateTaskWithDependenciesAndContext(sessionCtx, badDependency, []string{"missing-" + core.NewTaskID()}, store.TaskContextInput{}); err == nil {
			t.Fatal("missing dependency accepted")
		}
		badContext := newTask("mcp", "provenance-bad-context-"+core.NewTaskID())
		if err := st.CreateTaskWithDependenciesAndContext(sessionCtx, badContext, nil, store.TaskContextInput{RequirementIDs: []string{"req-missing-" + core.NewTaskID()}}); err == nil {
			t.Fatal("missing context accepted")
		}
		for _, task := range []core.Task{badDependency, badContext} {
			if _, err := st.GetTask(base, task.ID); err == nil {
				t.Fatalf("refused creation left task %s", task.ID)
			}
			events, err := st.ListEvents(base, task.ID)
			requireOK(t, err)
			if len(events) != 0 {
				t.Fatalf("refused creation left events: %+v", events)
			}
			if _, found, err := st.GetTaskByIntakeKey(base, task.IntakeKey); err != nil || found {
				t.Fatalf("refused creation left intake key found=%t err=%v", found, err)
			}
		}
	})

	t.Run("duplicate intake key keeps the first creator", func(t *testing.T) {
		key := "provenance-duplicate-" + core.NewTaskID()
		first := newTask("mcp", key)
		requireOK(t, st.CreateTaskWithDependenciesAndContext(sessionCtx, first, nil, store.TaskContextInput{}))
		other := core.AuthenticatedCredential{ID: "pat_provenance_other", OwnerUserID: "usr_provenance_other", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
		otherCtx := store.WithActor(store.WithCredential(base, other), store.Actor{ID: store.UserActorID(other.OwnerUserID), Role: core.ActorUser})
		second := newTask("mcp", key)
		if err := st.CreateTaskWithDependenciesAndContext(otherCtx, second, nil, store.TaskContextInput{}); err == nil {
			t.Fatal("duplicate intake key accepted")
		}
		if _, err := st.GetTask(base, second.ID); err == nil {
			t.Fatal("duplicate intake key left a task")
		}
		existing, found, err := st.GetTaskByIntakeKey(base, key)
		requireOK(t, err)
		if !found || existing.ID != first.ID {
			t.Fatalf("intake key resolves to %+v found=%t", existing, found)
		}
		if event := created(t, first.ID); event.ActorID != store.UserActorID(person.OwnerUserID) {
			t.Fatalf("first creator changed: %q", event.ActorID)
		}
	})
	t.Run("padded intake keys remain distinct", func(t *testing.T) {
		baseKey := "provenance-padded-" + core.NewTaskID()
		padded := newTask("mcp", baseKey+" ")
		requireOK(t, st.CreateTaskWithDependenciesAndContext(sessionCtx, padded, nil, store.TaskContextInput{}))
		unpadded := newTask("mcp", baseKey)
		requireOK(t, st.CreateTaskWithDependenciesAndContext(sessionCtx, unpadded, nil, store.TaskContextInput{}))
		for _, want := range []core.Task{padded, unpadded} {
			got, found, err := st.GetTaskByIntakeKey(base, want.IntakeKey)
			requireOK(t, err)
			if !found || got.ID != want.ID {
				t.Fatalf("intake key %q resolves to %+v found=%t, want %q", want.IntakeKey, got, found, want.ID)
			}
		}
	})
}
