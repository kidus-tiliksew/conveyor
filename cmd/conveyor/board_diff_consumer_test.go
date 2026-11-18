package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// boardDiffFakeServer serves the three reads that conveyor board diff
// composes: GET /v1/board, GET /v1/activity, and GET /v1/tasks/{id}/history
// (component-next-action invariant 47). When block is set, the next request
// waits until block closes or its client goes away.
type boardDiffFakeServer struct {
	mu       sync.Mutex
	date     time.Time
	board    []map[string]any
	rows     []map[string]any
	history  map[string][]map[string]any
	fail     map[string]int
	requests []*url.URL
	block    chan struct{}
	blocked  chan struct{}
	srv      *httptest.Server
}

func newBoardDiffFakeServer(t *testing.T, date time.Time) *boardDiffFakeServer {
	t.Helper()
	f := &boardDiffFakeServer{date: date, history: map[string][]map[string]any{}, fail: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL)
		block, blocked := f.block, f.blocked
		f.block, f.blocked = nil, nil
		f.mu.Unlock()
		if block != nil {
			close(blocked)
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Date", f.date.Format(http.TimeFormat))
		if status := f.fail[r.URL.Path]; status != 0 {
			http.Error(w, "failed", status)
			return
		}
		switch {
		case r.URL.Path == "/v1/board":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": f.board, "total": len(f.board)})
		case r.URL.Path == "/v1/activity":
			w.Header().Set("X-Conveyor-Total", strconv.Itoa(len(f.rows)))
			rows := f.rows
			if rows == nil {
				rows = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(rows)
		case strings.HasPrefix(r.URL.Path, "/v1/tasks/") && strings.HasSuffix(r.URL.Path, "/history"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/history")
			events := f.history[id]
			if events == nil {
				events = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"events": events, "has_older": false})
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *boardDiffFakeServer) set(change func(*boardDiffFakeServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *boardDiffFakeServer) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *boardDiffFakeServer) requestsFrom(index int) []*url.URL {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*url.URL(nil), f.requests[index:]...)
}

// blockNext makes the next request wait and returns the channel that closes
// once it waits and the function that lets it continue.
func (f *boardDiffFakeServer) blockNext() (<-chan struct{}, func()) {
	block, blocked := make(chan struct{}), make(chan struct{})
	f.set(func(f *boardDiffFakeServer) { f.block, f.blocked = block, blocked })
	var once sync.Once
	return blocked, func() { once.Do(func() { close(block) }) }
}

func boardDiffFakeRow(id string, lastEventAt time.Time) map[string]any {
	return map[string]any{"task": map[string]any{"id": id, "title": "Task " + id, "state": "running"}, "last_event_at": lastEventAt}
}

// boardDiffFakeEvent builds one history event. A nil payload is an omitted
// payload, as the history route serves it, with the given payload_index.
func boardDiffFakeEvent(id int, kind string, at time.Time, payload map[string]any, index map[string]any) map[string]any {
	event := map[string]any{"id": strconv.Itoa(id), "kind": kind, "at": at, "payload": payload}
	if payload == nil {
		event["payload_omitted"] = true
		event["payload_index"] = index
	}
	return event
}

func runBoardDiffJSON(t *testing.T, c *client, options boardDiffOptions) boardDiffOutput {
	t.Helper()
	options.asJSON = true
	var out bytes.Buffer
	if err := runBoardDiff(c, &out, options); err != nil {
		t.Fatal(err)
	}
	var result boardDiffOutput
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	return result
}

// watchBoardDiffLockWait replaces the lock wait hook for one test and returns
// a channel that closes the first time a run waits for a held lock.
func watchBoardDiffLockWait(t *testing.T) <-chan struct{} {
	t.Helper()
	waiting := make(chan struct{})
	var once sync.Once
	old := boardDiffLockWaiting
	boardDiffLockWaiting = func() { once.Do(func() { close(waiting) }) }
	t.Cleanup(func() { boardDiffLockWaiting = old })
	return waiting
}

func waitForBoardDiff(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Invariant 50: the stage comes only from the payload of a created or claimed
// event. Omitted claims and updates keep the saved stage and entered_at. An
// omitted create reports no stage change and lists the task in incomplete.
// A stage in payload_index is never read, in stage-start recovery or in
// reduction.
func TestBoardDiffOmittedPayloadsKeepStage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t0 := time.Now().UTC().Truncate(time.Second)
	f := newBoardDiffFakeServer(t, t0)
	f.set(func(f *boardDiffFakeServer) {
		f.board = []map[string]any{{"id": "a", "title": "Task a", "state": "running", "stage": "implement"}}
	})
	c := &client{base: f.srv.URL, workspace: "demo"}
	if first := runBoardDiffJSON(t, c, boardDiffOptions{}); !first.FirstRun || first.Summary.Stages["implement"] != 1 {
		t.Fatalf("seed %#v", first)
	}
	snapshot := func() boardDiffTask {
		t.Helper()
		state, reason, err := loadBoardDiffState(f.srv.URL, "demo", "", time.Now())
		if err != nil || reason != "" {
			t.Fatalf("snapshot reason %q err %v", reason, err)
		}
		return state.Tasks["a"]
	}
	step := func(k int, events ...map[string]any) boardDiffOutput {
		t.Helper()
		at := t0.Add(time.Duration(k) * 10 * time.Minute)
		f.set(func(f *boardDiffFakeServer) {
			f.date = at
			f.rows = []map[string]any{boardDiffFakeRow("a", at.Add(-time.Minute))}
			f.history["a"] = append(f.history["a"], events...)
		})
		return runBoardDiffJSON(t, c, boardDiffOptions{})
	}

	// The seed knows the stage but not its start. Recovery finds the start in
	// the payload of the first create and skips the omitted create whose
	// payload_index names another stage.
	t1 := t0.Add(10 * time.Minute)
	out := step(1,
		boardDiffFakeEvent(1, "task.created", t0.Add(-3*time.Hour), map[string]any{}, nil),
		boardDiffFakeEvent(2, "work_order.created", t0.Add(-2*time.Hour), map[string]any{"stage": "implement"}, nil),
		boardDiffFakeEvent(3, "work_order.created", t0.Add(-time.Hour), nil, map[string]any{"stage": "review", "attempt": 1}),
		boardDiffFakeEvent(4, "work_order.created", t1.Add(-time.Minute), map[string]any{"stage": "review"}, nil),
	)
	if len(out.Incomplete) != 0 || len(out.Changes) != 1 || len(out.Changes[0].Transitions) != 1 {
		t.Fatalf("recovery run %#v", out)
	}
	tr := out.Changes[0].Transitions[0]
	if tr.From != "implement" || tr.To != "review" || tr.PreviousSeconds == nil || *tr.PreviousSeconds != int64((2*time.Hour+9*time.Minute)/time.Second) {
		t.Fatalf("recovery transition %#v", tr)
	}
	entered := t1.Add(-time.Minute)
	if task := snapshot(); task.Stage != "review" || task.EnteredAt == nil || !task.EnteredAt.Equal(entered) {
		t.Fatalf("after create %#v", task)
	}

	// An omitted claim and an omitted update keep the stage, even with a stage
	// in payload_index, and add no incomplete entry.
	t2 := t0.Add(20 * time.Minute)
	out = step(2,
		boardDiffFakeEvent(5, "work_order.claimed", t2.Add(-2*time.Minute), nil, map[string]any{"stage": "implement", "attempt": 1}),
		boardDiffFakeEvent(6, "work_order.updated", t2.Add(-time.Minute), nil, map[string]any{"stage": "verify"}),
	)
	if len(out.Changes) != 0 || len(out.Incomplete) != 0 {
		t.Fatalf("omitted claim and update %#v", out)
	}
	if task := snapshot(); task.Stage != "review" || task.EnteredAt == nil || !task.EnteredAt.Equal(entered) {
		t.Fatalf("after omitted claim %#v", task)
	}

	// An omitted create reports no stage change and lists the task in
	// incomplete. The saved stage and its start stay.
	t3 := t0.Add(30 * time.Minute)
	out = step(3, boardDiffFakeEvent(7, "work_order.created", t3.Add(-time.Minute), nil, map[string]any{"stage": "verify"}))
	if len(out.Changes) != 0 || len(out.Incomplete) != 1 || out.Incomplete[0].ID != "a" || !strings.Contains(out.Incomplete[0].Reason, "history incomplete") {
		t.Fatalf("omitted create %#v", out)
	}
	if task := snapshot(); task.Stage != "review" || task.EnteredAt == nil || !task.EnteredAt.Equal(entered) {
		t.Fatalf("after omitted create %#v", task)
	}

	// The next claim with a payload stage reports the change from the kept
	// stage, timed from the kept start.
	t4 := t0.Add(40 * time.Minute)
	out = step(4, boardDiffFakeEvent(8, "work_order.claimed", t4.Add(-time.Minute), map[string]any{"stage": "verify"}, nil))
	if len(out.Incomplete) != 0 || len(out.Changes) != 1 || len(out.Changes[0].Transitions) != 1 {
		t.Fatalf("claim with payload %#v", out)
	}
	tr = out.Changes[0].Transitions[0]
	if tr.From != "review" || tr.To != "verify" || tr.PreviousSeconds == nil || *tr.PreviousSeconds != int64((30*time.Minute)/time.Second) {
		t.Fatalf("claim transition %#v", tr)
	}
}

// Invariant 56 and State and failure: each consumer has its own snapshot and
// position. The default consumer, omitted or named `default`, keeps board.json.
func TestBoardDiffConsumersKeepSeparateSnapshots(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t0 := time.Now().UTC().Truncate(time.Second)
	f := newBoardDiffFakeServer(t, t0)
	f.set(func(f *boardDiffFakeServer) {
		f.board = []map[string]any{{"id": "a", "title": "Task a", "state": "running", "stage": "implement"}}
	})
	c := &client{base: f.srv.URL, workspace: "demo", token: "do-not-persist"}
	alpha := boardDiffOptions{consumer: "alpha", consumerSet: true}
	beta := boardDiffOptions{consumer: "beta", consumerSet: true}
	for _, options := range []boardDiffOptions{alpha, beta} {
		if out := runBoardDiffJSON(t, c, options); !out.FirstRun {
			t.Fatalf("%s seed %#v", options.consumer, out)
		}
	}
	defaultPath, _ := boardDiffStatePath(f.srv.URL, "demo", "")
	alphaPath, _ := boardDiffStatePath(f.srv.URL, "demo", "alpha")
	betaPath, _ := boardDiffStatePath(f.srv.URL, "demo", "beta")
	if filepath.Base(alphaPath) != "board-alpha.json" || filepath.Base(betaPath) != "board-beta.json" || filepath.Dir(alphaPath) != filepath.Dir(defaultPath) {
		t.Fatalf("paths %s %s %s", defaultPath, alphaPath, betaPath)
	}
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("named consumers wrote the default snapshot: %v", err)
	}

	t1 := t0.Add(10 * time.Minute)
	f.set(func(f *boardDiffFakeServer) {
		f.date = t1
		f.rows = []map[string]any{boardDiffFakeRow("a", t1.Add(-time.Minute))}
		f.history["a"] = []map[string]any{boardDiffFakeEvent(1, "work_order.created", t1.Add(-time.Minute), map[string]any{"stage": "review"}, nil)}
	})
	if out := runBoardDiffJSON(t, c, alpha); len(out.Changes) != 1 || out.Position != t1.Format(time.RFC3339) {
		t.Fatalf("alpha change %#v", out)
	}
	if out := runBoardDiffJSON(t, c, alpha); len(out.Changes) != 0 {
		t.Fatalf("alpha repeat %#v", out)
	}
	t2 := t0.Add(20 * time.Minute)
	f.set(func(f *boardDiffFakeServer) { f.date = t2 })
	// Alpha's runs took nothing from beta: beta still reports the change.
	if out := runBoardDiffJSON(t, c, beta); len(out.Changes) != 1 || out.Changes[0].Transitions[0].To != "review" || out.Position != t2.Format(time.RFC3339) {
		t.Fatalf("beta change %#v", out)
	}
	for consumer, position := range map[string]time.Time{"alpha": t1, "beta": t2} {
		state, reason, err := loadBoardDiffState(f.srv.URL, "demo", consumer, time.Now())
		if err != nil || reason != "" || !state.Position.Equal(position) || state.Version != 1 || state.Workspace != "demo" || state.Tasks["a"].Stage != "review" {
			t.Fatalf("%s snapshot %#v reason %q err %v", consumer, state, reason, err)
		}
	}
	for path, mode := range map[string]os.FileMode{alphaPath: 0600, betaPath: 0600, filepath.Dir(alphaPath): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("mode of %s: %v %v", path, info, err)
		}
	}
	for _, path := range []string{alphaPath, betaPath} {
		data, err := os.ReadFile(path)
		if err != nil || bytes.Contains(data, []byte(c.token)) {
			t.Fatalf("snapshot %s holds a credential or failed: %v", path, err)
		}
	}

	// `--consumer default` and an omitted flag share board.json.
	if out := runBoardDiffJSON(t, c, boardDiffOptions{consumer: "default", consumerSet: true}); !out.FirstRun {
		t.Fatalf("default seed %#v", out)
	}
	if _, err := os.Stat(defaultPath); err != nil {
		t.Fatalf("default snapshot: %v", err)
	}
	if out := runBoardDiffJSON(t, c, boardDiffOptions{}); out.FirstRun {
		t.Fatalf("omitted consumer did not read board.json %#v", out)
	}
}

