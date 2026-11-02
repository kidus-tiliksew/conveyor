package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// The daemon-wide peer check on a backend without change notification
// (component-runtime, Peers; feature-quiet-idle-runtime, Peers;
// req-quiet-idle-runtime AC-1.3, AC-2.5). The daemon's queue runtime, HTTP
// server, and work-order service run on the volatile backend with their own
// hub, which hears none of the backend's commits: every write below stands
// for another daemon's commit. Time is an injected ticker, and the test
// counts statements: each PeerMarks read, log tail, and store read on the
// stream, await_review, and runtime paths.

const peerCheckToken = "peer-check-token"

// peerCounts counts the daemon's statements by name.
type peerCounts struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *peerCounts) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[name]++
}

func (c *peerCounts) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.n))
	for k, v := range c.n {
		out[k] = v
	}
	return out
}

func (c *peerCounts) get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[name]
}

// peerCountingBackend counts the reads the stream, await_review, and
// credential paths make.
type peerCountingBackend struct {
	store.Backend
	counts *peerCounts
}

func (b *peerCountingBackend) ReadTaskEventWindow(ctx context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	b.counts.add("ReadTaskEventWindow")
	return b.Backend.ReadTaskEventWindow(ctx, q)
}

func (b *peerCountingBackend) ReadTaskEventStream(ctx context.Context, q store.TaskEventStreamQuery) (store.TaskEventStreamPage, error) {
	b.counts.add("ReadTaskEventStream")
	return b.Backend.ReadTaskEventStream(ctx, q)
}

func (b *peerCountingBackend) ListCompactEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	b.counts.add("ListCompactEvents")
	return b.Backend.ListCompactEvents(ctx, taskID)
}

func (b *peerCountingBackend) GetTask(ctx context.Context, id string) (core.Task, error) {
	b.counts.add("GetTask")
	return b.Backend.GetTask(ctx, id)
}

func (b *peerCountingBackend) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	b.counts.add("GetWorkOrder")
	return b.Backend.GetWorkOrder(ctx, id)
}

func (b *peerCountingBackend) ListTaskWorkOrdersSnapshot(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	b.counts.add("ListTaskWorkOrdersSnapshot")
	return b.Backend.ListTaskWorkOrdersSnapshot(ctx, taskID)
}

// peerCountingLog counts the runtime's log reads.
type peerCountingLog struct {
	eventlog.Store
	counts *peerCounts
}

func (l *peerCountingLog) Tail(ctx context.Context, workspace string, after eventlog.Position, limit int) ([]eventlog.Event, error) {
	l.counts.add("Tail")
	return l.Store.Tail(ctx, workspace, after, limit)
}

func (l *peerCountingLog) Read(ctx context.Context, workspace string, stream eventlog.StreamID, after eventlog.Version, limit int) ([]eventlog.Event, error) {
	l.counts.add("Read")
	return l.Store.Read(ctx, workspace, stream, after, limit)
}

// peerModeStore is the backend as peer mode selection sees it: no change
// notification, and a counted PeerMarks read that can be made to fail once.
type peerModeStore struct {
	base     store.Backend
	reader   changes.PeerReader
	counts   *peerCounts
	mu       sync.Mutex
	failNext bool
}

func (s *peerModeStore) EnablePeers(ctx context.Context) bool { return s.base.EnablePeers(ctx) }

func (s *peerModeStore) PeerMarks(ctx context.Context) (map[string]changes.PeerMark, error) {
	s.counts.add("PeerMarks")
	s.mu.Lock()
	fail := s.failNext
	s.failNext = false
	s.mu.Unlock()
	if fail {
		return nil, errors.New("peer check connection reset")
	}
	return s.reader.PeerMarks(ctx)
}

type peerOutcome struct {
	published int
	err       error
}

// peerSSE reads one server-sent-event response into frames.
func peerSSE(t *testing.T, ctx context.Context, url string) <-chan string {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+peerCheckToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, response.StatusCode)
	}
	frames := make(chan string, 64)
	go func() {
		defer response.Body.Close()
		defer close(frames)
		reader := bufio.NewReader(response.Body)
		var frame strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\n" {
				frames <- frame.String()
				frame.Reset()
				continue
			}
			frame.WriteString(line)
		}
	}()
	return frames
}

