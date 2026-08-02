package postgres

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestLineageProjectionRebuildIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	suffix := core.NewTaskID()
	now := time.Now().UTC()
	parent := phase61Task(workspace, "lineage-parent-"+suffix, core.TaskRunning, "")
	dependency := phase61Task(workspace, "lineage-dependency-"+suffix, core.TaskRunning, "")
	parent.CreatedAt, dependency.CreatedAt = now, now
	for _, task := range []core.Task{parent, dependency} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: parent.ID, Content: "lineage parent",
		Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	child := phase61Task(workspace, "lineage-child-"+suffix, core.TaskRunning, parent.ID)
	child.OriginSpecVersion, child.OriginSubID, child.CreatedAt = spec.Version, "SUB-1", now
	if err := st.CreateTaskWithDependencies(ctx, child, []string{dependency.ID}); err != nil {
		t.Fatal(err)
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"materializes": false, "depends_on": false, "versions": false}
	for _, link := range links {
		if link.DstID == child.ID || link.SrcID == child.ID || link.SrcID == parent.ID {
			want[link.Kind] = true
		}
	}
	if !want["materializes"] || !want["depends_on"] || !want["versions"] {
		t.Fatalf("missing child lineage: %+v", links)
	}
	if _, err = st.pool.Exec(ctx, `DELETE FROM links WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	if count, rebuildErr := st.RebuildLineage(ctx); rebuildErr != nil || count < 3 {
		t.Fatalf("rebuild count=%d err=%v", count, rebuildErr)
	}
	links, err = st.ListLineageLinks(ctx)
	if err != nil || len(links) < 3 {
		t.Fatalf("rebuilt links=%+v err=%v", links, err)
	}
}
