package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// noticeSchema is one migrated schema that several stores, standing in for
// several daemons, open with their own pools.
type noticeSchema struct {
	url    string
	schema string
	admin  *pgxpool.Pool
}

func newNoticeSchema(t *testing.T) *noticeSchema {
	t.Helper()
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "change_notices_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	x := &noticeSchema{url: databaseURL, schema: schema, admin: admin}
	pool := x.pool(t, "")
	if err = Migrate(store.WithActor(t.Context(), store.SystemActor()), pool); err != nil {
		t.Fatal(err)
	}
	return x
}

// pool opens a pool on the schema; name, when set, is its application_name.
func (x *noticeSchema) pool(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(x.url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = x.schema
	if name != "" {
		config.ConnConfig.RuntimeParams["application_name"] = name
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// daemon opens a store, as one daemon does, closed when the test ends.
func (x *noticeSchema) daemon(t *testing.T, name string) *Store {
	t.Helper()
	st := newStore(x.pool(t, name))
	t.Cleanup(st.peers.close)
	return st
}

// seedTask bootstraps a workspace and creates one task in it.
func seedNoticeTask(t *testing.T, st *Store) (context.Context, string, string) {
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

func quiet(t *testing.T, s *changes.Subscription, d time.Duration, what string) {
	t.Helper()
	select {
	case n := <-s.C():
		t.Fatalf("%s: notice %+v", what, n)
	case <-time.After(d):
	}
}

func arrives(t *testing.T, s *changes.Subscription, d time.Duration, what string) changes.Notice {
	t.Helper()
	select {
	case n := <-s.C():
		return n
	case <-time.After(d):
		t.Fatalf("%s: no notice within %s", what, d)
	}
	return changes.Notice{}
}

// TestNoticesPublishOnlyAfterCommitIntegration holds a transaction open
// after it records a task event and enqueues a job, and proves neither
// notice is published until it commits; a failed callback and a rolled-back
// savepoint raise none (component-persistence, Change notices).
func TestNoticesPublishOnlyAfterCommitIntegration(t *testing.T) {
	x := newNoticeSchema(t)
	st := x.daemon(t, "")
	ctx, ws, taskID := seedNoticeTask(t, st)
	tasks := st.Changes().SubscribeTask(ws, taskID)
	defer tasks.Close()
	jobs := st.Changes().Subscribe(ws, changes.Job)
	defer jobs.Close()

	written, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- st.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
			if err := insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "notice.held"}); err != nil {
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
	quiet(t, tasks, 200*time.Millisecond, "task notice before commit")
	quiet(t, jobs, 10*time.Millisecond, "job notice before commit")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	arrives(t, tasks, time.Second, "task notice after commit")
	arrives(t, jobs, time.Second, "job notice after commit")

	failure := errors.New("callback failed")
	err := st.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "notice.rolled_back"}); err != nil {
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
	quiet(t, tasks, 100*time.Millisecond, "rolled-back task event")
	quiet(t, jobs, 10*time.Millisecond, "rolled-back enqueue")

	if err := st.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if err := insertEvent(ctx, db.New(sp), core.Event{TaskID: taskID, Kind: "notice.savepoint"}); err != nil {
			return err
		}
		return sp.Rollback(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	quiet(t, tasks, 100*time.Millisecond, "rolled-back savepoint")
	if err := st.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if err := insertEvent(ctx, db.New(sp), core.Event{TaskID: taskID, Kind: "notice.released"}); err != nil {
			return err
		}
		return sp.Commit(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	arrives(t, tasks, time.Second, "released savepoint after commit")
}

// TestPeerNotifySentInsideTransactionIntegration: with peers on, another
// daemon hears a notice only after the committing transaction commits
// (DEC-99(2)).
func TestPeerNotifySentInsideTransactionIntegration(t *testing.T) {
	x := newNoticeSchema(t)
	a, b := x.daemon(t, ""), x.daemon(t, "")
	ctx, ws, taskID := seedNoticeTask(t, a)
	if !a.EnablePeers(ctx) || !b.EnablePeers(ctx) {
		t.Fatal("PostgreSQL EnablePeers reported no notification")
	}
	remote := b.Changes().SubscribeTask(ws, taskID)
	defer remote.Close()
	local := a.Changes().SubscribeTask(ws, taskID)
	defer local.Close()
	written, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
			if err := insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "notice.peer"}); err != nil {
				return err
			}
			close(written)
			<-release
			return nil
		})
	}()
	<-written
	quiet(t, remote, 300*time.Millisecond, "peer notice before commit")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := arrives(t, remote, 2*time.Second, "peer notice after commit"); n.Workspace != ws || n.TaskID != taskID || n.Kind != changes.Task {
		t.Fatalf("peer notice=%+v", n)
	}
	arrives(t, local, time.Second, "local notice after commit")
}