func peerFrame(t *testing.T, frames <-chan string, what string) string {
	t.Helper()
	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatalf("%s: stream ended", what)
		}
		return frame
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no frame", what)
		return ""
	}
}

func peerNoFrame(t *testing.T, frames <-chan string, what string) {
	t.Helper()
	select {
	case frame := <-frames:
		t.Fatalf("%s: frame %q", what, frame)
	case <-time.After(50 * time.Millisecond):
	}
}

type peerAwait struct {
	result map[string]any
	err    error
}

func TestPeerCheckIdleDaemon(t *testing.T) {
	base := store.NewVolatileBackend()
	counts := &peerCounts{}
	workspaces := []string{"peer-a", "peer-b", "peer-c"}
	system := func(ws string) context.Context {
		return store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), ws)
	}
	if _, err := base.BootstrapIdentity(system(workspaces[0]), config.FirstOperatorIdentity{OrganizationName: "Peers", Email: "owner@example.test", DisplayName: "Owner"}, peerCheckToken); err != nil {
		t.Fatal(err)
	}
	owner, err := base.VerifyPersonalAccessToken(system(workspaces[0]), peerCheckToken)
	if err != nil {
		t.Fatal(err)
	}
	granter := store.WithActor(store.WithCredential(system(""), core.AuthenticatedCredential{ID: "fixture", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}),
		store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	for _, ws := range workspaces {
		if _, err = base.BootstrapWorkspaceConfig(system(ws), &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
			t.Fatal(err)
		}
		if members, _ := base.ListWorkspaceMembers(granter, ws, ""); len(members) == 0 {
			if _, err = base.GrantWorkspaceRole(granter, owner.Email, ws, core.WorkspaceRoleOperator); err != nil {
				t.Fatalf("grant %s: %v", ws, err)
			}
		}
	}
	newTask := func(ws string) string {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: ws, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id, Mode: core.TaskModeAuto, PolicyVersion: 1, Level: core.L2, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
		if err := base.CreateTask(system(ws), task); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// newSubmitted seeds a task whose implement order the session submitted,
	// so await_review waits on its review.
	newSubmitted := func(ws, session string) (string, string) {
		t.Helper()
		id := newTask(ws)
		ctx := system(ws)
		now := time.Now().UTC()
		job := core.Job{ID: id + "-implement", TaskID: id, Stage: core.StageImplement, State: core.JobPending}
		if err := base.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(base).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		order, err := storetest.For(base).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: session, ClientToken: "token-" + session, Agent: "codex", Model: "gpt", Lease: time.Hour, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		order.State = core.WorkOrderSubmitted
		if err = storetest.For(base).UpdateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
		return id, order.ID
	}
	settle := func(ws, taskID string) {
		t.Helper()
		if err := base.AppendEvent(system(ws), core.Event{TaskID: taskID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"verdict": "approved", "review_round": 1}), At: time.Now().UTC().Add(time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	streamA, streamB := newTask("peer-a"), newTask("peer-b")
	awaitB, orderB := newSubmitted("peer-b", "session-b")
	_, orderC := newSubmitted("peer-c", "session-c")
	lateAwait, lateOrder := newSubmitted("peer-c", "session-late")
	lateStream := newTask("peer-c")

	// Startup, in conveyord's order: select the peer mode, build the
	// runtime, run the check's first statement, start the runtime, then run
	// the check on its ticker.
	daemonHub := changes.NewHub()
	mode := &peerModeStore{base: base, reader: base.(changes.PeerReader), counts: counts}
	runCtx, stopDaemon := context.WithCancel(context.Background())
	defer stopDaemon()
	check := changes.SelectPeerCheck(runCtx, config.RuntimeSettings{Peers: true}.Peers, mode, daemonHub, logqueue.PeerTailInterval, t.Logf)
	if check == nil {
		t.Fatal("peers on a backend without notification selected no peer check")
	}
	ticks := make(chan time.Time)
	outcomes := make(chan peerOutcome, 16)
	check.Ticker = func(d time.Duration) (<-chan time.Time, func()) {
		if d != logqueue.PeerTailInterval {
			t.Errorf("peer check interval=%s", d)
		}
		return ticks, func() {}
	}
	check.Checked = func(published int, err error) { outcomes <- peerOutcome{published, err} }
	ran := make(chan string, 8)
	runtime := logqueue.NewRuntime(&peerCountingLog{Store: base.Log(), counts: counts}, logqueue.Options{Workspaces: workspaces, Changes: daemonHub})
	runtime.Register(queue.Registration{Kind: "peer_probe", Handle: func(_ context.Context, job queue.Job) error {
		ran <- job.WorkspaceID
		return nil
	}})
	if err := check.Check(runCtx); err != nil {
		t.Fatal(err)
	}
	<-outcomes
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runtime.Stop(ctx)
	}()
	go check.Run(runCtx)
	tick := func(what string) peerOutcome {
		t.Helper()
		select {
		case ticks <- time.Time{}:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the peer check did not take the tick", what)
		}
		select {
		case outcome := <-outcomes:
			return outcome
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the peer check did not finish", what)
			return peerOutcome{}
		}
	}

	counted := &peerCountingBackend{Backend: base, counts: counts}
	server := httpapi.NewServer(counted)
	server.Workspaces = base
	server.Changes = daemonHub
	never := make(chan time.Time)
	server.StreamKeepalive = func() (<-chan time.Time, func()) { return never, func() {} }
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	streamCtx, closeStreams := context.WithCancel(context.Background())
	defer closeStreams()
	notifyURL := func(ws, taskID string) string {
		return httpServer.URL + "/v1/tasks/" + taskID + "/events/stream?mode=notify&workspace_id=" + ws
	}
	framesA := peerSSE(t, streamCtx, notifyURL("peer-a", streamA))
	framesB := peerSSE(t, streamCtx, notifyURL("peer-b", streamB))
	for name, frames := range map[string]<-chan string{"stream a": framesA, "stream b": framesB} {
		if frame := peerFrame(t, frames, name+" ready"); frame != "event: ready\ndata: {}\n" {
			t.Fatalf("%s first frame=%q", name, frame)
		}
	}
	service := &workorder.Service{Store: counted, Changes: daemonHub}
	startAwait := func(ws, order, session string) chan peerAwait {
		done := make(chan peerAwait, 1)
		go func() {
			result, err := service.AwaitReview(system(ws), order, session, 10*time.Minute)
			done <- peerAwait{result, err}
		}()
		return done
	}
	doneB := startAwait("peer-b", orderB, "session-b")
	doneC := startAwait("peer-c", orderC, "session-c")
	// Wait for the first reads: one ListCompactEvents per await_review and one
	// catch-up per workspace.
	deadline := time.Now().Add(5 * time.Second)
	for counts.get("ListCompactEvents") < 2 || counts.get("Tail") < len(workspaces) {
		if time.Now().After(deadline) {
			t.Fatalf("first reads did not finish: %v", counts.snapshot())
		}
		time.Sleep(time.Millisecond)
	}

	// Idle: each interval runs exactly one statement, the peer check's read,
	// for three workspaces, two open streams, and two pending waits.
	for i := range 3 {
		before := counts.snapshot()
		outcome := tick(fmt.Sprintf("idle tick %d", i+1))
		if outcome.err != nil || outcome.published != 0 {
			t.Fatalf("idle tick %d: %+v", i+1, outcome)
		}
		after := counts.snapshot()
		for name, n := range after {
			want := before[name]
			if name == "PeerMarks" {
				want++
			}
			if n != want {
				t.Fatalf("idle tick %d: %s ran %d times, want %d (before %v, after %v)", i+1, name, n-before[name], want-before[name], before, after)
			}
		}
	}
	peerNoFrame(t, framesA, "idle stream a")
	peerNoFrame(t, framesB, "idle stream b")
	for name, done := range map[string]chan peerAwait{"b": doneB, "c": doneC} {
		select {
		case outcome := <-done:
			t.Fatalf("await_review %s returned while idle: %+v", name, outcome)
		default:
		}
	}

	// Another writer enqueues a job: nothing on this daemon hears it until
	// the next check, which wakes the runtime.
	tails := counts.get("Tail")
	if _, err := logqueue.Enqueue(system("peer-a"), base.Log(), "peer-a", "peer_probe", "one", struct{}{}, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	select {
	case ws := <-ran:
		t.Fatalf("the job in %s ran before the peer check", ws)
	case <-time.After(50 * time.Millisecond):
	}
	if got := counts.get("Tail"); got != tails {
		t.Fatalf("the runtime tailed %d times before the peer check", got-tails)
	}
	if outcome := tick("job check"); outcome.err != nil || outcome.published != 1 {
		t.Fatalf("job check: %+v, want one job notice", outcome)
	}
	select {
	case ws := <-ran:
		if ws != "peer-a" {
			t.Fatalf("job ran in %s", ws)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not run after the peer check")
	}

	// Another writer appends a task event: the open stream on that task
	// writes its activity frame only after the next check.
	if err := base.AppendEvent(system("peer-a"), core.Event{TaskID: streamA, Kind: "peer.probe"}); err != nil {
		t.Fatal(err)
	}
	peerNoFrame(t, framesA, "stream a before the check")
	if outcome := tick("task check"); outcome.err != nil || outcome.published < 1 {
		t.Fatalf("task check: %+v", outcome)
	}
	if frame := peerFrame(t, framesA, "stream a activity"); frame != "event: activity\ndata: {}\n" {
		t.Fatalf("stream a activity=%q", frame)
	}

	// Another writer settles a review round: await_review returns only after
	// the next check, and the other pending call stays pending.
	settle("peer-b", awaitB)
	select {
	case outcome := <-doneB:
		t.Fatalf("await_review returned before the peer check: %+v", outcome)
	case <-time.After(50 * time.Millisecond):
	}
	if outcome := tick("review check"); outcome.err != nil || outcome.published < 1 {
		t.Fatalf("review check: %+v", outcome)
	}
	select {
	case outcome := <-doneB:
		if outcome.err != nil || outcome.result["verdict"] != "approved" {
			t.Fatalf("await_review result=%v err=%v", outcome.result, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("await_review did not return after the peer check")
	}
	select {
	case outcome := <-doneC:
		t.Fatalf("await_review c returned on peer-b's change: %+v", outcome)
	default:
	}
	peerNoFrame(t, framesB, "stream b after peer-b's review round")

	// A failed check reads again at the next tick and still finds the change
	// committed before it.
	mode.mu.Lock()
	mode.failNext = true
	mode.mu.Unlock()
	if _, err := logqueue.Enqueue(system("peer-c"), base.Log(), "peer-c", "peer_probe", "two", struct{}{}, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if outcome := tick("failed check"); outcome.err == nil || outcome.published != 0 {
		t.Fatalf("failed check: %+v", outcome)
	}
	select {
	case ws := <-ran:
		t.Fatalf("the job in %s ran after a failed check", ws)
	case <-time.After(50 * time.Millisecond):
	}
	if outcome := tick("retried check"); outcome.err != nil || outcome.published < 1 {
		t.Fatalf("retried check: %+v", outcome)
	}
	select {
	case ws := <-ran:
		if ws != "peer-c" {
			t.Fatalf("job ran in %s", ws)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not run after the retried check")
	}

	// Opened between checks: a change committed before a stream or
	// await_review subscribed is in its own first read, with no check.
	settle("peer-c", lateAwait)
	select {
	case outcome := <-startAwait("peer-c", lateOrder, "session-late"):
		if outcome.err != nil || outcome.result["verdict"] != "approved" {
			t.Fatalf("late await_review result=%v err=%v", outcome.result, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an await_review opened after the round settled did not return from its first read")
	}
	if err := base.AppendEvent(system("peer-c"), core.Event{TaskID: lateStream, Kind: "peer.late"}); err != nil {
		t.Fatal(err)
	}
	legacy := peerSSE(t, streamCtx, httpServer.URL+"/v1/tasks/"+lateStream+"/events/stream?workspace_id=peer-c")
	sawLate := false
	for !sawLate {
		frame := peerFrame(t, legacy, "late legacy replay")
		sawLate = strings.Contains(frame, `"kind":"peer.late"`)
	}
	select {
	case outcome := <-doneC:
		t.Fatalf("await_review c returned without its round settling: %+v", outcome)
	default:
	}
	stopDaemon()
}