// An existing board.json from before consumers keeps working unchanged.
func TestBoardDiffDefaultSnapshotNeedsNoMigration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t0 := time.Now().UTC().Truncate(time.Second)
	f := newBoardDiffFakeServer(t, t0)
	c := &client{base: f.srv.URL}
	runBoardDiffJSON(t, c, boardDiffOptions{})
	entries, err := os.ReadDir(filepath.Join(root, "conveyor"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("state root %v %v", entries, err)
	}
	path := filepath.Join(root, "conveyor", entries[0].Name(), "default", "board.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out := runBoardDiffJSON(t, c, boardDiffOptions{}); out.FirstRun {
		t.Fatalf("existing board.json was not used %#v", out)
	}
	if out := runBoardDiffJSON(t, c, boardDiffOptions{consumer: "default", consumerSet: true}); out.FirstRun {
		t.Fatalf("--consumer default did not use board.json %#v", out)
	}
	var state map[string]any
	after, _ := os.ReadFile(path)
	if json.Unmarshal(after, &state) != nil || state["version"] != float64(1) || len(before) == 0 {
		t.Fatalf("snapshot %s", after)
	}
}

// State and failure: an empty value, `.`, `..`, or a value with `/` or `\`
// is a usage error, found before any request or snapshot access.
func TestBoardDiffConsumerNameErrors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	f := newBoardDiffFakeServer(t, time.Now().UTC())
	c := &client{base: f.srv.URL, workspace: "demo"}
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "../x", "/"} {
		for _, options := range []boardDiffOptions{{consumer: name, consumerSet: true}, {consumer: name, consumerSet: true, reset: true}, {consumer: name, consumerSet: true, since: time.Now().UTC().Format(time.RFC3339)}} {
			err := runBoardDiff(c, &bytes.Buffer{}, options)
			if err == nil || !strings.Contains(err.Error(), "--consumer") {
				t.Fatalf("consumer %q options %#v: %v", name, options, err)
			}
		}
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("usage errors sent %d requests", n)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("usage errors touched state: %v %v", entries, err)
	}
	// The real command line tells an explicit empty value from an omitted flag.
	for _, args := range [][]string{{"board", "diff", "--consumer", ""}, {"board", "diff", "--consumer="}, {"board", "diff", "--consumer", ".."}, {"board", "diff", "--consumer", `x\y`}} {
		stderr, status := runConveyorMain(t, f.srv.URL, args...)
		if status == 0 || !strings.Contains(stderr, "--consumer") {
			t.Fatalf("%q: status %d stderr %q", args, status, stderr)
		}
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("usage errors sent %d requests", n)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("usage errors touched state: %v %v", entries, err)
	}
}

