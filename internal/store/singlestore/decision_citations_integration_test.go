package singlestore

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestDecisionCitationsIntegration pins the database-membership citation read
// on one workspace: only whole, case-exact ASCII tokens count, a decision of
// any state is citable, archived and unconfirmed documents are not, a legacy
// confirmation stays citable, and the statement count follows decision
// chunks rather than the document corpus
// (component-document-corpus, Decision citations;
// req-document-operating-surfaces AC-8.5).
func TestDecisionCitationsIntegration(t *testing.T) {
	st := integrationStore(t)
	workspace := "decision-citations-" + core.NewTaskID()
	ctx := documentPlanWorkspace(t, st, workspace)
	decide := func(id, supersedes string, confirm bool) {
		t.Helper()
		if _, err := st.ProposeDecision(ctx, core.Decision{ID: id, Statement: "Fixture " + id + ".", Context: "Citation fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator, Supersedes: supersedes}); err != nil {
			t.Fatal(err)
		}
		if confirm {
			if _, err := st.ConfirmDecision(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	requirement := func(id, title, body string, confirm bool) {
		t.Helper()
		_, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: title}, core.RequirementVersion{Content: "# " + title + "\n\n" + body, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Cite decisions."}}, Origin: core.RequirementOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if confirm {
			if _, _, err = st.ConfirmRequirementVersion(ctx, id, version.Version); err != nil {
				t.Fatal(err)
			}
		}
	}
	decide("DEC-4", "", true)
	decide("DEC-40", "", false)
	decide("DEC-9", "", false)
	if _, err := st.DismissDecision(ctx, "DEC-9"); err != nil {
		t.Fatal(err)
	}
	decide("DEC-10", "", false)
	requirement("req-punct", "Tokens", "Cites (DEC-4), DEC-4, DEC-4/DEC-4, _DEC-40_, DEC-4_DEC-9, DEC-4DEC-9, DEC 4, dec-4, and DEC\u20114.", true)
	requirement("req-unicode", "Unicode", "Cites DEC-4é, éDEC-10, and DEC-10.", true)
	requirement("req-forty", "Forty", "Cites DEC-40 and DEC-4.", true)
	requirement("req-archived", "Archived", "Cites DEC-4.", true)
	requirement("req-pending", "Pending", "Cites DEC-9 and DEC-10.", false)
	requirement("req-oldconfirm", "Old confirmation", "Cites DEC-4.", true)
	_, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-cite", Title: "Design", Category: "Component design"},
		core.SystemDesignVersion{Content: "# Design\n\nCites DEC-9 and DEC-10.\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, "design-cite", version.Version); err != nil {
		t.Fatal(err)
	}
	if err = st.ArchiveRequirement(ctx, "req-archived", "operator", nil); err != nil {
		t.Fatal(err)
	}
	// A version confirmed before that column had an actor stays citable: the
	// read must not filter by confirmer.
	if _, err = st.db.ExecContext(ctx, `UPDATE requirement_versions SET confirmed_by='legacy' WHERE workspace_id=? AND requirement_id='req-oldconfirm'`, workspace); err != nil {
		t.Fatal(err)
	}

	req, sd := core.DecisionSweepTierRequirement, core.DecisionSweepTierSystemDesign
	want := []core.DecisionCitation{
		{DecisionID: "DEC-4", Tier: req, DocumentID: "req-forty", Title: "Forty", Version: 1},
		{DecisionID: "DEC-4", Tier: req, DocumentID: "req-oldconfirm", Title: "Old confirmation", Version: 1},
		{DecisionID: "DEC-4", Tier: req, DocumentID: "req-punct", Title: "Tokens", Version: 1},
		{DecisionID: "DEC-4", Tier: req, DocumentID: "req-unicode", Title: "Unicode", Version: 1},
		{DecisionID: "DEC-9", Tier: sd, DocumentID: "design-cite", Title: "Design", Version: 1},
		{DecisionID: "DEC-10", Tier: req, DocumentID: "req-unicode", Title: "Unicode", Version: 1},
		{DecisionID: "DEC-10", Tier: sd, DocumentID: "design-cite", Title: "Design", Version: 1},
		{DecisionID: "DEC-40", Tier: req, DocumentID: "req-forty", Title: "Forty", Version: 1},
	}
	got, err := st.ListDecisionCitations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("citations = %+v\nwant %+v", got, want)
	}
}

// TestDecisionCitationsBoundedStatementsIntegration proves the citation read
// costs one statement per bounded decision chunk, not one per document:
// noise documents' large bodies never leave the database
// (component-document-corpus, Decision citations).
func TestDecisionCitationsBoundedStatementsIntegration(t *testing.T) {
	st := integrationStore(t)
	workspace := "decision-citations-chunks-" + core.NewTaskID()
	ctx := documentPlanWorkspace(t, st, workspace)
	const decisions = 250
	const noiseDocuments = 20
	const noiseBodyBytes = 4 << 10
	for i := range decisions {
		if _, err := st.ProposeDecision(ctx, core.Decision{ID: fmt.Sprintf("DEC-%d", 100+i), Statement: "Fixture.", Context: "Citation fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator}); err != nil {
			t.Fatal(err)
		}
	}
	pad := strings.Repeat("unrelated requirement prose. ", noiseBodyBytes/28+1)
	for i := range noiseDocuments {
		id := fmt.Sprintf("req-chunk-noise-%d", i)
		if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: "Chunk noise " + id}, core.RequirementVersion{Content: "# Chunk noise\n\n" + pad, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Cite decisions."}}, Origin: core.RequirementOriginOperator}); err != nil {
			t.Fatal(err)
		}
	}
	st.ReadStats(1000)
	citations, err := st.ListDecisionCitations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if citations == nil || len(citations) != 0 {
		t.Fatalf("citations = %+v, want a non-nil empty list", citations)
	}
	total := readAccountingTotals(st.ReadStats(1000))["ListDecisionCitations"]
	wantQueries := 1 + (decisions+core.DecisionCitationChunk-1)/core.DecisionCitationChunk
	if total.Queries != uint64(wantQueries) {
		t.Fatalf("ListDecisionCitations queries = %d, want %d chunks plus the decision list", total.Queries, wantQueries)
	}
	if total.Rows != uint64(decisions) {
		t.Fatalf("ListDecisionCitations rows = %d, want %d decision IDs only", total.Rows, decisions)
	}
	if total.Bytes >= noiseDocuments*noiseBodyBytes {
		t.Fatalf("ListDecisionCitations returned %d bytes, want less than the %d noise body bytes", total.Bytes, noiseDocuments*noiseBodyBytes)
	}
}

// TestDecisionCitationsBindQuoteAndBackslashIDsIntegration proves decision
// IDs reach the citation statement as bound arguments: IDs that hold ' and \
// neither break the statement nor change in transit. A literal \- would read
// as -, so the stored ID DEC\-4 must not take DEC-4's citations
// (component-document-corpus, Decision citations).
func TestDecisionCitationsBindQuoteAndBackslashIDsIntegration(t *testing.T) {
	st := integrationStore(t)
	workspace := "decision-citations-bind-" + core.NewTaskID()
	ctx := documentPlanWorkspace(t, st, workspace)
	if _, err := st.ProposeDecision(ctx, core.Decision{ID: "DEC-4", Statement: "Fixture DEC-4.", Context: "Citation fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator}); err != nil {
		t.Fatal(err)
	}
	_, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-bind", Title: "Bound IDs"}, core.RequirementVersion{Content: "# Bound IDs\n\nCites DEC-4, O'Brien, and a path C:\\DEC-4\\.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Cite decisions."}}, Origin: core.RequirementOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, "req-bind", version.Version); err != nil {
		t.Fatal(err)
	}
	// ValidateDecision admits only DEC-n, so no command stores these IDs;
	// they are inserted directly, as rows an older writer could have left.
	odd := []string{`DEC-4'`, `O'Brien`, `DEC\-4`, `DEC-4\`, `\'DEC-4`, `DEC-4\\`}
	for _, id := range odd {
		if _, err = st.db.ExecContext(ctx, `INSERT INTO decisions(workspace_id,id,statement,context,alternatives_rejected,status,origin,created_at) VALUES(?,?,'Fixture.','Citation fixture.','None.','proposed','operator',UTC_TIMESTAMP(6))`, workspace, id); err != nil {
			t.Fatalf("insert decision %q: %v", id, err)
		}
	}
	st.ReadStats(1000)
	got, err := st.ListDecisionCitations(ctx)
	if err != nil {
		t.Fatalf("citations with quote and backslash IDs: %v", err)
	}
	want := []core.DecisionCitation{{DecisionID: "DEC-4", Tier: core.DecisionSweepTierRequirement, DocumentID: "req-bind", Title: "Bound IDs", Version: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("citations = %+v\nwant %+v", got, want)
	}
	reference := core.DeriveDecisionCitations(append([]string{"DEC-4"}, odd...), []core.DecisionCitationSource{{Tier: core.DecisionSweepTierRequirement, DocumentID: "req-bind", Title: "Bound IDs", Version: 1, Content: "# Bound IDs\n\nCites DEC-4, O'Brien, and a path C:\\DEC-4\\."}})
	if !reflect.DeepEqual(got, reference) {
		t.Fatalf("citations = %+v, reference derivation %+v", got, reference)
	}
	if queries := readAccountingTotals(st.ReadStats(1000))["ListDecisionCitations"].Queries; queries != 2 {
		t.Fatalf("ListDecisionCitations queries = %d, want the decision list and one batch", queries)
	}
}

// documentPlanWorkspace bootstraps workspace ws with one repository and
// returns a context that acts in it as the system actor.
func documentPlanWorkspace(t *testing.T, st *Store, ws string) context.Context {
	t.Helper()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), ws), store.SystemActor())
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	return ctx
}
