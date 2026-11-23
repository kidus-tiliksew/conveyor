package singlestore

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestDocumentEventRequirementDeliveryQuery checks the statement that
// ListRequirementDeliveryEventsForTasks sends. Each scan of
// events_columnstore filters on event_shard_key and requires task_id to equal
// it, so SingleStore prunes each scan to the named tasks' partitions
// (component-persistence). The shared RequirementDeliveryEvents conformance
// case proves the rows on a live SingleStore database.
func TestDocumentEventRequirementDeliveryQuery(t *testing.T) {
	const reviewScan = "WHERE x.workspace_id=? AND x.event_shard_key IN (%s) AND x.task_id=x.event_shard_key AND x.kind='review.round_completed'"
	const outerScan = "WHERE e.workspace_id=? AND e.event_shard_key IN (%s) AND e.task_id=e.event_shard_key AND e.kind IN ('merge.confirmed','merge.reconciled','review.round_completed','task.context_requirement_added','task.context_requirement_activated','task.context_requirement_removed')"
	const order = "\nORDER BY e.task_id,e.at,e.id"

	checkShape := func(t *testing.T, query, marks string) {
		t.Helper()
		review := strings.Replace(reviewScan, "%s", marks, 1)
		outer := strings.Replace(outerScan, "%s", marks, 1)
		if got := strings.Count(query, review); got != 1 {
			t.Fatalf("review subquery predicate %q occurs %d times, want 1 in:\n%s", review, got, query)
		}
		if got := strings.Count(query, outer); got != 1 {
			t.Fatalf("outer query predicate %q occurs %d times, want 1 in:\n%s", outer, got, query)
		}
		// The subquery's placeholders precede the outer query's, so the
		// arguments bind the review scan first.
		if strings.Index(query, review) > strings.Index(query, outer) {
			t.Fatalf("review subquery predicate follows the outer predicate in:\n%s", query)
		}
		if got := strings.Count(query, "IN ("+marks+")"); got != 2 {
			t.Fatalf("query has %d task ID lists %q, want 2 in:\n%s", got, marks, query)
		}
		if strings.Contains(query, "task_id IN (") {
			t.Fatalf("query still filters a scan by task_id IN (...):\n%s", query)
		}
		if !strings.HasSuffix(query, order) {
			t.Fatalf("query does not end with %q:\n%s", order, query)
		}
		if strings.Contains(query, "%s") {
			t.Fatalf("query keeps an unfilled %%s mark:\n%s", query)
		}
	}

	t.Run("duplicate IDs bind one deduplicated list per scan", func(t *testing.T) {
		query, args := requirementDeliveryEventsStatement("ws-a", []string{"task-b", "task-a", "task-b", "task-c", "task-a"})
		checkShape(t, query, "?,?,?")
		want := []any{"ws-a", "task-b", "task-a", "task-c", "ws-a", "task-b", "task-a", "task-c"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
		if got := strings.Count(query, "?"); got != len(args) {
			t.Fatalf("query has %d placeholders for %d arguments", got, len(args))
		}
	})

	t.Run("one ID binds one placeholder per scan", func(t *testing.T) {
		query, args := requirementDeliveryEventsStatement("ws-a", []string{"task-a"})
		checkShape(t, query, "?")
		want := []any{"ws-a", "task-a", "ws-a", "task-a"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
		if got := strings.Count(query, "?"); got != len(args) {
			t.Fatalf("query has %d placeholders for %d arguments", got, len(args))
		}
	})

	t.Run("empty input returns an empty map without a query", func(t *testing.T) {
		// The store has no database and the context has no workspace, so
		// any query attempt fails or panics.
		s := &Store{}
		for _, ids := range [][]string{nil, {}} {
			out, err := s.ListRequirementDeliveryEventsForTasks(context.Background(), ids)
			if err != nil {
				t.Fatalf("ListRequirementDeliveryEventsForTasks(%#v) error = %v", ids, err)
			}
			if out == nil || len(out) != 0 {
				t.Fatalf("ListRequirementDeliveryEventsForTasks(%#v) = %#v, want a non-nil empty map", ids, out)
			}
		}
	})
}
