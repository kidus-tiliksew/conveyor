package core

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
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
	foreignA := lineageTestLink(2, LineageTask, "task", LineageTask, "foreign-a", "depends_on")
	foreignA.Workspace = "other"
	foreignB := lineageTestLink(3, LineageTask, "unreachable", LineageTask, "foreign-b", "depends_on")
	foreignB.Workspace = "other"
	unreachable := lineageTestLink(4, LineageTask, "unreachable", LineageTask, "also-unreachable", "depends_on")
	unreachable.Workspace = "demo"
	got, err := TraverseLineage([]LineageLink{legacy, foreignA, foreignB, unreachable}, []LineageNode{{Type: LineageRequirement, ID: "req"}}, LineageTraversalBudget{MaxDepth: 3, MaxNodes: 8, Workspace: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 2 || got.Nodes[1].ID != "task" || len(got.Links) != 1 || got.ForeignWorkspaceLinksIgnored != 2 {
		t.Fatalf("mixed traversal=%+v", got)
	}
}

func TestTraverseLineageUsesRelationPriorityAndIsPermutationStable(t *testing.T) {
	root := []LineageNode{{Type: LineageTask, ID: "child"}}
	links := []LineageLink{
		lineageTestLink(1, LineageTask, "child", LineageEvidence, "evidence", "supports"),
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

// A requirement serving 40 tasks must not spend the node budget before the
// root's own work orders admit their verdict: the hub admits at most its
// fan-out, and each depth admits in rounds (component-lineage;
// req-260802-72fc68 AC-2.3, AC-2.4).
func TestTraverseLineageCapsHubFanoutAndAdmitsByRound(t *testing.T) {
	root := LineageNode{Type: LineageTask, ID: "root"}
	links := []LineageLink{lineageTestLink(1, LineageRequirement, "req", LineageTask, "root", "serves")}
	for index := 0; index < 40; index++ {
		links = append(links, lineageTestLink(int64(2+index), LineageRequirement, "req", LineageTask, fmt.Sprintf("sibling-%02d", index), "serves"))
	}
	links = append(links,
		lineageTestLink(50, LineageTask, "root", LineageWorkOrder, "root-implement", "dispatches"),
		lineageTestLink(51, LineageTask, "root", LineageWorkOrder, "root-review", "dispatches"),
		lineageTestLink(52, LineageWorkOrder, "root-review", LineageVerdict, "review:root-review", "produced_verdict"),
		lineageTestLink(53, LineageTask, "root", LineagePullRequest, "conveyor#7", "submitted_as"),
		lineageTestLink(54, LineageTask, "root", LineageCommitRange, "conveyor@a..b", "submitted_range"),
	)
	own := []LineageNode{
		{Type: LineageWorkOrder, ID: "root-implement"}, {Type: LineageWorkOrder, ID: "root-review"},
		{Type: LineageVerdict, ID: "review:root-review"}, {Type: LineagePullRequest, ID: "conveyor#7"},
		{Type: LineageCommitRange, ID: "conveyor@a..b"},
	}
	permuted := append([]LineageLink(nil), links...)
	rand.New(rand.NewSource(7)).Shuffle(len(permuted), func(i, j int) { permuted[i], permuted[j] = permuted[j], permuted[i] })
	for _, budget := range []LineageTraversalBudget{{MaxDepth: 5, MaxNodes: 32, MaxLinks: 128}, {MaxDepth: 5, MaxNodes: 10, MaxLinks: 40, MaxFanout: 100}} {
		got, err := TraverseLineage(links, []LineageNode{root}, budget)
		if err != nil {
			t.Fatal(err)
		}
		again, err := TraverseLineage(permuted, []LineageNode{root}, budget)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Nodes, again.Nodes) || !reflect.DeepEqual(got.Links, again.Links) || got.Omissions != again.Omissions {
			t.Fatalf("permuted walk differs: %+v vs %+v", got, again)
		}
		reached := map[LineageNode]bool{}
		siblings := 0
		for _, node := range got.Nodes {
			reached[node] = true
			if node.Type == LineageTask && node.ID != "root" {
				siblings++
			}
		}
		for _, node := range own {
			if !reached[node] {
				t.Fatalf("budget %+v lost the root's own %v: nodes=%v", budget, node, got.Nodes)
			}
		}
		if !got.Truncated {
			t.Fatalf("budget %+v walk not truncated: %+v", budget, got)
		}
		if budget.MaxFanout == 0 {
			// Default fan-out 8: the hub's first link returns to the root, so
			// it admits seven siblings and reports the cap.
			if siblings != 7 || got.Budget.MaxFanout != 8 || got.Omissions.FanoutNodes != 1 || !reflect.DeepEqual(got.ExhaustionReasons, []string{"fanout"}) {
				t.Fatalf("default fan-out walk siblings=%d budget=%+v omissions=%+v reasons=%v", siblings, got.Budget, got.Omissions, got.ExhaustionReasons)
			}
			continue
		}
		// Without an effective fan-out cap the node cap binds, and round
		// admission still admits the verdict before the hub's siblings.
		if len(got.Nodes) != 10 || got.Omissions.FanoutNodes != 0 || got.Omissions.NodeCapNodes == 0 || got.OmittedNodes != got.Omissions.NodeCapNodes || !reflect.DeepEqual(got.ExhaustionReasons, []string{"nodes"}) {
			t.Fatalf("node-capped walk nodes=%v omissions=%+v reasons=%v", got.Nodes, got.Omissions, got.ExhaustionReasons)
		}
	}
}

// FetchLineageNeighborhood must return a link set that traverses exactly as
// the whole graph does, within the documented row bound (component-lineage).
func TestFetchLineageNeighborhoodMatchesFullTraversal(t *testing.T) {
	random := rand.New(rand.NewSource(42))
	types := []LineageNodeType{LineageTask, LineageRequirement, LineageWorkOrder, LineageVerdict, LineageSystemDesignVersion, LineageRepositoryPath}
	kinds := []string{"serves", "depends_on", "dispatches", "produced_verdict", "consulted", "governs", "supports", "materializes"}
	nodes := make([]LineageNode, 120)
	for index := range nodes {
		nodes[index] = LineageNode{Type: types[index%len(types)], ID: fmt.Sprintf("n%03d", index)}
	}
	seenKeys := map[string]bool{}
	var links []LineageLink
	add := func(id int64, src, dst LineageNode, kind string) {
		link := lineageTestLink(id, src.Type, src.ID, dst.Type, dst.ID, kind)
		// Shared timestamps exercise the event-ID and node-key tie-breaks.
		link.CreatedAt = time.Unix(id/5, 0).UTC()
		if key := lineageTraversalLinkKey(link); src != dst && !seenKeys[key] {
			seenKeys[key] = true
			links = append(links, link)
		}
	}
	for id := int64(1); id <= 600; id++ {
		src, dst := nodes[random.Intn(len(nodes))], nodes[random.Intn(len(nodes))]
		// Three hubs collect most edges.
		if id%3 == 0 {
			src = nodes[int(id/3)%3]
		}
		add(id, src, dst, kinds[random.Intn(len(kinds))])
	}
	adjacent := map[LineageNode][]lineageNeighbor{}
	for _, link := range links {
		src, dst := LineageNode{Type: link.SrcType, ID: link.SrcID}, LineageNode{Type: link.DstType, ID: link.DstID}
		adjacent[src] = append(adjacent[src], lineageNeighbor{node: dst, link: link})
		adjacent[dst] = append(adjacent[dst], lineageNeighbor{node: src, link: link})
	}
	for node := range adjacent {
		sortLineageNeighbors(adjacent[node])
	}
	rows, calls := 0, 0
	fetch := func(_ context.Context, level []LineageNode, limit int) (map[LineageNode][]LineageLink, error) {
		calls++
		out := map[LineageNode][]LineageLink{}
		for _, node := range level {
			for index, neighbor := range adjacent[node] {
				if index == limit {
					break
				}
				out[node] = append(out[node], neighbor.link)
				rows++
			}
		}
		return out, nil
	}
	for _, budget := range []LineageTraversalBudget{
		{MaxDepth: 5, MaxNodes: 32, MaxLinks: 128},
		{MaxDepth: 2, MaxNodes: 64, MaxLinks: 64},
		{MaxDepth: 4, MaxNodes: 16, MaxLinks: 48, MaxFanout: 3},
		{MaxDepth: 0, MaxNodes: 4},
	} {
		for _, roots := range [][]LineageNode{{nodes[0]}, {nodes[7], nodes[1]}, {nodes[50]}} {
			budget = budget.Normalized()
			want, err := TraverseLineage(links, roots, budget)
			if err != nil {
				t.Fatal(err)
			}
			rows, calls = 0, 0
			fetched, err := FetchLineageNeighborhood(t.Context(), roots, budget, fetch)
			if err != nil {
				t.Fatal(err)
			}
			if bound := len(roots)*(max(budget.MaxNodes, budget.MaxFanout)+1) + budget.MaxNodes*(budget.MaxFanout+1); rows > bound || calls > budget.MaxDepth+1 {
				t.Fatalf("budget %+v fetched %d rows in %d calls, bound %d rows in %d calls", budget, rows, calls, bound, budget.MaxDepth+1)
			}
			// A fetch with a larger depth and node cap and the same fan-out
			// traverses identically, so callers may fetch with headroom.
			wider := budget
			wider.MaxDepth++
			wider.MaxNodes++
			widerLinks, err := FetchLineageNeighborhood(t.Context(), roots, wider, fetch)
			if err != nil {
				t.Fatal(err)
			}
			for _, input := range [][]LineageLink{fetched, widerLinks} {
				got, err := TraverseLineage(input, roots, budget)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.Nodes, want.Nodes) || !reflect.DeepEqual(got.Links, want.Links) || !reflect.DeepEqual(got.Depths, want.Depths) ||
					!reflect.DeepEqual(got.Paths, want.Paths) || got.Omissions != want.Omissions || got.OmittedNodes != want.OmittedNodes ||
					got.Truncated != want.Truncated || !reflect.DeepEqual(got.ExhaustionReasons, want.ExhaustionReasons) {
					t.Fatalf("budget %+v roots %v: fetched walk differs\ngot  %+v\nwant %+v", budget, roots, got, want)
				}
			}
		}
	}
}

func TestLineageRelationRankSQLRendersCoreRanking(t *testing.T) {
	rendered := LineageRelationRankSQL("l.kind")
	if !strings.HasPrefix(rendered, "CASE l.kind WHEN ") || !strings.HasSuffix(rendered, fmt.Sprintf(" ELSE %d END", LineageRelationDefaultPriority)) {
		t.Fatalf("rendered CASE=%s", rendered)
	}
	for _, kind := range LineageRankedRelationKinds() {
		if !strings.Contains(rendered, fmt.Sprintf("WHEN '%s' THEN %d", kind, LineageRelationPriority(kind))) {
			t.Fatalf("rendered CASE misses %s: %s", kind, rendered)
		}
	}
	for _, nodeType := range LineageNodeTypes() {
		if !nodeType.Valid() {
			t.Fatalf("listed node type %q is invalid", nodeType)
		}
	}
}

func TestLineageRelationPriorityRanksOnlyProjectedKinds(t *testing.T) {
	want := map[string]int{
		"serves": 0, "materializes": 1, "supersedes": 1, "depends_on": 2,
		"produced_verdict": 3, "merged_range": 3, "submitted_range": 3, "submitted_as": 3,
		"supports": 4,
	}
	kinds := LineageRankedRelationKinds()
	if len(kinds) != len(want) {
		t.Fatalf("ranked kinds=%v want %v", kinds, want)
	}
	for _, kind := range kinds {
		if priority, ok := want[kind]; !ok || LineageRelationPriority(kind) != priority {
			t.Fatalf("kind %q priority=%d want %d (ranked=%v)", kind, LineageRelationPriority(kind), want[kind], ok)
		}
	}
	// No projector case produces proved_by, so it ranks with every other kind.
	for _, kind := range []string{"proved_by", "dispatches", "historical_feature_assignment", ""} {
		if got := LineageRelationPriority(kind); got != LineageRelationDefaultPriority {
			t.Fatalf("kind %q priority=%d want default %d", kind, got, LineageRelationDefaultPriority)
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
		lineageTestLink(3, LineageTask, "task-b", LineageEvidence, "evidence-b", "supports"),
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
