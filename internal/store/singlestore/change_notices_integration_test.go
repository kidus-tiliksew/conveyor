package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func seedSingleStoreNoticeTask(t *testing.T, st *Store) (context.Context, string, string) {
	t.Helper()
	ws := "notices-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), ws)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	id := core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: ws, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return ctx, ws, id
}

func expectNoNotice(t *testing.T, s *changes.Subscription, d time.Duration, what string) {
	t.Helper()
	select {
	case n := <-s.C():
		t.Fatalf("%s: notice %+v", what, n)
	case <-time.After(d):
	}
}

func expectNotice(t *testing.T, s *changes.Subscription, d time.Duration, what string) {
	t.Helper()
	select {
	case <-s.C():
	case <-time.After(d):
		t.Fatalf("%s: no notice within %s", what, d)
	}
}

// TestSingleStoreNoticesPublishOnlyAfterCommitIntegration holds a withTx
// transaction open after it records a task event and enqueues a job, and
// proves neither notice is published until it commits; a failed callback
// raises none (component-persistence, Change notices).
func TestSingleStoreNoticesPublishOnlyAfterCommitIntegration(t *testing.T) {
	st := openOwnedStore(t)
	ctx, ws, taskID := seedSingleStoreNoticeTask(t, st)
	if st.EnablePeers(ctx) {
		t.Fatal("SingleStore claims change notification")
	}
	tasks := st.Changes().SubscribeTask(ws, taskID)
	defer tasks.Close()
	jobs := st.Changes().Subscribe(ws, changes.Job)
	defer jobs.Close()
	written, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- st.withTx(ctx, func(tx *sql.Tx) error {
			if err := insertEvent(ctx, tx, core.Event{TaskID: taskID, Kind: "notice.held"}); err != nil {
				return err
			}
			if _, err := logqueue.Enqueue(logTx(ctx, tx), st.log, ws, "notice_probe", "held", struct{}{}, 1, time.Now().UTC()); err != nil {
				return err
			}
			close(written)
			<-release
			return nil
		})
	}()
	<-written
	expectNoNotice(t, tasks, 200*time.Millisecond, "task notice before commit")
	expectNoNotice(t, jobs, 10*time.Millisecond, "job notice before commit")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	expectNotice(t, tasks, time.Second, "task notice after commit")
	expectNotice(t, jobs, time.Second, "job notice after commit")

	failure := errors.New("callback failed")
	err := st.withTx(ctx, func(tx *sql.Tx) error {
		if err := insertEvent(ctx, tx, core.Event{TaskID: taskID, Kind: "notice.rolled_back"}); err != nil {
			return err
		}
		if _, err := logqueue.Enqueue(logTx(ctx, tx), st.log, ws, "notice_probe", "rolled-back", struct{}{}, 1, time.Now().UTC()); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
	expectNoNotice(t, tasks, 100*time.Millisecond, "rolled-back task event")
	expectNoNotice(t, jobs, 10*time.Millisecond, "rolled-back enqueue")
}

func singleStoreNoticeRuntime(t *testing.T, st *Store, ws string) <-chan time.Time {
	t.Helper()
	started := make(chan time.Time, 1)
	rt := logqueue.NewRuntime(st.Log(), logqueue.Options{Workspaces: []string{ws}, Changes: st.Changes()})
	rt.Register(queue.Registration{Kind: "notice_probe", Handle: func(context.Context, queue.Job) error {
		started <- time.Now()
		return nil
	}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rt.Stop(ctx)
	})
	return started
}

func commitSingleStoreProbeJob(t *testing.T, st *Store, ctx context.Context, ws, taskID string) time.Time {
	t.Helper()
	if err := st.withTx(ctx, func(tx *sql.Tx) error {
		if err := insertEvent(ctx, tx, core.Event{TaskID: taskID, Kind: "notice.probe"}); err != nil {
			return err
		}
		inserted, err := logqueue.Enqueue(logTx(ctx, tx), st.log, ws, "notice_probe", taskID, struct{}{}, 1, time.Now().UTC())
		if err == nil && !inserted {
			err = errors.New("probe job not inserted")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return time.Now()
}

func singleStoreStartedWithin(t *testing.T, started <-chan time.Time, committed time.Time, bound time.Duration, what string) {
	t.Helper()
	select {
	case at := <-started:
		latency := at.Sub(committed)
		t.Logf("%s: job started %s after commit (bound %s)", what, latency, bound)
		if latency > bound {
			t.Fatalf("%s: job started %s after commit; bound %s", what, latency, bound)
		}
	case <-time.After(bound):
		t.Fatalf("%s: job did not start within %s of its commit", what, bound)
	}
}

// TestSingleStoreJobStartsWithinOneSecondOfCommitIntegration: the
// committing daemon starts the job on its own notice
// (req-quiet-idle-runtime AC-2.1).
func TestSingleStoreJobStartsWithinOneSecondOfCommitIntegration(t *testing.T) {
	st := openOwnedStore(t)
	ctx, ws, taskID := seedSingleStoreNoticeTask(t, st)
	started := singleStoreNoticeRuntime(t, st, ws)
	committed := commitSingleStoreProbeJob(t, st, ctx, ws, taskID)
	singleStoreStartedWithin(t, started, committed, time.Second, "same daemon")
}

// TestSingleStorePeerCheckStartsJobWithinThirtySecondsIntegration: a second
// daemon on SingleStore, which has no change notification, finds a job the
// first committed through its daemon-wide peer check, which runs every 30
// seconds and wakes the runtime with a job notice; the runtime keeps no peer
// timer of its own (feature-quiet-idle-runtime, Peers;
// req-quiet-idle-runtime AC-2.5).
func TestSingleStorePeerCheckStartsJobWithinThirtySecondsIntegration(t *testing.T) {
	committer := openOwnedStore(t)
	peer := reopenSharedStore(t)
	ctx, ws, taskID := seedSingleStoreNoticeTask(t, committer)
	checks := make(chan time.Time, 4)
	check := changes.SelectPeerCheck(ctx, config.RuntimeSettings{Peers: true}.Peers, peer, peer.Changes(), logqueue.PeerTailInterval, t.Logf)
	if check == nil {
		t.Fatal("SingleStore with peers selected no peer check")
	}
	check.Checked = func(_ int, err error) {
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("peer check: %v", err)
		}
		checks <- time.Now()
	}
	// The daemon's start order: the check's first statement, then the
	// runtime's first catch-up, then the check on its ticker.
	if err := check.Check(ctx); err != nil {
		t.Fatal(err)
	}
	<-checks
	started := singleStoreNoticeRuntime(t, peer, ws)
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go check.Run(runCtx)
	// Commit partway through the first period, so the job can reach the
	// peer only through the next check.
	time.Sleep(12 * time.Second)
	committed := commitSingleStoreProbeJob(t, committer, ctx, ws, taskID)
	select {
	case at := <-started:
		if at.Sub(committed) < time.Second {
			t.Fatalf("peer started the job %s after commit, before its next check", at.Sub(committed))
		}
		singleStoreStartedWithin(t, singleStoreReplay(at), committed, logqueue.PeerTailInterval, "peer check")
	case <-time.After(logqueue.PeerTailInterval):
		t.Fatalf("peer daemon did not start the job within %s of its commit", logqueue.PeerTailInterval)
	}
}

func singleStoreReplay(at time.Time) <-chan time.Time {
	replay := make(chan time.Time, 1)
	replay <- at
	return replay
}
