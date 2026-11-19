package postgres

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestMarkerFoldQueryShape pins the PostgreSQL marker statements: the
// placeholders read $1 to $n in text order, one per argument, no snapshot body
// key is named outside the compact payload, the review-round kinds are
// selected, kept for review tasks, and projected, and a task whose open
// review order ID needs JSON escaping keeps every claim.
func TestMarkerFoldQueryShape(t *testing.T) {
	in := store.MarkerFoldInput{Workspace: "ws", Page: []string{"task"}, ReviewTasks: []string{"task"}, LiveReviewTasks: []string{"task"},
		OpenReviewOrders: []store.MarkerReviewOrder{{TaskID: "task", ID: "order-1", JobID: "job-1"}, {TaskID: "task", ID: `odd"id`, JobID: "job-2"}}, Now: time.Unix(10, 5)}
	placeholder := regexp.MustCompile(`\$([0-9]+)`)
	check := func(name, query string, args []any) {
		t.Helper()
		// The binder numbers every occurrence in text order, so the
		// placeholders must read $1, $2, ... $n with n the argument count.
		matches := placeholder.FindAllStringSubmatch(query, -1)
		if len(matches) != len(args) {
			t.Errorf("%s statement holds %d placeholders for %d arguments", name, len(matches), len(args))
		}
		for i, match := range matches {
			if n, _ := strconv.Atoi(match[1]); n != i+1 {
				t.Fatalf("%s statement's placeholder %d is $%d, want $%d", name, i+1, n, i+1)
			}
		}
		// The compact payload column names the snapshot keys only to delete them.
		outside := strings.ReplaceAll(query, markerCompact, "")
		if strings.Contains(outside, "governance_snapshot'") || strings.Contains(outside, "served_requirement_snapshot'") {
			t.Errorf("%s statement names a snapshot body key outside the compact payload", name)
		}
		// The review recovery folds read the review-round kinds of review
		// tasks, and each kind is projected to the keys its decoder reads.
		for _, kind := range store.MarkerReviewRoundKinds {
			if strings.Count(query, "'"+kind+"'") < 3 {
				t.Errorf("%s statement does not select, filter, and project %s", name, kind)
			}
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
	full := in
	full.KeepAllClaims = true
	query, args = markerFoldQuery(full)
	check("keep-all-claims fold", query, args)
	if strings.Contains(query, "claim_horizon_rank>1") {
		t.Fatal("the keep-all-claims statement still reduces claims")
	}
}
