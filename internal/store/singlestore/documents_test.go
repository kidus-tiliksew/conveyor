package singlestore

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestDocumentSuccessorJSONPreservesOrder(t *testing.T) {
	want := []string{"req-third", "design-second", "req-first"}
	args := documentArgs([]any{want, []string(nil)})
	var got []string
	if err := (documentJSONStrings{&got}).Scan(args[0]); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("successors=%v", got)
	}
	if err := (documentJSONStrings{&got}).Scan(args[1]); err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("restore array=%v", got)
	}
	if err := (documentJSONStrings{&got}).Scan(`{"wrong":"shape"}`); err == nil {
		t.Fatal("invalid successor object accepted")
	}
}
func TestDocumentReadsRequireWorkspaceBeforeSQL(t *testing.T) {
	st := &Store{}
	if _, err := st.GetRequirement(t.Context(), "req"); !errors.Is(err, store.ErrWorkspaceRequired) {
		t.Fatal(err)
	}
	if _, err := st.ListLineageLinks(t.Context()); !errors.Is(err, store.ErrWorkspaceRequired) {
		t.Fatal(err)
	}
	if _, err := st.ListDocumentEventPage(t.Context(), core.LineageRequirement, "req", store.DocumentEventQuery{Limit: 1}); !errors.Is(err, store.ErrWorkspaceRequired) {
		t.Fatal(err)
	}
}
func TestDocumentLiveNameConflict(t *testing.T) {
	if err := checkReferenceName(true, 2); !errors.Is(err, store.ErrReferenceDocumentNameConflict) {
		t.Fatal(err)
	}
	if err := checkReferenceName(false, 2); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentDeliveryTraversalIsCausalAndBounded(t *testing.T) {
	links := []core.LineageLink{
		{Workspace: "demo", SrcType: core.LineageRequirement, SrcID: "req", DstType: core.LineageBlueprint, DstID: "plan", Kind: "serves"},
		{Workspace: "demo", SrcType: core.LineageBlueprint, SrcID: "plan", DstType: core.LineageBlueprintVersion, DstID: "plan:v1", Kind: "versions"},
		{Workspace: "demo", SrcType: core.LineageBlueprintVersion, SrcID: "plan:v1", DstType: core.LineageTask, DstID: "child", Kind: "materializes"},
		{Workspace: "demo", SrcType: core.LineageTask, SrcID: "child", DstType: core.LineageTask, DstID: "unrelated", Kind: "depends_on"},
		{Workspace: "foreign", SrcType: core.LineageRequirement, SrcID: "req", DstType: core.LineageTask, DstID: "foreign", Kind: "serves"},
	}
	budget := core.LineageTraversalBudget{Workspace: "demo", MaxDepth: 3, MaxNodes: 20, MaxLinks: 10}
	got := boundedRequirementDeliveryLinks(links, "req", budget)
	if len(got) != 3 || got[0].Kind != "serves" || got[1].Kind != "versions" || got[2].Kind != "materializes" {
		t.Fatalf("delivery path=%+v", got)
	}
	budget.MaxDepth = 1
	if got = boundedRequirementDeliveryLinks(links, "req", budget); len(got) != 1 {
		t.Fatalf("depth boundary=%+v", got)
	}
	budget.MaxDepth = 3
	budget.MaxLinks = 1
	if got = boundedRequirementDeliveryLinks(links, "req", budget); len(got) != 2 {
		t.Fatalf("overflow sentinel boundary=%+v", got)
	}
}
