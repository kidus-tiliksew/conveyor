package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/backend"
)

const startupRepairLine = `msg="start-up check repaired drift"`

// TestConveyordStartupChecksRepairDriftIntegration seeds each drift class the
// start-up checks repair on PostgreSQL, starts the real daemon, and checks
// one drift.repaired event and one error-level log line per repair; a second
// start repairs nothing, appends no event, and writes no repair line. A first
// start with no drift writes no repair line either (req-quiet-idle-runtime
// AC-4.1 to AC-4.3; component-runtime, Start-up checks and background loops).
// CI runs it in the PostgreSQL integration tests' daemon shard.
func TestConveyordStartupChecksRepairDriftIntegration(t *testing.T) {
	raw := durableStartupDatabase(t, "postgres")
	cfgPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	requireStartupWrite(t, cfgPath, []byte(durableStartupConfig("postgres", "startup")))

	// The first start migrates and bootstraps an empty workspace: no drift.
	logs := string(startDurableDaemon(t, raw, cfgPath, 90*time.Second).stop(t))
	if strings.Contains(logs, startupRepairLine) || strings.Contains(logs, "start-up check failed") {
		t.Fatalf("a start with no drift wrote a start-up check line:\n%s", logs)
	}

	st, err := backend.Open(store.WithActor(t.Context(), store.SystemActor()), config.Database{Backend: "postgres", URL: raw})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pool, err := pgxpool.New(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "startup")
	seed := driftSeeder{t: t, ctx: ctx, st: st, pool: pool}
	drifted := map[string]string{
		seed.queuedTaskWithoutJob():           store.DriftPassQueuedTasks,
		seed.runningTaskWithDiscardedJob():    store.DriftPassQueuedTasks,
		seed.openBlueprintParent():            store.DriftPassBlueprintClosures,
		seed.reviewWithoutPublication():       store.DriftPassReviewPublications,
		seed.issuePublicationWithoutJob():     store.DriftPassGitHubIssuePublications,
		seed.verificationDeliveryWithoutJob(): store.DriftPassVerificationDeliveries,
	}
	st.Close()

	logs = string(startDurableDaemon(t, raw, cfgPath, 90*time.Second).stop(t))
	if strings.Contains(logs, "start-up check failed") {
		t.Fatalf("a start-up check failed:\n%s", logs)
	}
	if got := strings.Count(logs, startupRepairLine); got != len(drifted) {
		t.Fatalf("repair lines=%d, want %d:\n%s", got, len(drifted), logs)
	}
	for taskID, pass := range drifted {
		if lines := matchingLines(logs, "level=error", startupRepairLine, "workspace=startup", "task="+taskID, "pass="+pass); len(lines) != 1 {
			t.Fatalf("task %s pass %s repair lines=%v\n%s", taskID, pass, lines, logs)
		}
		if n := durableScalar(t, "postgres", raw, "SELECT count(*) FROM events WHERE kind='drift.repaired' AND actor_role='system' AND task_id='"+taskID+"' AND payload_json->>'pass'='"+pass+"'"); n != "1" {
			t.Fatalf("task %s drift.repaired events=%s, want 1", taskID, n)
		}
	}
	total := durableScalar(t, "postgres", raw, "SELECT count(*) FROM events WHERE kind='drift.repaired'")

	// The second start finds nothing to repair.
	logs = string(startDurableDaemon(t, raw, cfgPath, 90*time.Second).stop(t))
	if strings.Contains(logs, startupRepairLine) || strings.Contains(logs, "start-up check failed") {
		t.Fatalf("the second start repaired again:\n%s", logs)
	}
	if again := durableScalar(t, "postgres", raw, "SELECT count(*) FROM events WHERE kind='drift.repaired'"); again != total {
		t.Fatalf("drift.repaired events after the second start=%s, want %s", again, total)
	}
}

func matchingLines(logs string, substrings ...string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		ok := true
		for _, s := range substrings {
			ok = ok && strings.Contains(line, s)
		}
		if ok {
			out = append(out, line)
		}
	}
	return out
}

type driftSeeder struct {
	t    *testing.T
	ctx  context.Context
	st   store.Backend
	pool *pgxpool.Pool
}

func (s driftSeeder) task(state core.TaskState, parentID string) string {
	s.t.Helper()
	id := core.NewTaskID()
	task := core.Task{
		ID: id, Workspace: "startup", Title: "drift " + id, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id,
		State: state, NextStage: core.StageImplement, ParentTaskID: parentID, CreatedAt: time.Now().UTC(),
	}
	if parentID != "" {
		task.OriginSpecVersion, task.OriginSubID = 1, "SUB-1"
	}
	if err := s.st.CreateTask(s.ctx, task); err != nil {
		s.t.Fatal(err)
	}
	return id
}

func (s driftSeeder) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(s.ctx, sql, args...); err != nil {
		s.t.Fatal(err)
	}
}

