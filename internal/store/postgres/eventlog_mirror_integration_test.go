package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// TestLegacyWritesMirrorIntoEventLog is the phase-1 dual-append contract:
// every legacy events row lands in the event log, on the stream the log core
// will own it under, inside the same transaction, carrying the legacy id.
func TestLegacyWritesMirrorIntoEventLog(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	ctx = store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	log := st.Log()

	taskID := core.NewTaskID()
	task := phase61Task(workspace, taskID, core.TaskQueued, "")
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "task.hold_changed", Payload: core.JSONPayload(map[string]any{"held": true})}); err != nil {
		t.Fatal(err)
	}
	legacy, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) == 0 {
		t.Fatal("no legacy events for task")
	}
	mirrored, err := log.Read(ctx, workspace, eventlog.TaskStream(taskID), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(mirrored) != len(legacy) {
		t.Fatalf("mirrored %d events for task, legacy has %d", len(mirrored), len(legacy))
	}
	for i := range legacy {
		if mirrored[i].LegacyID != legacy[i].ID {
			t.Fatalf("event %d: mirrored legacy id %d, legacy row id %d", i, mirrored[i].LegacyID, legacy[i].ID)
		}
		if mirrored[i].Kind != legacy[i].Kind {
			t.Fatalf("event %d: kind %q vs %q", i, mirrored[i].Kind, legacy[i].Kind)
		}
		if mirrored[i].Version != eventlog.Version(i+1) {
			t.Fatalf("event %d: version %d", i, mirrored[i].Version)
		}
		if !mirrored[i].At.Equal(legacy[i].At) {
			t.Fatalf("event %d: at %v vs %v", i, mirrored[i].At, legacy[i].At)
		}
		if mirrored[i].ActorID != legacy[i].ActorID || mirrored[i].ActorRole != string(legacy[i].ActorRole) {
			t.Fatalf("event %d: actor %s/%s vs %s/%s", i, mirrored[i].ActorID, mirrored[i].ActorRole, legacy[i].ActorID, legacy[i].ActorRole)
		}
	}

	// Workspace-scoped family: a reference document gets its own stream.
	documentID := "ref-" + core.NewTaskID()
	if _, _, err := st.CreateReferenceDocument(ctx,
		core.ReferenceDocument{ID: documentID, Name: "Mirror reference"},
		core.ReferenceDocumentVersion{Filename: "mirror.md", ContentType: "text/markdown", Content: "# Mirror"}); err != nil {
		t.Fatal(err)
	}
	docEvents, err := log.Read(ctx, workspace, eventlog.ReferenceDocumentStream(documentID), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(docEvents) != 1 || docEvents[0].Kind != "reference_document.created" {
		t.Fatalf("reference document stream=%+v", docEvents)
	}

	// The workspace tail sees everything in commit order with positions
	// strictly increasing, and the log's legacy ids are unique.
	tail, err := log.Tail(ctx, workspace, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for i, e := range tail {
		if i > 0 && e.Position <= tail[i-1].Position {
			t.Fatalf("tail positions not increasing at %d", i)
		}
		if e.LegacyID == 0 {
			t.Fatalf("tail entry %d has no legacy id: %+v", i, e)
		}
		if seen[e.LegacyID] {
			t.Fatalf("legacy id %d mirrored twice", e.LegacyID)
		}
		seen[e.LegacyID] = true
	}
	var legacyCount int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1`, workspace).Scan(&legacyCount); err != nil {
		t.Fatal(err)
	}
	if legacyCount != len(tail) {
		t.Fatalf("legacy events=%d, log tail=%d", legacyCount, len(tail))
	}
}

// TestMirrorRollsBackWithLegacyWrite pins atomicity: when the legacy
// transaction fails after the event insert, nothing reaches the log.
func TestMirrorRollsBackWithLegacyWrite(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	ctx = store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	stream := eventlog.WorkspaceStream(workspace)
	before, err := st.Log().Head(ctx, workspace, stream)
	if err != nil {
		t.Fatal(err)
	}
	failed := errors.New("legacy write failed after the event insert")
	err = st.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := insertWorkspaceEvent(ctx, q, core.Event{Kind: "config.updated", Payload: core.JSONPayload(map[string]any{"probe": true})}); err != nil {
			return err
		}
		head, err := st.Log().Head(pglog.WithTx(ctx, tx), workspace, stream)
		if err != nil {
			return err
		}
		if head != before+1 {
			return fmt.Errorf("head inside transaction=%d, want %d", head, before+1)
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("inTx err=%v", err)
	}
	after, err := st.Log().Head(ctx, workspace, stream)
	if err != nil || after != before {
		t.Fatalf("rolled-back mirror leaked: head before=%d after=%d err=%v", before, after, err)
	}
	var probes int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1 AND kind='config.updated' AND payload_json->>'probe'='true'`, workspace).Scan(&probes); err != nil {
		t.Fatal(err)
	}
	if probes != 0 {
		t.Fatalf("legacy row survived rollback: %d", probes)
	}
}
