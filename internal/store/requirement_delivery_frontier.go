package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// FetchRequirementDeliveryLineage shares source adjacency, not traversal budgets,
// between roots. MaxNodes remains validation-only, as in the original walk.
func FetchRequirementDeliveryLineage(ctx context.Context, ids []string, budget core.LineageTraversalBudget, fetch func(context.Context, []core.LineageNode, int) (map[core.LineageNode][]core.LineageLink, error)) (map[string][]core.LineageLink, error) {
	out := make(map[string][]core.LineageLink, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	if budget.MaxDepth < 0 || budget.MaxNodes <= 0 || budget.MaxLinks <= 0 {
		return nil, fmt.Errorf("requirement delivery lineage requires bounded depth, nodes, and links")
	}
	type walk struct {
		id       string
		frontier []core.LineageNode
		seen     map[core.LineageNode]bool
		links    []core.LineageLink
	}
	walks := make([]walk, 0, len(ids))
	roots := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("requirement id required")
		}
		if roots[id] {
			continue
		}
		roots[id] = true
		root := core.LineageNode{Type: core.LineageRequirement, ID: id}
		walks = append(walks, walk{id: id, frontier: []core.LineageNode{root}, seen: map[core.LineageNode]bool{root: true}})
	}
	adjacency := map[core.LineageNode][]core.LineageLink{}
	for range budget.MaxDepth {
		needed := []core.LineageNode{}
		requested := map[core.LineageNode]bool{}
		for _, w := range walks {
			if len(w.links) > budget.MaxLinks {
				continue
			}
			for _, node := range w.frontier {
				if _, ok := adjacency[node]; !ok && !requested[node] {
					requested[node] = true
					needed = append(needed, node)
				}
			}
		}
		sort.Slice(needed, func(i, j int) bool {
			if needed[i].Type != needed[j].Type {
				return needed[i].Type < needed[j].Type
			}
			return needed[i].ID < needed[j].ID
		})
		for start := 0; start < len(needed); start += 100 {
			end := min(start+100, len(needed))
			selected, err := fetch(ctx, needed[start:end], budget.MaxLinks+1)
			if err != nil {
				return nil, err
			}
			for _, node := range needed[start:end] {
				links := selected[node]
				sort.Slice(links, func(i, j int) bool { return lineageLinkKey(links[i]) < lineageLinkKey(links[j]) })
				adjacency[node] = links
			}
		}
		active := false
		for i := range walks {
			w := &walks[i]
			next := []core.LineageNode{}
			if len(w.links) > budget.MaxLinks {
				continue
			}
			for _, node := range w.frontier {
				for _, link := range adjacency[node] {
					w.links = append(w.links, link)
					if len(w.links) > budget.MaxLinks {
						break
					}
					dst := core.LineageNode{Type: link.DstType, ID: link.DstID}
					if !w.seen[dst] {
						w.seen[dst] = true
						next = append(next, dst)
					}
				}
				if len(w.links) > budget.MaxLinks {
					break
				}
			}
			w.frontier = next
			active = active || len(next) > 0 && len(w.links) <= budget.MaxLinks
		}
		if !active {
			break
		}
	}
	for _, w := range walks {
		if len(w.links) > 0 {
			out[w.id] = w.links
		}
	}
	return out, nil
}
