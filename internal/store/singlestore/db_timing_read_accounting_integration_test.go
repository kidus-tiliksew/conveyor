package singlestore

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// readAccountingFraming bounds the protocol bytes one statement and its
// one-row result add to the payload: the column count, column definition,
// row header, and end-of-result packets, plus TLS record framing when TLS is
// on. Each is well under 1 KiB for these reads.
const readAccountingFraming = 4096

// TestReadAccountingIntegration runs two labeled reads of known size through
// the production connector and requires each label to report its payload within
// the framing bound, then proves the store-boundary label through GetTask
// (component-persistence, Database read accounting).
func TestReadAccountingIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "read-accounting")
	st.ReadStats(0) // start from an empty interval

	reads := []struct {
		label string
		size  int
	}{{"ReadAccountingLarge", 1 << 20}, {"ReadAccountingSmall", 64 << 10}}
	for _, read := range reads {
		var payload string
		if err := st.db.QueryRowContext(dbmetrics.WithReadLabel(ctx, read.label), "SELECT RPAD('', ?, 'a')", read.size).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != read.size {
			t.Fatalf("%s returned %d bytes", read.label, len(payload))
		}
	}
	totals := readAccountingTotals(st.ReadStats(1000))
	for _, read := range reads {
		got := totals[read.label]
		t.Logf("%s: payload %d bytes, reported %+v", read.label, read.size, got)
		if got.Bytes < uint64(read.size) || got.Bytes > uint64(read.size+readAccountingFraming) || got.Rows != 1 || got.Queries != 1 {
			t.Fatalf("%s: %+v, want bytes in [%d, %d], one row, one query", read.label, got, read.size, read.size+readAccountingFraming)
		}
	}
	again := readAccountingTotals(st.ReadStats(1000))
	for _, read := range reads {
		if got, ok := again[read.label]; ok {
			t.Fatalf("%s reported again after a drain: %+v", read.label, got)
		}
	}

	// The store boundary labels a public read with its method name.
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "read-accounting", Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("b", 256<<10)
	task := core.Task{ID: core.NewTaskID(), Workspace: "read-accounting", Repo: "conveyor", BaseBranch: "main", Title: "Read accounting", Body: body, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	st.ReadStats(0)
	read, err := st.GetTask(ctx, task.ID)
	if err != nil || read.Body != body {
		t.Fatalf("GetTask body %d bytes, err %v", len(read.Body), err)
	}
	if got := readAccountingTotals(st.ReadStats(1000))["GetTask"]; got.Bytes < uint64(len(body)) || got.Queries == 0 {
		t.Fatalf("GetTask: %+v, want at least %d bytes", got, len(body))
	}
}

func readAccountingTotals(interval dbmetrics.ReadInterval) map[string]dbmetrics.ReadTotal {
	totals := make(map[string]dbmetrics.ReadTotal, len(interval.Top))
	for _, total := range interval.Top {
		totals[total.Label] = total
	}
	return totals
}
