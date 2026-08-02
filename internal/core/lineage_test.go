package core

import (
	"reflect"
	"testing"
	"time"
)

func TestLineageLinkRequiresEventProvenanceAndDistinctEndpoints(t *testing.T) {
	link := LineageLink{
		Workspace: "demo", SrcType: LineageTask, SrcID: "task-a",
		DstType: LineageTask, DstID: "task-b", Kind: "depends_on",
		CreatedByEventID: 7, CreatedAt: time.Now(),
	}
	if err := link.Validate(); err != nil {
		t.Fatal(err)
	}
	link.CreatedByEventID = 0
	if err := link.Validate(); err == nil {
		t.Fatal("lineage link without event provenance was accepted")
	}
	link.CreatedByEventID, link.DstID = 7, link.SrcID
	if err := link.Validate(); err == nil {
		t.Fatal("lineage self-link was accepted")
	}
	link.DstID, link.DstType = "task-b", LineageNodeType("unknown")
	if err := link.Validate(); err == nil {
		t.Fatal("unknown lineage node type was accepted")
	}
}

func TestLineageVersionIDsAreStable(t *testing.T) {
	if got := BlueprintVersionLineageID("task-1", 3); got != "task-1:v3" {
		t.Fatalf("blueprint version id=%q", got)
	}
	if got := RequirementVersionLineageID("req-1", 4); got != "req-1:v4" {
		t.Fatalf("requirement version id=%q", got)
	}
}

func TestTraverseLineageIsDeterministicAndBounded(t *testing.T) {
	links := []LineageLink{
		lineageTestLink(3, LineageTask, "task-b", LineageEvidence, "evidence-b", "proved_by"),
		lineageTestLink(1, LineageTask, "task-a", LineageWorkOrder, "order-a", "executes_as"),
		lineageTestLink(2, LineageTask, "task-a", LineageTask, "task-b", "depends_on"),
	}
	root := []LineageNode{{Type: LineageWorkOrder, ID: "order-a"}}
	got, err := TraverseLineage(links, root, LineageTraversalBudget{MaxDepth: 2, MaxNodes: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := []LineageNode{
		{Type: LineageWorkOrder, ID: "order-a"},
		{Type: LineageTask, ID: "task-a"},
		{Type: LineageTask, ID: "task-b"},
	}
	if !reflect.DeepEqual(got.Nodes, want) || !got.Truncated {
		t.Fatalf("traversal=%+v, want nodes=%+v truncated", got, want)
	}

	limited, err := TraverseLineage(links, root, LineageTraversalBudget{MaxDepth: 8, MaxNodes: 2})
	if err != nil || len(limited.Nodes) != 2 || !limited.Truncated {
		t.Fatalf("node-limited traversal=%+v err=%v", limited, err)
	}
	if _, err = TraverseLineage(links, root, LineageTraversalBudget{}); err == nil {
		t.Fatal("accepted an unbounded traversal budget")
	}
}

func TestSelectContextArtifactsUsesBoundedReachabilityAndStableOrder(t *testing.T) {
	now := time.Now().UTC()
	links := []LineageLink{
		lineageTestLink(1, LineageTask, "task-a", LineageTask, "task-b", "depends_on"),
		lineageTestLink(2, LineageTask, "task-a", LineageRequirement, "req-a", "serves"),
	}
	artifacts := []Artifact{
		{ID: "unrelated", TaskID: "task-c", CreatedAt: now.Add(-time.Minute)},
		{ID: "requirement", RequirementID: "req-a", CreatedAt: now},
		{ID: "sibling", TaskID: "task-b", CreatedAt: now.Add(-time.Second)},
	}
	selection, err := SelectContextArtifacts(links, []LineageNode{{Type: LineageTask, ID: "task-a"}}, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(selection.Artifacts))
	for _, artifact := range selection.Artifacts {
		ids = append(ids, artifact.ID)
	}
	if !reflect.DeepEqual(ids, []string{"sibling", "requirement"}) || selection.Truncated {
		t.Fatalf("selection=%+v ids=%v", selection, ids)
	}
}

func lineageTestLink(id int64, srcType LineageNodeType, srcID string, dstType LineageNodeType, dstID, kind string) LineageLink {
	return LineageLink{
		Workspace: "demo", SrcType: srcType, SrcID: srcID,
		DstType: dstType, DstID: dstID, Kind: kind,
		CreatedByEventID: id, CreatedAt: time.Unix(id, 0).UTC(),
	}
}
