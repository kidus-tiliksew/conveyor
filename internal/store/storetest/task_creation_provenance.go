package storetest

import (
	"context"
	"encoding/json"
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
		return found[0]
	}

	t.Run("agent creation records agent and owner", func(t *testing.T) {
		task := newTask("mcp", "provenance-agent-"+core.NewTaskID())
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
		// The additive field leaves the payload decodable as the task, with
		// the intake context IDs retries compare against.
		var decoded core.Task
		requireOK(t, json.Unmarshal(event.Payload, &decoded))
		if decoded.ID != task.ID || decoded.Source != "mcp" || decoded.Body != task.Body || decoded.Title != task.Title || !decoded.SpecApproval || !decoded.MergeApproval {
			t.Fatalf("decoded=%+v", decoded)
		}
		if len(decoded.Context.Designs) != 1 || decoded.Context.Designs[0].ID != design.ID {
			t.Fatalf("decoded context=%+v", decoded.Context)
		}
		persisted, err := st.GetTask(base, task.ID)
		requireOK(t, err)
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

	t.Run("human and system creations keep the plain payload", func(t *testing.T) {
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
}
