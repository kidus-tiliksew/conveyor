package lineagecontext

import (
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"testing"
)

func TestSelectionFreshnessAfterAttachmentAndZeroBudget(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "fresh", Workspace: "demo", State: core.TaskRunning}
	if e := st.CreateTask(ctx, task); e != nil {
		t.Fatal(e)
	}
	roots := []core.LineageNode{{Type: core.LineageTask, ID: task.ID}}
	budget := Budget{Depth: 5, Nodes: 32, Links: 128, RenderableBytes: 65536, ArtifactRefs: 64, AuthorityNodes: 256}
	read := func(b Budget) Result {
		r, e := AssembleWithBudget(ctx, st, b, roots, task.ID, true)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	first := read(budget)
	if read(budget).Snapshot.Revision != first.Snapshot.Revision {
		t.Fatal("unstable snapshot")
	}
	a, e := st.CreateArtifact(ctx, core.Artifact{Name: "correction.md", ContentType: "text/markdown", TaskID: task.ID}, []byte("correction"))
	if e != nil {
		t.Fatal(e)
	}
	next := read(budget)
	delta := core.CompareContext(next.Snapshot, &first.Snapshot)
	if delta.Additions.Count != 1 || delta.Additions.Items[0].ArtifactID != a.ID || next.Snapshot.Revision == first.Snapshot.Revision {
		t.Fatal(delta)
	}
	// Observational ledger writes do not project edges or invalidate selection.
	if e = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "context.refresh_observed", Payload: core.JSONPayload(map[string]string{"selection_revision": next.Snapshot.Revision})}); e != nil {
		t.Fatal(e)
	}
	if read(budget).Snapshot.Revision != next.Snapshot.Revision {
		t.Fatal("self-invalidating observation")
	}
	budget.ArtifactRefs = 0
	zero := read(budget)
	if zero.Snapshot.Omissions.Count != 1 || zero.Snapshot.Omissions.Items[0].ArtifactID != a.ID || len(zero.Artifacts) != 0 {
		t.Fatal(zero.Snapshot)
	}
	prior := zero.Snapshot.Revision
	_, e = st.CreateArtifact(ctx, core.Artifact{Name: "second.md", ContentType: "text/markdown", TaskID: task.ID}, []byte("second"))
	if e != nil {
		t.Fatal(e)
	}
	if read(budget).Snapshot.Revision == prior {
		t.Fatal("known omitted addition not detected")
	}
	budget.ArtifactRefs = 64
	budget.RenderableBytes = 0
	zero = read(budget)
	if zero.Snapshot.Omissions.Count != 2 || zero.Snapshot.Omissions.Items[0].OmissionReason != "renderable_bytes" {
		t.Fatal(zero.Snapshot)
	}
}
