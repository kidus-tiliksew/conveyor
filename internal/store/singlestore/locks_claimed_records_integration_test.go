package singlestore

// SingleStore claimed records and identifier claims (component-persistence,
// SingleStore claimed records and Ledgers; task 261101-187e02). A commit that
// spans partitions can release a lock row before the predecessor's other rows
// are visible. These tests stand for that window by removing a committed row
// directly, so the next holder finds the claim but not the record.

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// hiddenEvent is an events row a test removed to stand for a predecessor's
// committed event that is not yet visible on its partition.
type hiddenEvent struct {
	id                       int64
	workspace, shard         string
	taskID, jobID            sql.NullString
	kind, actorID, actorRole string
	payload                  []byte
	at                       time.Time
}

func hideEvent(t *testing.T, st *Store, workspace string, id int64) hiddenEvent {
	t.Helper()
	h := hiddenEvent{id: id, workspace: workspace}
	err := st.db.QueryRowContext(t.Context(), `SELECT event_shard_key,task_id,job_id,kind,actor_id,actor_role,payload_json,at FROM events WHERE workspace_id=? AND id=?`, workspace, id).
		Scan(&h.shard, &h.taskID, &h.jobID, &h.kind, &h.actorID, &h.actorRole, &h.payload, &h.at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.ExecContext(t.Context(), `DELETE FROM events WHERE workspace_id=? AND id=?`, workspace, id); err != nil {
		t.Fatal(err)
	}
	return h
}

// restore inserts the hidden row again with its original identity.
func (h hiddenEvent) restore(t *testing.T, st *Store) {
	t.Helper()
	if _, err := st.db.ExecContext(t.Context(), `INSERT INTO events (id,event_shard_key,task_id,job_id,kind,actor_id,actor_role,payload_json,at,workspace_id) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		h.id, h.shard, h.taskID, h.jobID, h.kind, h.actorID, h.actorRole, h.payload, h.at, h.workspace); err != nil {
		t.Fatal(err)
	}
}

// awaitClaimWait blocks until an observed command found its claimed record
// missing. The bound only guards a broken test; the signal is the event.
func awaitClaimWait(t *testing.T, obs *claimedRecordWaitObserver) {
	t.Helper()
	select {
	case <-obs.waits:
	case <-time.After(time.Minute):
		t.Fatal("the command never waited for its claimed record")
	}
}

// claimHolder reads the holder recorded on key's lock row.
func claimHolder(t *testing.T, st *Store, key string) string {
	t.Helper()
	var holder string
	if err := st.db.QueryRowContext(t.Context(), "SELECT holder FROM conveyor_locks WHERE `key`=?", lockRowKey(key)).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	return holder
}

// nameClaim writes holder on key's lock row in its own transaction.
func nameClaim(t *testing.T, st *Store, ctx context.Context, key, holder string) {
	t.Helper()
	if err := st.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := claimKey(ctx, tx, key); err != nil {
			return err
		}
		return setClaim(ctx, tx, key, holder)
	}); err != nil {
		t.Fatal(err)
	}
}

// shortClaimWait bounds the claimed-record wait for one test.
func shortClaimWait(t *testing.T) {
	t.Helper()
	previous := claimedRecordWaitLimit
	claimedRecordWaitLimit = 200 * time.Millisecond
	t.Cleanup(func() { claimedRecordWaitLimit = previous })
}

func seedClaimTask(t *testing.T, st *Store, prefix string) (context.Context, string, core.Task) {
	t.Helper()
	ws := prefix + "-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), ws)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: ws, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	return ctx, ws, task
}

func TestClaimedRecordWaitsForUnseenRecordIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, ws, task := seedClaimTask(t, st, "claimed-wait")
	key := "claimed-record-test:" + ws + ":" + task.ID
	var appended claimedRecord
	if err := st.withTx(ctx, func(tx *sql.Tx) error {
		if _, _, err := awaitClaimedRecord(ctx, tx, key, ws, task.ID); err != nil {
			return err
		}
		if err := taskEvent(ctx, tx, core.Event{TaskID: task.ID, Kind: "claimed.record_test", Payload: core.JSONPayload(map[string]any{"n": 1})}); err != nil {
			return err
		}
		var err error
		if appended, err = lastInsertedRecord(ctx, tx, claimedEventsTable, ws, true); err != nil {
			return err
		}
		appended.Note = "note:with:colons"
		return recordClaimedRecord(ctx, tx, key, appended)
	}); err != nil {
		t.Fatal(err)
	}
	if got := claimHolder(t, st, key); got != appended.holder() {
		t.Fatalf("holder=%q, want %q", got, appended.holder())
	}
	hidden := hideEvent(t, st, ws, appended.ID)
	waitCtx, obs := observeClaimedRecordWait(ctx)
	type outcome struct {
		record claimedRecord
		found  bool
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		var o outcome
		o.err = st.withTx(waitCtx, func(tx *sql.Tx) error {
			var err error
			o.record, o.found, err = awaitClaimedRecord(waitCtx, tx, key, ws, task.ID)
			return err
		})
		done <- o
	}()
	awaitClaimWait(t, obs)
	select {
	case o := <-done:
		t.Fatalf("returned before the claimed record was visible: %+v", o)
	default:
	}
	hidden.restore(t, st)
	o := <-done
	if o.err != nil || !o.found || o.record != appended {
		t.Fatalf("after restore: record=%+v found=%v err=%v, want %+v", o.record, o.found, o.err, appended)
	}
}

func TestClaimedRecordTimesOutIntegration(t *testing.T) {
	st := integrationStore(t)
	shortClaimWait(t)
	ctx, ws, task := seedClaimTask(t, st, "claimed-timeout")
	missing := claimedRecord{Table: claimedEventsTable, ID: 1 << 60}
	before := countTaskEvents(t, st, ws, task.ID)

	t.Run("helper", func(t *testing.T) {
		key := "claimed-record-test:" + ws + ":" + task.ID
		nameClaim(t, st, ctx, key, missing.holder())
		err := st.withTx(ctx, func(tx *sql.Tx) error {
			_, _, err := awaitClaimedRecord(ctx, tx, key, ws, task.ID)
			return err
		})
		if !errors.Is(err, store.ErrRetryable) {
			t.Fatalf("err=%v, want ErrRetryable", err)
		}
	})
	t.Run("malformed holder", func(t *testing.T) {
		key := "claimed-record-malformed:" + ws + ":" + task.ID
		nameClaim(t, st, ctx, key, "workspace-holder")
		err := st.withTx(ctx, func(tx *sql.Tx) error {
			_, _, err := awaitClaimedRecord(ctx, tx, key, ws, task.ID)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "names no record") {
			t.Fatalf("err=%v, want a refused holder", err)
		}
	})
	// Each claimed decision consults its claim before it reads the history:
	// a claim naming a record that never appears stops it with ErrRetryable
	// and nothing written.
	t.Run("context observation", func(t *testing.T) {
		job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobRunning}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		nameClaim(t, st, ctx, contextObservationClaimKey(ws, task.ID), missing.holder())
		_, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, store.ContextObservationCommand, func(lease taskops.TaskLease) (core.ContextObservation, error) {
			return st.RecordInProcessContextObservation(ctx, lease, core.ContextObservation{Schema: 1, Workspace: ws, TaskID: task.ID, JobID: job.ID})
		})
		if !errors.Is(err, store.ErrRetryable) {
			t.Fatalf("err=%v, want ErrRetryable", err)
		}
	})
	t.Run("lineage rebuild", func(t *testing.T) {
		nameClaim(t, st, ctx, "lineage-rebuild:"+ws+":claimed-rebuild", claimedRecord{Table: claimedEventsTable, ID: 1 << 60, Shard: "event/missing"}.holder())
		if _, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "claimed record", RequestID: "claimed-rebuild"}); !errors.Is(err, store.ErrRetryable) {
			t.Fatalf("err=%v, want ErrRetryable", err)
		}
		var rebuilt int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE workspace_id=? AND kind='lineage.rebuilt'`, ws).Scan(&rebuilt); err != nil || rebuilt != 0 {
			t.Fatalf("rebuilt events=%d err=%v", rebuilt, err)
		}
	})
	t.Run("design merge judgment", func(t *testing.T) {
		var causal int64
		if err := st.withTx(ctx, func(tx *sql.Tx) error {
			if err := taskEvent(ctx, tx, core.Event{TaskID: task.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"head_sha": "claimed-sha"})}); err != nil {
				return err
			}
			record, err := lastInsertedRecord(ctx, tx, claimedEventsTable, ws, true)
			causal = record.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		nameClaim(t, st, ctx, ws+":conveyor:design-merge-judgment:claimed-design:"+strconv.FormatInt(causal, 10), claimedRecord{Table: claimedMonitorActivityTable, ID: 1 << 60}.holder())
		if _, err := st.ResolveCausalSystemDesignMerge(ctx, "claimed-design", "repo", "claimed-sha", causal, "claimed-drift", []string{"x.go"}, true); !errors.Is(err, store.ErrRetryable) {
			t.Fatalf("err=%v, want ErrRetryable", err)
		}
	})
	if after := countTaskEvents(t, st, ws, task.ID); after != before+2 {
		t.Fatalf("task events %d -> %d, want only the fixture job and merge events", before, after)
	}
}

