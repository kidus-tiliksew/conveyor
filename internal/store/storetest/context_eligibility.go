package storetest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Runs unchanged in memory, PostgreSQL and SingleStore through Requirements.
func runContextEligibility(t *testing.T, factory RequirementFactory) {
	for _, kind := range []core.TaskContextProposalTargetKind{core.TaskContextProposalRequirement, core.TaskContextProposalSystemDesign} {
		t.Run("context eligibility "+string(kind), func(t *testing.T) {
			st, ctx, ws := newRequirementFixture(t, factory)
			id := "document-" + core.NewTaskID()
			confirm := func() {}
			archive := func() {}
			restore := func() {}
			if kind == core.TaskContextProposalRequirement {
				_, v, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: "Context eligibility"}, core.RequirementVersion{Content: "# Context eligibility", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Use active confirmed context."}}})
				if err != nil {
					t.Fatal(err)
				}
				confirm = func() {
					if _, _, err := st.ConfirmRequirementVersion(ctx, id, v.Version); err != nil {
						t.Fatal(err)
					}
				}
				archive = func() {
					if err := st.ArchiveRequirement(ctx, id, requirementConformanceActor, nil); err != nil {
						t.Fatal(err)
					}
				}
				restore = func() {
					if err := st.RestoreRequirement(ctx, id, requirementConformanceActor); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				_, v, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: "Context eligibility", Category: "Architecture"}, core.SystemDesignVersion{Content: designProposalContent("Context eligibility"), Origin: core.SystemDesignOriginOperator})
				if err != nil {
					t.Fatal(err)
				}
				confirm = func() {
					if _, _, err := st.ConfirmSystemDesignVersion(ctx, id, v.Version); err != nil {
						t.Fatal(err)
					}
				}
				archive = func() {
					if err := st.ArchiveSystemDesign(ctx, id, requirementConformanceActor, nil); err != nil {
						t.Fatal(err)
					}
				}
				restore = func() {
					if err := st.RestoreSystemDesign(ctx, id, requirementConformanceActor); err != nil {
						t.Fatal(err)
					}
				}
			}
			newTask := func() core.Task {
				taskID := core.NewTaskID()
				task := core.Task{ID: taskID, Branch: "conveyor/task-" + taskID, Workspace: ws, Repo: "conveyor", Title: "Context task", State: core.TaskRunning}
				if err := st.CreateTask(ctx, task); err != nil {
					t.Fatal(err)
				}
				return task
			}
			task := newTask()
			input := core.TaskContextProposalInput{TaskID: task.ID, TargetKind: kind, TargetID: id, Source: core.TaskContextProposalPlanning, Justification: "Use the document."}
			refs := store.TaskContextProposalInput(kind, id)
			assertArchive := func(err error) {
				t.Helper()
				var req *store.RequirementArchivedError
				var design *store.SystemDesignArchivedError
				if kind == core.TaskContextProposalRequirement {
					if !errors.As(err, &req) || req.RequirementID != id {
						t.Fatalf("archive error=%v", err)
					}
				} else if !errors.As(err, &design) || design.DocumentID != id {
					t.Fatalf("archive error=%v", err)
				}
			}
			snapshot := func() any {
				t.Helper()
				events, err := st.ListEvents(ctx, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				proposals, err := st.ListTaskContextProposals(ctx, task.ID, "")
				if err != nil {
					t.Fatal(err)
				}
				links, err := st.ListLineageLinks(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return []any{events, proposals, links}
			}
			before := snapshot()
			for _, target := range []string{id, "missing-" + id} {
				invalid := input
				invalid.TargetID = target
				_, _, err := st.ProposeTaskContext(ctx, invalid)
				var ref *store.TaskContextReferenceError
				if !errors.As(err, &ref) {
					t.Fatalf("pending/missing proposal error=%v", err)
				}
			}
			if kind == core.TaskContextProposalRequirement {
				if _, err := st.ProposeRequirementServes(ctx, task.ID, id, core.RequirementServesPlanning, false); err == nil {
					t.Fatal("legacy pending proposal accepted")
				}
			}
			if !reflect.DeepEqual(before, snapshot()) {
				t.Fatal("invalid proposal changed events, proposals or lineage")
			}

			// Archive while still pending, including the bundle-specific exception.
			session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Goal: core.PlanningGoalBundle})
			if err != nil {
				t.Fatal(err)
			}
			bundle := core.PlanningBundle{ID: "bundle-" + core.NewTaskID(), SessionID: session.ID, Title: "Context bundle", Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleDocumentKind(kind), ID: id, Version: 1}}, Tasks: []core.PlanningBundleTask{{MemberID: "one", Repo: "conveyor", Title: "Member", Body: "Use context", Context: core.PlanningBundleTaskContext{RequirementIDs: refs.RequirementIDs, DesignIDs: refs.DesignIDs}}}}
			pendingBundle, err := st.CreatePlanningBundle(ctx, bundle)
			if err != nil {
				t.Fatal(err)
			}
			archive()
			_, err = st.ApprovePlanningBundle(ctx, pendingBundle.ID)
			assertArchive(err)
			if _, err = st.GetTask(ctx, pendingBundle.Tasks[0].CreatedTaskID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("failed bundle created task: %v", err)
			}
			storedBundle, err := st.GetPlanningBundle(ctx, pendingBundle.ID)
			if err != nil || storedBundle.Status != core.PlanningBundlePending {
				t.Fatalf("bundle changed: %+v %v", storedBundle, err)
			}
			bundle.ID = "bundle-" + core.NewTaskID()
			_, err = st.CreatePlanningBundle(ctx, bundle)
			assertArchive(err)
			restore()
			confirm()

			// A valid proposal must be rechecked at decision time, before any write.
			proposal, _, err := st.ProposeTaskContext(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			var legacySession core.PlanningSession
			if kind == core.TaskContextProposalRequirement {
				legacySession, err = st.CreatePlanningSession(ctx, core.PlanningSession{ID: "legacy-" + core.NewTaskID(), RequirementContextID: id})
				if err != nil {
					t.Fatal(err)
				}
			}
			archive()
			before = snapshot()
			if kind == core.TaskContextProposalRequirement {
				_, err = st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{SessionID: legacySession.ID, TaskID: task.ID})
				assertArchive(err)
				saved, getErr := st.GetPlanningSession(ctx, legacySession.ID)
				if getErr != nil || saved.Status != core.PlanningSessionActive {
					t.Fatalf("failed finalization changed session: %+v %v", saved, getErr)
				}
			}

			// A new task must get no proposal row or task event for an already archived ID.
			rejectedTask := newTask()
			before = snapshot()
			rejectedInput := input
			rejectedInput.TaskID = rejectedTask.ID
			rejectedBefore, err := st.ListEvents(ctx, rejectedTask.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = st.ProposeTaskContext(ctx, rejectedInput)
			assertArchive(err)
			rejectedAfter, err := st.ListEvents(ctx, rejectedTask.ID)
			if err != nil || !reflect.DeepEqual(rejectedBefore, rejectedAfter) {
				t.Fatalf("archived proposal wrote events: %v", err)
			}
			rejectedProposals, err := st.ListTaskContextProposals(ctx, rejectedTask.ID, "")
			if err != nil || len(rejectedProposals) != 0 {
				t.Fatalf("archived proposal persisted: %+v %v", rejectedProposals, err)
			}
			_, _, err = st.ProposeTaskContext(ctx, input)
			assertArchive(err)
			_, err = st.ConfirmTaskContextProposal(ctx, task.ID, kind, id)
			assertArchive(err)
			_, err = st.UpdateTaskContext(ctx, task.ID, store.TaskContextChange{Add: refs})
			assertArchive(err)
			if kind == core.TaskContextProposalRequirement {
				_, err = st.ConfirmRequirementServes(ctx, task.ID, id)
				assertArchive(err)
				_, err = st.ProposeRequirementServes(ctx, task.ID, id, core.RequirementServesPlanning, false)
				assertArchive(err)
			}
			if !reflect.DeepEqual(before, snapshot()) {
				t.Fatal("archive refusal changed events, proposals or lineage")
			}
			dismissed, err := st.DismissTaskContextProposal(ctx, task.ID, kind, id)
			if err != nil || dismissed.State != core.TaskContextProposalDismissed || dismissed.CreatedByEventID != proposal.CreatedByEventID {
				t.Fatalf("dismissal=%+v %v", dismissed, err)
			}
			current, err := store.TaskContextForTask(ctx, st, task.ID)
			if err != nil || len(current.Requirements)+len(current.Designs) != 0 {
				t.Fatalf("dismissal attached context: %+v %v", current, err)
			}

			// Initial intake rejects archives atomically, and accepts restored authority.
			intakeID := core.NewTaskID()
			intake := core.Task{ID: intakeID, Branch: "conveyor/task-" + intakeID, Workspace: ws, Repo: "conveyor", Title: "Intake", State: core.TaskQueued}
			if kind == core.TaskContextProposalRequirement {
				intake.Context.Requirements = []core.TaskRequirementContext{{ID: id}}
			} else {
				intake.Context.Designs = []core.TaskDesignContext{{ID: id}}
			}
			assertArchive(st.CreateTaskWithDependenciesAndContext(ctx, intake, nil, refs))
			if _, err = st.GetTask(ctx, intake.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("failed intake persisted: %v", err)
			}
			restore()
			if err = st.CreateTaskWithDependenciesAndContext(ctx, intake, nil, refs); err != nil {
				t.Fatal(err)
			}
			task = newTask()
			input.TaskID = task.ID
			if _, _, err = st.ProposeTaskContext(ctx, input); err != nil {
				t.Fatal(err)
			}
			if _, err = st.ConfirmTaskContextProposal(ctx, task.ID, kind, id); err != nil {
				t.Fatal(err)
			}
			direct := newTask()
			if _, err = st.UpdateTaskContext(ctx, direct.ID, store.TaskContextChange{Add: refs}); err != nil {
				t.Fatal(err)
			}
			// Bundle references to active documents use the ordinary eligibility rule.
			bundle.ID = "bundle-" + core.NewTaskID()
			decision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Keep pending bundle work.", Context: "Bundle fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator})
			if err != nil {
				t.Fatal(err)
			}
			bundle.Documents = []core.PlanningBundleDocument{{Kind: core.PlanningBundleDecision, ID: decision.ID}}
			liveBundle, err := st.CreatePlanningBundle(ctx, bundle)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.ApprovePlanningBundle(ctx, liveBundle.ID); err != nil {
				t.Fatal(err)
			}
			archive()
			before = snapshot()
			current, err = store.TaskContextForTask(ctx, st, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if kind == core.TaskContextProposalRequirement {
				if len(current.Requirements) != 1 || !current.Requirements[0].Archived || current.Requirements[0].Version != 1 {
					t.Fatalf("lost archived attachment: %+v", current)
				}
			} else if len(current.Designs) != 1 || !current.Designs[0].Archived || current.Designs[0].Version != 1 {
				t.Fatalf("lost archived pin: %+v", current)
			}
			if _, err = st.ConfirmTaskContextProposal(ctx, task.ID, kind, id); err != nil {
				t.Fatalf("historical idempotent confirmation: %v", err)
			}
			if !reflect.DeepEqual(before, snapshot()) {
				t.Fatal("historical reads rewrote context")
			}
			bundle.ID = "bundle-" + core.NewTaskID()
			_, err = st.CreatePlanningBundle(ctx, bundle)
			assertArchive(err)
			// Archive conflicts remain actionable even when a bundle document version
			// has already been confirmed and would otherwise fail the pending check.
			bundle.ID = "bundle-" + core.NewTaskID()
			bundle.Documents = []core.PlanningBundleDocument{{Kind: core.PlanningBundleDocumentKind(kind), ID: id, Version: 1}}
			_, err = st.CreatePlanningBundle(ctx, bundle)
			assertArchive(err)

		})
	}
}
