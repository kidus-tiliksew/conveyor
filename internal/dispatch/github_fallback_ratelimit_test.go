package dispatch

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// seedBacklog creates queued tasks without a recorded pull request, as a
// workspace's never-started backlog is. It writes before the fallback starts,
// so it raises no notice the fallback reads.
func (f *fallbackFixture) seedBacklog(n int) {
	f.t.Helper()
	for i := range n {
		id := fmt.Sprintf("backlog-%03d", i)
		if err := f.st.CreateTask(f.ctx, core.Task{ID: id, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fallbackFixture) branchesRead() []string {
	f.readMu.Lock()
	defer f.readMu.Unlock()
	return append([]string(nil), f.readBranches...)
}

// TestGitHubFallbackSkipsBacklogWithoutPullRequests drives the timed
// fallback for a workspace without webhook delivery on the FunnelFlux shape:
// 285 queued tasks without a recorded pull request arm nothing and cost no
// GitHub call. One queued task that was bounced back to implement after it
// opened a pull request arms the timer, and each fire then makes exactly
// one pull-request read, of that task's branch (req-quiet-idle-runtime
// AC-3.5; component-runtime, Timed GitHub fallback).
func TestGitHubFallbackSkipsBacklogWithoutPullRequests(t *testing.T) {
	f := newFallbackFixture(t)
	f.seedBacklog(285)
	if s := f.run(); s.armed {
		t.Fatalf("backlog-only workspace armed: %+v", s)
	}
	if _, fired := f.advance(10 * time.Minute); fired || f.reads.Load() != 0 {
		t.Fatalf("backlog-only workspace fired=%t reads=%d", fired, f.reads.Load())
	}
	// A backlog task created while the timer is stopped reads one task and
	// leaves the timer stopped.
	f.createTask("backlog-late", core.TaskQueued)
	if s := f.awaitTask(false); s.armed {
		t.Fatalf("backlog notice armed: %+v", s)
	}

	f.createWaitingTask("bounced", core.TaskQueued)
	if s := f.awaitTask(true); !s.armed {
		t.Fatalf("bounced task with a pull request did not arm: %+v", s)
	}
	for fire := 1; fire <= 3; fire++ {
		if s, fired := f.advance(time.Minute); !fired || !s.armed {
			t.Fatalf("fire %d=%+v fired=%t", fire, s, fired)
		}
		if got := f.reads.Load(); got != int32(fire) {
			t.Fatalf("after fire %d reads=%d, want exactly one pull-request read per fire", fire, got)
		}
	}
	reads := f.branchesRead()
	if len(reads) != 3 {
		t.Fatalf("reads=%v, want three reads", reads)
	}
	for _, branch := range reads {
		if branch != "conveyor/bounced" {
			t.Fatalf("read branch %s; reads=%v", branch, reads)
		}
	}
}

// TestGitHubFallbackPausesOnRateLimit: a rate-limited pull-request read ends
// the fire's GitHub reads, writes one error line naming the workspace and
// GitHub's reset time, and arms nothing before that time, even for a notice
// that makes another task wait. Reads resume at the reset, at most once a
// minute (component-runtime, Timed GitHub fallback; req-quiet-idle-runtime
// AC-3.5).
func TestGitHubFallbackPausesOnRateLimit(t *testing.T) {
	f := newFallbackFixture(t)
	for _, id := range []string{"first", "second", "third"} {
		f.createWaitingTask(id, core.TaskQueued)
	}
	if s := f.run(); !s.armed {
		t.Fatalf("start step=%+v", s)
	}
	reset := f.start.Add(21 * time.Minute)
	f.limited.Store(&reset)
	s, fired := f.advance(time.Minute)
	if !fired || !s.armed || !s.due.Equal(reset) {
		t.Fatalf("rate-limited fire=%+v fired=%t, want armed for the reset %s", s, fired, reset)
	}
	if got := f.reads.Load(); got != 1 {
		t.Fatalf("reads=%d, want the fire to stop after the first rate-limited read", got)
	}
	paused := f.errorLines(`level=error msg="timed GitHub checks paused: GitHub rate limit exhausted" workspace=test reset_at=` + reset.Format(time.RFC3339))
	if len(paused) != 1 || !strings.Contains(paused[0], "rate limit") {
		t.Fatalf("pause lines=%v all=%v", paused, f.lines)
	}
	if all := f.errorLines("level=error"); len(all) != 1 {
		t.Fatalf("error lines=%v, want only the pause line", all)
	}

	f.createWaitingTask("fourth", core.TaskQueued)
	if s := f.awaitTask(true); !s.due.Equal(reset) {
		t.Fatalf("notice during the pause step=%+v, want due at the reset %s", s, reset)
	}
	if _, fired := f.advance(19 * time.Minute); fired {
		t.Fatal("fired before GitHub's reset time")
	}
	f.limited.Store(nil)
	if s, fired := f.advance(time.Minute); !fired || !s.armed || f.reads.Load() != 5 {
		t.Fatalf("fire at the reset=%+v fired=%t reads=%d, want every waiting task read", s, fired, f.reads.Load())
	}
	if _, fired := f.advance(59 * time.Second); fired {
		t.Fatal("fired within a minute of the resumed read")
	}
	if _, fired := f.advance(time.Second); !fired || f.reads.Load() != 9 {
		t.Fatalf("next fire reads=%d", f.reads.Load())
	}
	if lines := f.errorLines("timed GitHub checks paused"); len(lines) != 1 {
		t.Fatalf("pause lines after the reset=%v", lines)
	}
}

// TestGitHubFallbackMergeReadinessStopsOnRateLimit: with several
// merge-readiness-eligible tasks (gate-on approved and merge-gated, none with
// a recorded pull request), the fire's ReconcileMergeReadiness sweep stops at
// its first rate-limited GitHub read. No later task and no pull-request read
// follows; one error line names the workspace and reset time; nothing is read
// before the reset; and the sweep and the pull-request read resume at the
// reset (component-runtime, Timed GitHub fallback).
func TestGitHubFallbackMergeReadinessStopsOnRateLimit(t *testing.T) {
	f := newFallbackFixture(t)
	eligible := []core.Task{
		{ID: "approved-a", State: core.TaskApproved},
		{ID: "approved-b", State: core.TaskApproved},
		{ID: "merge-gate-a", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval},
		{ID: "merge-gate-b", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval},
	}
	for _, task := range eligible {
		task.Workspace, task.Repo, task.BaseBranch, task.Branch = "test", "app", "main", "conveyor/"+task.ID
		task.MergeApproval, task.NextStage, task.CreatedAt = true, core.StageImplement, time.Now().UTC()
		if err := f.st.CreateTask(f.ctx, task); err != nil {
			t.Fatal(err)
		}
		if !waitsOnGitHub(task, false) {
			t.Fatalf("fixture task %s does not wait on GitHub", task.ID)
		}
	}
	if s := f.run(); !s.armed {
		t.Fatalf("start step=%+v", s)
	}
	reset := f.start.Add(31 * time.Minute)
	f.limited.Store(&reset)
	if s, fired := f.advance(time.Minute); !fired || !s.armed || !s.due.Equal(reset) {
		t.Fatalf("rate-limited fire=%+v fired=%t, want armed for the reset %s", s, fired, reset)
	}
	if got := f.branchesRead(); len(got) != 1 {
		t.Fatalf("reads=%v, want only the first merge-readiness read", got)
	}
	if lines := f.errorLines("level=error"); len(lines) != 1 || !strings.Contains(lines[0], `msg="timed GitHub checks paused: GitHub rate limit exhausted" workspace=test reset_at=`+reset.Format(time.RFC3339)) {
		t.Fatalf("error lines=%v, want one pause line naming the workspace and reset", lines)
	}
	if _, fired := f.advance(29 * time.Minute); fired || len(f.branchesRead()) != 1 {
		t.Fatalf("fired=%t reads=%v before GitHub's reset", fired, f.branchesRead())
	}
	f.limited.Store(nil)
	if s, fired := f.advance(time.Minute); !fired || !s.armed {
		t.Fatalf("fire at the reset=%+v fired=%t", s, fired)
	}
	// The resumed fire reads each eligible task in the sweep and again in the
	// pull-request read.
	counts := map[string]int{}
	for _, branch := range f.branchesRead()[1:] {
		counts[branch]++
	}
	if len(counts) != len(eligible) {
		t.Fatalf("resumed reads=%v, want every eligible task", counts)
	}
	for _, task := range eligible {
		if counts["conveyor/"+task.ID] != 2 {
			t.Fatalf("resumed reads=%v, want two reads of %s", counts, task.ID)
		}
	}
	if lines := f.errorLines("timed GitHub checks paused"); len(lines) != 1 {
		t.Fatalf("pause lines=%v", lines)
	}
}

// TestGitHubFallbackRateLimitWithoutResetUsesDefaultPause: a rate-limited
// answer that names no reset time pauses for GitHubRateLimitPause. A reset
// that GitHub names more than one hour away (Retry-After: 7200) is honored
// as supplied: the line names it, nothing is read before it, and reads
// resume at it.
func TestGitHubFallbackRateLimitWithoutResetUsesDefaultPause(t *testing.T) {
	f := newFallbackFixture(t)
	f.createWaitingTask("waiting", core.TaskRunning)
	if s := f.run(); !s.armed {
		t.Fatalf("start step=%+v", s)
	}
	f.limited.Store(&time.Time{})
	fire := f.start.Add(time.Minute)
	if s, fired := f.advance(time.Minute); !fired || !s.due.Equal(fire.Add(GitHubRateLimitPause)) {
		t.Fatalf("fire=%+v fired=%t, want due %s", s, fired, fire.Add(GitHubRateLimitPause))
	}
	if lines := f.errorLines("reset_at=" + fire.Add(GitHubRateLimitPause).Format(time.RFC3339)); len(lines) != 1 {
		t.Fatalf("lines=%v", f.lines)
	}

	second := fire.Add(GitHubRateLimitPause)
	far := second.Add(7200 * time.Second)
	f.limited.Store(&far)
	if s, fired := f.advance(GitHubRateLimitPause); !fired || !s.armed || !s.due.Equal(far) {
		t.Fatalf("long-reset fire=%+v fired=%t, want due at the supplied reset %s", s, fired, far)
	}
	reads := f.reads.Load()
	if lines := f.errorLines("reset_at=" + far.Format(time.RFC3339)); len(lines) != 1 {
		t.Fatalf("lines=%v, want the supplied reset logged", f.lines)
	}
	if _, fired := f.advance(time.Hour); fired {
		t.Fatal("fired one hour into a two-hour reset")
	}
	if _, fired := f.advance(time.Hour - time.Second); fired || f.reads.Load() != reads {
		t.Fatalf("fired=%t reads=%d before the supplied reset", fired, f.reads.Load())
	}
	f.limited.Store(nil)
	if s, fired := f.advance(time.Second); !fired || !s.armed || f.reads.Load() != reads+1 {
		t.Fatalf("fire at the supplied reset=%+v fired=%t reads=%d", s, fired, f.reads.Load())
	}
	if lines := f.errorLines("timed GitHub checks paused"); len(lines) != 2 {
		t.Fatalf("pause lines=%v", lines)
	}
}

// TestWaitsOnGitHub covers the one predicate the start-up pull_requests pass,
// the fallback's per-fire read, and its arming use.
func TestWaitsOnGitHub(t *testing.T) {
	for _, tc := range []struct {
		name     string
		task     core.Task
		recorded bool
		want     bool
	}{
		{"queued backlog", core.Task{State: core.TaskQueued}, false, false},
		{"running without pull request", core.Task{State: core.TaskRunning}, false, false},
		{"claiming without pull request", core.Task{State: core.TaskClaiming}, false, false},
		{"parked without pull request", core.Task{State: core.TaskParked}, false, false},
		{"queued bounced with pull request", core.Task{State: core.TaskQueued}, true, true},
		{"parked with pull request", core.Task{State: core.TaskParked}, true, true},
		{"closed with pull request", core.Task{State: core.TaskClosed}, true, false},
		{"merged with pull request", core.Task{State: core.TaskMerged}, true, false},
		{"gate-on approved", core.Task{State: core.TaskApproved, MergeApproval: true}, false, true},
		{"gate-on merge gate", core.Task{State: core.TaskAwaiting, MergeApproval: true, GateKind: core.GateKindMergeApproval}, false, true},
		{"gate-on plan gate", core.Task{State: core.TaskAwaiting, MergeApproval: true, GateKind: core.GateKindPlanApproval}, false, false},
		{"gate-off approved", core.Task{State: core.TaskApproved}, false, true},
		{"gate-off queued with reviewed head", core.Task{State: core.TaskQueued, ReviewedHeadSHA: "head"}, false, true},
		{"gate-off running with approved head", core.Task{State: core.TaskRunning, ApprovedHeadSHA: "head"}, false, true},
		{"gate-off awaiting with reviewed head", core.Task{State: core.TaskAwaiting, ReviewedHeadSHA: "head"}, false, false},
	} {
		if got := waitsOnGitHub(tc.task, tc.recorded); got != tc.want {
			t.Errorf("%s: waitsOnGitHub=%t, want %t", tc.name, got, tc.want)
		}
	}
}
