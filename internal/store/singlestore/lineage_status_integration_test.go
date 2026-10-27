package singlestore

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestLineageStatusReadsOnlyNamedVersionRowsIntegration proves a status read
// returns only the version rows its version nodes name: a document root
// reads none of its 40 versions, and two version nodes read two rows
// (component-lineage).
func TestLineageStatusReadsOnlyNamedVersionRowsIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "lineage-status"), store.Actor{ID: "operator-lineage-status", Role: core.ActorHuman})
	if _, err := st.CreateWorkspace(ctx, "lineage-status", "Lineage status", &config.Config{Workspace: "lineage-status"}); err != nil {
		t.Fatal(err)
	}
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

// TestLineageVerdictStatusReadsOneRowPerRequestedOrderIntegration seeds a
// task with 60 other review.completed events and two for the first order.
// A status read returns one metadata row per requested verdict, the latest
// event's outcome, and nothing for another workspace (component-lineage).
func TestLineageVerdictStatusReadsOneRowPerRequestedOrderIntegration(t *testing.T) {
	st := integrationStore(t)
	workspace := "lineage-verdict"
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.Actor{ID: "operator-lineage-verdict", Role: core.ActorHuman})
	if _, err := st.CreateWorkspace(ctx, workspace, "Lineage verdict", &config.Config{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	foreign := store.WithWorkspace(ctx, workspace+"-foreign")
	_, orders := storetest.SeedLineageVerdictHistory(t, st, ctx, workspace, 3, 60)
	var mu sync.Mutex
	var fetched []string
	observe := func(orderID string) {
		mu.Lock()
		defer mu.Unlock()
		fetched = append(fetched, orderID)
	}
	observeLineageStatusVerdictRow.Store(&observe)
	defer observeLineageStatusVerdictRow.Store(nil)
	verdict := func(index int) core.LineageNode {
		return core.LineageNode{Type: core.LineageVerdict, ID: core.VerdictLineageID(orders[index])}
	}
	for _, read := range []struct {
		ctx   context.Context
		nodes []core.LineageNode
		rows  int
	}{
		{ctx, []core.LineageNode{verdict(0)}, 1},
		{ctx, []core.LineageNode{verdict(0), verdict(1), verdict(2), verdict(2)}, 3},
		{foreign, []core.LineageNode{verdict(0)}, 0},
	} {
		fetched = nil
		statuses, err := st.ListLineageNodeStatuses(read.ctx, read.nodes)
		if err != nil {
			t.Fatal(err)
		}
		if len(fetched) != read.rows || len(statuses) != read.rows {
			t.Fatalf("nodes %v fetched %d verdict rows (%v), want %d; statuses=%+v", read.nodes, len(fetched), fetched, read.rows, statuses)
		}
		if read.rows > 0 && statuses[verdict(0)] != (core.LineageNodeStatus{Outcome: "request_changes", ReviewRound: 1, ReviewSeat: 1}) {
			t.Fatalf("latest verdict status=%+v", statuses[verdict(0)])
		}
	}
}
