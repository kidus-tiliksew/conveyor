package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Task-event visibility on the peer check (component-persistence invariant
// 42; component-runtime invariant 36; req-quiet-idle-runtime AC-2.5).
//
// SingleStore commits a transaction partition by partition. The counter row
// in task_event_positions is sharded by workspace and an event row by its
// task, so a peer can read the advanced counter before the event row. These
// tests reproduce that order deterministically: a held transaction writes
// the task event and stays open, so its event row is invisible to every
// other connection, while the commit preparation for that same event runs
// and commits in a second transaction, as the counter's partition would.
// Releasing the held transaction then makes the event visible.

// heldTaskEvent is a task event written in a transaction that stays open.
type heldTaskEvent struct {
	st        *Store
	ctx       context.Context
	tx        *sql.Tx
	witnesses []eventWitness
}

// holdTaskEvent writes a task event through taskEvent in a registered store
// transaction and keeps the transaction open. The event's commit preparation
// moves to commitPositionFirst.
func holdTaskEvent(t *testing.T, st *Store, ctx context.Context, taskID, kind string) *heldTaskEvent {
	t.Helper()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	st.beginNotices(tx)
	held := &heldTaskEvent{st: st, ctx: ctx, tx: tx}
	t.Cleanup(func() {
		if held.tx != nil {
			_ = held.tx.Rollback()
			finishNotices(held.tx, false)
		}
	})
	if err = taskEvent(ctx, tx, core.Event{TaskID: taskID, Kind: kind}); err != nil {
		t.Fatal(err)
	}
	value, _ := txBatches.Load(tx)
	state := value.(*txNoticeState)
	ws := documentWorkspace(ctx)
	held.witnesses = state.taskEvents[ws]
	if len(held.witnesses) != 1 || held.witnesses[0].id == 0 {
		t.Fatalf("held event witnesses=%+v", held.witnesses)
	}
	delete(state.taskEvents, ws)
	return held
}

func (h *heldTaskEvent) id() int64 { return h.witnesses[0].id }

// commitPositionFirst runs the held events' commit preparation and commits
// it before their event rows: the counter's partition committed first.
func commitPositionFirst(t *testing.T, held ...*heldTaskEvent) {
	t.Helper()
	st, ctx := held[0].st, held[0].ctx
	if err := st.withTx(ctx, func(tx *sql.Tx) error {
		value, _ := txBatches.Load(tx)
		state := value.(*txNoticeState)
		for _, h := range held {
			ws := documentWorkspace(h.ctx)
			state.taskEvents[ws] = append(state.taskEvents[ws], h.witnesses...)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// release commits the held transaction: its event row becomes visible.
func (h *heldTaskEvent) release(t *testing.T) {
	t.Helper()
	err := commitTx(h.ctx, h.tx)
	finishNotices(h.tx, err == nil)
	if err != nil {
		t.Fatal(err)
	}
	h.tx = nil
}

func taskEventPosition(t *testing.T, st *Store, ws string) (position int64, witnesses int) {
	t.Helper()
	if err := st.db.QueryRowContext(t.Context(), `SELECT COALESCE(MAX(position),0) FROM task_event_positions WHERE workspace_id=?`, ws).Scan(&position); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_event_witnesses WHERE workspace_id=?`, ws).Scan(&witnesses); err != nil {
		t.Fatal(err)
	}
	return position, witnesses
}

// peerProbe runs a peer check on injected ticks and reports, per tick, which
// workspaces got a task notice. Each tick waits for its check's outcome, so
// no assertion depends on a sleep.
type peerProbe struct {
	t        *testing.T
	ticks    chan time.Time
	outcomes chan error
	subs     map[string]*changes.Subscription
}

func startPeerProbe(t *testing.T, reader changes.PeerReader, workspaces ...string) *peerProbe {
	t.Helper()
	hub := changes.NewHub()
	p := &peerProbe{t: t, ticks: make(chan time.Time), outcomes: make(chan error, 1), subs: map[string]*changes.Subscription{}}
	for _, ws := range workspaces {
		p.subs[ws] = hub.Subscribe(ws, changes.Task)
	}
	check := &changes.PeerCheck{Reader: reader, Hub: hub, Interval: time.Hour,
		Ticker:  func(time.Duration) (<-chan time.Time, func()) { return p.ticks, func() {} },
		Checked: func(_ int, err error) { p.outcomes <- err },
		Logf:    t.Logf,
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		check.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return p
}

// tick runs one check and returns the workspaces it sent a task notice.
func (p *peerProbe) tick(what string) map[string]bool {
	p.t.Helper()
	select {
	case p.ticks <- time.Now():
	case <-time.After(30 * time.Second):
		p.t.Fatalf("%s: the peer check took no tick", what)
	}
	select {
	case err := <-p.outcomes:
		if err != nil {
			p.t.Fatalf("%s: peer check: %v", what, err)
		}
	case <-time.After(30 * time.Second):
		p.t.Fatalf("%s: the peer check reported no outcome", what)
	}
	// Check publishes before it reports its outcome.
	got := map[string]bool{}
	for ws, sub := range p.subs {
		select {
		case <-sub.C():
			got[ws] = true
		default:
		}
	}
	return got
}

func (p *peerProbe) expect(what string, workspaces ...string) {
	p.t.Helper()
	got := p.tick(what)
	want := map[string]bool{}
	for _, ws := range workspaces {
		want[ws] = true
	}
	for ws := range p.subs {
		if got[ws] != want[ws] {
			p.t.Fatalf("%s: task notices %v, want %v", what, got, want)
		}
	}
}

// legacyPeerReader is the peer read before witnesses: each workspace's
// log and task-event counters, with no visibility check.
type legacyPeerReader struct{ db *sql.DB }

func (r legacyPeerReader) PeerMarks(ctx context.Context) (map[string]changes.PeerMark, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT workspace_id, 0, position FROM event_log_positions UNION ALL SELECT workspace_id, 1, position FROM task_event_positions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	marks := map[string]changes.PeerMark{}
	for rows.Next() {
		var ws string
		var source int
		var n int64
		if err = rows.Scan(&ws, &source, &n); err != nil {
			return nil, err
		}
		mark := marks[ws]
		if source == 0 {
			mark.Log = n
		} else {
			mark.Events = n
		}
		marks[ws] = mark
	}
	return marks, rows.Err()
}

// TestSingleStorePeerMarksVisibilityIntegration: a peer never accepts a
// task-event position whose event it cannot read, and accepts it at the
// first check after the event becomes visible, without another task event.
// Positions are taken in commit order, so a later visible event, or a higher
// event ID, never hides an earlier invisible one. Workspaces stay separate,
// a fresh peer accepts no unvalidated baseline, a failed read keeps the
// accepted positions, and a rollback advances nothing.
func TestSingleStorePeerMarksVisibilityIntegration(t *testing.T) {
	writer := openOwnedStore(t)
	ctxA, wsA, taskA := seedSingleStoreNoticeTask(t, writer)
	ctxB, wsB, taskB := seedSingleStoreNoticeTask(t, writer)
	taskA2 := core.NewTaskID()
	if err := writer.CreateTask(ctxA, core.Task{ID: taskA2, Workspace: wsA, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + taskA2, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	daemon, proxy := openProxiedStore(t)
	witnessRows := -1
	observe := func(rows int) { witnessRows = rows }
	observePeerWitnessRows.Store(&observe)
	t.Cleanup(func() { observePeerWitnessRows.Store(nil) })
	peer := startPeerProbe(t, daemon, wsA, wsB)
	peer.expect("first check", wsA, wsB)
	before := proxy.count()
	peer.expect("idle check")
	if sent := proxy.since(before); len(sent) != 1 || witnessRows != 0 {
		t.Fatalf("idle check sent %d statements with %d witness rows, want one statement and none:\n%s", len(sent), witnessRows, describeCommands(time.Now(), sent))
	}

	// The old read, the counter alone, fails this regression: it notices
	// the counter before the event is visible and never again after.
	legacy := startPeerProbe(t, legacyPeerReader{daemon.db}, wsA, wsB)
	legacy.expect("legacy first check", wsA, wsB)
	missed := holdTaskEvent(t, writer, ctxA, taskA, "peer.legacy")
	commitPositionFirst(t, missed)
	legacy.expect("legacy: counter visible, event not", wsA)
	peer.expect("counter visible, event not")
	missed.release(t)
	legacy.expect("legacy: event visible, nothing noticed")
	peer.expect("event visible", wsA)

	// The regression: the counter commits before the event row.
	start, _ := taskEventPosition(t, writer, wsA)
	held := holdTaskEvent(t, writer, ctxA, taskA, "peer.held")
	commitPositionFirst(t, held)
	if position, _ := taskEventPosition(t, writer, wsA); position != start+1 {
		t.Fatalf("counter=%d, want %d before the event is visible", position, start+1)
	}
	before = proxy.count()
	peer.expect("counter visible, event not")
	if sent := proxy.since(before); len(sent) != 1 || witnessRows != 1 {
		t.Fatalf("a changed check sent %d statements with %d witness rows, want one statement and one row", len(sent), witnessRows)
	}
	peer.expect("still invisible")
	held.release(t)
	peer.expect("event visible, no further event", wsA)
	peer.expect("accepted")
	if witnessRows != 0 {
		t.Fatalf("a read after acceptance returned %d witness rows", witnessRows)
	}

	// A workspace that appears after the peer accepted the others is read
	// through the ranges between and around them, and its first witnessed
	// position is accepted.
	for _, prefix := range []string{"a-first-", "zz-last-"} {
		ws := prefix + core.NewTaskID()
		wsCtx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), ws)
		if _, err := writer.BootstrapWorkspaceConfig(wsCtx, &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
			t.Fatal(err)
		}
		id := core.NewTaskID()
		if err := writer.CreateTask(wsCtx, core.Task{ID: id, Workspace: ws, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		counter, _ := taskEventPosition(t, writer, ws)
		marks, err := daemon.PeerMarks(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if counter == 0 || marks[ws].Events != counter {
			t.Fatalf("new workspace %s: accepted %d, counter %d", ws, marks[ws].Events, counter)
		}
	}
	peer.expect("other workspaces appeared")

	// Several pending transactions in two workspaces. Commit order differs
	// from event-ID order: the higher ID takes the earlier position.
	first := holdTaskEvent(t, writer, ctxA, taskA, "peer.first")
	second := holdTaskEvent(t, writer, ctxA, taskA2, "peer.second")
	other := holdTaskEvent(t, writer, ctxB, taskB, "peer.other")
	low, high := first, second
	if low.id() > high.id() {
		low, high = high, low
	}
	commitPositionFirst(t, high)
	commitPositionFirst(t, low)
	commitPositionFirst(t, other)
	peer.expect("three positions, no event visible")
	low.release(t)
	peer.expect("the later position's lower-ID event is visible, the earlier one is not")
	other.release(t)
	peer.expect("the other workspace's event is visible", wsB)
	high.release(t)
	peer.expect("both earlier events visible", wsA)
	peer.expect("all accepted")

	// A fresh peer accepts no position whose event it cannot read. It opens
	// before the held transaction, as a daemon would have started earlier.
	fresh, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	pending := holdTaskEvent(t, writer, ctxB, taskB, "peer.fresh")
	commitPositionFirst(t, pending)
	counter, _ := taskEventPosition(t, writer, wsB)
	marks, err := fresh.PeerMarks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if marks[wsB].Events >= counter {
		t.Fatalf("a fresh peer accepted position %d of counter %d before its event was visible", marks[wsB].Events, counter)
	}
	// A failed read returns nothing and keeps what the last one accepted.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if failed, err := fresh.PeerMarks(cancelled); err == nil || failed != nil {
		t.Fatalf("a cancelled read returned marks=%v err=%v", failed, err)
	}
	pending.release(t)
	if marks, err = fresh.PeerMarks(context.Background()); err != nil || marks[wsB].Events != counter {
		t.Fatalf("a fresh peer after the event became visible: marks=%+v err=%v, want position %d", marks[wsB], err, counter)
	}
	peer.expect("the long-running peer sees it too", wsB)

	// A rollback advances neither the counter nor the witnesses.
	position, witnesses := taskEventPosition(t, writer, wsA)
	rollback := errors.New("rollback probe")
	if err := writer.withTx(ctxA, func(tx *sql.Tx) error {
		if err := taskEvent(ctxA, tx, core.Event{TaskID: taskA, Kind: "peer.rollback"}); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("rollback probe: %v", err)
	}
	if p, w := taskEventPosition(t, writer, wsA); p != position || w != witnesses {
		t.Fatalf("rollback moved position %d->%d, witnesses %d->%d", position, p, witnesses, w)
	}
	peer.expect("after rollback")
}

// TestSingleStoreTaskEventWritePathIntegration: every task-event commit
// advances its workspace's position once and records one witness per event;
// a later commit removes witnesses whose events it can read; a task event in
// a transaction without notice state is refused before it is written; and
// verification delivery commits through the same preparation.
func TestSingleStoreTaskEventWritePathIntegration(t *testing.T) {
	writer := openOwnedStore(t)
	ctxA, wsA, taskA := seedSingleStoreNoticeTask(t, writer)
	ctxB, wsB, taskB := seedSingleStoreNoticeTask(t, writer)
	var mu sync.Mutex
	var prepared [][]string
	observe := func(workspaces []string) {
		mu.Lock()
		prepared = append(prepared, append([]string(nil), workspaces...))
		mu.Unlock()
	}
	observeTaskEventPreparation.Store(&observe)
	t.Cleanup(func() { observeTaskEventPreparation.Store(nil) })
	takePrepared := func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		out := prepared
		prepared = nil
		return out
	}

	// An ordinary event: one position, one witness, earlier ones removed.
	startA, _ := taskEventPosition(t, writer, wsA)
	takePrepared()
	if err := writer.AppendEvent(ctxA, core.Event{TaskID: taskA, Kind: "peer.ordinary"}); err != nil {
		t.Fatal(err)
	}
	if p, w := taskEventPosition(t, writer, wsA); p != startA+1 || w != 1 {
		t.Fatalf("ordinary event: position %d->%d with %d witnesses, want +1 with 1", startA, p, w)
	}
	if got := takePrepared(); len(got) != 1 || len(got[0]) != 1 || got[0][0] != wsA {
		t.Fatalf("ordinary event preparations=%v", got)
	}

	// One transaction with two events in one workspace and one in another:
	// each workspace advances once, with one witness per event.
	startB, _ := taskEventPosition(t, writer, wsB)
	if err := writer.withTx(ctxA, func(tx *sql.Tx) error {
		for _, kind := range []string{"peer.multi.one", "peer.multi.two"} {
			if err := taskEvent(ctxA, tx, core.Event{TaskID: taskA, Kind: kind}); err != nil {
				return err
			}
		}
		return taskEvent(ctxB, tx, core.Event{TaskID: taskB, Kind: "peer.multi.other"})
	}); err != nil {
		t.Fatal(err)
	}
	if p, w := taskEventPosition(t, writer, wsA); p != startA+2 || w != 2 {
		t.Fatalf("multi-event commit in %s: position %d with %d witnesses, want %d with 2", wsA, p, w, startA+2)
	}
	if p, w := taskEventPosition(t, writer, wsB); p != startB+1 || w != 1 {
		t.Fatalf("multi-event commit in %s: position %d with %d witnesses, want %d with 1", wsB, p, w, startB+1)
	}
	if got := takePrepared(); len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("multi-event preparations=%v, want one with both workspaces", got)
	}

	// A held event's witness survives later commits until its event is
	// visible to them.
	held := holdTaskEvent(t, writer, ctxA, taskA, "peer.pending")
	commitPositionFirst(t, held)
	if err := writer.AppendEvent(ctxA, core.Event{TaskID: taskA, Kind: "peer.after"}); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := writer.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_event_witnesses WHERE workspace_id=? AND event_id=?`, wsA, held.id()).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("the invisible event's witness count=%d err=%v, want it kept", kept, err)
	}
	held.release(t)
	if err := writer.AppendEvent(ctxA, core.Event{TaskID: taskA, Kind: "peer.later"}); err != nil {
		t.Fatal(err)
	}
	if _, w := taskEventPosition(t, writer, wsA); w != 1 {
		t.Fatalf("witnesses after the held event became visible=%d, want only the newest", w)
	}
	takePrepared()

	// A task event outside a registered transaction is refused unwritten.
	var events int
	countEvents := func() int {
		var n int
		if err := writer.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=? AND task_id=?`, wsA, taskA).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	events = countEvents()
	tx, err := writer.db.BeginTx(ctxA, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = taskEvent(ctxA, tx, core.Event{TaskID: taskA, Kind: "peer.unregistered"})
	_ = tx.Rollback()
	if !errors.Is(err, errTaskEventOutsideNotices) {
		t.Fatalf("unregistered task event err=%v, want %v", err, errTaskEventOutsideNotices)
	}
	if n := countEvents(); n != events {
		t.Fatalf("the refused event was written: %d -> %d", events, n)
	}

	// Verification delivery commits through the same preparation.
	position, _ := taskEventPosition(t, writer, wsA)
	args := queue.VerificationPublicationArgs{WorkspaceID: wsA, Repository: "owner/repo", PullRequestNumber: 1}
	if err := writer.RunVerificationDelivery(ctxA, args, func(*core.VerificationDelivery, func(string, core.VerificationDelivery) error) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := takePrepared(); len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("verification delivery preparations=%v, want one with no task event", got)
	}
	if p, _ := taskEventPosition(t, writer, wsA); p != position {
		t.Fatalf("verification delivery without a task event moved the position %d -> %d", position, p)
	}
	// So does the reconcile pass, and a task event in one of its per-key
	// transactions advances the position like any other commit.
	if err := writer.ReconcileVerificationDeliveries(ctxA); err != nil {
		t.Fatal(err)
	}
	if got := takePrepared(); len(got) == 0 {
		t.Fatal("the verification reconcile pass committed without the task-event preparation")
	}
	keyHeld, err := writer.withKeyTx(ctxA, "verification-publication:"+wsA+":owner/repo#1", func(tx *sql.Tx) error {
		return taskEvent(ctxA, tx, core.Event{TaskID: taskA, Kind: "peer.key_tx"})
	})
	if err != nil || !keyHeld {
		t.Fatalf("per-key transaction held=%v err=%v", keyHeld, err)
	}
	if p, w := taskEventPosition(t, writer, wsA); p != position+1 || w != 1 {
		t.Fatalf("a task event in a per-key transaction: position %d -> %d with %d witnesses, want +1 with 1", position, p, w)
	}
	if got := takePrepared(); len(got) != 1 || len(got[0]) != 1 || got[0][0] != wsA {
		t.Fatalf("per-key transaction preparations=%v", got)
	}
}

// TestSingleStoreTaskEventWitnessUpgradeIntegration: a database upgraded
// from before migration 0046 keeps its counters but has no witnesses. A peer
// accepts none of those unvalidated positions and accepts the first position
// a new commit witnesses.
func TestSingleStoreTaskEventWitnessUpgradeIntegration(t *testing.T) {
	writer := openOwnedStore(t)
	ctx, ws, taskID := seedSingleStoreNoticeTask(t, writer)
	// Reconstruct the pre-0046 schema: drop the table and its ledger row,
	// then run startup migration again.
	if _, err := writer.db.ExecContext(ctx, `DROP TABLE task_event_witnesses`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.db.ExecContext(ctx, `DELETE FROM conveyor_singlestore_migrations WHERE version=46`); err != nil {
		t.Fatal(err)
	}
	if err := writer.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	counter, witnesses := taskEventPosition(t, writer, ws)
	if counter == 0 || witnesses != 0 {
		t.Fatalf("upgraded database: counter=%d witnesses=%d, want a counter and no witness", counter, witnesses)
	}
	peer, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	marks, err := peer.PeerMarks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if marks[ws].Events != 0 {
		t.Fatalf("a peer accepted unwitnessed position %d", marks[ws].Events)
	}
	if again, err := peer.PeerMarks(context.Background()); err != nil || again[ws] != marks[ws] {
		t.Fatalf("an idle read moved the mark: %+v -> %+v (err %v)", marks[ws], again[ws], err)
	}
	if err := writer.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "peer.upgraded"}); err != nil {
		t.Fatal(err)
	}
	if marks, err = peer.PeerMarks(context.Background()); err != nil || marks[ws].Events != counter+1 {
		t.Fatalf("after the first witnessed commit: marks=%+v err=%v, want position %d", marks[ws], err, counter+1)
	}
}
