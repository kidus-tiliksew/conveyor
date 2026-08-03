package postgres

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestListArtifactsForLineagePlanUsesTypedIndexes(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "SET enable_seqscan=off"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, "EXPLAIN (COSTS OFF) "+listArtifactsForLineageSQL, workspace,
		[]string{"task", "requirement", "planning_session", "evidence"}, []string{"task-id", "req-id", "session-id", "artifact-id"})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var planLines []string
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		planLines = append(planLines, line)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(planLines, "\n")
	for _, index := range []string{"artifact_links_task_unique", "artifact_links_requirement_unique", "artifact_links_planning_session_unique", "artifact_links_lineage_evidence_idx"} {
		if !strings.Contains(plan, index) {
			t.Fatalf("lineage artifact plan omitted %s:\n%s", index, plan)
		}
	}
}

func TestPostgresLineageRebuildPreservesLiveShapedUnregenerableLinks(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-" + taskID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	sibling := workspace + "-sibling"
	siblingCtx := store.WithWorkspace(t.Context(), sibling)
	if _, err := st.BootstrapWorkspaceConfig(siblingCtx, &config.Config{Workspace: sibling, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	siblingID := core.NewTaskID()
	siblingTask := core.Task{ID: siblingID, Workspace: sibling, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + siblingID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(siblingCtx, siblingTask); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(siblingCtx, core.Event{TaskID: siblingTask.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": siblingTask.ID + "-implement-1"})}); err != nil {
		t.Fatal(err)
	}
	var siblingBefore int
	if err := st.pool.QueryRow(siblingCtx, `SELECT count(*) FROM links WHERE workspace_id=$1`, sibling).Scan(&siblingBefore); err != nil {
		t.Fatal(err)
	}
	for number := 1; number <= 53; number++ {
		var eventID int64
		if err := st.pool.QueryRow(ctx, `INSERT INTO events
			(workspace_id,task_id,kind,actor_id,actor_role,payload_json,at)
			VALUES ($1,$2,'pull_request.opened','legacy','system',$3,now()) RETURNING id`,
			workspace, taskID, core.JSONPayload(map[string]any{"number": number})).Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `INSERT INTO links
			(workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
			VALUES ($1,'task',$2,'pull_request',$3,'submitted_as',$4,now())`, workspace, taskID, fmt.Sprint(number), eventID); err != nil {
			t.Fatal(err)
		}
	}
	request := core.LineageRebuildRequest{Reason: "live-shaped repair", RequestID: "postgres-preserve-53"}
	result, err := st.RebuildLineage(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.PreservedUnregenerable != 53 || result.Unsupported != 53 || result.Projected == 53 {
		t.Fatalf("result=%+v", result)
	}
	var count int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM links WHERE workspace_id=$1 AND kind='submitted_as' AND dst_id !~ '#'`, workspace).Scan(&count); err != nil || count != 53 {
		t.Fatalf("preserved count=%d err=%v", count, err)
	}
	if repeated, err := st.RebuildLineage(ctx, request); err != nil || repeated != result {
		t.Fatalf("idempotent result=%+v err=%v want=%+v", repeated, err, result)
	}
	var siblingAfter int
	if err := st.pool.QueryRow(siblingCtx, `SELECT count(*) FROM links WHERE workspace_id=$1`, sibling).Scan(&siblingAfter); err != nil || siblingAfter != siblingBefore || siblingAfter == 0 {
		t.Fatalf("populated sibling before=%d after=%d err=%v", siblingBefore, siblingAfter, err)
	}
}

func TestLineageRebuildCrossStoreParityIntegration(t *testing.T) {
	pg, ctx, workspace := newPhase61IntegrationStore(t)
	defer pg.Close()
	memory := store.NewMemoryWithConfig(&config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}})
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	stores := []store.Store{pg, memory}
	for _, st := range stores {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		for _, event := range []core.Event{
			{TaskID: task.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": task.ID + "-implement-1"})},
			{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "number": 7, "base_sha": "base", "head_sha": "head"})},
		} {
			if err := st.AppendEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
	}
	request := core.LineageRebuildRequest{Reason: "cross-store parity", RequestID: "cross-store-1"}
	results := make([]core.LineageRebuildResult, 0, 2)
	snapshots := make([][]string, 0, 2)
	for _, st := range stores {
		result, err := st.RebuildLineage(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		repeated, err := st.RebuildLineage(ctx, request)
		if err != nil || repeated != result {
			t.Fatalf("idempotent result=%+v repeated=%+v err=%v", result, repeated, err)
		}
		links, err := st.ListLineageLinks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(links))
		for _, link := range links {
			keys = append(keys, fmt.Sprintf("%s:%s:%s:%s:%s", link.SrcType, link.SrcID, link.Kind, link.DstType, link.DstID))
		}
		sort.Strings(keys)
		results, snapshots = append(results, result), append(snapshots, keys)
	}
	if results[0] != results[1] || fmt.Sprint(snapshots[0]) != fmt.Sprint(snapshots[1]) {
		t.Fatalf("postgres result=%+v links=%v; memory result=%+v links=%v", results[0], snapshots[0], results[1], snapshots[1])
	}
}

func TestPostgresLineageConformance(t *testing.T) {
	storetest.RunLineageConformance(t, func(t *testing.T, _ []config.Repo) storetest.LineageFixture {
		st, ctx, workspace := newPhase61IntegrationStore(t)
		t.Cleanup(st.Close)
		return storetest.LineageFixture{Store: st, Context: ctx, Workspace: workspace,
			SeedLegacy: func(t *testing.T, taskID string) (int, func(*testing.T)) {
				session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Migrated context"})
				if err != nil {
					t.Fatal(err)
				}
				requirementID := "req-" + core.NewTaskID()
				_, _, err = st.CreateRequirement(ctx, core.Requirement{ID: requirementID, Title: "Migrated requirement"}, core.RequirementVersion{
					Content: "Migrated context", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
					Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve migrated context."}},
				})
				if err != nil {
					t.Fatal(err)
				}
				artifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "migrated.txt", ContentType: "text/plain", Role: core.ArtifactRoleTaskContext, RequirementID: requirementID}, []byte("migrated context"))
				if err != nil {
					t.Fatal(err)
				}
				var eventID int64
				if err = st.pool.QueryRow(ctx, `SELECT min(id) FROM events WHERE workspace_id=$1 AND task_id=$2`, workspace, taskID).Scan(&eventID); err != nil {
					t.Fatal(err)
				}
				if _, err = st.pool.Exec(ctx, `INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event)
					VALUES ($1,'requirement',$2,'task',$3,'historical_feature_assignment','feature.migrated')`, workspace, requirementID, taskID); err != nil {
					t.Fatal(err)
				}
				if _, err = st.pool.Exec(ctx, `INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id)
					VALUES ($1,'task',$2,'task',$3,'legacy_event_note',$4)`, workspace, taskID, taskID+"-note", eventID); err != nil {
					t.Fatal(err)
				}
				assert := func(t *testing.T) {
					root := core.LineageNode{Type: core.LineageTask, ID: taskID}
					budget := core.LineageTraversalBudget{MaxDepth: config.DefaultLineageContextDepth, MaxNodes: config.DefaultLineageContextNodes, Workspace: workspace}
					links, err := st.ListLineageNeighborhood(ctx, []core.LineageNode{root}, budget)
					if err != nil {
						t.Fatal(err)
					}
					graph, err := core.TraverseLineage(links, []core.LineageNode{root}, budget)
					if err != nil {
						t.Fatal(err)
					}
					artifacts, err := st.ListArtifactsForLineage(ctx, graph.Nodes)
					if err != nil {
						t.Fatal(err)
					}
					selection, err := core.SelectContextArtifacts(links, []core.LineageNode{root}, artifacts, core.ContextArtifactSelectionOptions{Workspace: workspace, LocalTaskID: taskID})
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, selected := range selection.Artifacts {
						found = found || selected.ID == artifact.ID
					}
					if !found {
						t.Fatalf("migrated artifact absent from selection: %+v", selection)
					}
					var count int
					if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM links WHERE workspace_id=$1 AND kind IN ('historical_feature_assignment','legacy_event_note')`, workspace).Scan(&count); err != nil || count != 2 {
						t.Fatalf("retained legacy count=%d err=%v", count, err)
					}
				}
				return 2, assert
			},
		}
	})
}

