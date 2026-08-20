package store

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestTaskContextProposalLifecycleAndDeduplication(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	requirement, requirementVersion, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-proposal", Title: "Proposal intent"}, core.RequirementVersion{
		Content: "Proposal intent", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Confirm proposed context."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, requirementVersion.Version); err != nil {
		t.Fatal(err)
	}
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-proposal", Title: "Proposal design", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Proposal\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "proposal-task", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	requirementProposal, suppressed, err := st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: task.ID, TargetKind: core.TaskContextProposalRequirement,
		TargetID: requirement.ID, Source: core.TaskContextProposalTriage, Justification: "The task implements REQ-1."})
	if err != nil || suppressed || requirementProposal.Justification == "" {
		t.Fatalf("proposal=%+v suppressed=%t err=%v", requirementProposal, suppressed, err)
	}
	if repeated, duplicate, repeatErr := st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: task.ID, TargetKind: core.TaskContextProposalRequirement,
		TargetID: requirement.ID, Source: core.TaskContextProposalTriage, Justification: "duplicate"}); repeatErr != nil || !duplicate || repeated.CreatedByEventID != requirementProposal.CreatedByEventID {
		t.Fatalf("repeat=%+v duplicate=%t err=%v", repeated, duplicate, repeatErr)
	}
	designProposal, suppressed, err := st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: task.ID, TargetKind: core.TaskContextProposalSystemDesign,
		TargetID: design.ID, Source: core.TaskContextProposalPlanning, Justification: "The design governs the store path."})
	if err != nil || suppressed {
		t.Fatalf("design proposal=%+v suppressed=%t err=%v", designProposal, suppressed, err)
	}
	if _, err = st.ConfirmTaskContextProposal(ctx, task.ID, core.TaskContextProposalRequirement, requirement.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DismissTaskContextProposal(ctx, task.ID, core.TaskContextProposalSystemDesign, design.ID); err != nil {
		t.Fatal(err)
	}
	context, err := TaskContextForTask(ctx, st, task.ID)
	if err != nil || len(context.Requirements) != 1 || len(context.Designs) != 0 || len(context.Proposals) != 0 {
		t.Fatalf("context=%+v err=%v", context, err)
	}
	if _, duplicate, err := st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: task.ID, TargetKind: core.TaskContextProposalRequirement,
		TargetID: requirement.ID, Source: core.TaskContextProposalOperator, Justification: "already attached"}); err != nil || !duplicate {
		t.Fatalf("active-context duplicate=%t err=%v", duplicate, err)
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.SrcID == design.ID && link.DstID == task.ID {
			t.Fatalf("dismissed proposal emitted lineage: %+v", link)
		}
	}
}

func TestTaskContextProposalValidation(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-unconfirmed", Title: "Pending"}, core.RequirementVersion{Content: "Pending", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Remain pending."}}})
	if err != nil {
		t.Fatal(err)
	}
	open := core.Task{ID: "open", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, open); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: open.ID, TargetKind: core.TaskContextProposalRequirement, TargetID: requirement.ID,
		Source: core.TaskContextProposalTriage, Justification: "not confirmed"}); err == nil {
		t.Fatal("unconfirmed requirement was proposable")
	}
	terminal := core.Task{ID: "terminal", Workspace: "demo", State: core.TaskMerged, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: terminal.ID, TargetKind: core.TaskContextProposalRequirement, TargetID: requirement.ID,
		Source: core.TaskContextProposalTriage, Justification: "terminal"}); !errors.Is(err, ErrTaskTerminal) {
		t.Fatalf("terminal error=%v", err)
	}
	if _, _, err = st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: open.ID, TargetKind: core.TaskContextProposalRequirement, TargetID: requirement.ID,
		Source: core.TaskContextProposalTriage}); err == nil {
		t.Fatal("empty justification was accepted")
	}
}
