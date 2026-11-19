package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The cost of a task-event commit and witness pruning on SingleStore
// (component-persistence, Change notices, invariants 27 and 42;
// component-runtime invariant 36; req-quiet-idle-runtime AC-1.3, AC-2.5).
// An ordinary commit sends two statements per written workspace beyond its
// event writes; a workspace's first commit adds the counter insert; a commit
// that takes a prune position adds one delete of fixed shape.

// witnessRows returns the workspace's witnesses as position -> event IDs.
func witnessRows(t *testing.T, st *Store, ws string) map[int64][]int64 {
	t.Helper()
	rows, err := st.db.QueryContext(t.Context(), `SELECT position,event_id FROM task_event_witnesses WHERE workspace_id=?`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var position, id int64
		if err = rows.Scan(&position, &id); err != nil {
			t.Fatal(err)
		}
		out[position] = append(out[position], id)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func countWitnesses(rows map[int64][]int64) int {
	n := 0
	for _, ids := range rows {
		n += len(ids)
	}
	return n
}

// lastPrunePosition is the highest prune position at or below position.
func lastPrunePosition(position int64) int64 {
	return position - position%taskEventPruneCadence
}

// requireCounterWitnessed fails unless the workspace's counter position
// carries exactly want witnesses: the commit that took the position wrote
// its witnesses at the position the counter upsert returned.
func requireCounterWitnessed(t *testing.T, st *Store, ws string, want int, what string) int64 {
	t.Helper()
	counter, _ := taskEventPosition(t, st, ws)
	rows := witnessRows(t, st, ws)
	if counter == 0 || len(rows[counter]) != want {
		t.Fatalf("%s: counter %d has witnesses %v, want %d (all witnesses %v)", what, counter, rows[counter], want, rows)
	}
	for position := range rows {
		if position > counter {
			t.Fatalf("%s: witness at position %d above the counter %d", what, position, counter)
		}
	}
	return counter
}

// commitToPosition appends single events to taskID until the workspace
// counter reaches target.
func commitToPosition(t *testing.T, st *Store, ctx context.Context, ws, taskID string, target int64) {
	t.Helper()
	for {
		counter, _ := taskEventPosition(t, st, ws)
		if counter >= target {
			if counter != target {
				t.Fatalf("counter %d passed target %d", counter, target)
			}
			return
		}
		if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "peer.advance"}); err != nil {
			t.Fatal(err)
		}
	}
}

// nextPositionNotPrune commits single events until the next commit takes a
// position that is not a prune position.
func nextPositionNotPrune(t *testing.T, st *Store, ctx context.Context, ws, taskID string) {
	t.Helper()
	counter, _ := taskEventPosition(t, st, ws)
	if isTaskEventPrunePosition(counter + 1) {
		commitToPosition(t, st, ctx, ws, taskID, counter+1)
	}
}

// lastInsertIDUpsertSQL is the counter upsert that would return the new
// position in its OK packet through LAST_INSERT_ID(expr).
const lastInsertIDUpsertSQL = `INSERT INTO task_event_positions(workspace_id,position) VALUES(?,LAST_INSERT_ID(1)) ON DUPLICATE KEY UPDATE position=LAST_INSERT_ID(position+1)`

