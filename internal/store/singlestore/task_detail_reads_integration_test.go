package singlestore

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestTaskDetailReadsSelectBoundedRowsIntegration observes every value the
// database returns to the three detail selections before and after unrelated
// captures, document versions and bodies, other documents, and artifact blobs
// grow: each selection stays one query with the same rows and value bytes,
// and no returned value carries the unrelated sentinel (component-persistence,
// Task-detail batch reads).
func TestTaskDetailReadsSelectBoundedRowsIntegration(t *testing.T) {
	const sentinel = "SINGLESTORE-DETAIL-READ-SENTINEL"
	st := integrationStore(t)
	workspace := "detail-reads-" + strings.ToLower(core.NewTaskID())
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	seed := storetest.SeedTaskDetailReads(t, st, ctx, "s2-detail-"+strings.ToLower(core.NewTaskID()))

	recorder := &selectedBytesRecorder{needles: [][]byte{[]byte(sentinel)}}
	connector, err := mysql.NewConnector(sharedDatabase.cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(recordingConnector{Connector: connector, recorder: recorder})
	t.Cleanup(func() { _ = db.Close() })
	recorded := &Store{db: db, log: s2log.New(db)}

	type observation struct{ queries, rows, bytes int }
	observe := func() (map[string]observation, string) {
		t.Helper()
		out := map[string]observation{}
		for name, read := range map[string]func() error{
			"captures": func() error { _, err := recorded.ListTaskTranscriptCaptures(ctx, seed.TaskID); return err },
			"snapshots": func() error {
				_, err := recorded.ListTaskActivitySnapshots(ctx, seed.TaskID, seed.OrderIDs)
				return err
			},
			"metadata": func() error { _, err := recorded.ListDocumentMetadata(ctx, seed.DocumentIDs); return err },
		} {
			recorder.start()
			if err := read(); err != nil {
				t.Fatal(err)
			}
			rows, bytes := recorder.totals()
			queries, hits := recorder.stop()
			if len(hits) != 0 {
				t.Fatalf("%s returned unrelated values: %v", name, hits)
			}
			out[name] = observation{queries: len(queries), rows: rows, bytes: bytes}
		}
		return out, storetest.ReadTaskDetailSelections(t, recorded, ctx, seed)
	}
	before, beforeValues := observe()
	for name, rows := range map[string]int{"captures": 2, "snapshots": 1, "metadata": 2} {
		if before[name].queries != 1 || before[name].rows != rows {
			t.Fatalf("%s = %+v, want one query returning %d rows", name, before[name], rows)
		}
	}
	storetest.GrowUnrelatedTaskDetailHistory(t, st, ctx, seed, sentinel, 2)
	storetest.GrowUnrelatedTaskDetailHistory(t, st, ctx, seed, sentinel, 8)
	after, afterValues := observe()
	for name := range before {
		if after[name] != before[name] {
			t.Fatalf("unrelated growth changed %s: before %+v after %+v", name, before[name], after[name])
		}
	}
	if afterValues != beforeValues || strings.Contains(afterValues, sentinel) {
		t.Fatalf("unrelated growth changed returned values:\nbefore %s\nafter  %s", beforeValues, afterValues)
	}
	// Growing the selected output keeps one query per selection and follows
	// the stated row bounds: one row per capture, at most one snapshot row
	// per requested order, one metadata row per requested tier and ID.
	storetest.GrowSelectedTaskDetail(t, st, ctx, &seed, 4)
	grown, _ := observe()
	snapshotRows := 0
	for _, id := range seed.OrderIDs {
		if _, found, err := st.GetWorkOrderActivitySnapshot(ctx, id); err != nil {
			t.Fatal(err)
		} else if found {
			snapshotRows++
		}
	}
	for name, rows := range map[string]int{"captures": before["captures"].rows + 4, "snapshots": snapshotRows, "metadata": before["metadata"].rows + 4} {
		if grown[name].queries != 1 || grown[name].rows != rows || grown[name].bytes <= 0 {
			t.Fatalf("selected growth %s = %+v, want one query returning %d rows", name, grown[name], rows)
		}
	}
	if snapshotRows > len(seed.OrderIDs) || grown["captures"].bytes <= before["captures"].bytes || grown["metadata"].bytes <= before["metadata"].bytes {
		t.Fatalf("selected growth bytes: before %+v grown %+v", before, grown)
	}
}