// TestPeerNoticeSurvivesFailedLocalCommitIntegration is the regression for a
// lost peer wake: daemon A's transaction raised a job notice for a workspace
// and is inside COMMIT, after its pg_notify, when daemon B commits a job in
// the same workspace. A must publish B's identical payload, and A's own
// commit then fails, so B's notice is the only wake A gets
// (req-quiet-idle-runtime AC-2.1; DEC-99(2); component-persistence, Peers).
func TestPeerNoticeSurvivesFailedLocalCommitIntegration(t *testing.T) {
	x := newNoticeSchema(t)
	name := "notices-" + strings.ReplaceAll(core.NewTaskID(), "-", "")
	a, b := x.daemon(t, name), x.daemon(t, "")
	ctx, ws, taskID := seedNoticeTask(t, a)
	if !a.EnablePeers(ctx) || !b.EnablePeers(ctx) || !a.peers.listening() {
		t.Fatal("peer listeners not up")
	}
	// A deferred constraint trigger runs at COMMIT, after A's pg_notify: it
	// waits for the test's advisory lock and then fails the commit.
	const gateKey = 7311
	if _, err := x.pool(t, "").Exec(t.Context(), `
CREATE TABLE notice_commit_gate (id int);
CREATE FUNCTION notice_commit_gate_fail() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	PERFORM pg_advisory_xact_lock(7311);
	RAISE EXCEPTION 'commit gate closed';
END $$;
CREATE CONSTRAINT TRIGGER notice_commit_gate_fail AFTER INSERT ON notice_commit_gate
	DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION notice_commit_gate_fail();`); err != nil {
		t.Fatal(err)
	}
	gate, err := x.admin.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Release()
	if _, err := gate.Exec(t.Context(), `SELECT pg_advisory_lock($1)`, gateKey); err != nil {
		t.Fatal(err)
	}
	unlocked := false
	defer func() {
		if !unlocked {
			_, _ = gate.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, gateKey)
		}
	}()

	jobs := a.Changes().Subscribe(ws, changes.Job)
	defer jobs.Close()
	done := make(chan error, 1)
	go func() {
		done <- a.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			// The job notice is raised directly: a real job append would
			// hold the workspace's log position lock and serialize B's job
			// behind this transaction.
			raiseTx(tx, changes.Notice{Workspace: ws, Kind: changes.Job})
			_, err := tx.Exec(ctx, `INSERT INTO notice_commit_gate VALUES (1)`)
			return err
		})
	}()
	// Wait until A's COMMIT is blocked in the deferred trigger.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := x.admin.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
			WHERE application_name=$1 AND wait_event_type='Lock' AND wait_event='advisory'`, name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon A never reached its blocked COMMIT")
		}
		time.Sleep(10 * time.Millisecond)
	}
	quiet(t, jobs, 100*time.Millisecond, "job notice before any commit")

	commitProbeJob(t, b, ctx, ws, taskID)
	if n := arrives(t, jobs, 2*time.Second, "peer job notice while the local commit is pending"); n.Workspace != ws || n.Kind != changes.Job {
		t.Fatalf("peer notice=%+v", n)
	}

	if _, err := gate.Exec(t.Context(), `SELECT pg_advisory_unlock($1)`, gateKey); err != nil {
		t.Fatal(err)
	}
	unlocked = true
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("local commit succeeded; the gate should have failed it")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("local commit did not finish after the gate opened")
	}
	quiet(t, jobs, 200*time.Millisecond, "failed local commit")
}

func listenerBackends(t *testing.T, admin *pgxpool.Pool, name string) int {
	t.Helper()
	var count int
	if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1`, listenerApplicationName(name)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestPeerListenerReconnectsOnNextRequestIntegration closes every
