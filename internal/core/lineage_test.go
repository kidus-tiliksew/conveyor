package core

import (
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
