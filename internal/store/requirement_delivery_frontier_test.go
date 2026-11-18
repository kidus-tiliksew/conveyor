package store

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestRequirementDeliveryFrontierMatchesFullWalk(t *testing.T) {
	link := func(src core.LineageNodeType, id string, dst core.LineageNodeType, target, kind string) core.LineageLink {
		return core.LineageLink{Workspace: "ws", SrcType: src, SrcID: id, DstType: dst, DstID: target, Kind: kind}
	}
	links := []core.LineageLink{
		link(core.LineageRequirement, "a", core.LineageBlueprint, "shared", "serves"),
		link(core.LineageRequirement, "b", core.LineageBlueprint, "shared", "serves"),
		link(core.LineageBlueprint, "shared", core.LineageBlueprintVersion, "v1", "versions"),
		link(core.LineageBlueprintVersion, "v1", core.LineageTask, "task", "materializes"),
		link(core.LineageTask, "task", core.LineageRequirement, "a", "serves"),
	}
	for i := range 250 {
		links = append(links, link(core.LineageRequirement, "a", core.LineageTask, fmt.Sprintf("fanout-%03d", i), "serves"))
		links = append(links, link(core.LineageRequirement, fmt.Sprintf("noise-%03d", i), core.LineageTask, "noise-task", "serves"))
	}
	for _, depth := range []int{0, 1, 2, 3, 9} {
		for _, maxLinks := range []int{1, 2, 10, 500} {
			budget := core.LineageTraversalBudget{Workspace: "ws", MaxDepth: depth, MaxNodes: 1, MaxLinks: maxLinks}
			fetched := map[core.LineageNode]int{}
			got, err := FetchRequirementDeliveryLineage(context.Background(), []string{"a", "b", "a", "missing"}, budget, func(_ context.Context, nodes []core.LineageNode, limit int) (map[core.LineageNode][]core.LineageLink, error) {
				out := map[core.LineageNode][]core.LineageLink{}
				for _, node := range nodes {
					fetched[node]++
					for _, edge := range links {
						if edge.SrcType == node.Type && edge.SrcID == node.ID && requirementDeliveryLink(edge) {
							out[node] = append(out[node], edge)
						}
					}
					sort.Slice(out[node], func(i, j int) bool { return lineageLinkKey(out[node][i]) < lineageLinkKey(out[node][j]) })
					out[node] = out[node][:min(limit, len(out[node]))]
				}
				return out, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"a", "b", "missing"} {
				want := boundedRequirementDeliveryLinks(links, id, budget)
				if len(want) == 0 && len(got[id]) == 0 {
					continue
				}
				if !reflect.DeepEqual(got[id], want) {
					t.Fatalf("depth %d budget %d root %s: got %#v want %#v", depth, maxLinks, id, got[id], want)
				}
			}
			for node, count := range fetched {
				if count != 1 {
					t.Fatalf("source %v fetched %d times", node, count)
				}
			}
		}
	}
}
