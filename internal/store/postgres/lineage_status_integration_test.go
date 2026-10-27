package postgres

import (
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestLineageStatusReadsOnlyNamedVersionRowsIntegration proves a status read
// returns only the version rows its version nodes name: a document root
// reads none of its 40 versions, and two version nodes read two rows
// (component-lineage).
func TestLineageStatusReadsOnlyNamedVersionRowsIntegration(t *testing.T) {
	st, ctx, _ := newPhase61IntegrationStore(t)
	defer st.Close()
	ctx = store.WithActor(ctx, store.Actor{ID: "operator-lineage-status", Role: core.ActorHuman})
	id := storetest.SeedLineageVersionHistory(t, st, ctx, 40)
	var mu sync.Mutex
	var fetched []int
	observe := func(documentID string, version int) {
		mu.Lock()
		defer mu.Unlock()
		if documentID == id {
			fetched = append(fetched, version)
		}
	}
	observeLineageStatusVersionRow.Store(&observe)
	defer observeLineageStatusVersionRow.Store(nil)
	root := core.LineageNode{Type: core.LineageRequirement, ID: id}
	version := func(number int) core.LineageNode {
		return core.LineageNode{Type: core.LineageRequirementVersion, ID: core.RequirementVersionLineageID(id, number)}
	}
	for _, read := range []struct {
		nodes []core.LineageNode
		rows  []int
	}{
		{[]core.LineageNode{root}, nil},
		{[]core.LineageNode{root, version(3), version(3), version(40)}, []int{3, 40}},
	} {
		fetched = nil
		statuses, err := st.ListLineageNodeStatuses(ctx, read.nodes)
		if err != nil {
			t.Fatal(err)
		}
		sort.Ints(fetched)
		if !reflect.DeepEqual(fetched, read.rows) || statuses[root].CurrentVersion != 40 {
			t.Fatalf("nodes %v fetched version rows %v want %v; statuses=%+v", read.nodes, fetched, read.rows, statuses)
		}
	}
}
