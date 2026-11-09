package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// planStateIntakePlan passes the content rules of a submitted plan.
const planStateIntakePlan = "## Approach\nAdd the plan state.\n\n## Files touched\n- internal/store/plan_state.go\n\n## Ordering\n1. Store, then intake.\n\n## Risks\n- None.\n\n## Done criteria\n- Every task read carries plan_state."

// runPlanStateIntake proves on every backend that each creating transaction
// records the task's plan state and its intake move, that a plan filed at
// intake becomes plan version 1 with origin intake and its base commit, that
// a filer's approval records an approve intervention and spec.version_approved
// under the credential's person in the same transaction, and that each
// refusal records nothing (component-plan-state invariants 1 to 3 and
// Verification; component-task-lifecycle invariants 20 and 22;
// req-task-plan-state AC-2.2, AC-2.3, AC-2.5).
func runPlanStateIntake(t *testing.T, x Fixture) {
	st, base := x.Backend, x.Context
	repo := x.Config.Repos[0].Name
	person := core.AuthenticatedCredential{ID: "pat_plan_state", OwnerUserID: "usr_plan_state_filer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	personCtx := store.WithActor(store.WithCredential(base, person), store.Actor{ID: store.UserActorID(person.OwnerUserID), Role: core.ActorUser})
	systemCtx := store.WithActor(base, store.SystemActor())
	commit := "0123456789abcdef0123456789abcdef01234567"

	newTask := func() core.Task {
		id := core.NewTaskID()
		return core.Task{
			ID: id, Workspace: x.Workspace, Source: "api", Title: "Plan state " + id, Body: "Plan state body",
			Repo: repo, BaseBranch: "main", Branch: "conveyor/task-" + id, MergeApproval: true, PolicyVersion: 1,
			State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC(),
		}
	}
	events := func(t *testing.T, taskID string) []core.Event {
		t.Helper()
		listed, err := st.ListEvents(base, taskID)
		requireOK(t, err)
		return listed
	}
	kinds := func(listed []core.Event) map[string]int {
		counts := map[string]int{}
		for _, event := range listed {
			counts[event.Kind]++
		}
		return counts
	}
	requireIntakeMove := func(t *testing.T, taskID string, want core.PlanState) {
		t.Helper()
		var moves []core.PlanStateChange
		var created store.TaskCreatedEvent
		for _, event := range events(t, taskID) {
			switch event.Kind {
			case core.EventTaskPlanStateChanged:
				var move core.PlanStateChange
				requireOK(t, json.Unmarshal(event.Payload, &move))
				moves = append(moves, move)
			case "task.created":
				requireOK(t, json.Unmarshal(event.Payload, &created))
			}
		}
		if len(moves) != 1 || moves[0].From != nil || moves[0].To != want || moves[0].Cause != core.PlanCauseIntake {
			t.Fatalf("task %s plan-state moves = %+v, want one intake move to %s from null", taskID, moves, want)
		}
		if created.PlanState != want {
			t.Fatalf("task %s task.created plan_state = %q, want %q", taskID, created.PlanState, want)
		}
	}
	requireStored := func(t *testing.T, taskID string, want core.PlanState) core.Task {
		t.Helper()
		got, err := st.GetTask(base, taskID)
		requireOK(t, err)
		if got.PlanState != want {
			t.Fatalf("task %s plan_state = %q, want %q", taskID, got.PlanState, want)
		}
		// The retained column bridges routing until plan_state is read there.
		if got.SpecApproval != store.SpecApprovalBridge(want) {
			t.Fatalf("task %s spec_approval = %t, want the bridge of %s", taskID, got.SpecApproval, want)
		}
		listed, err := st.ListTasks(base)
		requireOK(t, err)
		for _, task := range listed {
			if task.ID == taskID && task.PlanState != want {
				t.Fatalf("listed task %s plan_state = %q, want %q", taskID, task.PlanState, want)
			}
		}
		requireIntakeMove(t, taskID, want)
		return got
	}
	requireNothingRecorded := func(t *testing.T, task core.Task) {
		t.Helper()
		if _, err := st.GetTask(base, task.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("refused task %s was stored: %v", task.ID, err)
		}
		if listed := events(t, task.ID); len(listed) != 0 {
			t.Fatalf("refused task %s recorded events %+v", task.ID, listed)
		}
		if items, err := st.ListInterventions(base, task.ID); err != nil || len(items) != 0 {
			t.Fatalf("refused task %s recorded interventions %+v err=%v", task.ID, items, err)
		}
		if _, found, err := st.GetLatestSpecVersion(base, task.ID); err != nil || found {
			t.Fatalf("refused task %s recorded a plan version: found=%t err=%v", task.ID, found, err)
		}
	}

	t.Run("each resolved state is stored and moved at intake", func(t *testing.T) {
		for _, state := range []core.PlanState{core.PlanNotRequired, core.PlanNeeded} {
			task := newTask()
			task.PlanState = state
			requireOK(t, st.CreateTaskWithDependenciesAndContext(personCtx, task, nil, store.TaskContextInput{}))
			requireStored(t, task.ID, state)
		}
	})

	t.Run("a task built without a plan state takes its legacy policy", func(t *testing.T) {
		for _, legacy := range []struct {
			name          string
			policyVersion int
			specApproval  bool
			level         core.EscalationLevel
			want          core.PlanState
		}{
			{"spec_approval true at policy version 1", 1, true, "", core.PlanNeeded},
			{"spec_approval false at policy version 1", 1, false, core.L3, core.PlanNotRequired},
			{"L2 at policy version 0", 0, false, core.L2, core.PlanNeeded},
			{"L3 at policy version 0", 0, false, core.L3, core.PlanNeeded},
			{"L1 at policy version 0", 0, true, core.L1, core.PlanNotRequired},
		} {
			task := newTask()
			task.PolicyVersion, task.SpecApproval, task.Level = legacy.policyVersion, legacy.specApproval, legacy.level
			requireOK(t, st.CreateTask(personCtx, task))
			got, err := st.GetTask(base, task.ID)
			requireOK(t, err)
			if got.PlanState != legacy.want || got.SpecApproval != legacy.specApproval {
				t.Fatalf("%s: plan_state=%q spec_approval=%t", legacy.name, got.PlanState, got.SpecApproval)
			}
			requireIntakeMove(t, task.ID, legacy.want)
		}
	})

	t.Run("a filed plan becomes plan version 1 awaiting approval", func(t *testing.T) {
		for _, withCommit := range []bool{true, false} {
			task := newTask()
			task.IntakePlan = &core.IntakePlan{Content: planStateIntakePlan}
			if withCommit {
				task.IntakePlan.BaseCommit = commit
			}
			requireOK(t, st.CreateTaskWithDependenciesAndContext(personCtx, task, nil, store.TaskContextInput{}))
			requireStored(t, task.ID, core.PlanAwaitingApproval)
			plan, found, err := st.GetSpecVersion(base, task.ID, core.IntakePlanVersion)
			requireOK(t, err)
			if !found || plan.Origin != core.PlanOriginIntake || plan.Approved || plan.Content != planStateIntakePlan {
				t.Fatalf("intake plan = %+v found=%t", plan, found)
			}
			if withCommit != (plan.BaseCommit != nil) || (withCommit && *plan.BaseCommit != commit) {
				t.Fatalf("intake plan base_commit = %v, want recorded=%t", plan.BaseCommit, withCommit)
			}
			latest, found, err := st.GetLatestSpecVersion(base, task.ID)
			requireOK(t, err)
			if !found || latest.Version != core.IntakePlanVersion || latest.Origin != core.PlanOriginIntake {
				t.Fatalf("latest plan = %+v", latest)
			}
			if _, approved, err := st.GetApprovedSpecVersion(base, task.ID); err != nil || approved {
				t.Fatalf("unapproved intake plan reads approved=%t err=%v", approved, err)
			}
			counts := kinds(events(t, task.ID))
			if counts["spec.version_created"] != 1 || counts["spec.version_approved"] != 0 || counts["intervention.approve"] != 0 {
				t.Fatalf("intake plan events = %+v", counts)
			}
			for _, event := range events(t, task.ID) {
				if event.Kind == "task.created" {
					var created store.TaskCreatedEvent
					requireOK(t, json.Unmarshal(event.Payload, &created))
					if created.PlanVersion != core.IntakePlanVersion {
						t.Fatalf("task.created plan_version = %d", created.PlanVersion)
					}
				}
			}
			if items, err := st.ListInterventions(base, task.ID); err != nil || len(items) != 0 {
				t.Fatalf("unapproved intake plan interventions = %+v err=%v", items, err)
			}
		}
	})

	t.Run("the filer's approval is recorded under the credential's person", func(t *testing.T) {
		task := newTask()
		task.IntakePlan = &core.IntakePlan{Content: planStateIntakePlan, BaseCommit: commit, Approved: true}
		requireOK(t, st.CreateTaskWithDependenciesAndContext(personCtx, task, nil, store.TaskContextInput{}))
		requireStored(t, task.ID, core.PlanCompleted)
		approved, found, err := st.GetApprovedSpecVersion(base, task.ID)
		requireOK(t, err)
		if !found || approved.Version != core.IntakePlanVersion || !approved.Approved || approved.ApprovedAt.IsZero() || approved.Origin != core.PlanOriginIntake || approved.BaseCommit == nil || *approved.BaseCommit != commit {
			t.Fatalf("approved intake plan = %+v found=%t", approved, found)
		}
		items, err := st.ListInterventions(base, task.ID)
		requireOK(t, err)
		wantActor := store.UserActorID(person.OwnerUserID)
		if len(items) != 1 || items[0].Action != core.InterventionApprove || items[0].ActorID != wantActor || items[0].ActorRole != core.ActorUser || items[0].ReasonCode != core.IntakePlanApprovalReasonCode {
			t.Fatalf("intake approval interventions = %+v", items)
		}
		listed := events(t, task.ID)
		counts := kinds(listed)
		if counts["spec.version_created"] != 1 || counts["intervention.approve"] != 1 || counts["spec.version_approved"] != 1 || counts[core.EventTaskPlanStateChanged] != 1 {
			t.Fatalf("intake approval events = %+v", counts)
		}
		for _, event := range listed {
			if event.Kind == "intervention.approve" && (event.ActorID != wantActor || event.ActorRole != core.ActorUser) {
				t.Fatalf("approval event actor = %s/%s", event.ActorID, event.ActorRole)
			}
		}
	})

	t.Run("an attachment intake records the plan in its transaction", func(t *testing.T) {
		task := newTask()
		task.IntakePlan = &core.IntakePlan{Content: planStateIntakePlan, Approved: true}
		upload := store.ArtifactUpload{Name: "notes.txt", ContentType: "text/plain", Content: []byte("plan-state attachment")}
		requireOK(t, st.CreateTaskWithAttachments(personCtx, task, nil, store.TaskContextInput{}, []store.ArtifactUpload{upload}))
		requireStored(t, task.ID, core.PlanCompleted)
		if _, found, err := st.GetApprovedSpecVersion(base, task.ID); err != nil || !found {
			t.Fatalf("attachment intake approved plan found=%t err=%v", found, err)
		}
		if items, err := st.ListInterventions(base, task.ID); err != nil || len(items) != 1 {
			t.Fatalf("attachment intake interventions = %+v err=%v", items, err)
		}
	})

	t.Run("each refusal records nothing", func(t *testing.T) {
		upload := store.ArtifactUpload{Name: "notes.txt", ContentType: "text/plain", Content: []byte("refused attachment")}
		for _, refusal := range []struct {
			name   string
			ctx    context.Context
			change func(*core.Task)
		}{
			{"approval without a person's credential", systemCtx, func(task *core.Task) {
				task.IntakePlan = &core.IntakePlan{Content: planStateIntakePlan, Approved: true}
			}},
			{"base commit that is not a full commit ID", personCtx, func(task *core.Task) {
				task.IntakePlan = &core.IntakePlan{Content: planStateIntakePlan, BaseCommit: "ABCDEF"}
			}},
			{"empty plan", personCtx, func(task *core.Task) { task.IntakePlan = &core.IntakePlan{Content: " \n"} }},
			{"unknown plan state", personCtx, func(task *core.Task) { task.PlanState = "planned" }},
		} {
			for _, attachments := range []bool{false, true} {
				task := newTask()
				refusal.change(&task)
				var err error
				if attachments {
					err = st.CreateTaskWithAttachments(refusal.ctx, task, nil, store.TaskContextInput{}, []store.ArtifactUpload{upload})
				} else {
					err = st.CreateTaskWithDependenciesAndContext(refusal.ctx, task, nil, store.TaskContextInput{})
				}
				if err == nil {
					t.Fatalf("%s (attachments=%t) was accepted", refusal.name, attachments)
				}
				requireNothingRecorded(t, task)
			}
		}
	})

	t.Run("a submitted plan version records its origin", func(t *testing.T) {
		task := newTask()
		task.PlanState = core.PlanNeeded
		requireOK(t, st.CreateTask(personCtx, task))
		submitted, err := st.CreateSpecVersion(personCtx, core.SpecVersion{TaskID: task.ID, Content: planStateIntakePlan, Acceptance: json.RawMessage(`[]`), Decomposition: json.RawMessage(`[]`)})
		requireOK(t, err)
		stored, found, err := st.GetSpecVersion(base, task.ID, submitted.Version)
		requireOK(t, err)
		if !found || stored.Origin != core.PlanOriginPlanOrder || stored.BaseCommit != nil {
			t.Fatalf("submitted plan = %+v found=%t", stored, found)
		}
		withCommit := commit
		operator, err := st.CreateSpecVersion(personCtx, core.SpecVersion{TaskID: task.ID, Content: planStateIntakePlan, Origin: core.PlanOriginOperator, BaseCommit: &withCommit, Acceptance: json.RawMessage(`[]`), Decomposition: json.RawMessage(`[]`)})
		requireOK(t, err)
		latest, found, err := st.GetLatestSpecVersion(base, task.ID)
		requireOK(t, err)
		if !found || latest.Version != operator.Version || latest.Origin != core.PlanOriginOperator || latest.BaseCommit == nil || *latest.BaseCommit != commit {
			t.Fatalf("operator plan = %+v found=%t", latest, found)
		}
		malformed := "not-a-commit"
		if _, err = st.CreateSpecVersion(personCtx, core.SpecVersion{TaskID: task.ID, Content: planStateIntakePlan, BaseCommit: &malformed, Acceptance: json.RawMessage(`[]`), Decomposition: json.RawMessage(`[]`)}); err == nil {
			t.Fatal("a malformed plan base commit was accepted")
		}
		if again, _, err := st.GetLatestSpecVersion(base, task.ID); err != nil || again.Version != operator.Version {
			t.Fatalf("a refused plan version was recorded: %+v err=%v", again, err)
		}
	})

	t.Run("bundle members take their stated or legacy plan state", func(t *testing.T) {
		session, err := st.CreatePlanningSession(personCtx, core.PlanningSession{ID: "plan-state-session-" + core.NewTaskID(), Goal: core.PlanningGoalBundle})
		requireOK(t, err)
		req, revision, err := st.CreateRequirement(personCtx, core.Requirement{ID: "req-plan-state-" + core.NewTaskID(), Title: "Plan-state bundle intent"}, core.RequirementVersion{Content: "# Bundle intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Members carry a plan state."}}, Origin: core.RequirementOriginOperator})
		requireOK(t, err)
		bundle, err := st.CreatePlanningBundle(personCtx, core.PlanningBundle{
			ID: "plan-state-bundle-" + core.NewTaskID(), SessionID: session.ID, Title: "Plan-state bundle",
			Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleRequirement, ID: req.ID, Version: revision.Version}},
			Tasks: []core.PlanningBundleTask{
				{MemberID: "stated", Title: "Stated", Body: "Member with a stated plan state", Repo: repo, BaseBranch: "main", PlanState: core.PlanNotRequired, MergeApproval: true},
				{MemberID: "legacy-on", Title: "Legacy on", Body: "Member stored before plan_state", Repo: repo, BaseBranch: "main", SpecApproval: true},
				{MemberID: "legacy-off", Title: "Legacy off", Body: "Member stored before plan_state", Repo: repo, BaseBranch: "main"},
			},
		})
		requireOK(t, err)
		approved, err := st.ApprovePlanningBundle(personCtx, bundle.ID)
		requireOK(t, err)
		want := map[string]core.PlanState{"stated": core.PlanNotRequired, "legacy-on": core.PlanNeeded, "legacy-off": core.PlanNotRequired}
		for _, member := range approved.Tasks {
			requireStored(t, member.CreatedTaskID, want[member.MemberID])
		}

		// A stored member with an invalid plan_state is refused before any
		// task is created, rather than read through the legacy alias.
		invalid, err := st.CreatePlanningBundle(personCtx, core.PlanningBundle{
			ID: "plan-state-invalid-bundle-" + core.NewTaskID(), SessionID: session.ID, Title: "Invalid plan-state bundle",
			Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleRequirement, ID: req.ID, Version: revision.Version}},
			Tasks: []core.PlanningBundleTask{
				{MemberID: "valid", Title: "Valid", Body: "Valid member", Repo: repo, BaseBranch: "main", PlanState: core.PlanNeeded},
				{MemberID: "invalid", Title: "Invalid", Body: "Invalid member", Repo: repo, BaseBranch: "main", PlanState: "planned"},
			},
		})
		requireOK(t, err)
		if _, err = st.ApprovePlanningBundle(personCtx, invalid.ID); err == nil {
			t.Fatal("a bundle member with an invalid plan_state was approved")
		}
		for _, member := range invalid.Tasks {
			if _, err := st.GetTask(base, member.CreatedTaskID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("refused bundle created member %s: %v", member.MemberID, err)
			}
		}
	})
}
