package dispatch

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func newTriageContextProposalFixture(t *testing.T) (*Dispatcher, core.Task, core.Requirement, core.SystemDesign) {
	t.Helper()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), store.Actor{ID: "dispatcher", Role: core.ActorSystem})
	st := store.NewMemory()
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-triage-context", Title: "Triage context"}, core.RequirementVersion{
		Content: "# Triage context\n\nTasks are grounded in confirmed intent.", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Triage proposes grounded context."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-triage-context", Title: "Triage context design", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Triage context design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "triage-context-task", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	return New(st, nil, nil), task, requirement, design
}

func TestTriageRecordsUnifiedRequirementAndSystemDesignProposals(t *testing.T) {
	d, task, requirement, design := newTriageContextProposalFixture(t)
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), store.Actor{ID: "dispatcher", Role: core.ActorSystem})
	d.recordTriageContextProposals(ctx, task, pipeline.Triage{
		RequirementProposals:  []pipeline.ContextProposal{{ID: requirement.ID, Justification: "REQ-1 requires grounded triage."}},
		SystemDesignProposals: []pipeline.ContextProposal{{ID: design.ID, Justification: "The confirmed body governs dispatch."}},
	})
	proposals, err := d.Store.ListTaskContextProposals(ctx, task.ID, core.TaskContextProposalProposed)
	if err != nil || len(proposals) != 2 {
		t.Fatalf("proposals=%+v err=%v", proposals, err)
	}
	for _, proposal := range proposals {
		if proposal.Source != core.TaskContextProposalTriage || proposal.Justification == "" {
			t.Fatalf("proposal=%+v", proposal)
		}
	}
	links, err := d.Store.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.DstID == task.ID && (link.SrcID == requirement.ID || link.SrcID == design.ID) {
			t.Fatalf("unconfirmed proposal created authority link: %+v", link)
		}
	}
}

func TestTriageDropsInvalidDuplicateAndUnjustifiedProposals(t *testing.T) {
	d, task, requirement, _ := newTriageContextProposalFixture(t)
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), store.Actor{ID: "dispatcher", Role: core.ActorSystem})
	result := pipeline.Triage{RequirementProposals: []pipeline.ContextProposal{
		{ID: requirement.ID, Justification: "Confirmed intent."},
		{ID: requirement.ID, Justification: "Duplicate."},
		{ID: "req-missing", Justification: "Unknown."},
		{ID: requirement.ID, Justification: "  "},
	}}
	d.recordTriageContextProposals(ctx, task, result)
	proposals, err := d.Store.ListTaskContextProposals(ctx, task.ID, core.TaskContextProposalProposed)
	if err != nil || len(proposals) != 1 || proposals[0].TargetID != requirement.ID {
		t.Fatalf("proposals=%+v err=%v", proposals, err)
	}
}