// TestSingleStoreTaskEventPositionResultIntegration: the counter update
// returns the new position in its OK packet, and every commit writes its
// witnesses at the position its counter advance took: a workspace's first
// commit, later commits, on one connection across workspaces, after an
// event insert that set an auto-increment ID, after a rollback, and across
// pooled commits. The first subtest records why the counter is not an
// upsert: SingleStore does not return LAST_INSERT_ID(expr) from the upsert's
// update branch.
func TestSingleStoreTaskEventPositionResultIntegration(t *testing.T) {
	st := openOwnedStore(t)

	t.Run("upsert last insert ID", func(t *testing.T) {
		tx, err := st.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		ws := "position-" + core.NewTaskID()
		for _, branch := range []string{"insert", "update"} {
			result, err := tx.ExecContext(t.Context(), lastInsertIDUpsertSQL, ws)
			if err != nil {
				t.Fatal(err)
			}
			returned, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			var stored int64
			if err := tx.QueryRowContext(t.Context(), `SELECT position FROM task_event_positions WHERE workspace_id=?`, ws).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			t.Logf("upsert %s branch: last insert ID %d, stored position %d", branch, returned, stored)
		}
	})

	// The production statements on one connection: a missing row, its
	// insert, updates, and another workspace in between.
	t.Run("update last insert ID", func(t *testing.T) {
		tx, err := st.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		first, second := "position-a-"+core.NewTaskID(), "position-b-"+core.NewTaskID()
		for i, step := range []struct {
			ws   string
			want int64
		}{{first, 1}, {first, 2}, {second, 1}, {first, 3}, {second, 2}, {first, 4}} {
			got, err := advanceTaskEventCounter(t.Context(), tx, step.ws)
			if err != nil || got != step.want {
				t.Fatalf("step %d: %s advanced to %d (err %v), want %d", i, step.ws, got, err, step.want)
			}
			var stored int64
			if err := tx.QueryRowContext(t.Context(), `SELECT position FROM task_event_positions WHERE workspace_id=?`, step.ws).Scan(&stored); err != nil || stored != step.want {
				t.Fatalf("step %d: %s stored position %d (err %v), want %d", i, step.ws, stored, err, step.want)
			}
		}
	})

	// One pooled connection, so every commit below reuses the connection
	// whose earlier statements set LAST_INSERT_ID to event IDs and to other
	// workspaces' positions.
	st.db.SetMaxOpenConns(1)
	t.Cleanup(func() { st.db.SetMaxOpenConns(0) })

	// A new workspace's first task-event commit, on the same connection as
	// another workspace's commits.
	ctxA, wsA, taskA := seedSingleStoreNoticeTask(t, st)
	ctxB, wsB, taskB := seedSingleStoreNoticeTask(t, st)
	for _, ws := range []string{wsA, wsB} {
		if counter, _ := taskEventPosition(t, st, ws); counter != 1 {
			t.Fatalf("%s: a new workspace's first commit left the counter at %d, want 1", ws, counter)
		}
		if rows := witnessRows(t, st, ws); len(rows) != 1 || len(rows[1]) == 0 {
			t.Fatalf("%s: the first commit's witnesses %v, want all at position 1", ws, rows)
		}
	}

	// Later commits, after the event insert set an auto-increment ID on the
	// same connection.
	for i := int64(1); i <= 3; i++ {
		if err := st.AppendEvent(ctxA, core.Event{TaskID: taskA, Kind: "peer.update"}); err != nil {
			t.Fatal(err)
		}
		if got := requireCounterWitnessed(t, st, wsA, 1, "update"); got != 1+i {
			t.Fatalf("update: counter %d, want %d", got, 1+i)
		}
	}

	// One transaction advances two workspaces: each witness takes its own
	// workspace's position.
	startA, _ := taskEventPosition(t, st, wsA)
	startB, _ := taskEventPosition(t, st, wsB)
	if err := st.withTx(ctxA, func(tx *sql.Tx) error {
		if err := taskEvent(ctxA, tx, core.Event{TaskID: taskA, Kind: "peer.pair.a"}); err != nil {
			return err
		}
		return taskEvent(ctxB, tx, core.Event{TaskID: taskB, Kind: "peer.pair.b"})
	}); err != nil {
		t.Fatal(err)
	}
	if got := requireCounterWitnessed(t, st, wsA, 1, "pair"); got != startA+1 {
		t.Fatalf("pair: %s counter %d, want %d", wsA, got, startA+1)
	}
	if got := requireCounterWitnessed(t, st, wsB, 1, "pair"); got != startB+1 {
		t.Fatalf("pair: %s counter %d, want %d", wsB, got, startB+1)
	}

	// A rolled-back transaction on the same connection leaves no stale
	// position for the next commit.
	rollback := errors.New("rollback probe")
	if err := st.withTx(ctxA, func(tx *sql.Tx) error {
		if err := taskEvent(ctxA, tx, core.Event{TaskID: taskA, Kind: "peer.rolled.back"}); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("rollback probe: %v", err)
	}
	if got, _ := taskEventPosition(t, st, wsA); got != startA+1 {
		t.Fatalf("the rollback moved %s to %d, want %d", wsA, got, startA+1)
	}
	if err := st.AppendEvent(ctxB, core.Event{TaskID: taskB, Kind: "peer.after.rollback"}); err != nil {
		t.Fatal(err)
	}
	if got := requireCounterWitnessed(t, st, wsB, 1, "after a rollback"); got != startB+2 {
		t.Fatalf("after a rollback: %s counter %d, want %d", wsB, got, startB+2)
	}
}

// preparationStatements returns the statements a transaction sent after its
// last event write and before its COMMIT: its commit preparation.
func preparationStatements(t *testing.T, sent []proxyCommand) []string {
	t.Helper()
	commit := -1
	for i, c := range sent {
		if strings.EqualFold(strings.TrimSpace(c.text), "COMMIT") {
			commit = i
		}
	}
	if commit < 0 {
		t.Fatalf("no COMMIT among:\n%s", describeCommands(time.Now(), sent))
	}
	start := commit
	for start > 0 && isMaintenanceStatement(sent[start-1].text) {
		start--
	}
	var out []string
	for _, c := range sent[start:commit] {
		out = append(out, strings.Join(strings.Fields(c.text), " "))
	}
	return out
}

func countMaintenance(sent []proxyCommand) int {
	n := 0
	for _, c := range sent {
		if isMaintenanceStatement(c.text) {
			n++
		}
	}
	return n
}

// requireNoEventTableRead fails if any statement but the event insert
// names events_columnstore.
func requireNoEventTableRead(t *testing.T, sent []proxyCommand) {
	t.Helper()
	for _, c := range sent {
		if strings.Contains(c.text, "events_columnstore") && !strings.HasPrefix(strings.TrimSpace(c.text), "INSERT INTO `events_columnstore`") {
			t.Fatalf("an ordinary commit read the event table: %s", c.text)
		}
	}
}

func isMaintenanceStatement(text string) bool {
	return strings.Contains(text, "task_event_positions") || strings.Contains(text, "task_event_witnesses")
}

// requirePreparation fails unless the statements are the given maintenance
// statements, in order, by prefix.
func requirePreparation(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		ok = strings.HasPrefix(got[i], want[i])
	}
	if !ok {
		t.Fatalf("%s: preparation %q, want statements starting %q", what, got, want)
	}
}

const (
	counterUpdate  = "UPDATE task_event_positions SET position=LAST_INSERT_ID(position+1)"
	counterInsert  = "INSERT INTO task_event_positions(workspace_id,position) VALUES"
	witnessInsert  = "INSERT INTO task_event_witnesses"
	witnessPrune   = "DELETE w FROM task_event_witnesses w JOIN"
	firstCommitMax = 3
)

