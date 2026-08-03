package core

import (
	"fmt"
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

func TestTraverseLineageKeepsLegacyHistoryAndDropsForeignWorkspace(t *testing.T) {
	legacy := LineageLink{Workspace: "demo", SrcType: LineageRequirement, SrcID: "req", DstType: LineageTask, DstID: "task", Kind: "historical_feature_assignment", LegacyCreatedByEvent: "feature.migrated", CreatedAt: time.Now().UTC()}
	foreign := lineageTestLink(2, LineageTask, "task", LineageTask, "foreign", "depends_on")
	foreign.Workspace = "other"
	got, err := TraverseLineage([]LineageLink{legacy, foreign}, []LineageNode{{Type: LineageRequirement, ID: "req"}}, LineageTraversalBudget{MaxDepth: 3, MaxNodes: 8, Workspace: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 2 || got.Nodes[1].ID != "task" || len(got.Links) != 1 || got.ForeignWorkspaceDropped != 1 {
		t.Fatalf("mixed traversal=%+v", got)
	}
}

func TestTraverseLineageUsesRelationPriorityAndIsPermutationStable(t *testing.T) {
	root := []LineageNode{{Type: LineageTask, ID: "child"}}
	links := []LineageLink{
		lineageTestLink(1, LineageTask, "child", LineageEvidence, "evidence", "proved_by"),
		lineageTestLink(3, LineageRequirement, "req", LineageTask, "child", "serves"),
		lineageTestLink(2, LineageTask, "child", LineageTask, "dependency", "depends_on"),
	}
	want := []LineageNode{{Type: LineageTask, ID: "child"}, {Type: LineageRequirement, ID: "req"}, {Type: LineageTask, ID: "dependency"}, {Type: LineageEvidence, ID: "evidence"}}
	permuted := []LineageLink{links[2], links[0], links[1]}
	for _, input := range [][]LineageLink{links, permuted} {
		got, err := TraverseLineage(input, root, LineageTraversalBudget{MaxDepth: 1, MaxNodes: 4, MaxLinks: 2})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Nodes, want) || len(got.Links) != 2 || got.OmittedLinks != 1 || !got.Truncated || !reflect.DeepEqual(got.ExhaustionReasons, []string{"links"}) {
			t.Fatalf("priority traversal=%+v", got)
		}
	}
}

func TestSelectContextArtifactsTiersDeduplicatesFiltersAndReportsOmitted(t *testing.T) {
	now := time.Now().UTC()
	links := []LineageLink{lineageTestLink(1, LineageTask, "local", LineageTask, "derived", "depends_on")}
	artifacts := []Artifact{
		{ID: "local-context", Role: ArtifactRoleTaskContext, TaskID: "local", CreatedAt: now.Add(-time.Hour)},
		{ID: "evidence", Role: ArtifactRoleVerificationEvidence, TaskID: "local", ContentType: "image/png", SizeBytes: 1, CreatedAt: now},
		{ID: "audit", Role: ArtifactRoleGeneratedAudit, TaskID: "local", CreatedAt: now},
	}
	for i := 0; i < 70; i++ {
		artifact := Artifact{ID: fmt.Sprintf("derived-%02d", i), Role: ArtifactRoleTaskContext, TaskID: "derived", CreatedAt: now.Add(time.Duration(i) * time.Second)}
		artifacts = append(artifacts, artifact)
		if i == 69 {
			artifacts = append(artifacts, artifact)
		}
	}
	selection, err := SelectContextArtifacts(links, []LineageNode{{Type: LineageTask, ID: "local"}}, artifacts, ContextArtifactSelectionOptions{Workspace: "demo", LocalTaskID: "local", IncludeLocalVerificationEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Artifacts) != DefaultContextArtifactMaxRefs || !selection.Truncated || selection.Omitted != 8 {
		t.Fatalf("selection count=%d truncated=%v omitted=%d", len(selection.Artifacts), selection.Truncated, selection.Omitted)
	}
	seen := map[string]bool{}
	for _, artifact := range selection.Artifacts {
		if seen[artifact.ID] {
			t.Fatalf("duplicate %s", artifact.ID)
		}
		seen[artifact.ID] = true
	}
	if !seen["local-context"] || !seen["evidence"] || seen["audit"] || !seen["derived-69"] || seen["derived-00"] {
		t.Fatalf("tiered IDs=%v", seen)
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
	if !reflect.DeepEqual(got.Roots, root) || len(got.Links) != 2 || got.Links[0].Kind != "executes_as" || got.Links[1].Kind != "depends_on" {
		t.Fatalf("graph roots/links=%+v/%+v", got.Roots, got.Links)
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
	if !reflect.DeepEqual(ids, []string{"requirement", "sibling"}) || selection.Truncated {
		t.Fatalf("selection=%+v ids=%v", selection, ids)
	}
}

func TestSelectContextArtifactsKeepsEvidenceAndPrefersNearerOlder(t *testing.T) {
	now := time.Now().UTC()
	links := []LineageLink{
		lineageTestLink(1, LineageTask, "local", LineageTask, "near", "depends_on"),
		lineageTestLink(2, LineageTask, "near", LineageTask, "far", "depends_on"),
	}
	artifacts := []Artifact{
		{ID: "far-new", Role: ArtifactRoleTaskContext, TaskID: "far", CreatedAt: now},
		{ID: "near-old", Role: ArtifactRoleTaskContext, TaskID: "near", CreatedAt: now.Add(-time.Hour)},
		{ID: "review-evidence", Role: ArtifactRoleVerificationEvidence, TaskID: "local", ContentType: "image/png", SizeBytes: 1, CreatedAt: now.Add(-2 * time.Hour)},
	}
	selection, err := SelectContextArtifacts(links, []LineageNode{{Type: LineageTask, ID: "local"}}, artifacts, ContextArtifactSelectionOptions{
		LocalTaskID: "local", IncludeLocalVerificationEvidence: true,
		Budget: LineageTraversalBudget{MaxDepth: 3, MaxNodes: 8}, MaxArtifactRefs: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{selection.Artifacts[0].ID, selection.Artifacts[1].ID}; !reflect.DeepEqual(got, []string{"review-evidence", "near-old"}) || selection.Omitted != 1 || !selection.Truncated {
		t.Fatalf("bounded tier selection=%+v ids=%v", selection, got)
	}
}

func TestSelectContextArtifactsReservesTaskLocalContextUnderEvidenceFanout(t *testing.T) {
	now := time.Now().UTC()
	artifacts := []Artifact{{ID: "local-context", Role: ArtifactRoleTaskContext, TaskID: "local", CreatedAt: now.Add(-time.Hour)}}
	for i := 0; i < DefaultContextArtifactMaxRefs+8; i++ {
		artifacts = append(artifacts, Artifact{ID: fmt.Sprintf("evidence-%02d", i), Role: ArtifactRoleVerificationEvidence, TaskID: "local", ContentType: "image/png", SizeBytes: 1, CreatedAt: now.Add(time.Duration(i) * time.Second)})
	}
	selection, err := SelectContextArtifacts(nil, []LineageNode{{Type: LineageTask, ID: "local"}}, artifacts, ContextArtifactSelectionOptions{
		LocalTaskID: "local", IncludeLocalVerificationEvidence: true,
		Budget: LineageTraversalBudget{MaxDepth: 1, MaxNodes: 2}, MaxArtifactRefs: DefaultContextArtifactMaxRefs,
	})
	if err != nil {
		t.Fatal(err)
	}
	seenLocal, evidence := false, 0
	for _, artifact := range selection.Artifacts {
		seenLocal = seenLocal || artifact.ID == "local-context"
		if artifact.EligibleVerificationEvidence() {
			evidence++
		}
	}
	if len(selection.Artifacts) != DefaultContextArtifactMaxRefs || !seenLocal || evidence == 0 || !selection.Truncated || selection.Omitted != 9 {
		t.Fatalf("protected selection count=%d local=%t evidence=%d truncated=%t omitted=%d", len(selection.Artifacts), seenLocal, evidence, selection.Truncated, selection.Omitted)
	}
}

func lineageTestLink(id int64, srcType LineageNodeType, srcID string, dstType LineageNodeType, dstID, kind string) LineageLink {
	return LineageLink{
		Workspace: "demo", SrcType: srcType, SrcID: srcID,
		DstType: dstType, DstID: dstID, Kind: kind,
		CreatedByEventID: id, CreatedAt: time.Unix(id, 0).UTC(),
	}
}
