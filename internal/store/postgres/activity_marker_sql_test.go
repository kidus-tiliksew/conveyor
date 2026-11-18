package postgres

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestMarkerFoldQueryShape pins the PostgreSQL marker statements: every
// numbered placeholder from $1 to $n meets one argument, no snapshot body
// key is named outside the compact payload, and a task whose open review order ID needs JSON escaping
// keeps every claim.
func TestMarkerFoldQueryShape(t *testing.T) {
	in := store.MarkerFoldInput{Workspace: "ws", Page: []string{"task"}, ReviewTasks: []string{"task"}, LiveReviewTasks: []string{"task"},
		OpenReviewOrders: []store.MarkerReviewOrder{{TaskID: "task", ID: "order-1", JobID: "job-1"}, {TaskID: "task", ID: `odd"id`, JobID: "job-2"}}, Now: time.Unix(10, 5)}
	placeholder := regexp.MustCompile(`\$([0-9]+)`)
	check := func(name, query string, args []any) {
		t.Helper()
		highest := 0
		for _, match := range placeholder.FindAllStringSubmatch(query, -1) {
			n, _ := strconv.Atoi(match[1])
			highest = max(highest, n)
		}
		if highest != len(args) {
			t.Errorf("%s statement uses placeholders up to $%d for %d arguments", name, highest, len(args))
		}
		// The compact payload column names the snapshot keys only to delete them.
		outside := strings.ReplaceAll(query, markerCompact, "")
		if strings.Contains(outside, "governance_snapshot'") || strings.Contains(outside, "served_requirement_snapshot'") {
			t.Errorf("%s statement names a snapshot body key outside the compact payload", name)
		}
	}
	query, args := markerFoldQuery(in)
	check("open review fold", query, args)
	if !strings.Contains(query, "AND NOT (task_id=ANY(") {
		t.Fatal("a task with an order ID outside the safe alphabet is not excluded from the claim reduction")
	}
	plain := in
	plain.LiveReviewTasks, plain.OpenReviewOrders = nil, nil
	query, args = markerFoldQuery(plain)
	check("fold", query, args)
	query, args = markerClaimHistoryQuery(in, []store.MarkerClaimPartition{{TaskID: "task", JobID: "job-1"}})
	check("claim history", query, args)
}