// TestSingleStoreTaskEventCommitStatementsIntegration counts, through a wire
// proxy, every statement a task-event commit sends. An ordinary commit sends
// two maintenance statements per written workspace, the counter update and
// the witness insert, and none of them reads events_columnstore. A
// workspace's first commit adds the counter insert, alone or beside an
// existing workspace. A commit at a prune position adds the one delete.
func TestSingleStoreTaskEventCommitStatementsIntegration(t *testing.T) {
	writer := openOwnedStore(t)
	ctxA, wsA, taskA := seedSingleStoreNoticeTask(t, writer)
	ctxB, wsB, taskB := seedSingleStoreNoticeTask(t, writer)
	st, proxy := openProxiedStore(t)

	// A workspace's first commit: update, counter insert, witness insert.
	wsFirst := "notices-first-" + core.NewTaskID()
	ctxFirst := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), wsFirst)
	if _, err := writer.BootstrapWorkspaceConfig(ctxFirst, &config.Config{Workspace: wsFirst, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	if counter, _ := taskEventPosition(t, writer, wsFirst); counter != 0 {
		t.Fatalf("a bootstrapped workspace has counter %d before its first task event", counter)
	}
	before := proxy.count()
	firstTask := core.NewTaskID()
	if err := st.CreateTask(ctxFirst, core.Task{ID: firstTask, Workspace: wsFirst, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + firstTask, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	sent := proxy.since(before)
	t.Logf("a workspace's first commit:\n%s", describeCommands(sent[0].at, sent))
	requirePreparation(t, "first commit", preparationStatements(t, sent), counterUpdate, counterInsert, witnessInsert)
	requireNoEventTableRead(t, sent)
	if requireCounterWitnessed(t, writer, wsFirst, len(witnessRows(t, writer, wsFirst)[1]), "first commit") != 1 {
		t.Fatal("a workspace's first commit did not take position 1")
	}

	// One event in one workspace.
	nextPositionNotPrune(t, writer, ctxA, wsA, taskA)
	before = proxy.count()
	if err := st.withTx(ctxA, func(tx *sql.Tx) error {
		return taskEvent(ctxA, tx, core.Event{TaskID: taskA, Kind: "peer.count.one"})
	}); err != nil {
		t.Fatal(err)
	}
	sent = proxy.since(before)
	t.Logf("a one-event commit:\n%s", describeCommands(sent[0].at, sent))
	requirePreparation(t, "one-event commit", preparationStatements(t, sent), counterUpdate, witnessInsert)
	if countMaintenance(sent) != 2 {
		t.Fatalf("a one-event commit sent maintenance statements outside its preparation")
	}
	requireNoEventTableRead(t, sent)
	requireCounterWitnessed(t, writer, wsA, 1, "one-event commit")

	// The public command sends the same preparation.
	nextPositionNotPrune(t, writer, ctxA, wsA, taskA)
	before = proxy.count()
	if err := st.AppendEvent(ctxA, core.Event{TaskID: taskA, Kind: "peer.count.append"}); err != nil {
		t.Fatal(err)
	}
	sent = proxy.since(before)
	requirePreparation(t, "AppendEvent", preparationStatements(t, sent), counterUpdate, witnessInsert)
	if countMaintenance(sent) != 2 {
		t.Fatalf("AppendEvent sent maintenance statements outside its preparation:\n%s", describeCommands(sent[0].at, sent))
	}
	requireNoEventTableRead(t, sent)

	// Events in two workspaces: two statements per workspace, one witness
	// insert carrying both events of the first.
	nextPositionNotPrune(t, writer, ctxA, wsA, taskA)
	nextPositionNotPrune(t, writer, ctxB, wsB, taskB)
	before = proxy.count()
	if err := st.withTx(ctxA, func(tx *sql.Tx) error {
		for _, kind := range []string{"peer.count.a1", "peer.count.a2"} {
			if err := taskEvent(ctxA, tx, core.Event{TaskID: taskA, Kind: kind}); err != nil {
				return err
			}
		}
		return taskEvent(ctxB, tx, core.Event{TaskID: taskB, Kind: "peer.count.b"})
	}); err != nil {
		t.Fatal(err)
	}
	sent = proxy.since(before)
	t.Logf("a two-workspace commit:\n%s", describeCommands(sent[0].at, sent))
	prep := preparationStatements(t, sent)
	perWorkspace := map[string]int{}
	for _, text := range prep {
		for _, ws := range []string{wsA, wsB} {
			if strings.Contains(text, ws) {
				perWorkspace[ws]++
			}
		}
	}
	if len(prep) != 4 || countMaintenance(sent) != 4 || perWorkspace[wsA] != 2 || perWorkspace[wsB] != 2 {
		t.Fatalf("a two-workspace preparation %q, want two statements for each workspace", prep)
	}
	requireNoEventTableRead(t, sent)
	requireCounterWitnessed(t, writer, wsA, 2, "two-workspace commit")
	requireCounterWitnessed(t, writer, wsB, 1, "two-workspace commit")

	// One transaction holds a new workspace's first task and an existing
	// workspace's event: three statements for the first, two for the other.
	wsMixed := "notices-mixed-" + core.NewTaskID()
	ctxMixed := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), wsMixed)
	if _, err := writer.BootstrapWorkspaceConfig(ctxMixed, &config.Config{Workspace: wsMixed, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	nextPositionNotPrune(t, writer, ctxA, wsA, taskA)
	mixedTask := core.NewTaskID()
	before = proxy.count()
	if err := st.withTx(ctxA, func(tx *sql.Tx) error {
		if err := st.createTaskTx(ctxMixed, tx, core.Task{ID: mixedTask, Workspace: wsMixed, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + mixedTask, State: core.TaskRunning, CreatedAt: time.Now().UTC()}, nil, store.TaskContextInput{}, nil); err != nil {
			return err
		}
		return taskEvent(ctxA, tx, core.Event{TaskID: taskA, Kind: "peer.count.mixed"})
	}); err != nil {
		t.Fatal(err)
	}
	sent = proxy.since(before)
	t.Logf("a first commit beside an existing workspace:\n%s", describeCommands(sent[0].at, sent))
	prep = preparationStatements(t, sent)
	perWorkspace = map[string]int{}
	for _, text := range prep {
		for _, ws := range []string{wsA, wsMixed} {
			if strings.Contains(text, ws) {
				perWorkspace[ws]++
			}
		}
	}
	if len(prep) != 5 || perWorkspace[wsMixed] != firstCommitMax || perWorkspace[wsA] != 2 {
		t.Fatalf("a mixed preparation %q, want three statements for the new workspace and two for the existing one", prep)
	}
	requireNoEventTableRead(t, sent)
	if p := requireCounterWitnessed(t, writer, wsMixed, len(witnessRows(t, writer, wsMixed)[1]), "mixed, new workspace"); p != 1 {
		t.Fatalf("mixed: the new workspace took position %d, want 1", p)
	}
	requireCounterWitnessed(t, writer, wsA, 1, "mixed, existing workspace")

	// A commit at a prune position adds the one delete; the next commit is
	// ordinary again.
	counter, _ := taskEventPosition(t, writer, wsA)
	commitToPosition(t, writer, ctxA, wsA, taskA, lastPrunePosition(counter)+taskEventPruneCadence-1)
	before = proxy.count()
	if err := st.AppendEvent(ctxA, core.Event{TaskID: taskA, Kind: "peer.count.prune"}); err != nil {
		t.Fatal(err)
	}
	sent = proxy.since(before)
	t.Logf("a prune commit:\n%s", describeCommands(sent[0].at, sent))
	requirePreparation(t, "prune commit", preparationStatements(t, sent), counterUpdate, witnessPrune, witnessInsert)
	before = proxy.count()
	if err := st.AppendEvent(ctxA, core.Event{TaskID: taskA, Kind: "peer.count.after"}); err != nil {
		t.Fatal(err)
	}
	sent = proxy.since(before)
	requireNoEventTableRead(t, sent)
	requirePreparation(t, "the commit after a prune", preparationStatements(t, sent), counterUpdate, witnessInsert)
}

// newCounterlessWorkspace bootstraps a workspace that has no task-event
// counter yet and returns its context, its ID, and a task constructor.
func newCounterlessWorkspace(t *testing.T, writer *Store, prefix string) (context.Context, string, func() core.Task) {
	t.Helper()
	ws := prefix + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), ws)
	if _, err := writer.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	if counter, _ := taskEventPosition(t, writer, ws); counter != 0 {
		t.Fatalf("a bootstrapped workspace has counter %d before its first task event", counter)
	}
	newTask := func() core.Task {
		id := core.NewTaskID()
		return core.Task{ID: id, Workspace: ws, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	}
	return ctx, ws, newTask
}

// witnessOwnership returns, for each witnessed position of the workspace,
// the one task whose events it witnesses. It fails unless each position's
// witnesses are one transaction's complete set: every witness joins a
// committed event, a position witnesses the events of one task only, no
// task's events are split over two positions, no event is witnessed twice,
// and every committed event of the given tasks is witnessed.
func witnessOwnership(t *testing.T, st *Store, ws string, tasks []string, what string) map[int64]string {
	t.Helper()
	rows, err := st.db.QueryContext(t.Context(), `SELECT w.position,w.event_id,COALESCE(e.task_id,'') FROM task_event_witnesses w LEFT JOIN events_columnstore e ON e.event_shard_key=w.event_shard_key AND e.workspace_id=w.workspace_id AND e.id=w.event_id WHERE w.workspace_id=?`, ws)
	if err != nil {
		t.Fatal(err)
	}
	owners := map[int64]string{}
	positionOf := map[string]int64{}
	witnessed := map[int64]bool{}
	for rows.Next() {
		var position, id int64
		var task string
		if err = rows.Scan(&position, &id, &task); err != nil {
			t.Fatal(err)
		}
		switch {
		case task == "":
			t.Fatalf("%s: the witness of event %d at position %d joins no committed event", what, id, position)
		case witnessed[id]:
			t.Fatalf("%s: event %d is witnessed twice", what, id)
		case owners[position] != "" && owners[position] != task:
			t.Fatalf("%s: position %d witnesses events of tasks %s and %s", what, position, owners[position], task)
		case positionOf[task] != 0 && positionOf[task] != position:
			t.Fatalf("%s: task %s's events are witnessed at positions %d and %d", what, task, positionOf[task], position)
		}
		witnessed[id] = true
		owners[position] = task
		positionOf[task] = position
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, task := range tasks {
		events, err := st.db.QueryContext(t.Context(), `SELECT id FROM events_columnstore WHERE workspace_id=? AND task_id=?`, ws, task)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for events.Next() {
			var id int64
			if err = events.Scan(&id); err != nil {
				t.Fatal(err)
			}
			if !witnessed[id] {
				t.Fatalf("%s: committed event %d of task %s has no witness", what, id, task)
			}
			n++
		}
		if err = events.Err(); err != nil {
			t.Fatal(err)
		}
		events.Close()
		if n == 0 {
			t.Fatalf("%s: task %s has no committed event", what, task)
		}
	}
	return owners
}

// transactionShape counts the transaction control statements a store sent:
// a replayed or rolled-back attempt shows up as a second START TRANSACTION
// or a ROLLBACK.
func transactionShape(sent []proxyCommand) (begins, commits, rollbacks int) {
	for _, c := range sent {
		switch text := strings.ToUpper(strings.TrimSpace(c.text)); {
		case strings.HasPrefix(text, "START TRANSACTION"):
			begins++
		case text == "COMMIT":
			commits++
		case text == "ROLLBACK":
			rollbacks++
		}
	}
	return begins, commits, rollbacks
}

// requireOneTransaction fails unless the statements hold exactly one
// transaction, committed, and no rollback.
func requireOneTransaction(t *testing.T, what string, sent []proxyCommand) {
	t.Helper()
	if begins, commits, rollbacks := transactionShape(sent); begins != 1 || commits != 1 || rollbacks != 0 {
		t.Fatalf("%s: %d transaction starts, %d commits, %d rollbacks, want one committed transaction:\n%s", what, begins, commits, rollbacks, describeCommands(sent[0].at, sent))
	}
}

// losingFirstCommit holds the first counter miss of ws until release, so the
// transaction that misses first loses the counter insert to a later one that
// commits in the meantime. Every later miss passes. It returns a channel
// closed at the first miss, the release, and the miss count; the release is
// also a cleanup, so a failed test never leaves the loser waiting.
func losingFirstCommit(t *testing.T, ws string) (missed <-chan struct{}, release func(), misses *atomic.Int32) {
	t.Helper()
	first := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(released) }) }
	misses = &atomic.Int32{}
	observe := func(missedWS string) {
		if missedWS != ws || misses.Add(1) != 1 {
			return
		}
		close(first)
		select {
		case <-released:
		case <-time.After(time.Minute):
		}
	}
	observeTaskEventCounterMiss.Store(&observe)
	t.Cleanup(func() {
		observeTaskEventCounterMiss.Store(nil)
		release()
	})
	return first, release, misses
}