func TestLineageRebuildRepairsHistoricalRequirementSupersession(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	requirementID := "req-" + core.NewTaskID()
	// Seed the durable state and immutable event payloads exactly as they
	// existed before supersedes_version was recorded by the writer.
	if _, err := st.pool.Exec(ctx, `INSERT INTO requirements
		(workspace_id,id,slug,title,current_version) VALUES ($1,$2,$2,'Historical supersession',NULL)`, workspace, requirementID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO requirement_versions
		(workspace_id,requirement_id,version,content,origin,confirmed,confirmed_by,confirmed_at)
		VALUES ($1,$2,1,'confirmed v1','feature_migration',true,'legacy',now()-interval '2 minutes'),
		       ($1,$2,2,'abandoned v2','feature_migration',false,'',NULL),
		       ($1,$2,3,'confirmed v3','feature_migration',true,'legacy',now()-interval '1 minute')`, workspace, requirementID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE requirements SET current_version=3 WHERE workspace_id=$1 AND id=$2`, workspace, requirementID); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{1, 3} {
		if _, err := st.pool.Exec(ctx, `INSERT INTO events
			(workspace_id,kind,actor_id,actor_role,payload_json,at)
			VALUES ($1,'requirement.version_confirmed','legacy','system',$2,now())`, workspace,
			core.JSONPayload(map[string]any{"workspace_id": workspace, "requirement_id": requirementID, "version": version})); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "repair historical requirement lineage", RequestID: core.NewTaskID()}); err != nil {
		t.Fatal(err)
	}
	wantSrc := core.RequirementVersionLineageID(requirementID, 3)
	wantDst := core.RequirementVersionLineageID(requirementID, 1)
	badDst := core.RequirementVersionLineageID(requirementID, 2)
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.Kind != "supersedes" || link.SrcID != wantSrc {
			continue
		}
		if link.DstID == badDst {
			t.Fatalf("historical rebuild linked abandoned draft: %+v", link)
		}
		if link.DstID == wantDst {
			return
		}
	}
	t.Fatalf("historical supersession %s -> %s missing: %+v", wantSrc, wantDst, links)
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
