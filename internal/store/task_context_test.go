package store

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestAttachSubmissionGovernanceIsAdditivePinnedAndAudited(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory()
	createConfirmed := func(id, path string) (core.SystemDesign, core.SystemDesignVersion) {
		t.Helper()
		design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Architecture"}, core.SystemDesignVersion{
			Content: "# " + id + "\n\n```conveyor:governs\n- repo: app\n  paths:\n    - " + path + "\n```", Origin: core.SystemDesignOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, id, version.Version); err != nil {
			t.Fatal(err)
		}
		return design, version
	}
	existing, first := createConfirmed("design-existing", "internal/**")
	derived, derivedVersion := createConfirmed("design-derived", "internal/workorder/**")
	task := core.Task{ID: "submission-context", Workspace: "demo", Repo: "app", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTaskWithDependenciesAndContext(ctx, task, nil, TaskContextInput{DesignIDs: []string{existing.ID}}); err != nil {
		t.Fatal(err)
	}
	second, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: existing.ID, Content: "# New\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, existing.ID, second.Version, first.Version); err != nil {
		t.Fatal(err)
	}

	attribution := SubmissionGovernanceAttribution{WorkOrderID: "implement-2", SessionID: "worker-session"}
	attached, err := st.AttachSubmissionGovernance(ctx, task.ID, task.Repo, []string{"internal/workorder/service.go"}, attribution)
	if err != nil {
		t.Fatal(err)
	}
	if len(attached) != 1 || attached[0].ID != derived.ID || attached[0].Version != derivedVersion.Version {
		t.Fatalf("attached=%+v", attached)
	}
	if again, repeatErr := st.AttachSubmissionGovernance(ctx, task.ID, task.Repo, []string{"internal/workorder/service.go"}, attribution); repeatErr != nil || len(again) != 0 {
		t.Fatalf("repeat=%+v err=%v", again, repeatErr)
	}
	context, err := TaskContextForTask(ctx, st, task.ID)
	if err != nil || len(context.Designs) != 2 || context.Designs[0].ID != derived.ID || context.Designs[1].ID != existing.ID || context.Designs[1].Version != first.Version {
		t.Fatalf("context=%+v err=%v", context, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	derivedEvents := 0
	for _, event := range events {
		if event.Kind != TaskContextDesignAdded {
			continue
		}
		var payload struct {
			ID          string   `json:"id"`
			Source      string   `json:"source"`
			WorkOrderID string   `json:"work_order_id"`
			SessionID   string   `json:"session_id"`
			Paths       []string `json:"matching_paths"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.ID == derived.ID {
			derivedEvents++
			if payload.Source != "submission_diff" || payload.WorkOrderID != attribution.WorkOrderID || payload.SessionID != attribution.SessionID || len(payload.Paths) != 1 {
				t.Fatalf("derived payload=%+v", payload)
			}
		}
	}
	if derivedEvents != 1 {
		t.Fatalf("derived events=%d", derivedEvents)
	}
}

func TestTaskContextFeedsRequirementAndGovernanceAuthority(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory().(*memory)
	requirement, proposed, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-task", Title: "Task delivery"}, core.RequirementVersion{
		Content: "# Task delivery", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Deliver task context."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, proposed.Version); err != nil {
		t.Fatal(err)
	}
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-task", Title: "Task mechanism", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Task mechanism\n\n```conveyor:governs\n- repo: other\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "context-task", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTaskWithDependenciesAndContext(ctx, task, nil, TaskContextInput{RequirementIDs: []string{requirement.ID}, DesignIDs: []string{design.ID}}); err != nil {
		t.Fatal(err)
	}
	served, err := ServedRequirementsForTask(ctx, st, task.ID)
	if err != nil || len(served.Requirements) != 1 || served.Requirements[0].ID != requirement.ID {
		t.Fatalf("served=%+v err=%v", served, err)
	}
	governance, err := GovernanceForTask(ctx, st, task.ID, task.Repo)
	if err != nil || len(governance.Designs) != 1 || governance.Designs[0].ID != design.ID || governance.Designs[0].Version != 1 {
		t.Fatalf("governance=%+v err=%v", governance, err)
	}

	second, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Task mechanism v2\n\n```conveyor:governs\n- repo: other\n  paths:\n    - internal/v2/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, second.Version, 1); err != nil {
		t.Fatal(err)
	}
	governance, err = GovernanceForTask(ctx, st, task.ID, task.Repo)
	if err != nil || governance.Designs[0].Version != 1 {
		t.Fatalf("pinned governance=%+v err=%v", governance, err)
	}
}

func TestGovernanceMarksAttachmentDowngradeAndCarriesNoProposalEvidence(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory().(*memory)
	design, first, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-pin", Title: "Pinned mechanism", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# V1\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "governance-malformed", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTaskWithDependenciesAndContext(ctx, task, nil, TaskContextInput{DesignIDs: []string{design.ID}}); err != nil {
		t.Fatal(err)
	}
	second, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# V2\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/v2/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, second.Version, first.Version); err != nil {
		t.Fatal(err)
	}
	_, err = st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Pending\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - cmd/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	governance, err := GovernanceForTask(ctx, st, task.ID, task.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(governance.Designs) != 1 || governance.Designs[0].Version != first.Version || !governance.Designs[0].PinnedAtAttachment {
		t.Fatalf("attachment downgrade provenance=%+v", governance.Designs)
	}
	if governance.PendingDesignProposals == nil || len(governance.PendingDesignProposals) != 0 || len(governance.ResolutionNotes) != 0 {
		t.Fatalf("a pending task-origin version reached the governance snapshot: %+v", governance)
	}
}

func TestTaskContextRejectsUnknownReferenceWithoutCreatingTask(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory()
	task := core.Task{ID: "not-created", Workspace: "demo", State: core.TaskQueued}
	err := st.CreateTaskWithDependenciesAndContext(ctx, task, nil, TaskContextInput{RequirementIDs: []string{"req-missing"}})
	var referenceErr *TaskContextReferenceError
	if !errors.As(err, &referenceErr) || referenceErr.ID != "req-missing" {
		t.Fatalf("error=%v", err)
	}
	if _, err = st.GetTask(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial task err=%v", err)
	}
}

// TestTaskDesignProvenanceFoldsAttachmentHistory keeps attachment order and
// reads the submission attribution (req-design-delivery AC-1.2;
// req-task-centric-delivery AC-6.3).
func TestTaskDesignProvenanceFoldsAttachmentHistory(t *testing.T) {
	t.Parallel()
	event := func(kind, id, source string) core.Event {
		return core.Event{Kind: kind, Payload: core.JSONPayload(map[string]any{"id": id, "version": 1, "source": source})}
	}
	got := TaskDesignProvenance([]core.Event{
		event(TaskContextDesignAdded, "z-first", ""),
		event(TaskContextDesignAdded, "a-second", ""),
		event(TaskContextDesignAdded, "removed", ""),
		event(TaskContextDesignAdded, "diff-kept", TaskContextSourceSubmissionDiff),
		event(TaskContextDesignAdded, "diff-removed", TaskContextSourceSubmissionDiff),
		event(TaskContextDesignAdded, "diff-then-operator", TaskContextSourceSubmissionDiff),
		event(TaskContextDesignRemoved, "removed", ""),
		event(TaskContextDesignRemoved, "diff-removed", ""),
		event(TaskContextDesignRemoved, "diff-then-operator", ""),
		event(TaskContextDesignAdded, "diff-then-operator", ""),
		{Kind: TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": "req-ignored"})},
		{Kind: TaskContextDesignAdded, Payload: []byte("not json")},
	})
	if want := []string{"z-first", "a-second", "diff-then-operator"}; !slices.Equal(got.Attached, want) {
		t.Fatalf("attached=%v want %v", got.Attached, want)
	}
	if want := []string{"diff-kept"}; !slices.Equal(got.DiffScoped, want) {
		t.Fatalf("diff=%v want %v", got.DiffScoped, want)
	}
}