// State and failure: a failed run leaves the consumer's old file unchanged,
// and every error releases the lock.
func TestBoardDiffConsumerFailureKeepsSnapshotAndReleasesLock(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t0 := time.Now().UTC().Truncate(time.Second)
	f := newBoardDiffFakeServer(t, t0)
	c := &client{base: f.srv.URL, workspace: "demo"}
	options := boardDiffOptions{consumer: "c", consumerSet: true}
	runBoardDiffJSON(t, c, options)
	path, _ := boardDiffStatePath(f.srv.URL, "demo", "c")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	waits := 0
	old := boardDiffLockWaiting
	boardDiffLockWaiting = func() { waits++ }
	defer func() { boardDiffLockWaiting = old }()

	f.set(func(f *boardDiffFakeServer) { f.date = t0.Add(time.Minute) })
	oldRename := boardDiffStateWriter.rename
	boardDiffStateWriter.rename = func(string, string) error { return errors.New("rename failed") }
	err = runBoardDiff(c, &bytes.Buffer{}, options)
	boardDiffStateWriter.rename = oldRename
	if err == nil {
		t.Fatal("expected a failed replace")
	}
	f.set(func(f *boardDiffFakeServer) { f.fail["/v1/activity"] = http.StatusInternalServerError })
	if err := runBoardDiff(c, &bytes.Buffer{}, options); err == nil {
		t.Fatal("expected a failed activity read")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(original, after) {
		t.Fatalf("old snapshot changed: %v", err)
	}
	f.set(func(f *boardDiffFakeServer) { delete(f.fail, "/v1/activity") })
	if out := runBoardDiffJSON(t, c, options); out.FirstRun || out.Position != t0.Add(time.Minute).Format(time.RFC3339) {
		t.Fatalf("run after errors %#v", out)
	}
	if waits != 0 {
		t.Fatalf("a failed run kept the lock: %d waits", waits)
	}
}

// startBoardDiffChild runs `conveyor board diff` through the real CLI entry
// point in another process.
func startBoardDiffChild(t *testing.T, server string, args ...string) *exec.Cmd {
	t.Helper()
	home := t.TempDir()
	args = append([]string{"--workspace", "demo", "board", "diff", "--json"}, args...)
	command := exec.Command(os.Args[0], "-test.run=^TestTaskWaitProcessHelper$")
	command.Dir = t.TempDir()
	command.Env = append(taskWaitChildEnvironment(),
		taskWaitHelperEnv+"=1", taskWaitHelperArgsEnv+"="+strings.Join(args, "\x1f"),
		"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"CONVEYOR_ADDR="+server,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	return command
}

func runBoardDiffAsync(c *client, options boardDiffOptions) <-chan error {
	done := make(chan error, 1)
	go func() {
		options.asJSON = true
		done <- runBoardDiff(c, &bytes.Buffer{}, options)
	}()
	return done
}

// State and failure: a run in another process holds the snapshot lock, so a
// second run on the same snapshot waits until the first ends and then reads
// the position that the first saved.
func TestBoardDiffSnapshotLockAcrossProcesses(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t0 := time.Now().UTC().Truncate(time.Second)
	f := newBoardDiffFakeServer(t, t0)
	blocked, release := f.blockNext()
	defer release()
	child := startBoardDiffChild(t, f.srv.URL, "--consumer", "shared")
	waitForBoardDiff(t, blocked, "the first run to hold the lock")

	waiting := watchBoardDiffLockWait(t)
	c := &client{base: f.srv.URL, workspace: "demo"}
	second := runBoardDiffAsync(c, boardDiffOptions{consumer: "shared", consumerSet: true})
	waitForBoardDiff(t, waiting, "the second run to wait")
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-second:
		t.Fatalf("second run ended while the first held the lock: %v", err)
	default:
	}
	if n := f.requestCount(); n != 1 {
		t.Fatalf("second run sent requests while it waited: %d requests", n)
	}

	release()
	if err := child.Wait(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	// The first run, a first run, read one activity page and one board page.
	// Every later request is the second run's.
	all := f.requestsFrom(0)
	if len(all) != 3 || all[0].Path != "/v1/activity" || all[1].Path != "/v1/board" {
		t.Fatalf("requests %v", all)
	}
	later := all[2:]
	if later[0].Path != "/v1/activity" {
		t.Fatalf("second run requests %v", later)
	}
	from, err := time.Parse(time.RFC3339Nano, later[0].Query().Get("updated_from"))
	if err != nil || !from.Equal(t0.Add(-2*time.Minute)) {
		t.Fatalf("second run did not read the first run's position: %v %v", from, err)
	}
}

// State and failure: the operating system releases the lock of a process
// that exits, so a killed run leaves no owner behind.
func TestBoardDiffSnapshotLockReleasedOnProcessExit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := newBoardDiffFakeServer(t, time.Now().UTC().Truncate(time.Second))
	blocked, release := f.blockNext()
	defer release()
	child := startBoardDiffChild(t, f.srv.URL, "--consumer", "shared")
	waitForBoardDiff(t, blocked, "the first run to hold the lock")

	waiting := watchBoardDiffLockWait(t)
	c := &client{base: f.srv.URL, workspace: "demo"}
	second := runBoardDiffAsync(c, boardDiffOptions{consumer: "shared", consumerSet: true})
	waitForBoardDiff(t, waiting, "the second run to wait")
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lock stayed held after its process exited")
	}
	if state, reason, err := loadBoardDiffState(f.srv.URL, "demo", "shared", time.Now()); err != nil || reason != "" || state.Position.IsZero() {
		t.Fatalf("second run snapshot reason %q err %v", reason, err)
	}
}

