package postgres

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestPostgresLineageConformance(t *testing.T) {
	storetest.RunLineageConformance(t, func(t *testing.T, _ []config.Repo) storetest.LineageFixture {
		st, ctx, workspace := newPhase61IntegrationStore(t)
		t.Cleanup(st.Close)
		return storetest.LineageFixture{Store: st, Context: ctx, Workspace: workspace}
	})
}

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
	if restored, ok, getErr := st.GetSpecVersion(ctx, parent.ID, spec.Version); getErr != nil || !ok || restored.Content != spec.Content {
		t.Fatalf("exact spec=%+v ok=%t err=%v", restored, ok, getErr)
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
	if _, err = st.pool.Exec(ctx, `INSERT INTO links
		(workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event)
		VALUES ($1,'requirement','legacy-requirement','task',$2,'historical_feature_assignment','feature.migrated')`, workspace, child.ID); err != nil {
		t.Fatal(err)
	}
	if result, rebuildErr := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "test", RequestID: "postgres-1"}); rebuildErr != nil || result.Projected < 3 {
		t.Fatalf("rebuild result=%+v err=%v", result, rebuildErr)
	} else if result.Existing != 1 {
		t.Fatalf("rebuild did not report preserved legacy link: %+v", result)
	}
	var preserved int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM links WHERE workspace_id=$1 AND kind='historical_feature_assignment'`, workspace).Scan(&preserved); err != nil || preserved != 1 {
		t.Fatalf("legacy link preserved=%d err=%v", preserved, err)
	}
	links, err = st.ListLineageLinks(ctx)
	if err != nil || len(links) < 3 {
		t.Fatalf("rebuilt links=%+v err=%v", links, err)
	}
}
