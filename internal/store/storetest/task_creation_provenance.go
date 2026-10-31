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

// runTaskCreationProvenance proves that task.created names the agent
// credential and its owning user when an agent files a task, on every backend
// and inside the creation transaction (req-intake-and-triage AC-4.2, AC-4.3;
// DEC-60; component-persistence).
func runTaskCreationProvenance(t *testing.T, x Fixture) {
	st, base := x.Backend, x.Context
	repo := x.Config.Repos[0].Name
	design := createConfirmedDesign(t, st, base, "provenance-design-"+core.NewTaskID(), "internal/store/**")
	agent := core.AuthenticatedCredential{ID: "agt_provenance", OwnerUserID: "usr_provenance_owner", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}
	agentCtx := store.WithActor(store.WithCredential(base, agent), store.Actor{ID: store.AgentActorID(agent.ID), Role: core.ActorAgent})
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

	t.Run("agent creation records agent and owner", func(t *testing.T) {
		task := newTask("mcp", "provenance-agent-"+core.NewTaskID())
		task.Body = strings.Repeat("The complete task body belongs in task storage, not audit events.\n", 20000)
		requireOK(t, st.CreateTaskWithDependenciesAndContext(agentCtx, task, nil, store.TaskContextInput{DesignIDs: []string{design.ID}}))
		event := created(t, task.ID)
		if event.ActorID != store.AgentActorID(agent.ID) || event.ActorRole != core.ActorAgent {
			t.Fatalf("actor=%q role=%q", event.ActorID, event.ActorRole)
		}
		provenance, ok, err := store.TaskCreatedProvenance(event.Payload)
		requireOK(t, err)
		if !ok || provenance.AgentCredentialID != agent.ID || provenance.OwnerUserID != agent.OwnerUserID {
			t.Fatalf("provenance=%+v ok=%t payload=%s", provenance, ok, event.Payload)
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
			t.Fatalf("agent task did not enter triage: state=%s next=%s", persisted.State, persisted.NextStage)
		}
		// The two durable backends commit the dispatch intent in the same
		// transaction as the task and its creation event.
		if st.IsDurable() {
			_, err := logqueue.Load(base, st.Log(), x.Workspace, logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), task.ID))
			requireOK(t, err)
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
		children := approveBlueprint(t, agentCtx, st, parent.ID, version.Version)
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
		if !ok || provenance.AgentCredentialID != agent.ID {
			t.Fatalf("child creation provenance=%+v ok=%t", provenance, ok)
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
		approved, err := st.ApprovePlanningBundle(agentCtx, bundle.ID)
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
		if !ok || provenance.AgentCredentialID != agent.ID {
			t.Fatalf("planning task creation provenance=%+v ok=%t", provenance, ok)
		}
	})

	t.Run("incomplete identity and client source invent nothing", func(t *testing.T) {
		// A credential without an owner is not an authenticated identity, so
		// no provenance is written even under an agent actor.
		ownerless := store.WithActor(store.WithCredential(base, core.AuthenticatedCredential{ID: "agt_ownerless", Kind: core.CredentialAgent}), store.Actor{ID: store.AgentActorID("agt_ownerless"), Role: core.ActorAgent})
		task := newTask("mcp:agent=agt_spoofed;owner=usr_spoofed", "")
		requireOK(t, st.CreateTaskWithDependenciesAndContext(ownerless, task, nil, store.TaskContextInput{}))
		event := created(t, task.ID)
		if _, ok, err := store.TaskCreatedProvenance(event.Payload); err != nil || ok {
			t.Fatalf("ownerless credential produced provenance ok=%t err=%v", ok, err)
		}
		// An agent's own source text never replaces the verified identity.
		spoof := newTask("mcp:agent=agt_spoofed;owner=usr_spoofed", "")
		requireOK(t, st.CreateTaskWithDependenciesAndContext(agentCtx, spoof, nil, store.TaskContextInput{}))
		provenance, ok, err := store.TaskCreatedProvenance(created(t, spoof.ID).Payload)
		requireOK(t, err)
		if !ok || provenance.AgentCredentialID != agent.ID || provenance.OwnerUserID != agent.OwnerUserID {
			t.Fatalf("provenance followed client source: %+v", provenance)
		}
	})

	t.Run("refused creation leaves no task or event", func(t *testing.T) {
		badDependency := newTask("mcp", "provenance-bad-dependency-"+core.NewTaskID())
		if err := st.CreateTaskWithDependenciesAndContext(agentCtx, badDependency, []string{"missing-" + core.NewTaskID()}, store.TaskContextInput{}); err == nil {
			t.Fatal("missing dependency accepted")
		}
		badContext := newTask("mcp", "provenance-bad-context-"+core.NewTaskID())
		if err := st.CreateTaskWithDependenciesAndContext(agentCtx, badContext, nil, store.TaskContextInput{RequirementIDs: []string{"req-missing-" + core.NewTaskID()}}); err == nil {
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

	t.Run("duplicate intake key keeps the first provenance", func(t *testing.T) {
		key := "provenance-duplicate-" + core.NewTaskID()
		first := newTask("mcp", key)
		requireOK(t, st.CreateTaskWithDependenciesAndContext(agentCtx, first, nil, store.TaskContextInput{}))
		other := core.AuthenticatedCredential{ID: "agt_provenance_other", OwnerUserID: agent.OwnerUserID, Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}
		otherCtx := store.WithActor(store.WithCredential(base, other), store.Actor{ID: store.AgentActorID(other.ID), Role: core.ActorAgent})
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
		provenance, ok, err := store.TaskCreatedProvenance(created(t, first.ID).Payload)
		requireOK(t, err)
		if !ok || provenance.AgentCredentialID != agent.ID {
			t.Fatalf("first provenance changed: %+v", provenance)
		}
	})
	t.Run("padded intake keys remain distinct", func(t *testing.T) {
		baseKey := "provenance-padded-" + core.NewTaskID()
		padded := newTask("mcp", baseKey+" ")
		requireOK(t, st.CreateTaskWithDependenciesAndContext(agentCtx, padded, nil, store.TaskContextInput{}))
		unpadded := newTask("mcp", baseKey)
		requireOK(t, st.CreateTaskWithDependenciesAndContext(agentCtx, unpadded, nil, store.TaskContextInput{}))
		for _, want := range []core.Task{padded, unpadded} {
			got, found, err := st.GetTaskByIntakeKey(base, want.IntakeKey)
			requireOK(t, err)
			if !found || got.ID != want.ID {
				t.Fatalf("intake key %q resolves to %+v found=%t, want %q", want.IntakeKey, got, found, want.ID)
			}
		}
	})
}
