package store_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestReferenceDocumentConsultationAndPromotionLineage(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	ctx = store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	st := store.NewMemory()
	document, first, err := st.CreateReferenceDocument(ctx, core.ReferenceDocument{ID: "ref-product", Name: "Product Overview"}, core.ReferenceDocumentVersion{Filename: "overview.md", ContentType: "text/markdown", Content: "# Billing rule\n\nCharges retry."})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.SupersedeReferenceDocument(ctx, document.ID, core.ReferenceDocumentVersion{Filename: "overview.md", ContentType: "text/markdown", Content: "# Billing rule\n\nCharges retry twice."})
	if err != nil || second.Version != 2 || second.SupersedesVersion != first.Version {
		t.Fatalf("supersede=%+v err=%v", second, err)
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-promotion", Goal: core.PlanningGoalRequirement})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.RecordReferenceDocumentConsulted(ctx, document.ID, second.Version, session.ID); err != nil {
		t.Fatal(err)
	}
	statements := []core.RequirementStatement{{ID: "REQ-1", Statement: "Charges retry twice.", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "When a charge fails, the system shall retry twice."}}}}
	doc, err := pipeline.RenderRequirementDocument("Billing retries are bounded.", statements)
	if err != nil {
		t.Fatal(err)
	}
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-billing", Title: "Billing retries"}, core.RequirementVersion{Content: doc.Markdown, Statements: statements, Origin: core.RequirementOriginChat, OriginSessionID: session.ID, DerivedFrom: &core.RequirementDerivation{DocumentID: document.ID, Version: second.Version, SectionAnchor: "#billing-rule", TargetID: "AC-1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"consulted": false, "derived_from": false, "supersedes": false}
	for _, link := range links {
		if _, ok := want[link.Kind]; ok {
			want[link.Kind] = true
		}
	}
	for kind, found := range want {
		if !found {
			t.Errorf("missing %s edge in %+v", kind, links)
		}
	}
	if err = st.DeleteReferenceDocument(ctx, document.ID); err != nil {
		t.Fatal(err)
	}
	history, err := st.ListReferenceDocumentVersions(ctx, document.ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("history after delete=%+v err=%v", history, err)
	}
}