// connection of a daemon with pg_terminate_backend, as a suspending
// database does, and proves the listener stays closed while no request
// arrives, the next request succeeds, and only then does the listener
// reconnect and catch every workspace up (req-quiet-idle-runtime AC-1.4).
func TestPeerListenerReconnectsOnNextRequestIntegration(t *testing.T) {
	x := newNoticeSchema(t)
	name := "notices-" + strings.ReplaceAll(core.NewTaskID(), "-", "")
	st := x.daemon(t, name)
	ctx, ws, _ := seedNoticeTask(t, st)
	if !st.EnablePeers(ctx) || !st.peers.listening() {
		t.Fatal("listener not up after EnablePeers")
	}
	if got := listenerBackends(t, x.admin, name); got != 1 {
		t.Fatalf("listener backends=%d, want 1", got)
	}
	if _, err := x.admin.Exec(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name IN ($1, $2)`, name, listenerApplicationName(name)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for st.peers.listening() {
		if time.Now().After(deadline) {
			t.Fatal("listener did not notice its connection closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	catchUp := st.Changes().Subscribe(ws, changes.Job)
	defer catchUp.Close()
	// No request: the listener stays closed. The wait also outlasts pgx's
	// one-second idle threshold, after which an acquired connection is
	// pinged and replaced when dead.
	for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if st.peers.listening() || listenerBackends(t, x.admin, name) != 0 {
			t.Fatal("listener reconnected without a request")
		}
	}
	quiet(t, catchUp, 10*time.Millisecond, "catch-up before a request")
	if _, err := st.GetWorkspace(ctx, ws); err != nil {
		t.Fatalf("first request after the database closed every connection: %v", err)
	}
	if n := arrives(t, catchUp, 5*time.Second, "catch-up after reconnect"); n.Workspace != "" || n.Kind != changes.Job {
		t.Fatalf("catch-up notice=%+v, want every workspace", n)
	}
	if !st.peers.listening() || listenerBackends(t, x.admin, name) != 1 {
		t.Fatal("listener not reconnected after the request")
	}
}

// noticeRuntime starts a runtime on st that records when its probe job
// starts.
func noticeRuntime(t *testing.T, st *Store, ws string) <-chan time.Time {
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

// commitProbeJob enqueues the probe job with a task event in one store
// transaction and returns when the transaction committed.
func commitProbeJob(t *testing.T, st *Store, ctx context.Context, ws, taskID string) time.Time {
	t.Helper()
	if err := st.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "notice.probe"}); err != nil {
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

func startedWithin(t *testing.T, started <-chan time.Time, committed time.Time, bound time.Duration, what string) {
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

// TestJobStartsWithinOneSecondOfCommitIntegration: the committing daemon
// starts the job on its own notice (req-quiet-idle-runtime AC-2.1).
func TestJobStartsWithinOneSecondOfCommitIntegration(t *testing.T) {
	x := newNoticeSchema(t)
	st := x.daemon(t, "")
	ctx, ws, taskID := seedNoticeTask(t, st)
	started := noticeRuntime(t, st, ws)
	committed := commitProbeJob(t, st, ctx, ws, taskID)
	startedWithin(t, started, committed, time.Second, "same daemon")
}

// TestPeerDaemonStartsJobWithinTwoSecondsIntegration: with peers on, a
// second daemon sharing the database starts a job the first committed
// (req-quiet-idle-runtime AC-2.1).
func TestPeerDaemonStartsJobWithinTwoSecondsIntegration(t *testing.T) {
	x := newNoticeSchema(t)
	committer, peer := x.daemon(t, ""), x.daemon(t, "")
	ctx, ws, taskID := seedNoticeTask(t, committer)
	committer.EnablePeers(ctx)
	if !peer.EnablePeers(ctx) || !peer.peers.listening() {
		t.Fatal("peer listener not up")
	}
	started := noticeRuntime(t, peer, ws)
	// Let the peer's start-up catch-up finish on an empty log, so the job
	// can reach it only through the peer notification.
	time.Sleep(200 * time.Millisecond)
	committed := commitProbeJob(t, committer, ctx, ws, taskID)
	startedWithin(t, started, committed, 2*time.Second, "peer daemon")
}