// finishJob appends the given terminal history to an active job, as a worker
// would.
func (s driftSeeder) finishJob(kind, key string, history ...string) {
	s.t.Helper()
	job, err := logqueue.Load(s.ctx, s.st.Log(), "startup", logqueue.StreamFor(kind, key))
	if err != nil || !job.Active() {
		s.t.Fatalf("job %s:%s=%+v err=%v, want an active job", kind, key, job, err)
	}
	head := job.Head
	for i, eventKind := range history {
		payload := map[string]any{"attempt": i/2 + 1, "worker": "seed", "claimed_at": time.Now().UTC(), "error": "seeded", "next_at": time.Now().UTC()}
		encoded, _ := json.Marshal(payload)
		if _, err := s.st.Log().Append(s.ctx, "startup", job.Stream, head, []eventlog.NewEvent{{Kind: eventKind, ActorID: "seed", ActorRole: "system", Payload: encoded}}); err != nil {
			s.t.Fatal(err)
		}
		head++
	}
}

// queuedTaskWithoutJob: a queued task whose dispatch job completed.
func (s driftSeeder) queuedTaskWithoutJob() string {
	id := s.task(core.TaskQueued, "")
	s.finishJob(queue.DispatchTaskArgs{}.Kind(), id, logqueue.KindClaimed, logqueue.KindCompleted)
	return id
}

// runningTaskWithDiscardedJob: a running task whose dispatch job was
// discarded after its final attempt.
func (s driftSeeder) runningTaskWithDiscardedJob() string {
	id := s.task(core.TaskQueued, "")
	s.exec(`UPDATE tasks SET state='running' WHERE workspace_id='startup' AND id=$1`, id)
	history := []string{logqueue.KindClaimed}
	for attempt := 1; attempt < queue.DispatchTaskMaxAttempts; attempt++ {
		history = append(history, logqueue.KindFailed, logqueue.KindClaimed)
	}
	history = append(history, logqueue.KindDiscarded)
	s.finishJob(queue.DispatchTaskArgs{}.Kind(), id, history...)
	return id
}

// openBlueprintParent: a queued blueprint parent whose only child closed.
func (s driftSeeder) openBlueprintParent() string {
	parent := s.task(core.TaskQueued, "")
	child := s.task(core.TaskQueued, parent)
	s.exec(`UPDATE tasks SET state='closed' WHERE workspace_id='startup' AND id=$1`, child)
	return parent
}

// reviewWithoutPublication: a completed, publication-eligible review with no
// publication row.
func (s driftSeeder) reviewWithoutPublication() string {
	id := s.task(core.TaskRunning, "")
	jobID := id + "-review"
	if err := s.st.CreateJob(s.ctx, core.Job{ID: jobID, TaskID: id, Stage: core.StageReview, State: core.JobPending}); err != nil {
		s.t.Fatal(err)
	}
	if err := s.st.AppendEvent(s.ctx, core.Event{TaskID: id, JobID: jobID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
		"publication_eligible": true, "review_work_order_id": id + "-review-1", "verdict": "approve", "summary": "seeded",
	})}); err != nil {
		s.t.Fatal(err)
	}
	return id
}

// issuePublicationWithoutJob: a queued GitHub issue publication whose job
// completed.
func (s driftSeeder) issuePublicationWithoutJob() string {
	id := s.task(core.TaskRunning, "")
	if err := s.st.QueueGitHubLifecycle(s.ctx, core.GitHubLifecycle{TaskID: id, Repository: "acme/repo", SpecVersion: 1}); err != nil {
		s.t.Fatal(err)
	}
	s.finishJob(queue.GitHubIssuePublicationArgs{}.Kind(), id, logqueue.KindClaimed, logqueue.KindCompleted)
	return id
}

// verificationDeliveryWithoutJob: a pending verification delivery with no
// job. The row is written directly, with foreign-key triggers off for the
// transaction, because its source publication is not under test.
func (s driftSeeder) verificationDeliveryWithoutJob() string {
	id := s.task(core.TaskRunning, "")
	delivery := core.VerificationDelivery{WorkspaceID: "startup", Repository: "acme/repo", TaskID: id, ContextID: "context-" + id,
		SourcePublicationID: "publication-" + id, ID: "delivery-" + id, Generation: 1, PullRequestNumber: 7, State: "pending",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	body, err := json.Marshal(delivery)
	if err != nil {
		s.t.Fatal(err)
	}
	tx, err := s.pool.Begin(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	defer tx.Rollback(s.ctx)
	for _, statement := range []string{"SET LOCAL session_replication_role = replica", `INSERT INTO verification_publication_deliveries
		(workspace_id,id,task_id,context_id,source_publication_id,pr_key,generation,state,body)
		VALUES ('startup',$1,$2,$3,$4,$5,1,'pending',$6)`} {
		args := []any{}
		if strings.HasPrefix(statement, "INSERT") {
			args = []any{delivery.ID, id, delivery.ContextID, delivery.SourcePublicationID, store.VerificationDeliveryKey(delivery.Repository, delivery.PullRequestNumber), body}
		}
		if _, err = tx.Exec(s.ctx, statement, args...); err != nil {
			s.t.Fatal(err)
		}
	}
	if err = tx.Commit(s.ctx); err != nil {
		s.t.Fatal(err)
	}
	return id
}
