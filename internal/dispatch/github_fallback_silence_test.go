package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Silence fallback tests (component-runtime, Silence fallback;
// req-quiet-idle-runtime AC-3.8). Time comes from the fixture's injected
// clock and GitHub from its fake forge; no test waits real minutes.

const (
	silenceStartedLine = `level=warn msg="GitHub webhook deliveries silent; timed GitHub checks started" workspace=test`
	silenceStoppedLine = `level=info msg="GitHub webhook delivery accepted; timed GitHub checks stopped" workspace=test`
)

// deliveredAt sets the workspace's stored last accepted delivery time.
func (f *fallbackFixture) deliveredAt(at time.Time) {
	f.delivery.Store(&at)
}

// approveTask creates a task in state approved, created at created, with
// its task.state_changed event to approved recorded at approved.
func (f *fallbackFixture) approveTask(id string, mergeApproval bool, created, approved time.Time) {
	f.t.Helper()
	task := core.Task{ID: id, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskApproved, MergeApproval: mergeApproval, CreatedAt: created}
	if err := f.st.CreateTask(f.ctx, task); err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.AppendEvent(f.ctx, core.Event{TaskID: id, Kind: "task.state_changed", At: approved, Payload: core.JSONPayload(map[string]any{"from": core.TaskRunning, "to": core.TaskApproved})}); err != nil {
		f.t.Fatal(err)
	}
}

// restart stops the fallback and starts a fresh one over the same store,
// clock, and forge, as a daemon restart does, and returns its start step.
func (f *fallbackFixture) restart() fallbackStep {
	f.t.Helper()
	f.cancel()
	f.fallback.Wait()
	ctx, cancel := context.WithCancel(store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test"))
	f.t.Cleanup(cancel)
	f.ctx, f.cancel = ctx, cancel
	old := f.fallback
	f.fallback = &GitHubFallback{
		Dispatcher: old.Dispatcher, Hub: old.Hub, Workspaces: old.Workspaces, Webhooks: old.Webhooks,
		Monitor: old.Monitor, PollMonitor: old.PollMonitor, Now: old.Now, Timer: old.Timer, Logf: old.Logf, Stepped: old.Stepped,
	}
	for len(f.steps) > 0 {
		<-f.steps
	}
	return f.run()
}

// silent drives a webhook workspace into a running silence fallback: the
// last delivery is an hour before start, and a gate-off task created two
// hours before start is approved at start.
func silentFixture(t *testing.T) *fallbackFixture {
	t.Helper()
	f := newFallbackFixture(t)
	f.deliveredAt(f.start.Add(-time.Hour))
	f.approveTask("approved", false, f.start.Add(-2*time.Hour), f.start)
	s := f.run()
	if !s.armed || !s.due.Equal(f.start.Add(GitHubSilenceWindow)) || f.fallback.SilenceActive("test") {
		t.Fatalf("start step=%+v active=%t, want armed for the approval plus 15 minutes and not active", s, f.fallback.SilenceActive("test"))
	}
	if _, fired := f.advance(GitHubSilenceWindow - time.Minute); fired || f.reads.Load() != 0 {
		t.Fatalf("fired or read before 15 minutes: reads=%d", f.reads.Load())
	}
	s, fired := f.advance(time.Minute)
	if !fired || !s.armed || !f.fallback.SilenceActive("test") || f.reads.Load() == 0 {
		t.Fatalf("15-minute fire=%+v fired=%t active=%t reads=%d, want a timed read and an active silence fallback", s, fired, f.fallback.SilenceActive("test"), f.reads.Load())
	}
	if lines := f.errorLines(silenceStartedLine); len(lines) != 1 {
		t.Fatalf("start lines=%v, want one warning", f.lines)
	}
	return f
}

// TestGitHubFallbackSilenceStartsAndLocalDeliveryStopsIt: a gate-off
// approved task that waited 15 minutes with no delivery since its approval
// starts timed reads at most once a minute; a local delivery stops them at
// once with one info line.
func TestGitHubFallbackSilenceStartsAndLocalDeliveryStopsIt(t *testing.T) {
	f := silentFixture(t)
	reads := f.reads.Load()
	if _, fired := f.advance(30 * time.Second); fired {
		t.Fatal("fired twice within a minute")
	}
	if s, fired := f.advance(30 * time.Second); !fired || !s.armed || f.reads.Load() <= reads {
		t.Fatalf("second fire=%+v fired=%t reads=%d", s, fired, f.reads.Load())
	}
	if lines := f.errorLines(silenceStartedLine); len(lines) != 1 {
		t.Fatalf("start lines=%v, want one warning", f.lines)
	}
	f.fallback.WebhookDelivered("test")
	if s := f.await("delivered"); s.armed || f.fallback.SilenceActive("test") {
		t.Fatalf("delivered step=%+v active=%t, want stopped", s, f.fallback.SilenceActive("test"))
	}
	if lines := f.errorLines(silenceStoppedLine); len(lines) != 1 {
		t.Fatalf("stop lines=%v, want one info line", f.lines)
	}
	reads = f.reads.Load()
	for range 60 {
		if _, fired := f.advance(time.Minute); fired {
			t.Fatal("timer fired after a delivery")
		}
	}
	if f.reads.Load() != reads {
		t.Fatalf("reads after delivery=%d, want %d", f.reads.Load(), reads)
	}
}

// TestGitHubFallbackSilencePeerDeliveryStopsIt: a delivery accepted by a
// peer daemon stops the silence fallback at the next fire with no GitHub
// call and one info line.
func TestGitHubFallbackSilencePeerDeliveryStopsIt(t *testing.T) {
	f := silentFixture(t)
	reads := f.reads.Load()
	f.deliveredAt(f.clock.Now())
	if s, fired := f.advance(time.Minute); !fired || s.armed || f.fallback.SilenceActive("test") || f.reads.Load() != reads {
		t.Fatalf("peer delivery fire=%+v fired=%t active=%t reads=%d, want stopped without a read", s, fired, f.fallback.SilenceActive("test"), f.reads.Load())
	}
	if lines := f.errorLines(silenceStoppedLine); len(lines) != 1 {
		t.Fatalf("stop lines=%v, want one info line", f.lines)
	}
	if _, fired := f.advance(time.Hour); fired {
		t.Fatal("timer fired after a peer delivery")
	}
}

// TestGitHubFallbackSilenceStopsWhenNoWatchedTaskRemains: a fire that finds
// no watched task stops the silence fallback before any GitHub call and
// writes no line.
func TestGitHubFallbackSilenceStopsWhenNoWatchedTaskRemains(t *testing.T) {
	f := silentFixture(t)
	reads := f.reads.Load()
	f.closeTask("approved")
	f.awaitTask(true)
	if s, fired := f.advance(time.Minute); !fired || s.armed || f.fallback.SilenceActive("test") || f.reads.Load() != reads {
		t.Fatalf("fire without a watched task=%+v fired=%t active=%t reads=%d, want stopped without a read", s, fired, f.fallback.SilenceActive("test"), f.reads.Load())
	}
	if lines := f.errorLines(silenceStoppedLine); len(lines) != 0 {
		t.Fatalf("stop lines=%v, want none", lines)
	}
	if _, fired := f.advance(time.Hour); fired {
		t.Fatal("timer fired with no watched task")
	}
}

// TestGitHubFallbackSilenceSurvivesRestart: a fresh fallback over the same
// store recomputes the due silence from stored times and starts it at once;
// its first timed read follows the start-up read by one minute.
func TestGitHubFallbackSilenceSurvivesRestart(t *testing.T) {
	f := silentFixture(t)
	reads := f.reads.Load()
	s := f.restart()
	now := f.clock.Now()
	if !s.armed || !s.due.Equal(now.Add(time.Minute)) || !f.fallback.SilenceActive("test") {
		t.Fatalf("restart step=%+v active=%t, want active and armed one minute out", s, f.fallback.SilenceActive("test"))
	}
	if lines := f.errorLines(silenceStartedLine); len(lines) != 2 {
		t.Fatalf("start lines=%v, want one warning per start", f.lines)
	}
	if s, fired := f.advance(time.Minute); !fired || !s.armed || f.reads.Load() <= reads {
		t.Fatalf("first fire after restart=%+v fired=%t reads=%d", s, fired, f.reads.Load())
	}
}

// TestGitHubFallbackSilenceNeedsSilenceSinceApproval: a webhook workspace
// whose last delivery arrived after the approval, and one with no watched
// task, arms no timer and makes no timed call.
func TestGitHubFallbackSilenceNeedsSilenceSinceApproval(t *testing.T) {
	f := newFallbackFixture(t)
	f.deliveredAt(f.start.Add(-5 * time.Minute))
	f.approveTask("approved", false, f.start.Add(-time.Hour), f.start.Add(-10*time.Minute))
	if s := f.run(); s.armed {
		t.Fatalf("start step=%+v, want no timer when a delivery followed the approval", s)
	}
	if _, fired := f.advance(time.Hour); fired || f.reads.Load() != 0 || f.fallback.SilenceActive("test") {
		t.Fatalf("fired or read: reads=%d", f.reads.Load())
	}

	g := newFallbackFixture(t)
	g.deliveredAt(g.start.Add(-time.Hour))
	if s := g.run(); s.armed {
		t.Fatalf("start step=%+v, want no timer without a watched task", s)
	}
	if _, fired := g.advance(time.Hour); fired || g.reads.Load() != 0 {
		t.Fatalf("fired or read without a watched task: reads=%d", g.reads.Load())
	}
}

// TestGitHubFallbackSilenceIgnoresUnwatchedTasks: a parked task with a
// recorded pull request, an awaiting_human task, a merge-gated approved
// task, and queued and running tasks wait an hour with no delivery and arm
// nothing. A gate-off approval noticed afterwards arms the timer for the
// approval plus 15 minutes.
func TestGitHubFallbackSilenceIgnoresUnwatchedTasks(t *testing.T) {
	f := newFallbackFixture(t)
	f.deliveredAt(f.start.Add(-2 * time.Hour))
	f.createTask("parked-with-pr", core.TaskParked)
	openPullRequest(t, f.st, f.ctx, "parked-with-pr", 51, "parked-head")
	f.createTask("awaiting", core.TaskAwaiting)
	f.createTask("queued", core.TaskQueued)
	f.createTask("running", core.TaskRunning)
	f.approveTask("merge-gated", true, f.start.Add(-time.Hour), f.start.Add(-time.Hour))
	if s := f.run(); s.armed {
		t.Fatalf("start step=%+v, want no timer for unwatched tasks", s)
	}
	for range 60 {
		if _, fired := f.advance(time.Minute); fired {
			t.Fatal("timer fired for unwatched tasks")
		}
	}
	if f.reads.Load() != 0 || f.fallback.SilenceActive("test") {
		t.Fatalf("reads=%d active=%t, want no timed call", f.reads.Load(), f.fallback.SilenceActive("test"))
	}
	approved := f.clock.Now()
	f.approveTask("gate-off", false, f.start, approved)
	if s := f.awaitTask(true); !s.due.Equal(approved.Add(GitHubSilenceWindow)) {
		t.Fatalf("gate-off approval step=%+v, want due at %s", s, approved.Add(GitHubSilenceWindow))
	}
	if s, fired := f.advance(GitHubSilenceWindow); !fired || !f.fallback.SilenceActive("test") || f.reads.Load() == 0 {
		t.Fatalf("watched fire=%+v fired=%t reads=%d, want the silence fallback running", s, fired, f.reads.Load())
	}
}
