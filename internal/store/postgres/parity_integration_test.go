package postgres

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestLogParityAfterGenesisIsCleanThenReportsDrift: right after an import
// every entity matches; a legacy write afterwards shows up as exactly one
// drifted entity with the kinds that caused it, and nothing else moves.
func TestLogParityAfterGenesisIsCleanThenReportsDrift(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	ctx = store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})

	taskA, taskB := core.NewTaskID(), core.NewTaskID()
	for _, id := range []string{taskA, taskB} {
		if err := st.CreateTask(ctx, phase61Task(workspace, id, core.TaskQueued, "")); err != nil {
			t.Fatal(err)
		}
	}
	documentID := "ref-" + core.NewTaskID()
	if _, _, err := st.CreateReferenceDocument(ctx,
		core.ReferenceDocument{ID: documentID, Name: "Parity reference"},
		core.ReferenceDocumentVersion{Filename: "parity.md", ContentType: "text/markdown", Content: "# Parity"}); err != nil {
		t.Fatal(err)
	}

	// Before any import the bridge has already recorded each entity's state
	// at creation, so parity is clean. Wipe the log to simulate a deployment
	// that predates it: now every entity is missing.
	before, err := st.LogParity(ctx, workspace, ParityOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !before.Clean() {
		t.Fatalf("parity not clean with the bridge alone: %+v", before.Families)
	}
	for _, table := range []string{"event_log", "event_log_streams", "event_log_positions", "event_snapshots"} {
		if _, err := st.pool.Exec(ctx, `DELETE FROM `+table+` WHERE workspace_id = $1`, workspace); err != nil {
			t.Fatal(err)
		}
	}
	wiped, err := st.LogParity(ctx, workspace, ParityOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if wiped.Clean() {
		t.Fatal("parity clean with an empty log")
	}
	if fam := familyReport(wiped, "task"); fam.Missing != 2 || fam.Match != 0 {
		t.Fatalf("task family with an empty log=%+v", fam)
	}

	if _, err := st.ImportGenesis(ctx, GenesisOptions{Workspaces: []string{workspace}}); err != nil {
		t.Fatal(err)
	}
	clean, err := st.LogParity(ctx, workspace, ParityOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !clean.Clean() {
		t.Fatalf("parity after import not clean: %+v", clean.Families)
	}
	if fam := familyReport(clean, "task"); fam.Match != 2 || fam.Orphans != 0 {
		t.Fatalf("task family after import=%+v", fam)
	}
	if fam := familyReport(clean, "reference_document"); fam.Match != 1 {
		t.Fatalf("reference_document family after import=%+v", fam)
	}
	if fam := familyReport(clean, "workspace"); fam.Match != 1 {
		t.Fatalf("workspace family after import=%+v", fam)
	}
	if clean.Streams == 0 || clean.Position == 0 {
		t.Fatalf("report=%+v", clean)
	}

	// A legacy write through the store changes task B's row; the bridge
	// records the new state at commit, so parity stays clean without an
	// import.
	if _, err := st.SetTaskHold(ctx, taskB, true); err != nil {
		t.Fatal(err)
	}
	bridged, err := st.LogParity(ctx, workspace, ParityOptions{MaxDrifts: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !bridged.Clean() {
		t.Fatalf("parity not clean after a bridged write: %+v", familyReport(bridged, "task"))
	}
	streamB, _ := st.Log().Read(ctx, workspace, eventlog.TaskStream(taskB), 0, 0)
	if head := streamB[len(streamB)-1]; head.Kind != eventlog.StateRecordedKind {
		t.Fatalf("task B head=%s, want recorded state", head.Kind)
	}

	// A write that bypasses the store (and so the bridge) is exactly what
	// parity exists to catch: drift with no kinds to blame.
	if _, err := st.pool.Exec(ctx, `UPDATE tasks SET title = 'edited behind the store' WHERE workspace_id = $1 AND id = $2`, workspace, taskB); err != nil {
		t.Fatal(err)
	}
	drifted, err := st.LogParity(ctx, workspace, ParityOptions{MaxDrifts: 5})
	if err != nil {
		t.Fatal(err)
	}
	if drifted.Clean() {
		t.Fatal("parity clean after a write behind the store")
	}
	fam := familyReport(drifted, "task")
	if fam.Drift != 1 || fam.Match != 1 || fam.Missing != 0 {
		t.Fatalf("task family after write=%+v", fam)
	}
	if len(fam.Drifts) != 1 || fam.Drifts[0].Stream != eventlog.TaskStream(taskB) || len(fam.Drifts[0].KindsSince) != 0 || fam.UnfoldedKinds != nil {
		t.Fatalf("drifts=%+v unfolded=%v", fam.Drifts, fam.UnfoldedKinds)
	}
	if other := familyReport(drifted, "reference_document"); other.Drift != 0 || other.Match != 1 {
		t.Fatalf("unrelated family moved: %+v", other)
	}

	// Re-importing re-snapshots the drifted entity and parity is clean again.
	if _, err := st.ImportGenesis(ctx, GenesisOptions{Workspaces: []string{workspace}}); err != nil {
		t.Fatal(err)
	}
	after, err := st.LogParity(ctx, workspace, ParityOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !after.Clean() {
		t.Fatalf("parity not clean after re-import: %+v", after.Families)
	}
}

func familyReport(report ParityReport, family string) ParityFamilyReport {
	for _, f := range report.Families {
		if f.Family == family {
			return f
		}
	}
	return ParityFamilyReport{Family: family}
}