// State and failure: different consumers do not share a lock, a --since run
// takes no lock and neither reads nor writes the snapshot, and --reset waits
// for the lock like any other run on that snapshot.
func TestBoardDiffSnapshotLockScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t0 := time.Now().UTC().Truncate(time.Second)
	f := newBoardDiffFakeServer(t, t0)
	blocked, release := f.blockNext()
	defer release()
	child := startBoardDiffChild(t, f.srv.URL, "--consumer", "held")
	waitForBoardDiff(t, blocked, "the first run to hold the lock")

	waits := 0
	old := boardDiffLockWaiting
	boardDiffLockWaiting = func() { waits++ }
	c := &client{base: f.srv.URL, workspace: "demo"}
	if out := runBoardDiffJSON(t, c, boardDiffOptions{consumer: "other", consumerSet: true}); !out.FirstRun {
		t.Fatalf("other consumer %#v", out)
	}
	heldPath, _ := boardDiffStatePath(f.srv.URL, "demo", "held")
	before := f.requestCount()
	if out := runBoardDiffJSON(t, c, boardDiffOptions{consumer: "held", consumerSet: true, since: t0.Format(time.RFC3339)}); out.FirstRun {
		t.Fatalf("since run %#v", out)
	}
	if later := f.requestsFrom(before); len(later) != 1 || later[0].Path != "/v1/activity" {
		t.Fatalf("since run requests %v", later)
	}
	if _, err := os.Stat(heldPath); !os.IsNotExist(err) {
		t.Fatalf("since run wrote the held snapshot: %v", err)
	}
	boardDiffLockWaiting = old
	if waits != 0 {
		t.Fatalf("runs without a shared lock waited %d times", waits)
	}

	waiting := watchBoardDiffLockWait(t)
	reset := runBoardDiffAsync(c, boardDiffOptions{consumer: "held", consumerSet: true, reset: true})
	waitForBoardDiff(t, waiting, "the reset run to wait")
	select {
	case err := <-reset:
		t.Fatalf("reset ended while the lock was held: %v", err)
	default:
	}
	release()
	if err := child.Wait(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := <-reset; err != nil {
		t.Fatal(err)
	}
}
