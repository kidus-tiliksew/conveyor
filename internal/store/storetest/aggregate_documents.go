package storetest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func runDecisions(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	first, err := st.ProposeDecision(ctx, core.Decision{Statement: "Use mechanism one.", Context: "Fixture decision.", AlternativesRejected: "No decision.", Origin: core.DecisionOriginOperator})
	requireOK(t, err)
	_, err = st.ConfirmDecision(ctx, first.ID)
	requireOK(t, err)
	req, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-sweep", Title: "Sweep"}, core.RequirementVersion{Content: first.ID + " governs this requirement.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep citations current."}}, Origin: core.RequirementOriginOperator})
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, req.ID, version.Version)
	requireOK(t, err)
	second, err := st.ProposeDecision(ctx, core.Decision{Statement: "Use mechanism two.", Context: "Replace prior mechanism.", AlternativesRejected: "Two conflicting choices.", Origin: core.DecisionOriginOperator, Supersedes: first.ID})
	requireOK(t, err)
	second, err = st.ConfirmDecision(ctx, second.ID)
	requireOK(t, err)
	if len(second.Sweep.Entries) != 1 || second.Sweep.Clean {
		t.Fatal("supersession did not open citation sweep")
	}
	prior, err := st.GetDecision(ctx, first.ID)
	requireOK(t, err)
	if prior.Status != core.DecisionSuperseded || prior.SupersededBy != second.ID {
		t.Fatal("prior decision was not superseded")
	}
	version.Content = "The current choice governs this requirement."
	version.RequirementID = req.ID
	version, err = st.ProposeRequirementVersion(ctx, version)
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, req.ID, version.Version)
	requireOK(t, err)
	second, err = st.GetDecision(ctx, second.ID)
	requireOK(t, err)
	if !second.Sweep.Clean || second.Sweep.Entries[0].Status != core.DecisionSweepAutoCleared {
		t.Fatal("removed citation did not clear sweep")
	}
	version.Content = first.ID + " is cited again."
	version, err = st.ProposeRequirementVersion(ctx, version)
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, req.ID, version.Version)
	requireOK(t, err)
	entry, err := st.DismissDecisionSupersessionSweep(ctx, second.ID, core.DecisionSweepTierRequirement, req.ID)
	requireOK(t, err)
	if entry.Status != core.DecisionSweepDismissed {
		t.Fatal("sweep dismissal missing")
	}
	if _, err := st.DismissDecisionSupersessionSweep(ctx, first.ID, core.DecisionSweepTierRequirement, req.ID); err == nil {
		t.Fatal("unrelated sweep dismissal succeeded")
	}
	draft, err := st.ProposeDecision(ctx, core.Decision{Statement: "Discard this proposal.", Context: "Fixture.", AlternativesRejected: "Keep it.", Origin: core.DecisionOriginOperator})
	requireOK(t, err)
	dismissed, err := st.DismissDecision(ctx, draft.ID)
	requireOK(t, err)
	if dismissed.Status != core.DecisionDismissed {
		t.Fatal("decision was not dismissed")
	}
	decisions, err := st.ListDecisions(ctx)
	requireOK(t, err)
	if len(decisions) != 3 {
		t.Fatal("decision history was lost")
	}
}

func runArchiveRestore(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	for _, id := range []string{"old", "new-a", "new-b"} {
		_, _, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, core.RequirementVersion{Content: "Requirement.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Retain version history."}}, Origin: core.RequirementOriginOperator})
		requireOK(t, err)
		_, _, err = st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Component design"}, core.SystemDesignVersion{Content: designContent(id), Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(ctx, id, 1)
		requireOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, id, 1)
		requireOK(t, err)
	}
	successors := []string{"new-b", "new-a"}
	requireOK(t, st.ArchiveRequirement(ctx, "old", "operator", successors))
	requireOK(t, st.ArchiveSystemDesign(ctx, "old", "operator", successors))
	req, err := st.GetRequirement(ctx, "old")
	requireOK(t, err)
	design, err := st.GetSystemDesign(ctx, "old")
	requireOK(t, err)
	if !req.Archived || !design.Archived || !reflect.DeepEqual(req.SupersededBy, successors) || !reflect.DeepEqual(design.SupersededBy, successors) {
		t.Fatal("archive lost ordered successors")
	}
	requireOK(t, st.RestoreRequirement(ctx, "old", "operator"))
	requireOK(t, st.RestoreSystemDesign(ctx, "old", "operator"))
	req, err = st.GetRequirement(ctx, "old")
	requireOK(t, err)
	design, err = st.GetSystemDesign(ctx, "old")
	requireOK(t, err)
	if req.Archived || design.Archived || len(req.SupersededBy) != 0 || len(design.SupersededBy) != 0 {
		t.Fatal("restore retained archive metadata")
	}
	if err := st.ArchiveRequirement(ctx, "old", "operator", []string{"missing"}); !errors.Is(err, store.ErrNotFound) && err == nil {
		t.Fatal("unknown successor accepted")
	}
	versions, err := st.ListRequirementVersions(ctx, "old")
	requireOK(t, err)
	designVersions, err := st.ListSystemDesignVersions(ctx, "old")
	requireOK(t, err)
	if len(versions) != 1 || len(designVersions) != 1 {
		t.Fatal("archive or restore rewrote version history")
	}
}