// TestClaimedObservationAndRebuildNameTheirRecordsIntegration checks that the
// claimed decisions name what they appended, so the next holder waits for it.
func TestClaimedObservationAndRebuildNameTheirRecordsIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, ws, _ := seedClaimTask(t, st, "claimed-names")
	result, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "claimed record", RequestID: "named-rebuild"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := parseClaimedRecord(claimHolder(t, st, "lineage-rebuild:"+ws+":named-rebuild"))
	if err != nil {
		t.Fatal(err)
	}
	var kind string
	if err = st.db.QueryRowContext(ctx, `SELECT kind FROM events WHERE workspace_id=? AND event_shard_key=? AND id=?`, ws, record.Shard, record.ID).Scan(&kind); err != nil || kind != "lineage.rebuilt" {
		t.Fatalf("claim names %+v (kind %q, err %v), want the lineage.rebuilt event", record, kind, err)
	}
	hidden := hideEvent(t, st, ws, record.ID)
	waitCtx, obs := observeClaimedRecordWait(ctx)
	type outcome struct {
		result core.LineageRebuildResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		replay, err := st.RebuildLineage(waitCtx, core.LineageRebuildRequest{Reason: "claimed record", RequestID: "named-rebuild"})
		done <- outcome{replay, err}
	}()
	awaitClaimWait(t, obs)
	hidden.restore(t, st)
	o := <-done
	if o.err != nil || o.result != result {
		t.Fatalf("replay=%+v err=%v, want the recorded result %+v", o.result, o.err, result)
	}
	var rebuilt int
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE workspace_id=? AND kind='lineage.rebuilt'`, ws).Scan(&rebuilt); err != nil || rebuilt != 1 {
		t.Fatalf("rebuilt events=%d err=%v, want one", rebuilt, err)
	}
}

func TestIdentifierClaimsOutliveUnseenRowsIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, ws, task := seedClaimTask(t, st, "identifier-claims")
	if got := claimHolder(t, st, "task-id:"+task.ID); got != ws {
		t.Fatalf("task identifier claim=%q, want %q", got, ws)
	}
	// Another workspace's committed row that this transaction cannot see yet
	// stands in for the claim alone.
	const foreign = "identifier-claims-foreign"
	t.Run("task", func(t *testing.T) {
		id := core.NewTaskID()
		nameClaim(t, st, ctx, "task-id:"+id, foreign)
		err := st.CreateTask(ctx, core.Task{ID: id, Workspace: ws, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: time.Now().UTC()})
		if err == nil || !strings.Contains(err.Error(), "task identifier already exists") {
			t.Fatalf("task under a foreign claim: %v", err)
		}
	})
	t.Run("job", func(t *testing.T) {
		id := task.ID + "-job-claimed"
		nameClaim(t, st, ctx, "job-id:"+id, foreign)
		if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}); !errors.Is(err, store.ErrDispatchJobConflict) {
			t.Fatalf("job under a foreign claim: %v", err)
		}
		own := task.ID + "-job-own"
		if err := st.CreateJob(ctx, core.Job{ID: own, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if got := claimHolder(t, st, "job-id:"+own); got != ws {
			t.Fatalf("job identifier claim=%q, want %q", got, ws)
		}
	})
	t.Run("work order", func(t *testing.T) {
		id := task.ID + "-implement-claimed"
		nameClaim(t, st, ctx, "work-order-id:"+id, foreign)
		job := core.Job{ID: task.ID + "-job-order", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		_, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
			return struct{}{}, st.CreateWorkOrderCommand(ctx, lease, core.WorkOrder{ID: id, JobID: job.ID, TaskID: task.ID, Stage: core.StageImplement, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)})
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("work order under a foreign claim: %v", err)
		}
	})
}

func countTaskEvents(t *testing.T, st *Store, workspace, taskID string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM events WHERE workspace_id=? AND task_id=?`, workspace, taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
