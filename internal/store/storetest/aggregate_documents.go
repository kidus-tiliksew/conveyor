package storetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func runDecisions(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	if x.WorkspaceEvents == nil {
		t.Fatal("factory must provide WorkspaceEvents for decision sweep conformance")
	}
	first, err := st.ProposeDecision(ctx, core.Decision{Statement: "Use mechanism one.", Context: "Fixture decision.", AlternativesRejected: "No decision.", Origin: core.DecisionOriginOperator})
	requireOK(t, err)
	_, err = st.ConfirmDecision(ctx, first.ID)
	requireOK(t, err)
	req, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-sweep", Title: "Sweep"}, core.RequirementVersion{Content: "# " + first.ID + " governs this requirement.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep citations current."}}, Origin: core.RequirementOriginOperator})
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
	requireSweepTransitions(t, x, second.ID, req.ID, sweepCounts{opened: 1})
	prior, err := st.GetDecision(ctx, first.ID)
	requireOK(t, err)
	if prior.Status != core.DecisionSuperseded || prior.SupersededBy != second.ID {
		t.Fatal("prior decision was not superseded")
	}
	version.Content = "# The current choice governs this requirement."
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
	requireSweepTransitions(t, x, second.ID, req.ID, sweepCounts{opened: 1, autoCleared: 1})
	version.Content = "# " + first.ID + " is cited again."
	version, err = st.ProposeRequirementVersion(ctx, version)
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, req.ID, version.Version)
	requireOK(t, err)
	requireSweepTransitions(t, x, second.ID, req.ID, sweepCounts{opened: 1, reopened: 1, autoCleared: 1})
	// A recompute that finds the row already open appends nothing.
	version.Content = "# " + first.ID + " remains cited."
	version, err = st.ProposeRequirementVersion(ctx, version)
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, req.ID, version.Version)
	requireOK(t, err)
	requireSweepTransitions(t, x, second.ID, req.ID, sweepCounts{opened: 1, reopened: 1, autoCleared: 1})
	entry, err := st.DismissDecisionSupersessionSweep(ctx, second.ID, core.DecisionSweepTierRequirement, req.ID)
	requireOK(t, err)
	if entry.Status != core.DecisionSweepDismissed {
		t.Fatal("sweep dismissal missing")
	}
	// A dismissed row neither clears nor reopens on later revisions.
	for _, content := range []string{"# The current choice governs this requirement.", "# " + first.ID + " is cited once more."} {
		version.Content = content
		version, err = st.ProposeRequirementVersion(ctx, version)
		requireOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(ctx, req.ID, version.Version)
		requireOK(t, err)
		requireSweepTransitions(t, x, second.ID, req.ID, sweepCounts{opened: 1, reopened: 1, autoCleared: 1, dismissed: 1})
	}
	current, err := st.GetDecision(ctx, second.ID)
	requireOK(t, err)
	if len(current.Sweep.Entries) != 1 || current.Sweep.Entries[0].Status != core.DecisionSweepDismissed {
		t.Fatalf("dismissed sweep row changed: %+v", current.Sweep.Entries)
	}

	// System Design and reference-document revisions follow the same
	// transitions: first insert opens, clearing auto-clears, and a returning
	// citation reopens (component-document-corpus).
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-sweep", Title: "Sweep design", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("cites " + first.ID), Origin: core.SystemDesignOriginOperator})
	requireOK(t, err)
	_, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version)
	requireOK(t, err)
	requireSweepTransitions(t, x, second.ID, design.ID, sweepCounts{opened: 1})
	for i, content := range []string{designContent("cites nothing"), designContent("cites " + first.ID + " again")} {
		designVersion, err = st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: content, Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version)
		requireOK(t, err)
		requireSweepTransitions(t, x, second.ID, design.ID, sweepCounts{opened: 1, autoCleared: 1, reopened: i})
	}
	reference, _, err := st.CreateReferenceDocument(ctx, core.ReferenceDocument{ID: "ref-sweep", Name: "Sweep reference"}, core.ReferenceDocumentVersion{Filename: "sweep.md", ContentType: "text/markdown", Content: "# Notes\n\n" + first.ID + " applies."})
	requireOK(t, err)
	requireSweepTransitions(t, x, second.ID, reference.ID, sweepCounts{opened: 1})
	for i, content := range []string{"# Notes\n\nNothing superseded.", "# Notes\n\n" + first.ID + " applies again."} {
		_, err = st.SupersedeReferenceDocument(ctx, reference.ID, core.ReferenceDocumentVersion{Filename: "sweep.md", ContentType: "text/markdown", Content: content})
		requireOK(t, err)
		requireSweepTransitions(t, x, second.ID, reference.ID, sweepCounts{opened: 1, autoCleared: 1, reopened: i})
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

type sweepCounts struct{ opened, reopened, autoCleared, dismissed int }

// requireSweepTransitions counts one sweep row's transition events by kind.
// A historical ledger may record a reopen as _opened; new transitions use
// _reopened, so the counts pin both kinds exactly.
func requireSweepTransitions(t *testing.T, x Fixture, decisionID, documentID string, want sweepCounts) {
	t.Helper()
	events, err := x.WorkspaceEvents(x.Context, "decision.supersession_sweep_")
	requireOK(t, err)
	var got sweepCounts
	for _, event := range events {
		var entry core.DecisionSupersessionSweepEntry
		if json.Unmarshal(event.Payload, &entry) != nil || entry.DecisionID != decisionID || entry.DocumentID != documentID {
			continue
		}
		switch event.Kind {
		case "decision.supersession_sweep_opened":
			got.opened++
		case "decision.supersession_sweep_reopened":
			got.reopened++
		case "decision.supersession_sweep_auto_cleared":
			got.autoCleared++
		case "decision.supersession_sweep_dismissed":
			got.dismissed++
		default:
			t.Fatalf("unknown sweep event kind %q", event.Kind)
		}
	}
	if got != want {
		t.Fatalf("sweep %s/%s transitions=%+v want %+v", decisionID, documentID, got, want)
	}
}

func runArchiveRestore(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	for _, id := range []string{"old", "new-a", "new-b"} {
		_, _, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, core.RequirementVersion{Content: "# Requirement.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Retain version history."}}, Origin: core.RequirementOriginOperator})
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
