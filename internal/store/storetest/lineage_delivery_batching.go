package storetest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Shared-blueprint delivery graph shape: every blueprint and every blueprint
// version sits at one depth, so each of those depths holds more than one
// 100-source fetch batch.
const (
	sharedDeliveryBlueprints     = 110
	sharedDeliveryMaterializeGap = 4
	sharedDeliveryDirectTasks    = 3
	deliveryFetchBatch           = 100
)

// assertSharedBlueprintDeliveryWalks proves the batched delivery walk on the
// backend under test (component-lineage): several requirement roots share
// more than 100 blueprints and blueprint versions at one depth. The batched
// read, the single-root read, and an independent walk over every workspace
// link return the same ordered links under several budgets. On a backend
// with a read ledger, the batched read fetches each distinct eligible source
// once, never fetches a task, and spends at most ceil(sources/100)
// statements per depth.
func assertSharedBlueprintDeliveryWalks(t *testing.T, fixture LineageFixture) {
	t.Helper()
	st, ctx, workspace := fixture.Store, fixture.Context, fixture.Workspace
	roots := make([]string, 4)
	versions := make([]int, len(roots))
	for i := range roots {
		requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: fmt.Sprintf("req-shared-delivery-%d-%s", i, core.NewTaskID()), Title: fmt.Sprintf("Shared delivery %d", i)}, core.RequirementVersion{
			Content: "# Shared delivery.", Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Deliver through shared blueprints."}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
			t.Fatal(err)
		}
		roots[i], versions[i] = requirement.ID, version.Version
	}
	newTask := func(title, parent string) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: workspace, Title: title, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
		if parent != "" {
			task.ParentTaskID, task.OriginSpecVersion = parent, 1
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	appendEvent := func(event core.Event) {
		t.Helper()
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	// Root 0 serves every blueprint; roots 1 to 3 share overlapping slices
	// of them, so most sources are reached from more than one root.
	servedBy := func(index int) []int {
		served := []int{0}
		if index < 30 {
			served = append(served, 1)
		}
		if index >= sharedDeliveryBlueprints-30 {
			served = append(served, 2)
		}
		if index < 5 {
			served = append(served, 3)
		}
		return served
	}
	for index := range sharedDeliveryBlueprints {
		blueprint := newTask(fmt.Sprintf("Shared blueprint %d", index), "")
		for _, root := range servedBy(index) {
			appendEvent(core.Event{TaskID: blueprint.ID, Kind: "requirement.serves_confirmed", Payload: core.JSONPayload(map[string]any{"requirement_id": roots[root]})})
		}
		appendEvent(core.Event{TaskID: blueprint.ID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": 1})})
		if index%sharedDeliveryMaterializeGap == 0 {
			newTask(fmt.Sprintf("Materialized delivery %d", index), blueprint.ID)
		}
	}
	for index := range sharedDeliveryDirectTasks {
		task := newTask(fmt.Sprintf("Direct delivery %d", index), "")
		appendEvent(core.Event{TaskID: task.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": roots[3], "version": versions[3]})})
	}

	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adjacency := referenceDeliveryAdjacency(links, workspace)
	// Unbounded links: the walk reaches tasks at depth 3, and the fourth
	// depth would fetch them if tasks were sources.
	full := core.LineageTraversalBudget{Workspace: workspace, MaxDepth: 4, MaxNodes: 1 << 16, MaxLinks: 1 << 16}
	sources := referenceDeliverySourcesByDepth(adjacency, roots, full.MaxDepth)
	if len(sources) < 3 || len(sources[1]) <= deliveryFetchBatch || len(sources[2]) <= deliveryFetchBatch {
		t.Fatalf("fixture sources per depth = %v, want more than %d sources at depths 1 and 2", sourceCounts(sources), deliveryFetchBatch)
	}
	wantStatements, wantRows := uint64(0), uint64(0)
	for _, depth := range sources {
		wantStatements += uint64((len(depth) + deliveryFetchBatch - 1) / deliveryFetchBatch)
		for _, node := range depth {
			wantRows += uint64(min(len(adjacency[node]), full.MaxLinks+1))
		}
	}
	accounted := fixture.DrainReadQueries != nil && fixture.DrainReadRows != nil
	if accounted {
		fixture.DrainReadQueries()
		fixture.DrainReadRows()
	}
	batched, err := st.ListRequirementDeliveryLineageByRequirement(ctx, roots, full)
	if err != nil {
		t.Fatal(err)
	}
	if accounted {
		// Each depth is ceil(new sources/100) statements, and the rows equal
		// one fetch of each distinct eligible source: a repeated source, a
		// fetched task, or an extra batch raises one of the two.
		label := "ListRequirementDeliveryLineageByRequirement"
		if got := fixture.DrainReadQueries()[label]; got != wantStatements {
			t.Fatalf("batched delivery statements = %d, want %d for sources per depth %v", got, wantStatements, sourceCounts(sources))
		}
		if got := fixture.DrainReadRows()[label]; got != wantRows {
			t.Fatalf("batched delivery rows = %d, want %d (one fetch per distinct source)", got, wantRows)
		}
	}
	assertDeliveryWalksAgree(t, st, ctx, adjacency, roots, full, batched)

	for _, budget := range []core.LineageTraversalBudget{
		{Workspace: workspace, MaxDepth: 1, MaxNodes: 1 << 16, MaxLinks: 1 << 16},
		{Workspace: workspace, MaxDepth: 2, MaxNodes: 1 << 16, MaxLinks: 120},
		{Workspace: workspace, MaxDepth: 4, MaxNodes: 1, MaxLinks: 50},
		{Workspace: workspace, MaxDepth: 4, MaxNodes: 1, MaxLinks: 1},
	} {
		got, err := st.ListRequirementDeliveryLineageByRequirement(ctx, append([]string{roots[1]}, roots...), budget)
		if err != nil {
			t.Fatal(err)
		}
		assertDeliveryWalksAgree(t, st, ctx, adjacency, roots, budget, got)
	}
	overflow := core.LineageTraversalBudget{Workspace: workspace, MaxDepth: 4, MaxNodes: 1, MaxLinks: 50}
	capped, err := st.ListRequirementDeliveryLineageByRequirement(ctx, roots, overflow)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(capped[roots[0]]); got != overflow.MaxLinks+1 {
		t.Fatalf("over-budget root returned %d links, want the %d-link overflow sentinel", got, overflow.MaxLinks+1)
	}
	if got, want := len(capped[roots[3]]), len(referenceDeliveryWalk(adjacency, roots[3], full)); got != want || want > overflow.MaxLinks {
		t.Fatalf("within-budget root returned %d links, want its whole %d-link walk beside an over-budget root", got, want)
	}
	if foreign, err := st.ListRequirementDeliveryLineageByRequirement(fixture.ForeignContext, roots, full); err != nil || len(foreign) != 0 {
		t.Fatalf("cross-workspace batched delivery lineage leaked: links=%v err=%v", foreign, err)
	}
}

// assertDeliveryWalksAgree compares the batched result for every root with
// the single-root read and the reference walk, link by link and in order.
func assertDeliveryWalksAgree(t *testing.T, st store.Store, ctx context.Context, adjacency map[core.LineageNode][]core.LineageLink, roots []string, budget core.LineageTraversalBudget, batched map[string][]core.LineageLink) {
	t.Helper()
	for _, root := range roots {
		want := deliveryLinkKeys(referenceDeliveryWalk(adjacency, root, budget))
		single, err := st.ListRequirementDeliveryLineage(ctx, root, budget)
		if err != nil {
			t.Fatal(err)
		}
		if got := deliveryLinkKeys(batched[root]); !equalStrings(got, want) {
			t.Fatalf("budget %+v root %s: batched walk has %d links, reference walk %d\nbatched=%v\nreference=%v", budget, root, len(got), len(want), got, want)
		}
		if got := deliveryLinkKeys(single); !equalStrings(got, want) {
			t.Fatalf("budget %+v root %s: single-root walk has %d links, reference walk %d", budget, root, len(got), len(want))
		}
	}
}

// referenceDeliveryAdjacency groups every delivery link of the workspace by
// source in destination order. It restates the delivery edge kinds rather
// than calling the production filter.
func referenceDeliveryAdjacency(links []core.LineageLink, workspace string) map[core.LineageNode][]core.LineageLink {
	adjacency := map[core.LineageNode][]core.LineageLink{}
	for _, link := range links {
		delivery := link.Kind == "serves" && link.SrcType == core.LineageRequirement && (link.DstType == core.LineageTask || link.DstType == core.LineageBlueprint) ||
			link.Kind == "versions" && link.SrcType == core.LineageBlueprint && link.DstType == core.LineageBlueprintVersion ||
			link.Kind == "materializes" && link.SrcType == core.LineageBlueprintVersion && link.DstType == core.LineageTask
		if link.Workspace != workspace || !delivery {
			continue
		}
		src := core.LineageNode{Type: link.SrcType, ID: link.SrcID}
		adjacency[src] = append(adjacency[src], link)
	}
	for src := range adjacency {
		sort.Slice(adjacency[src], func(i, j int) bool {
			a, b := adjacency[src][i], adjacency[src][j]
			if a.DstType != b.DstType {
				return a.DstType < b.DstType
			}
			if a.DstID != b.DstID {
				return a.DstID < b.DstID
			}
			return a.Kind < b.Kind
		})
	}
	return adjacency
}

// referenceDeliveryWalk is a breadth-first walk from one requirement: each
// visited node adds every delivery link in destination order until the walk
// holds MaxLinks+1 links, the overflow sentinel.
func referenceDeliveryWalk(adjacency map[core.LineageNode][]core.LineageLink, root string, budget core.LineageTraversalBudget) []core.LineageLink {
	type step struct {
		node  core.LineageNode
		depth int
	}
	start := core.LineageNode{Type: core.LineageRequirement, ID: root}
	queue := []step{{node: start}}
	seen := map[core.LineageNode]bool{start: true}
	var out []core.LineageLink
	for len(queue) > 0 && len(out) <= budget.MaxLinks {
		current := queue[0]
		queue = queue[1:]
		if current.depth >= budget.MaxDepth {
			continue
		}
		for _, link := range adjacency[current.node] {
			out = append(out, link)
			if len(out) > budget.MaxLinks {
				break
			}
			dst := core.LineageNode{Type: link.DstType, ID: link.DstID}
			if !seen[dst] {
				seen[dst] = true
				queue = append(queue, step{node: dst, depth: current.depth + 1})
			}
		}
	}
	return out
}

// referenceDeliverySourcesByDepth lists, per depth below maxDepth, the
// distinct non-task nodes that some root first reaches at that depth and no
// earlier depth fetched: the sources one batched walk must fetch there.
func referenceDeliverySourcesByDepth(adjacency map[core.LineageNode][]core.LineageLink, roots []string, maxDepth int) [][]core.LineageNode {
	frontiers := make([][]core.LineageNode, len(roots))
	seen := make([]map[core.LineageNode]bool, len(roots))
	for i, root := range roots {
		node := core.LineageNode{Type: core.LineageRequirement, ID: root}
		frontiers[i], seen[i] = []core.LineageNode{node}, map[core.LineageNode]bool{node: true}
	}
	fetched := map[core.LineageNode]bool{}
	var out [][]core.LineageNode
	for range maxDepth {
		depth := []core.LineageNode{}
		for _, frontier := range frontiers {
			for _, node := range frontier {
				if node.Type != core.LineageTask && !fetched[node] {
					fetched[node] = true
					depth = append(depth, node)
				}
			}
		}
		if len(depth) > 0 {
			out = append(out, depth)
		}
		active := false
		for i, frontier := range frontiers {
			next := []core.LineageNode{}
			for _, node := range frontier {
				for _, link := range adjacency[node] {
					dst := core.LineageNode{Type: link.DstType, ID: link.DstID}
					if !seen[i][dst] {
						seen[i][dst] = true
						next = append(next, dst)
					}
				}
			}
			frontiers[i] = next
			active = active || len(next) > 0
		}
		if !active {
			break
		}
	}
	return out
}

func sourceCounts(depths [][]core.LineageNode) []int {
	counts := make([]int, len(depths))
	for i, depth := range depths {
		counts[i] = len(depth)
	}
	return counts
}

func deliveryLinkKeys(links []core.LineageLink) []string {
	keys := make([]string, len(links))
	for i, link := range links {
		keys[i] = strings.Join([]string{link.Workspace, string(link.SrcType), link.SrcID, string(link.DstType), link.DstID, link.Kind}, "|")
	}
	return keys
}