// recordCounterRecoveries records each counter insert of ws that lost to a
// concurrent first commit, with the server's error.
func recordCounterRecoveries(t *testing.T, ws string) func() []error {
	t.Helper()
	var mu sync.Mutex
	var errs []error
	observe := func(recoveredWS string, err error) {
		if recoveredWS != ws {
			return
		}
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	observeTaskEventCounterRecovery.Store(&observe)
	t.Cleanup(func() { observeTaskEventCounterRecovery.Store(nil) })
	return func() []error {
		mu.Lock()
		defer mu.Unlock()
		return append([]error(nil), errs...)
	}
}

// requireDuplicateCounterKey fails unless every recorded error is the
// server's PRIMARY duplicate key on the counter insert.
func requireDuplicateCounterKey(t *testing.T, recovered []error, want int) {
	t.Helper()
	if len(recovered) != want {
		t.Fatalf("%d counter inserts lost to a concurrent first commit, want %d: %v", len(recovered), want, recovered)
	}
	for _, err := range recovered {
		var driverErr *mysql.MySQLError
		if !errors.As(err, &driverErr) || !isDuplicateKey(err, "PRIMARY") {
			t.Fatalf("the lost counter insert returned %v, want the server's PRIMARY duplicate key", err)
		}
		t.Logf("the lost counter insert's server error: %d %s", driverErr.Number, driverErr.Message)
	}
}

// TestSingleStoreTaskEventDuplicateKeyRecoveryIntegration is the server
// proof for the recovery of a lost first counter insert. Two transactions
// find no counter for a new workspace. The first to miss waits while the
// second inserts position 1 and commits. The first's insert then gets the
// server's real PRIMARY duplicate key, and the same transaction runs the
// counter update again, takes position 2, writes its witnesses, and commits
// its task and events: one transaction start, one commit, and no rollback.
// A recovered transaction that rolls back instead leaves the counter, the
// witnesses, and its task as the winner left them, and the next commit
// takes position 2.
func TestSingleStoreTaskEventDuplicateKeyRecoveryIntegration(t *testing.T) {
	writer := openOwnedStore(t)

	t.Run("commit", func(t *testing.T) {
		loserStore, loserProxy := openProxiedStore(t)
		ctx, ws, newTask := newCounterlessWorkspace(t, writer, "notices-recovery-")
		missed, release, misses := losingFirstCommit(t, ws)
		recovered := recordCounterRecoveries(t, ws)
		loser, winner := newTask(), newTask()

		before := loserProxy.count()
		loserDone := make(chan error, 1)
		go func() { loserDone <- loserStore.CreateTask(ctx, loser) }()
		select {
		case <-missed:
		case err := <-loserDone:
			t.Fatalf("the first commit ended before its counter update missed: %v", err)
		case <-time.After(time.Minute):
			t.Fatal("the first commit's counter update did not miss")
		}
		// The winner's update also misses; it inserts position 1 and
		// commits while the loser waits.
		if err := writer.CreateTask(ctx, winner); err != nil {
			t.Fatalf("the winning first commit: %v", err)
		}
		if counter, _ := taskEventPosition(t, writer, ws); counter != 1 {
			t.Fatalf("the winner left the counter at %d, want 1", counter)
		}
		release()
		select {
		case err := <-loserDone:
			if err != nil {
				t.Fatalf("the first commit that lost the counter insert failed: %v", err)
			}
		case <-time.After(2 * time.Minute):
			t.Fatal("the first commit that lost the counter insert did not finish")
		}
		if got := misses.Load(); got != 2 {
			t.Fatalf("%d counter updates missed, want both", got)
		}
		requireDuplicateCounterKey(t, recovered(), 1)

		sent := loserProxy.since(before)
		t.Logf("the losing first commit's statements:\n%s", describeCommands(sent[0].at, sent))
		requireOneTransaction(t, "the losing first commit", sent)
		requirePreparation(t, "the losing first commit", preparationStatements(t, sent), counterUpdate, counterInsert, counterUpdate, witnessInsert)
		requireNoEventTableRead(t, sent)

		if counter, _ := taskEventPosition(t, writer, ws); counter != 2 {
			t.Fatalf("after the recovery the counter is %d, want 2", counter)
		}
		owners := witnessOwnership(t, writer, ws, []string{winner.ID, loser.ID}, "after the recovery")
		if len(owners) != 2 || owners[1] != winner.ID || owners[2] != loser.ID {
			t.Fatalf("after the recovery the positions are owned by %v, want 1=%s (winner) and 2=%s (recovered)", owners, winner.ID, loser.ID)
		}
		for _, task := range []core.Task{winner, loser} {
			if _, err := writer.GetTask(ctx, task.ID); err != nil {
				t.Fatalf("task %s after the recovery: %v", task.ID, err)
			}
		}
	})

	t.Run("rollback", func(t *testing.T) {
		ctx, ws, newTask := newCounterlessWorkspace(t, writer, "notices-recovery-rollback-")
		missed, release, misses := losingFirstCommit(t, ws)
		recovered := recordCounterRecoveries(t, ws)
		loser, winner := newTask(), newTask()

		// The loser is a registered store transaction whose commit
		// preparation runs, and which then rolls back as withTx does when
		// its commit fails.
		tx, err := writer.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		writer.beginNotices(tx)
		open := true
		t.Cleanup(func() {
			if open {
				_ = tx.Rollback()
				finishNotices(tx, false)
			}
		})
		if err = writer.createTaskTx(ctx, tx, loser, nil, store.TaskContextInput{}, nil); err != nil {
			t.Fatal(err)
		}
		prepared := make(chan error, 1)
		go func() { prepared <- prepareTaskEventPositions(ctx, tx) }()
		select {
		case <-missed:
		case err := <-prepared:
			t.Fatalf("the preparation ended before its counter update missed: %v", err)
		case <-time.After(time.Minute):
			t.Fatal("the preparation's counter update did not miss")
		}
		if err = writer.CreateTask(ctx, winner); err != nil {
			t.Fatalf("the winning first commit: %v", err)
		}
		release()
		select {
		case err = <-prepared:
			if err != nil {
				t.Fatalf("the preparation that lost the counter insert failed: %v", err)
			}
		case <-time.After(2 * time.Minute):
			t.Fatal("the preparation that lost the counter insert did not finish")
		}
		if got := misses.Load(); got != 2 {
			t.Fatalf("%d counter updates missed, want both", got)
		}
		requireDuplicateCounterKey(t, recovered(), 1)
		var inside int64
		var insideWitnesses int
		if err = tx.QueryRowContext(ctx, `SELECT position FROM task_event_positions WHERE workspace_id=?`, ws).Scan(&inside); err != nil || inside != 2 {
			t.Fatalf("inside the recovered transaction the counter is %d (err %v), want 2", inside, err)
		}
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_event_witnesses WHERE workspace_id=? AND position=2`, ws).Scan(&insideWitnesses); err != nil || insideWitnesses == 0 {
			t.Fatalf("inside the recovered transaction position 2 has %d witnesses (err %v), want its own", insideWitnesses, err)
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		finishNotices(tx, false)
		open = false

		if counter, _ := taskEventPosition(t, writer, ws); counter != 1 {
			t.Fatalf("the rolled-back recovery left the counter at %d, want the winner's 1", counter)
		}
		if owners := witnessOwnership(t, writer, ws, []string{winner.ID}, "after the rollback"); len(owners) != 1 || owners[1] != winner.ID {
			t.Fatalf("after the rollback the positions are owned by %v, want only 1=%s", owners, winner.ID)
		}
		if _, err = writer.GetTask(ctx, loser.ID); err == nil {
			t.Fatal("the rolled-back transaction's task was written")
		}
		// The next commit takes position 2: the rollback left no advance.
		next := newTask()
		if err = writer.CreateTask(ctx, next); err != nil {
			t.Fatal(err)
		}
		owners := witnessOwnership(t, writer, ws, []string{winner.ID, next.ID}, "after the next commit")
		if len(owners) != 2 || owners[1] != winner.ID || owners[2] != next.ID {
			t.Fatalf("after the next commit the positions are owned by %v, want 1=%s and 2=%s", owners, winner.ID, next.ID)
		}
	})
}

// TestSingleStoreTaskEventFirstCommitRaceIntegration: two first commits of a
// workspace whose counter updates both match no row race on the counter
// insert, each through one CreateTask call. The winner takes position 1.
// The loser's insert gets the duplicate key, and the same transaction runs
// the counter update again and takes position 2. Both calls succeed with no
// retry: each store sends one transaction, commits it, and rolls nothing
// back. Each position holds the complete witness set of its own task's
// events, and both tasks are written.
func TestSingleStoreTaskEventFirstCommitRaceIntegration(t *testing.T) {
	writer := openOwnedStore(t)
	firstStore, firstProxy := openProxiedStore(t)
	secondStore, secondProxy := openProxiedStore(t)
	ctx, ws, newTask := newCounterlessWorkspace(t, writer, "notices-race-")

	// Both updates miss before either insert runs.
	var mu sync.Mutex
	misses := 0
	bothMissed := make(chan struct{})
	observe := func(missed string) {
		if missed != ws {
			return
		}
		mu.Lock()
		misses++
		if misses == 2 {
			close(bothMissed)
		}
		mu.Unlock()
		select {
		case <-bothMissed:
		case <-time.After(30 * time.Second):
		}
	}
	observeTaskEventCounterMiss.Store(&observe)
	t.Cleanup(func() { observeTaskEventCounterMiss.Store(nil) })
	recovered := recordCounterRecoveries(t, ws)

	type racer struct {
		st     *Store
		proxy  *statementProxy
		before int
		task   core.Task
		err    error
	}
	racers := []*racer{{st: firstStore, proxy: firstProxy, task: newTask()}, {st: secondStore, proxy: secondProxy, task: newTask()}}
	var wg sync.WaitGroup
	for _, r := range racers {
		r.before = r.proxy.count()
		wg.Add(1)
		go func(r *racer) {
			defer wg.Done()
			r.err = r.st.CreateTask(ctx, r.task)
		}(r)
	}
	wg.Wait()
	observeTaskEventCounterMiss.Store(nil)
	if misses != 2 {
		t.Fatalf("%d counter updates missed, want both", misses)
	}
	for _, r := range racers {
		if r.err != nil {
			t.Fatalf("a racing first commit failed: %v", r.err)
		}
	}
	requireDuplicateCounterKey(t, recovered(), 1)

	// The winner's preparation inserted the counter; the loser's lost the
	// insert and ran the update again, all in its one transaction.
	var winner, loser *racer
	for _, r := range racers {
		sent := r.proxy.since(r.before)
		t.Logf("a racing store's statements:\n%s", describeCommands(sent[0].at, sent))
		requireOneTransaction(t, "a racing first commit", sent)
		requireNoEventTableRead(t, sent)
		switch prep := preparationStatements(t, sent); len(prep) {
		case 3:
			requirePreparation(t, "the winner", prep, counterUpdate, counterInsert, witnessInsert)
			winner = r
		case 4:
			requirePreparation(t, "the loser", prep, counterUpdate, counterInsert, counterUpdate, witnessInsert)
			loser = r
		default:
			t.Fatalf("a racing first commit's preparation %q, want the winner's three statements or the loser's four", prep)
		}
	}
	if winner == nil || loser == nil {
		t.Fatal("the race did not have one winner and one loser")
	}

	if counter, _ := taskEventPosition(t, writer, ws); counter != 2 {
		t.Fatalf("after the race the counter is %d, want 2", counter)
	}
	owners := witnessOwnership(t, writer, ws, []string{winner.task.ID, loser.task.ID}, "after the race")
	if len(owners) != 2 || owners[1] != winner.task.ID || owners[2] != loser.task.ID {
		t.Fatalf("after the race the positions are owned by %v, want 1=%s (winner) and 2=%s (loser)", owners, winner.task.ID, loser.task.ID)
	}
	for _, r := range racers {
		if _, err := writer.GetTask(ctx, r.task.ID); err != nil {
			t.Fatalf("task %s after the race: %v", r.task.ID, err)
		}
	}
}

// TestSingleStoreTaskEventConcurrentFirstCommitsIntegration: many first
// task commits of one new workspace run at once, each through one
// CreateTask call. Every counter update misses before any insert runs. One
// insert wins, every other insert loses and its transaction runs the
// counter update again, and every call succeeds with no retry. The
// committed transactions take positions 1..n, each with the complete
// witness set of its own task's events, and every task is written.
func TestSingleStoreTaskEventConcurrentFirstCommitsIntegration(t *testing.T) {
	writer := openOwnedStore(t)
	ctx, ws, newTask := newCounterlessWorkspace(t, writer, "notices-first-many-")
	const creators = 8

	// Every creator's update misses before any insert runs.
	var mu sync.Mutex
	misses := 0
	allMissed := make(chan struct{})
	observe := func(missed string) {
		if missed != ws {
			return
		}
		mu.Lock()
		misses++
		if misses == creators {
			close(allMissed)
		}
		mu.Unlock()
		select {
		case <-allMissed:
		case <-time.After(time.Minute):
		}
	}
	observeTaskEventCounterMiss.Store(&observe)
	t.Cleanup(func() { observeTaskEventCounterMiss.Store(nil) })
	recovered := recordCounterRecoveries(t, ws)

	tasks := make([]core.Task, creators)
	ids := make([]string, creators)
	for i := range tasks {
		tasks[i] = newTask()
		ids[i] = tasks[i].ID
	}
	var wg sync.WaitGroup
	errs := make(chan error, creators)
	for _, task := range tasks {
		wg.Add(1)
		go func(task core.Task) {
			defer wg.Done()
			if err := writer.CreateTask(ctx, task); err != nil {
				errs <- err
			}
		}(task)
	}
	wg.Wait()
	observeTaskEventCounterMiss.Store(nil)
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent first commit failed: %v", err)
	}
	if t.Failed() {
		t.FailNow()
	}
	if misses != creators {
		t.Fatalf("%d counter updates missed, want all %d", misses, creators)
	}
	requireDuplicateCounterKey(t, recovered(), creators-1)
	t.Logf("%d concurrent first commits, %d recovered a lost counter insert in their own transaction", creators, creators-1)

	if counter, _ := taskEventPosition(t, writer, ws); counter != creators {
		t.Fatalf("counter %d after %d task creations, want one position each", counter, creators)
	}
	owners := witnessOwnership(t, writer, ws, ids, "after the concurrent first commits")
	for position := int64(1); position <= creators; position++ {
		if owners[position] == "" {
			t.Fatalf("position %d has no witness: %v", position, owners)
		}
	}
	if len(owners) != creators {
		t.Fatalf("positions %v, want exactly 1..%d", owners, creators)
	}
	for _, id := range ids {
		if _, err := writer.GetTask(ctx, id); err != nil {
			t.Fatalf("task %s after the concurrent first commits: %v", id, err)
		}
	}
}

// TestSingleStoreTaskEventPruneEmptySelectionIntegration: a prune position
// whose earlier witnesses all name invisible events deletes nothing, and the
// next prune position after they become visible removes them.
func TestSingleStoreTaskEventPruneEmptySelectionIntegration(t *testing.T) {
	st := openOwnedStore(t)
	ctx, ws, taskID := seedSingleStoreNoticeTask(t, st)
	commitToPosition(t, st, ctx, ws, taskID, taskEventPruneCadence-1)
	// Positions 64..127 each count one held, invisible event. The prune at
	// 64 removes every visible witness below it. Each held event keeps a
	// connection, so they run on a second pool without the store's
	// connection cap.
	holder, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(holder.Close)
	holder.db.SetMaxOpenConns(0)
	held := make([]*heldTaskEvent, 0, taskEventPruneCadence)
	for len(held) < taskEventPruneCadence {
		h := holdTaskEvent(t, holder, ctx, taskID, "peer.empty.held")
		commitPositionFirst(t, h)
		held = append(held, h)
	}
	requireWitnessModel(t, st, ws, taskEventPruneCadence, nil, "held positions")
	// The prune at 128 selects nothing: every witness below it is invisible.
	if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "peer.empty.prune"}); err != nil {
		t.Fatal(err)
	}
	counter, _ := taskEventPosition(t, st, ws)
	if counter != 2*taskEventPruneCadence {
		t.Fatalf("counter %d, want the prune position %d", counter, 2*taskEventPruneCadence)
	}
	kept := map[int64]bool{}
	for position := int64(taskEventPruneCadence); position < 2*taskEventPruneCadence; position++ {
		kept[position] = true
	}
	requireWitnessModel(t, st, ws, taskEventPruneCadence+1, kept, "empty selection")
	for _, h := range held {
		h.release(t)
	}
	commitToPosition(t, st, ctx, ws, taskID, 3*taskEventPruneCadence)
	requireWitnessModel(t, st, ws, 1, nil, "after release")
}

// TestSingleStoreTaskEventPruneReadAccountingIntegration: a prune adds no
// row to the read account of the request that commits it. AppendEvent reads
// the same rows at a prune position, with 63 visible witnesses to delete, as
// at an ordinary position.
func TestSingleStoreTaskEventPruneReadAccountingIntegration(t *testing.T) {
	st := openOwnedStore(t)
	ctx, ws, taskID := seedSingleStoreNoticeTask(t, st)
	reads := newConformanceReadLedger(st)
	measure := func(kind string) (uint64, uint64) {
		t.Helper()
		reads.drainRows()
		reads.drainQueries()
		if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: kind}); err != nil {
			t.Fatal(err)
		}
		return reads.drainRows()["AppendEvent"], reads.drainQueries()["AppendEvent"]
	}
	commitToPosition(t, st, ctx, ws, taskID, 2*taskEventPruneCadence-2)
	ordinaryRows, ordinaryQueries := measure("peer.reads.ordinary")
	_, before := taskEventPosition(t, st, ws)
	pruneRows, pruneQueries := measure("peer.reads.prune")
	if counter, after := taskEventPosition(t, st, ws); counter != 2*taskEventPruneCadence || after != 1 || before != taskEventPruneCadence {
		t.Fatalf("the measured prune at %d left %d of %d witnesses, want 1 of %d", counter, after, before, taskEventPruneCadence)
	}
	t.Logf("AppendEvent read %d rows in %d statements at an ordinary position, %d rows in %d statements at a prune position", ordinaryRows, ordinaryQueries, pruneRows, pruneQueries)
	if pruneRows != ordinaryRows {
		t.Fatalf("a prune changed the request's read rows: %d at an ordinary position, %d at a prune position", ordinaryRows, pruneRows)
	}
	if pruneQueries != ordinaryQueries+1 {
		t.Fatalf("a prune sent %d statements, want the ordinary %d and the one delete", pruneQueries, ordinaryQueries)
	}
}

// requireWitnessModel fails unless the workspace retains exactly want
// witnesses, every retained position at or above the last prune position
// still holds all its witnesses, and below it only the positions named in
// kept keep theirs.
func requireWitnessModel(t *testing.T, st *Store, ws string, want int, kept map[int64]bool, what string) map[int64][]int64 {
	t.Helper()
	counter, _ := taskEventPosition(t, st, ws)
	rows := witnessRows(t, st, ws)
	if got := countWitnesses(rows); got != want {
		t.Fatalf("%s: position %d retains %d witnesses, want %d", what, counter, got, want)
	}
	floor := lastPrunePosition(counter)
	if floor == 0 {
		return rows
	}
	for position := range rows {
		if position < floor && !kept[position] {
			t.Fatalf("%s: position %d below the prune at %d kept witnesses %v", what, position, floor, rows[position])
		}
	}
	for position := floor; position <= counter; position++ {
		if len(rows[position]) == 0 {
			t.Fatalf("%s: position %d at or above the prune at %d lost its witnesses", what, position, floor)
		}
	}
	return rows
}

// TestSingleStoreTaskEventPruneCadenceIntegration: pruning runs only at
// multiples of taskEventPruneCadence, never reads anything a peer wrote, and
// keeps a workspace that commits one event per position at or below
// taskEventPruneCadence witnesses with no peer reading. A rolled-back
// commit at a prune position prunes nothing.
func TestSingleStoreTaskEventPruneCadenceIntegration(t *testing.T) {
	st := openOwnedStore(t)
	ctx, ws, taskID := seedSingleStoreNoticeTask(t, st)
	counter, retained := taskEventPosition(t, st, ws)
	rollback := errors.New("rollback probe")
	rolledBack := false
	highest := 0
	for counter < 4*taskEventPruneCadence+9 {
		if counter == 2*taskEventPruneCadence-1 && !rolledBack {
			// The next commit would prune. Roll it back: neither the
			// counter nor any witness moves.
			if err := st.withTx(ctx, func(tx *sql.Tx) error {
				if err := taskEvent(ctx, tx, core.Event{TaskID: taskID, Kind: "peer.cadence.rollback"}); err != nil {
					return err
				}
				return rollback
			}); !errors.Is(err, rollback) {
				t.Fatalf("rollback probe: %v", err)
			}
			if p, w := taskEventPosition(t, st, ws); p != counter || w != retained {
				t.Fatalf("a rolled-back prune commit moved position %d->%d, witnesses %d->%d", counter, p, retained, w)
			}
			rolledBack = true
		}
		if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "peer.cadence"}); err != nil {
			t.Fatal(err)
		}
		counter++
		if isTaskEventPrunePosition(counter) {
			// Every earlier event is visible: the prune removes them all.
			retained = 0
		}
		retained++
		requireWitnessModel(t, st, ws, retained, nil, fmt.Sprintf("commit at %d", counter))
		if counter >= taskEventPruneCadence && retained > highest {
			highest = retained
		}
		if counter >= taskEventPruneCadence && retained > taskEventPruneCadence {
			t.Fatalf("position %d retains %d witnesses, above the bound %d", counter, retained, taskEventPruneCadence)
		}
	}
	if !rolledBack || highest != taskEventPruneCadence {
		t.Fatalf("rolled back=%v, highest retained=%d, want the bound %d reached", rolledBack, highest, taskEventPruneCadence)
	}
	t.Logf("%d positions committed; at most %d witnesses retained after the first prune", counter, highest)
}

// TestSingleStoreTaskEventPruneBacklogIntegration: the retained witnesses
// follow W' = W - min(V, taskEventPositionPrune) + E at each prune position.
// A prune keeps the witness of an event it cannot read however many prune
// positions pass, removes at most taskEventPositionPrune visible witnesses
// oldest first, and drains a larger visible backlog at the next prune
// positions.
func TestSingleStoreTaskEventPruneBacklogIntegration(t *testing.T) {
	st := openOwnedStore(t)
	ctx, ws, taskID := seedSingleStoreNoticeTask(t, st)

	held := holdTaskEvent(t, st, ctx, taskID, "peer.backlog.held")
	commitPositionFirst(t, held)
	heldAt, _ := taskEventPosition(t, st, ws)
	kept := map[int64]bool{heldAt: true}

	// One transaction writes more events than one prune removes.
	const batch = taskEventPositionPrune + 100
	nextPositionNotPrune(t, st, ctx, ws, taskID)
	if err := st.withTx(ctx, func(tx *sql.Tx) error {
		for i := 0; i < batch; i++ {
			if err := taskEvent(ctx, tx, core.Event{TaskID: taskID, Kind: "peer.backlog.batch"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	batchAt := requireCounterWitnessed(t, st, ws, batch, "batch commit")

	// The first prune position: every witness below it but the held one is
	// visible. It removes the oldest taskEventPositionPrune of them.
	prune := lastPrunePosition(batchAt) + taskEventPruneCadence
	commitToPosition(t, st, ctx, ws, taskID, prune-1)
	_, before := taskEventPosition(t, st, ws)
	commitToPosition(t, st, ctx, ws, taskID, prune)
	visible := before - 1
	want := before - min(visible, taskEventPositionPrune) + 1
	// Oldest first: the budget runs out inside the batch, so the batch's
	// newest witnesses and every later position keep theirs.
	for position := batchAt; position < prune; position++ {
		kept[position] = true
	}
	rows := requireWitnessModel(t, st, ws, want, kept, "first prune")
	if len(rows[heldAt]) != 1 || rows[heldAt][0] != held.id() {
		t.Fatalf("the invisible event's witness at %d=%v, want it kept", heldAt, rows[heldAt])
	}
	for position := range rows {
		if position != heldAt && position != prune && position < batchAt {
			t.Fatalf("an older visible witness at %d survived while newer ones at %d were pruned", position, batchAt)
		}
	}
	if left := len(rows[batchAt]); left == 0 || left == batch {
		t.Fatalf("the batch at %d kept %d of %d witnesses, want a partial prune", batchAt, left, batch)
	}
	for position := batchAt + 1; position < prune; position++ {
		if len(rows[position]) != 1 {
			t.Fatalf("position %d after the batch has witnesses %v, want its one event kept", position, rows[position])
		}
	}
	t.Logf("first prune at %d: %d -> %d witnesses (%d visible, budget %d)", prune, before, want, visible, taskEventPositionPrune)

	// The next prune position drains the rest of the visible backlog.
	commitToPosition(t, st, ctx, ws, taskID, prune+taskEventPruneCadence-1)
	_, before = taskEventPosition(t, st, ws)
	commitToPosition(t, st, ctx, ws, taskID, prune+taskEventPruneCadence)
	visible = before - 1
	if visible > taskEventPositionPrune {
		t.Fatalf("second prune: %d visible witnesses, above one prune's budget", visible)
	}
	requireWitnessModel(t, st, ws, 2, map[int64]bool{heldAt: true}, "second prune")

	// Released, the held event's witness goes at the next prune position.
	held.release(t)
	commitToPosition(t, st, ctx, ws, taskID, prune+2*taskEventPruneCadence)
	requireWitnessModel(t, st, ws, 1, nil, "after release")

	// A peer that never read during all this accepts the counter.
	peer, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	counter, _ := taskEventPosition(t, st, ws)
	if marks, err := peer.PeerMarks(context.Background()); err != nil || marks[ws].Events != counter {
		t.Fatalf("a fresh peer: marks=%+v err=%v, want position %d", marks[ws], err, counter)
	}
}

// TestSingleStoreTaskEventConcurrentWritersIntegration: concurrent writers
// in one workspace advance the counter once per transaction, every retained
// position keeps its complete witness set, every pruned witness's event is
// visible, an earlier held event keeps every later visible position from
// being accepted, and once it is visible a peer reaches the last position.
func TestSingleStoreTaskEventConcurrentWritersIntegration(t *testing.T) {
	st := openOwnedStore(t)
	ctx, ws, firstTask := seedSingleStoreNoticeTask(t, st)
	const writers, commits = 6, 40
	tasks := []string{firstTask}
	for len(tasks) < writers {
		id := core.NewTaskID()
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: ws, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, id)
	}
	peer, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	held := holdTaskEvent(t, st, ctx, firstTask, "peer.concurrent.held")
	commitPositionFirst(t, held)
	heldAt, _ := taskEventPosition(t, st, ws)

	var mu sync.Mutex
	advances := 0
	observe := func(workspaces []string) {
		n := 0
		for _, w := range workspaces {
			if w == ws {
				n++
			}
		}
		mu.Lock()
		advances += n
		mu.Unlock()
	}
	observeTaskEventPreparation.Store(&observe)
	t.Cleanup(func() { observeTaskEventPreparation.Store(nil) })

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for _, taskID := range tasks {
		wg.Add(1)
		go func(taskID string) {
			defer wg.Done()
			for i := 0; i < commits; i++ {
				if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "peer.concurrent"}); err != nil {
					errs <- err
					return
				}
			}
		}(taskID)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	observeTaskEventPreparation.Store(nil)

	counter, _ := taskEventPosition(t, st, ws)
	if counter != heldAt+writers*commits || advances != writers*commits {
		t.Fatalf("counter %d after %d commits from %d, %d advances: want one advance per transaction", counter, writers*commits, heldAt, advances)
	}
	if lastPrunePosition(counter) <= heldAt {
		t.Fatalf("the writers crossed no prune position above the held event at %d (counter %d)", heldAt, counter)
	}
	// Every position from the last prune keeps its one witness; below it
	// only the held event's witness stays.
	rows := requireWitnessModel(t, st, ws, int(counter-lastPrunePosition(counter))+2, map[int64]bool{heldAt: true}, "after the writers")
	if len(rows[heldAt]) != 1 || rows[heldAt][0] != held.id() {
		t.Fatalf("the held event's witness at %d=%v, want it kept", heldAt, rows[heldAt])
	}
	ids := map[int64]bool{}
	for position, witnessed := range rows {
		if len(witnessed) != 1 {
			t.Fatalf("position %d has witnesses %v, want its one event", position, witnessed)
		}
		if ids[witnessed[0]] {
			t.Fatalf("event %d is witnessed twice", witnessed[0])
		}
		ids[witnessed[0]] = true
	}
	// Every pruned witness's event is visible: all the writers' events are,
	// and the retained ones but the held event join a visible row.
	var written, joined int
	if err := st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=? AND kind='peer.concurrent'`, ws).Scan(&written); err != nil || written != writers*commits {
		t.Fatalf("visible writer events=%d err=%v, want %d", written, err, writers*commits)
	}
	if err := st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_event_witnesses w JOIN events_columnstore e ON e.event_shard_key=w.event_shard_key AND e.workspace_id=w.workspace_id AND e.id=w.event_id WHERE w.workspace_id=?`, ws).Scan(&joined); err != nil || joined != len(ids)-1 {
		t.Fatalf("retained witnesses joining a visible event=%d err=%v, want all %d but the held one", joined, err, len(ids))
	}

	// The held earlier event keeps the peer below it, however many later
	// positions are visible.
	marks, err := peer.PeerMarks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if marks[ws].Events >= heldAt {
		t.Fatalf("the peer accepted position %d past the invisible event at %d", marks[ws].Events, heldAt)
	}
	held.release(t)
	if marks, err = peer.PeerMarks(context.Background()); err != nil || marks[ws].Events != counter {
		t.Fatalf("after the held event became visible: marks=%+v err=%v, want the last position %d", marks[ws], err, counter)
	}
}
