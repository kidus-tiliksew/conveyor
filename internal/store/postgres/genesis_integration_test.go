package postgres

import (
	"encoding/json"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestGenesisImportBuildsLogFromLegacyState is the phase-1 import contract:
// a deployment with legacy rows but an empty log ends up with every legacy
// event on its stream, a snapshot at the head of every entity stream, and a
// second run that writes nothing.
func TestGenesisImportBuildsLogFromLegacyState(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	ctx = store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})

	taskA, taskB := core.NewTaskID(), core.NewTaskID()
	for _, id := range []string{taskA, taskB} {
		if err := st.CreateTask(ctx, phase61Task(workspace, id, core.TaskQueued, "")); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: taskA, Kind: "task.hold_changed", Payload: core.JSONPayload(map[string]any{"held": true})}); err != nil {
		t.Fatal(err)
	}
	// Telemetry stays in the legacy table and must not be imported.
	if err := st.AppendEvent(ctx, core.Event{TaskID: taskA, Kind: "work_order.lease_renewed", Payload: core.JSONPayload(map[string]any{"attempt_id": "a1"})}); err != nil {
		t.Fatal(err)
	}
	documentID := "ref-" + core.NewTaskID()
	if _, _, err := st.CreateReferenceDocument(ctx,
		core.ReferenceDocument{ID: documentID, Name: "Genesis reference"},
		core.ReferenceDocumentVersion{Filename: "genesis.md", ContentType: "text/markdown", Content: "# Genesis"}); err != nil {
		t.Fatal(err)
	}

	// Simulate a deployment that predates the log: legacy rows exist, the
	// log does not.
	for _, table := range []string{"event_log", "event_log_streams", "event_log_positions", "event_snapshots"} {
		if _, err := st.pool.Exec(ctx, `DELETE FROM `+table+` WHERE workspace_id = $1`, workspace); err != nil {
			t.Fatal(err)
		}
	}
	var legacyCount, telemetryCount int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id = $1 AND NOT (kind = ANY($2))`, workspace, telemetryKindList()).Scan(&legacyCount); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id = $1 AND kind = ANY($2)`, workspace, telemetryKindList()).Scan(&telemetryCount); err != nil {
		t.Fatal(err)
	}
	if legacyCount == 0 || telemetryCount != 1 {
		t.Fatalf("fixture legacy=%d telemetry=%d", legacyCount, telemetryCount)
	}

	report, err := st.ImportGenesis(ctx, GenesisOptions{Workspaces: []string{workspace}, BatchSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Workspaces) != 1 {
		t.Fatalf("report=%+v", report)
	}
	wr := report.Workspaces[0]
	if wr.HistoryImported != legacyCount || wr.HistoryAlreadyInLog != 0 {
		t.Fatalf("history imported=%d already=%d, legacy=%d", wr.HistoryImported, wr.HistoryAlreadyInLog, legacyCount)
	}
	if wr.SnapshotsWritten["task"] != 2 || wr.SnapshotsWritten["reference_document"] != 1 || wr.SnapshotsWritten["workspace"] != 1 {
		t.Fatalf("snapshots written=%v", wr.SnapshotsWritten)
	}
	if !wr.MarkerWritten {
		t.Fatal("genesis marker not written")
	}
	log := st.Log()

	// History landed on the task stream in legacy order, then the snapshot.
	allLegacy, err := st.ListEvents(ctx, taskA)
	if err != nil {
		t.Fatal(err)
	}
	var legacy []core.Event
	for _, e := range allLegacy {
		if !IsTelemetryKind(e.Kind) {
			legacy = append(legacy, e)
		}
	}
	if len(legacy) != len(allLegacy)-1 {
		t.Fatalf("expected exactly one telemetry event on task A, legacy=%d all=%d", len(legacy), len(allLegacy))
	}
	streamA, err := log.Read(ctx, workspace, eventlog.TaskStream(taskA), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(streamA) != len(legacy)+1 {
		t.Fatalf("task stream has %d events, legacy %d (+1 snapshot)", len(streamA), len(legacy))
	}
	for i, e := range legacy {
		if streamA[i].LegacyID != e.ID || streamA[i].Kind != e.Kind || !streamA[i].At.Equal(e.At) {
			t.Fatalf("history %d: %+v vs legacy %+v", i, streamA[i], e)
		}
	}
	head := streamA[len(streamA)-1]
	if head.Kind != eventlog.SnapshotImportedKind || head.LegacyID != 0 {
		t.Fatalf("head=%+v", head)
	}
	var snapshot genesisSnapshot
	if err := json.Unmarshal(head.Payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Family != "task" || snapshot.ID != taskA || snapshot.Row["id"] != taskA || snapshot.Row["state"] != string(core.TaskQueued) {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if snapshot.ContentHash == "" {
		t.Fatal("snapshot has no content hash")
	}

	// The reference document snapshot carries its version rows as children.
	docStream, _ := log.Read(ctx, workspace, eventlog.ReferenceDocumentStream(documentID), 0, 0)
	var docSnapshot genesisSnapshot
	if err := json.Unmarshal(docStream[len(docStream)-1].Payload, &docSnapshot); err != nil {
		t.Fatal(err)
	}
	if versions := docSnapshot.Children["reference_document_versions"]; len(versions) != 1 || versions[0]["content"] != "# Genesis" {
		t.Fatalf("document snapshot children=%v", docSnapshot.Children)
	}

	// The workspace stream ends with its snapshot, which carries the config
	// document; the genesis marker lives on the bookkeeping stream.
	wsStream, _ := log.Read(ctx, workspace, eventlog.WorkspaceStream(workspace), 0, 0)
	if wsStream[len(wsStream)-1].Kind != eventlog.SnapshotImportedKind {
		t.Fatalf("workspace head=%s", wsStream[len(wsStream)-1].Kind)
	}
	var wsSnapshot genesisSnapshot
	if err := json.Unmarshal(wsStream[len(wsStream)-1].Payload, &wsSnapshot); err != nil {
		t.Fatal(err)
	}
	if wsSnapshot.Row["id"] != workspace || wsSnapshot.Row["config_yaml"] == nil {
		t.Fatalf("workspace snapshot=%+v", wsSnapshot.Row)
	}
	markers, _ := log.Read(ctx, workspace, eventlog.GenesisStream, 0, 0)
	if len(markers) != 1 || markers[0].Kind != eventlog.GenesisCompletedKind {
		t.Fatalf("genesis markers=%+v", markers)
	}

	// Every legacy event is in the log exactly once.
	var mirrored int
	if err := st.pool.QueryRow(ctx, `SELECT count(DISTINCT legacy_event_id) FROM event_log WHERE workspace_id = $1 AND legacy_event_id IS NOT NULL`, workspace).Scan(&mirrored); err != nil {
		t.Fatal(err)
	}
	if mirrored != legacyCount {
		t.Fatalf("mirrored=%d legacy=%d", mirrored, legacyCount)
	}

	// Second run: nothing changes, nothing is written.
	tailBefore, _ := log.Tail(ctx, workspace, 0, 0)
	again, err := st.ImportGenesis(ctx, GenesisOptions{Workspaces: []string{workspace}})
	if err != nil {
		t.Fatal(err)
	}
	wr = again.Workspaces[0]
	if wr.HistoryImported != 0 || wr.HistoryAlreadyInLog != legacyCount || len(wr.SnapshotsWritten) != 0 || wr.MarkerWritten {
		t.Fatalf("second run report=%+v", wr)
	}
	tailAfter, _ := log.Tail(ctx, workspace, 0, 0)
	if len(tailAfter) != len(tailBefore) {
		t.Fatalf("second run appended %d events", len(tailAfter)-len(tailBefore))
	}

	// A legacy write after the import is mirrored live and the bridge
	// records the resulting state at commit, so the next run has nothing to
	// import and nothing to re-snapshot.
	if err := st.AppendEvent(ctx, core.Event{TaskID: taskB, Kind: "task.hold_changed", Payload: core.JSONPayload(map[string]any{"held": true})}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetTaskHold(ctx, taskB, true); err != nil {
		t.Fatal(err)
	}
	third, err := st.ImportGenesis(ctx, GenesisOptions{Workspaces: []string{workspace}})
	if err != nil {
		t.Fatal(err)
	}
	wr = third.Workspaces[0]
	if wr.HistoryImported != 0 {
		t.Fatalf("live-mirrored events re-imported: %+v", wr)
	}
	if len(wr.SnapshotsWritten) != 0 {
		t.Fatalf("third run snapshots=%v (bridge should have kept every entity current)", wr.SnapshotsWritten)
	}
	streamB, _ := log.Read(ctx, workspace, eventlog.TaskStream(taskB), 0, 0)
	if streamB[len(streamB)-1].Kind != eventlog.StateRecordedKind {
		t.Fatalf("task B head=%s, want recorded state", streamB[len(streamB)-1].Kind)
	}
	var sawImported, sawHold bool
	for _, e := range streamB {
		switch e.Kind {
		case eventlog.SnapshotImportedKind:
			sawImported = true
		case "task.hold_changed":
			sawHold = sawImported // the live write must come after the import
		}
	}
	if !sawImported || !sawHold {
		t.Fatalf("task B stream lacks import-then-live ordering: imported=%t hold=%t", sawImported, sawHold)
	}

	// A write behind the store is the one case the import must repair.
	if _, err := st.pool.Exec(ctx, `UPDATE tasks SET title = 'edited behind the store' WHERE workspace_id = $1 AND id = $2`, workspace, taskB); err != nil {
		t.Fatal(err)
	}
	fourth, err := st.ImportGenesis(ctx, GenesisOptions{Workspaces: []string{workspace}})
	if err != nil {
		t.Fatal(err)
	}
	if wr = fourth.Workspaces[0]; wr.SnapshotsWritten["task"] != 1 || len(wr.SnapshotsWritten) != 1 {
		t.Fatalf("fourth run snapshots=%v (want exactly the edited task)", wr.SnapshotsWritten)
	}
}

// TestGenesisImportDeploymentUsersExcludePasswordHash pins that the
// deployment-level user snapshot never carries a credential column.
func TestGenesisImportDeploymentUsersExcludePasswordHash(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	if _, err := st.ImportGenesis(ctx, GenesisOptions{Workspaces: []string{workspace}}); err != nil {
		t.Fatal(err)
	}
	var users int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users == 0 {
		t.Skip("no users in fixture")
	}
	tail, err := st.Log().Tail(ctx, eventlog.DeploymentWorkspace, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, e := range tail {
		if e.Kind != eventlog.SnapshotImportedKind || e.Stream.Type() != "user" {
			continue
		}
		seen++
		var snapshot genesisSnapshot
		if err := json.Unmarshal(e.Payload, &snapshot); err != nil {
			t.Fatal(err)
		}
		if _, leaked := snapshot.Row["password_hash"]; leaked {
			t.Fatalf("user snapshot leaked password_hash: %v", snapshot.Row)
		}
		if snapshot.Row["email"] == nil {
			t.Fatalf("user snapshot missing email: %v", snapshot.Row)
		}
	}
	if seen == 0 {
		t.Fatal("no user snapshots under the deployment workspace")
	}
}
