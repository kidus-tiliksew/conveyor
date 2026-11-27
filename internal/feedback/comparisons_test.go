package feedback

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// workFixture runs the background work against the memory backend with a
// stubbed forge and an injected clock.
type workFixture struct {
	t           *testing.T
	st          store.Backend
	ctx         context.Context
	forge       *stubForge
	environment store.Environment
	key         store.IssuedEnvironmentKey
	mu          sync.Mutex
	now         time.Time
	logs        []string
}

func newWorkFixture(t *testing.T) *workFixture {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	workspace := "feedback-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor())
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{
		{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main", GitHub: "acme/conveyor"},
		{Name: "app", URL: "https://example.test/app", Base: "main", GitHub: "acme/app"},
	}}); err != nil {
		t.Fatal(err)
	}
	record, err := st.CreateEnvironment(ctx, store.EnvironmentInput{Name: "Production"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := st.IssueEnvironmentKey(ctx, record.ID, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	return &workFixture{t: t, st: st, ctx: ctx, forge: newStubForge(), environment: record.Environment, key: key, now: statusBase}
}

func (f *workFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *workFixture) advance(by time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(by)
}

func (f *workFixture) work() Work {
	return Work{Store: f.st, Forge: f.forge, Now: f.clock, Logf: func(format string, args ...any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.logs = append(f.logs, fmt.Sprintf(format, args...))
	}}
}

// signal records an accepted signal for the api service at the clock and
// runs the background work for it.
func (f *workFixture) signal(versions ...store.FeedbackSignalVersion) {
	f.t.Helper()
	if err := f.process(versions...); err != nil {
		f.t.Fatal(err)
	}
}

// process records an accepted signal like signal and returns the background
// work's error.
func (f *workFixture) process(versions ...store.FeedbackSignalVersion) error {
	f.t.Helper()
	signal := store.FeedbackSignal{EnvironmentID: f.environment.ID, KeyID: f.key.ID, ArrivedAt: f.clock(), NextSignalSeconds: 60, Services: []store.FeedbackSignalService{{Name: "api", Repository: "conveyor", Versions: versions}}}
	if err := f.st.RecordFeedbackSignal(f.ctx, signal); err != nil {
		f.t.Fatal(err)
	}
	signal.ArrivedAt = store.EnvironmentTime(signal.ArrivedAt)
	request := SignalRequest{Signal: signal, Environment: f.environment, Repositories: []Repository{{Name: "conveyor", Slug: "acme/conveyor"}, {Name: "app", Slug: "acme/app"}}}
	return f.work().ProcessSignal(f.ctx, request)
}

func commitVersion(commit string) store.FeedbackSignalVersion {
	return store.FeedbackSignalVersion{Commit: commit[:12]}
}

// merged creates a merged task of the conveyor repository whose merge
// commit is merge.
func (f *workFixture) merged(merge string) string {
	f.t.Helper()
	id := core.NewTaskID()
	if err := f.st.CreateTask(f.ctx, core.Task{ID: id, Workspace: workspaceKeyForTest(f.ctx), Repo: "conveyor", Title: id, State: core.TaskMerged, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.AppendEvent(f.ctx, core.Event{TaskID: id, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"merge_commit_sha": merge})}); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *workFixture) comparison(base, head string) (store.FeedbackComparison, bool) {
	f.t.Helper()
	got, err := f.st.ListFeedbackComparisons(f.ctx, []store.FeedbackComparisonKey{{Repository: "acme/conveyor", Base: base, Head: head}})
	if err != nil {
		f.t.Fatal(err)
	}
	if len(got) == 0 {
		return store.FeedbackComparison{}, false
	}
	return got[0], true
}

func (f *workFixture) resolution(commit string) store.FeedbackResolution {
	f.t.Helper()
	inputs, err := f.st.ReadFeedbackInputs(f.ctx, []string{f.environment.ID})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, resolution := range inputs.Resolutions {
		if resolution.Commit == commit[:12] {
			return resolution
		}
	}
	f.t.Fatalf("no resolution for %s in %+v", commit, inputs.Resolutions)
	return store.FeedbackResolution{}
}

// line returns n commits named after a prefix.
func line(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = sha(fmt.Sprintf("%s%04x", prefix, i))
	}
	return out
}

func init() {
	// The fixture's workspace is read from the context the store reads.
	workspaceKeyForTest = func(ctx context.Context) string { workspace, _ := store.WorkspaceFromContext(ctx); return workspace }
}

var workspaceKeyForTest func(context.Context) string

// TestProcessSignalResolvesAfterTheRule covers invariant 8 and
// req-production-feedback AC-1.4: a forge failure leaves the version
// unresolved, a later signal tries again only 10 minutes after the attempt,
// and a resolved version is never resolved again.
func TestProcessSignalResolvesAfterTheRule(t *testing.T) {
	f := newWorkFixture(t)
	commit := sha("abc123456789")
	f.forge.fail["acme/conveyor|"+commit[:12]] = &github.Error{Category: github.ForgeRequest, Err: errors.New("transport")}
	f.signal(commitVersion(commit))
	if got := f.resolution(commit); got.Resolved() || got.Reason != string(github.ForgeRequest) || !got.NextAttemptAt.Equal(statusBase.Add(10*time.Minute)) {
		t.Fatalf("resolution=%+v", got)
	}
	delete(f.forge.fail, "acme/conveyor|"+commit[:12])
	f.forge.chain("acme/conveyor", "", commit)
	f.advance(9 * time.Minute)
	f.signal(commitVersion(commit))
	if reads, _ := f.forge.calls(); reads != 1 {
		t.Fatalf("a signal inside 10 minutes read GitHub: reads=%d", reads)
	}
	f.advance(time.Minute)
	f.signal(commitVersion(commit))
	if got := f.resolution(commit); !got.Resolved() || got.ResolvedCommit != commit || got.ResolvedRepository != "conveyor" {
		t.Fatalf("resolution=%+v", got)
	}
	f.advance(time.Hour)
	f.signal(commitVersion(commit))
	if reads, _ := f.forge.calls(); reads != 2 {
		t.Fatalf("a resolved version was read again: reads=%d", reads)
	}
}

// TestProcessSignalWithoutApp covers State and failure: with no connected
// GitHub App a version stays unresolved with that reason and no forge call
// runs.
func TestProcessSignalWithoutApp(t *testing.T) {
	f := newWorkFixture(t)
	commit := sha("abc123456789")
	signal := store.FeedbackSignal{EnvironmentID: f.environment.ID, KeyID: f.key.ID, ArrivedAt: statusBase, NextSignalSeconds: 60, Services: []store.FeedbackSignalService{{Name: "api", Versions: []store.FeedbackSignalVersion{commitVersion(commit)}}}}
	if err := f.st.RecordFeedbackSignal(f.ctx, signal); err != nil {
		t.Fatal(err)
	}
	if err := f.work().ProcessSignal(f.ctx, SignalRequest{Signal: signal, Environment: f.environment, NoApp: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.resolution(commit); got.Reason != store.FeedbackReasonNoGitHubApp {
		t.Fatalf("resolution=%+v", got)
	}
	if reads, compares := f.forge.calls(); reads != 0 || compares != 0 {
		t.Fatalf("forge calls reads=%d compares=%d", reads, compares)
	}
}

// TestFirstVersionComparesNewestFirst covers FB-3: a first version is
// compared with each merged task directly, newest merge first, at most 60 a
// minute for the workspace, and a stored answer is never asked again.
func TestFirstVersionComparesNewestFirst(t *testing.T) {
	f := newWorkFixture(t)
	merges := line("aa", 70)
	f.forge.chain("acme/conveyor", "", merges...)
	head := sha("ff00ff00ff00")
	f.forge.chain("acme/conveyor", merges[69], head)
	for _, merge := range merges {
		f.merged(merge)
	}
	f.signal(commitVersion(head))
	_, compares := f.forge.calls()
	if compares != 60 {
		t.Fatalf("compares=%d, want the 60-a-minute bound", compares)
	}
	// The newest merges were asked first.
	if !strings.Contains(f.forge.compare[0], merges[69]) {
		t.Fatalf("first compare=%s", f.forge.compare[0])
	}
	if _, ok := f.comparison(merges[0], head); ok {
		t.Fatal("the oldest merge was compared inside the bound")
	}
	// Inside the minute the rest waits; after it a later signal finishes.
	f.advance(30 * time.Second)
	f.signal(commitVersion(head))
	if _, compares := f.forge.calls(); compares != 60 {
		t.Fatalf("compares inside the minute=%d", compares)
	}
	f.advance(31 * time.Second)
	f.signal(commitVersion(head))
	if _, compares := f.forge.calls(); compares != 70 {
		t.Fatalf("compares after the minute=%d", compares)
	}
	for _, merge := range merges {
		if got, ok := f.comparison(merge, head); !ok || got.Status != store.FeedbackCompareAhead {
			t.Fatalf("comparison of %s=%+v ok=%v", merge, got, ok)
		}
	}
	f.advance(time.Hour)
	f.signal(commitVersion(head))
	if _, compares := f.forge.calls(); compares != 70 {
		t.Fatalf("a stored answer was asked again: compares=%d", compares)
	}
}

// TestForwardDeployListsArrivedCommits covers FB-3: when a version descends
// from the previous one, one comparison lists the commits that arrived, and
// merged tasks among them, or held by the previous version, are contained
// without a direct comparison.
func TestForwardDeployListsArrivedCommits(t *testing.T) {
	f := newWorkFixture(t)
	history := line("bb", 6)
	f.forge.chain("acme/conveyor", "", history...)
	inherited, previous, arrived, current := history[1], history[2], history[4], history[5]
	f.merged(inherited)
	f.merged(previous)
	f.signal(commitVersion(previous))
	_, before := f.forge.calls()
	if before != 1 {
		t.Fatalf("first version compares=%d", before)
	}
	f.merged(arrived)
	f.advance(time.Minute)
	f.signal(commitVersion(current))
	_, after := f.forge.calls()
	if after-before != 1 || f.forge.compare[before] != "acme/conveyor|"+previous+"|"+current {
		t.Fatalf("forward compares=%v", f.forge.compare[before:])
	}
	for _, merge := range []string{inherited, previous, arrived} {
		if got, ok := f.comparison(merge, current); !ok || !got.Contains() {
			t.Fatalf("comparison of %s=%+v ok=%v", merge, got, ok)
		}
	}
	if got, ok := f.comparison(previous, current); !ok || got.Status != store.FeedbackCompareAhead {
		t.Fatalf("descent=%+v ok=%v", got, ok)
	}
}

// TestRateLimitedDescentCheckStopsTheJob covers FB-3 plan step 4: when GitHub
// rate-limits the descent check of a forward deploy, the job keeps the
// answers that needed no request, starts no direct comparison, and leaves
// every unproven pair pending for a later signal.
func TestRateLimitedDescentCheckStopsTheJob(t *testing.T) {
	f := newWorkFixture(t)
	history := line("ea", 6)
	f.forge.chain("acme/conveyor", "", history...)
	inherited, previous, arrived, current := history[1], history[2], history[4], history[5]
	f.merged(inherited)
	f.merged(previous)
	f.signal(commitVersion(previous))
	f.merged(arrived)
	f.merged(current)
	descent := "compare|acme/conveyor|" + previous + "|" + current
	f.forge.fail[descent] = github.RateLimitedError(statusBase.Add(time.Hour), errors.New("rate limited"))
	f.advance(time.Minute)
	_, before := f.forge.calls()
	if err := f.process(commitVersion(current)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err=%v, want ErrRateLimited", err)
	}
	if _, after := f.forge.calls(); after-before != 1 || f.forge.compare[before] != strings.TrimPrefix(descent, "compare|") {
		t.Fatalf("compares after the rate-limited descent check=%v", f.forge.compare[before:])
	}
	// The merge commit equal to the head needed no request and is kept.
	if got, ok := f.comparison(current, current); !ok || got.Status != store.FeedbackCompareIdentical {
		t.Fatalf("identical=%+v ok=%v", got, ok)
	}
	for _, merge := range []string{inherited, previous, arrived} {
		if got, ok := f.comparison(merge, current); ok {
			t.Fatalf("comparison of %s was stored as %+v", merge, got)
		}
	}
	// A later signal compares the pairs left pending.
	delete(f.forge.fail, descent)
	f.advance(store.FeedbackAdmissionLease + time.Second)
	f.signal(commitVersion(current))
	for _, merge := range []string{inherited, previous, arrived} {
		if got, ok := f.comparison(merge, current); !ok || !got.Contains() {
			t.Fatalf("comparison of %s after the limit=%+v ok=%v", merge, got, ok)
		}
	}
}

// TestRateLimitedComparisonStopsTheSignal covers FB-3 plan step 4: when GitHub
// rate-limits a direct comparison of the first version a signal names, the
// job compares no other version, and both versions stay pending for a later
// signal.
func TestRateLimitedComparisonStopsTheSignal(t *testing.T) {
	f := newWorkFixture(t)
	history := line("eb", 4)
	f.forge.chain("acme/conveyor", "", history...)
	merge, first, second := history[1], history[2], history[3]
	f.merged(merge)
	limited := "compare|acme/conveyor|" + merge + "|" + first
	f.forge.fail[limited] = github.RateLimitedError(statusBase.Add(time.Hour), errors.New("rate limited"))
	if err := f.process(commitVersion(first), commitVersion(second)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err=%v, want ErrRateLimited", err)
	}
	if _, compares := f.forge.calls(); compares != 1 || f.forge.compare[0] != strings.TrimPrefix(limited, "compare|") {
		t.Fatalf("compares=%v, want only the rate-limited one", f.forge.compare)
	}
	for _, head := range []string{first, second} {
		if got, ok := f.comparison(merge, head); ok {
			t.Fatalf("comparison with %s was stored as %+v", head, got)
		}
	}
	// A later signal compares both versions.
	delete(f.forge.fail, limited)
	f.advance(store.FeedbackAdmissionLease + time.Second)
	f.signal(commitVersion(first), commitVersion(second))
	for _, head := range []string{first, second} {
		if got, ok := f.comparison(merge, head); !ok || !got.Contains() {
			t.Fatalf("comparison with %s after the limit=%+v ok=%v", head, got, ok)
		}
	}
}

// TestRefreshBranchesStopsOnARateLimit proves that a rate-limited branch read
// stops the pass and returns ErrRateLimited, and that the branches not read
// stay eligible for a later read.
func TestRefreshBranchesStopsOnARateLimit(t *testing.T) {
	f := newWorkFixture(t)
	head := sha("ec01")
	f.forge.refs["acme/conveyor|heads/main"] = head
	f.forge.refs["acme/conveyor|heads/release"] = head
	f.forge.fail["acme/conveyor|heads/main"] = github.RateLimitedError(statusBase.Add(time.Hour), errors.New("rate limited"))
	branches := []BranchRef{{Slug: "acme/conveyor", Branch: "main"}, {Slug: "acme/conveyor", Branch: "release"}}
	if err := f.work().RefreshBranches(f.ctx, branches); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err=%v, want ErrRateLimited", err)
	}
	if reads, _ := f.forge.calls(); reads != 1 {
		t.Fatalf("reads=%d, want the pass to stop after the rate-limited read", reads)
	}
	if heads, _ := f.st.ListFeedbackBranchHeads(f.ctx); len(heads) != 0 {
		t.Fatalf("heads=%+v", heads)
	}
	delete(f.forge.fail, "acme/conveyor|heads/main")
	f.advance(store.FeedbackAdmissionLease + time.Second)
	if err := f.work().RefreshBranches(f.ctx, branches); err != nil {
		t.Fatal(err)
	}
	if heads, _ := f.st.ListFeedbackBranchHeads(f.ctx); len(heads) != 2 {
		t.Fatalf("heads after the limit=%+v", heads)
	}
}

// TestRollbackComparesDirectly covers FB-3: a version that does not descend
// from the previous one is compared with each merged task directly.
func TestRollbackComparesDirectly(t *testing.T) {
	f := newWorkFixture(t)
	history := line("cc", 4)
	f.forge.chain("acme/conveyor", "", history...)
	f.merged(history[3])
	f.signal(commitVersion(history[3]))
	f.advance(time.Minute)
	f.signal(commitVersion(history[1]))
	if got, ok := f.comparison(history[3], history[3]); !ok || got.Status != store.FeedbackCompareIdentical {
		t.Fatalf("identical=%+v ok=%v", got, ok)
	}
	if got, ok := f.comparison(history[3], history[1]); !ok || got.Status != store.FeedbackCompareBehind || got.Contains() {
		t.Fatalf("rollback=%+v ok=%v", got, ok)
	}
}

// TestCompareDirectFailures covers every forge failure of a comparison: a
// merge commit GitHub proves missing is stored as missing, a rate-limited
// answer stops the pass, and other failures leave the pair pending.
func TestCompareDirectFailures(t *testing.T) {
	f := newWorkFixture(t)
	head, missing, failing, later := sha("dd01"), sha("dd02"), sha("dd03"), sha("dd04")
	f.forge.chain("acme/conveyor", "", head, failing, later)
	keys := []store.FeedbackComparisonKey{{Repository: "acme/conveyor", Base: missing, Head: head}, {Repository: "acme/conveyor", Base: failing, Head: head}, {Repository: "acme/conveyor", Base: later, Head: head}}
	f.forge.fail["compare|acme/conveyor|"+failing+"|"+head] = github.StatusError(502, errors.New("bad gateway"))
	f.forge.fail["compare|acme/conveyor|"+later+"|"+head] = github.RateLimitedError(time.Time{}, errors.New("rate limited"))
	if err := f.work().CompareDirect(f.ctx, keys); !errors.Is(err, ErrRateLimited) || !isRateLimited(err) {
		t.Fatalf("err=%v, want ErrRateLimited", err)
	}
	if got, ok := f.comparison(missing, head); !ok || got.Status != store.FeedbackCompareMissing {
		t.Fatalf("missing=%+v ok=%v", got, ok)
	}
	for _, base := range []string{failing, later} {
		if _, ok := f.comparison(base, head); ok {
			t.Fatalf("a failed comparison of %s was stored", base)
		}
	}
	if !strings.Contains(strings.Join(f.logs, "\n"), "category=forge_status") || !strings.Contains(strings.Join(f.logs, "\n"), "category=forge_rate_limited") {
		t.Fatalf("logs=%v", f.logs)
	}
	// A compare that GitHub cannot answer because the head is missing, while
	// the merge commit exists, stays pending.
	f.forge.fail = map[string]error{}
	other := sha("dd05")
	if err := f.work().CompareDirect(f.ctx, []store.FeedbackComparisonKey{{Repository: "acme/conveyor", Base: head, Head: other}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.comparison(head, other); ok {
		t.Fatal("an ambiguous compare failure was stored")
	}
}

// TestRefreshBranches stores a tracked branch head read through the same
// admission.
func TestRefreshBranches(t *testing.T) {
	f := newWorkFixture(t)
	head := sha("ee01")
	f.forge.refs["acme/conveyor|heads/main"] = head
	branches := []BranchRef{{Slug: "acme/conveyor", Branch: "main"}, {Slug: "acme/conveyor", Branch: "gone"}}
	if err := f.work().RefreshBranches(f.ctx, branches); err != nil {
		t.Fatal(err)
	}
	// GitHub confirms that gone does not exist, so its absence is stored.
	heads, err := f.st.ListFeedbackBranchHeads(f.ctx)
	if err != nil || len(heads) != 2 || heads[0].Branch != "gone" || !heads[0].Absent() || heads[1].Commit != head || heads[1].Branch != "main" || !heads[1].ObservedAt.Equal(statusBase) {
		t.Fatalf("heads=%+v err=%v", heads, err)
	}
	// In flight: a second request inside the lease reads nothing.
	if err := f.work().RefreshBranches(f.ctx, branches); err != nil {
		t.Fatal(err)
	}
	if reads, _ := f.forge.calls(); reads != 2 {
		t.Fatalf("reads=%d", reads)
	}
}

// TestRefreshBranchesDeletedBranch proves that a confirmed absence of a
// tracked branch supersedes a stored head that held the task, a forge failure
// that proves nothing keeps the stored head, and a recreated branch
// supersedes the absence (req-production-feedback AC-3.2).
func TestRefreshBranchesDeletedBranch(t *testing.T) {
	f := newWorkFixture(t)
	merge, head := sha("aa01"), sha("ee01")
	f.forge.refs["acme/conveyor|heads/main"] = head
	f.forge.chain("acme/conveyor", "", merge, head)
	branches := []BranchRef{{Slug: "acme/conveyor", Branch: "main"}}
	merged := store.FeedbackMergedTask{TaskID: "t1", Repository: "conveyor", MergeCommit: merge, MergedAt: statusBase}
	environment := f.environment
	environment.TrackedBranches = map[string]string{"conveyor": "main"}
	// read derives the status a read serves from storage alone.
	read := func() (TaskStatus, Missing) {
		t.Helper()
		heads, err := f.st.ListFeedbackBranchHeads(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		state, err := f.st.ReadFeedbackInputs(f.ctx, []string{environment.ID})
		if err != nil {
			t.Fatal(err)
		}
		in := Inputs{Now: f.clock(), Environments: []store.Environment{environment}, State: state, BranchHeads: heads, Slugs: map[string]string{"conveyor": "acme/conveyor"}}
		if in.Comparisons, err = f.st.ListFeedbackComparisons(f.ctx, RequiredComparisons(in, []store.FeedbackMergedTask{merged})); err != nil {
			t.Fatal(err)
		}
		statuses, _, missing := TaskStatuses(in, merged)
		if len(statuses) != 1 {
			t.Fatalf("statuses=%+v missing=%+v", statuses, missing)
		}
		return statuses[0], missing
	}
	// refresh runs the background branch read once the admission lease and
	// the head age have passed.
	refresh := func() {
		t.Helper()
		f.advance(store.FeedbackBranchHeadAge)
		if err := f.work().RefreshBranches(f.ctx, branches); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.work().RefreshBranches(f.ctx, branches); err != nil {
		t.Fatal(err)
	}
	if err := f.work().CompareDirect(f.ctx, []store.FeedbackComparisonKey{{Repository: "acme/conveyor", Base: merge, Head: head}}); err != nil {
		t.Fatal(err)
	}
	if status, missing := read(); status.Status != StatusNotLiveYet || !status.OnTrackedBranch || !missing.Empty() {
		t.Fatalf("status=%+v missing=%+v", status, missing)
	}

	// A forge failure that proves nothing keeps the stored head.
	f.forge.fail["acme/conveyor|heads/main"] = github.StatusError(502, errors.New("bad gateway"))
	refresh()
	if heads, _ := f.st.ListFeedbackBranchHeads(f.ctx); len(heads) != 1 || heads[0].Commit != head {
		t.Fatalf("heads after a transient failure=%+v", heads)
	}
	delete(f.forge.fail, "acme/conveyor|heads/main")

	// The branch is deleted: the confirmed absence supersedes the head, and
	// the next read never reports the task on the branch.
	delete(f.forge.refs, "acme/conveyor|heads/main")
	refresh()
	reads, compares := f.forge.calls()
	if status, missing := read(); status.Status != StatusNotLiveYet || status.OnTrackedBranch || !missing.Empty() {
		t.Fatalf("status after the branch was deleted=%+v missing=%+v", status, missing)
	}
	if r, c := f.forge.calls(); r != reads || c != compares {
		t.Fatalf("the read called the forge: reads %d->%d compares %d->%d", reads, r, compares, c)
	}

	// The branch is recreated at the same head: the new head supersedes the
	// absence and the stored answer serves the read again.
	f.forge.refs["acme/conveyor|heads/main"] = head
	refresh()
	if status, missing := read(); !status.OnTrackedBranch || !missing.Empty() {
		t.Fatalf("status after the branch was recreated=%+v missing=%+v", status, missing)
	}
}

func TestPreviousVersion(t *testing.T) {
	report := func(n int64) store.FeedbackOrigin { return store.FeedbackOrigin{Report: n} }
	v := func(commit string) store.FeedbackVersion { return store.FeedbackVersion{Commit: commit} }
	deploys := []store.FeedbackDeploy{
		{Service: "api", FeedbackVersion: v("a"), DeployedAt: at(1), Origin: report(1)},
		{Service: "web", FeedbackVersion: v("x"), DeployedAt: at(2), Origin: report(2)},
		{Service: "api", FeedbackVersion: v("b"), DeployedAt: at(3), Origin: report(3)},
		{Service: "api", FeedbackVersion: v("a"), DeployedAt: at(4), Origin: report(4)},
	}
	got, ok := PreviousVersion(deploys, store.FeedbackVersionRef{EnvironmentID: "env", Service: "api", FeedbackVersion: v("a")})
	if !ok || got.Commit != "b" {
		t.Fatalf("previous=%+v ok=%v", got, ok)
	}
	if _, ok := PreviousVersion(deploys[:1], store.FeedbackVersionRef{Service: "api", FeedbackVersion: v("a")}); ok {
		t.Fatal("a first version has a previous one")
	}
}

// slowForge records when each comparison starts. A comparison of a pair in
// slow takes two seconds of the fixture clock, and crossing passes 61
// seconds closes reached.
type slowForge struct {
	*stubForge
	f       *workFixture
	slow    map[string]bool
	mu      sync.Mutex
	starts  []time.Time
	reached chan struct{}
	once    sync.Once
}

func (s *slowForge) Compare(ctx context.Context, slug, base, head string, arrived int) (CompareResult, error) {
	s.mu.Lock()
	s.starts = append(s.starts, s.f.clock())
	s.mu.Unlock()
	if s.slow[base] {
		s.f.advance(2 * time.Second)
		if s.f.clock().After(statusBase.Add(61 * time.Second)) {
			s.once.Do(func() { close(s.reached) })
		}
	}
	return s.stubForge.Compare(ctx, slug, base, head, arrived)
}

func (s *slowForge) ReadLineChange(context.Context, string, string, string, int) (github.LineChange, error) {
	return github.LineChange{}, errors.New("this forge stub answers no line read")
}

// mostInAMinute returns the largest number of starts in any minute.
func mostInAMinute(starts []time.Time) int {
	most := 0
	for _, end := range starts {
		count := 0
		for _, start := range starts {
			if start.After(end.Add(-store.FeedbackComparisonWindow)) && !start.After(end) {
				count++
			}
		}
		most = max(most, count)
	}
	return most
}

// TestComparisonLimitHoldsAcrossSlowWorkers covers the FB-3 bound: each
// comparison is charged when it starts, so a slow pass that runs past a
// minute and a second worker together never start more than 60 comparisons
// in any minute for the workspace.
func TestComparisonLimitHoldsAcrossSlowWorkers(t *testing.T) {
	f := newWorkFixture(t)
	head := sha("aa00aa00aa00")
	slowPairs, fastPairs := line("51", 60), line("fa", 60)
	f.forge.chain("acme/conveyor", "", append(append(slices.Clone(slowPairs), fastPairs...), head)...)
	forge := &slowForge{stubForge: f.forge, f: f, slow: map[string]bool{}, reached: make(chan struct{})}
	keys := func(bases []string) []store.FeedbackComparisonKey {
		out := make([]store.FeedbackComparisonKey, len(bases))
		for i, base := range bases {
			forge.slow[base] = slices.Contains(slowPairs, base)
			out[i] = store.FeedbackComparisonKey{Repository: "acme/conveyor", Base: base, Head: head}
		}
		return out
	}
	slowKeys, fastKeys := keys(slowPairs), keys(fastPairs)
	work := f.work()
	work.Forge = forge
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- work.CompareDirect(f.ctx, slowKeys)
	}()
	go func() {
		defer wg.Done()
		// The second worker starts after the first has run past a minute.
		<-forge.reached
		errs <- work.CompareDirect(f.ctx, fastKeys)
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	forge.mu.Lock()
	defer forge.mu.Unlock()
	if most := mostInAMinute(forge.starts); most > store.FeedbackComparisonsPerMinute {
		t.Fatalf("%d comparisons started in one minute, want at most %d", most, store.FeedbackComparisonsPerMinute)
	}
	fast := 0
	for _, key := range fastKeys {
		if _, ok := f.comparison(key.Base, head); ok {
			fast++
		}
	}
	if fast == 0 || fast == len(fastKeys) {
		t.Fatalf("the second worker answered %d of %d pairs, want some held back by the bound", fast, len(fastKeys))
	}
}

// TestFailingPairsDoNotHoldBackLaterPairs covers FB-3 fairness: when the
// first 60 missing pairs keep failing, a later read more than the in-flight
// lease after the first tries the pairs never tried before them.
func TestFailingPairsDoNotHoldBackLaterPairs(t *testing.T) {
	f := newWorkFixture(t)
	head := sha("bb00bb00bb00")
	bases := line("0f", 70)
	f.forge.chain("acme/conveyor", "", append(slices.Clone(bases), head)...)
	keys := make([]store.FeedbackComparisonKey, len(bases))
	for i, base := range bases {
		keys[i] = store.FeedbackComparisonKey{Repository: "acme/conveyor", Base: base, Head: head}
		if i < 60 {
			f.forge.fail["compare|acme/conveyor|"+base+"|"+head] = github.StatusError(502, errors.New("bad gateway"))
		}
	}
	if err := f.work().CompareDirect(f.ctx, keys); err != nil {
		t.Fatal(err)
	}
	if _, compares := f.forge.calls(); compares != 60 {
		t.Fatalf("first read compares=%d", compares)
	}
	for _, key := range keys[60:] {
		if _, ok := f.comparison(key.Base, head); ok {
			t.Fatal("a pair past the allowance was compared in the first minute")
		}
	}
	f.advance(3 * time.Minute)
	if err := f.work().CompareDirect(f.ctx, keys); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys[60:] {
		if got, ok := f.comparison(key.Base, head); !ok || got.Status != store.FeedbackCompareAhead {
			t.Fatalf("pair %s after the failing pairs=%+v ok=%v", key.Base, got, ok)
		}
	}
	if _, compares := f.forge.calls(); compares != 120 {
		t.Fatalf("compares=%d, want the 10 new pairs and 50 retries", compares)
	}
}

// cancelRefusingStore refuses every call on a cancelled context, as the
// durable backends do.
type cancelRefusingStore struct {
	store.FeedbackStore
}

func (s cancelRefusingStore) ClaimFeedbackResolution(ctx context.Context, ref store.FeedbackVersionRef, signalAt, now time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.FeedbackStore.ClaimFeedbackResolution(ctx, ref, signalAt, now)
}

func (s cancelRefusingStore) RecordFeedbackResolution(ctx context.Context, resolution store.FeedbackResolution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.FeedbackStore.RecordFeedbackResolution(ctx, resolution)
}

func (s cancelRefusingStore) RecordFeedbackComparisons(ctx context.Context, comparisons []store.FeedbackComparison) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.FeedbackStore.RecordFeedbackComparisons(ctx, comparisons)
}

func (s cancelRefusingStore) RecordFeedbackBranchHead(ctx context.Context, head store.FeedbackBranchHead) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.FeedbackStore.RecordFeedbackBranchHead(ctx, head)
}

func (s cancelRefusingStore) AdmitFeedbackComparisons(ctx context.Context, keys []store.FeedbackComparisonKey, now time.Time, most int) ([]store.FeedbackComparisonKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.FeedbackStore.AdmitFeedbackComparisons(ctx, keys, now, most)
}

// expiringForge waits until its context expires. Then it fails as the REST
// transport fails, or, with answer set, it returns that comparison answer
// as one that arrives just as the time runs out.
type expiringForge struct {
	answer string
}

func (f expiringForge) ReadCommit(ctx context.Context, _, _ string) (string, error) {
	<-ctx.Done()
	return "", &github.Error{Category: github.ForgeRequest, Err: fmt.Errorf("GitHub REST transport: %w", ctx.Err())}
}

func (f expiringForge) Compare(ctx context.Context, _, _, _ string, _ int) (CompareResult, error) {
	<-ctx.Done()
	if f.answer != "" {
		return CompareResult{Status: f.answer}, nil
	}
	return CompareResult{}, &github.Error{Category: github.ForgeRequest, Err: fmt.Errorf("GitHub REST transport: %w", ctx.Err())}
}

func (f expiringForge) ReadLineChange(context.Context, string, string, string, int) (github.LineChange, error) {
	return github.LineChange{}, errors.New("this forge stub answers no line read")
}

// TestForgeTimeoutKeepsTheOutcome covers component-production-feedback
// invariants 7 and 8 and req-production-feedback AC-1.4: when the job's time
// runs out during a forge call, the outcome is still saved, with the job's
// workspace, on a store that refuses cancelled writes. The version keeps the
// failure's reason and the 10-minute rule, not pending, and a later signal
// inside the 10 minutes asks nothing.
func TestForgeTimeoutKeepsTheOutcome(t *testing.T) {
	f := newWorkFixture(t)
	commit := sha("abc123456789")
	signal := store.FeedbackSignal{EnvironmentID: f.environment.ID, KeyID: f.key.ID, ArrivedAt: statusBase, NextSignalSeconds: 60, Services: []store.FeedbackSignalService{{Name: "api", Repository: "conveyor", Versions: []store.FeedbackSignalVersion{commitVersion(commit)}}}}
	if err := f.st.RecordFeedbackSignal(f.ctx, signal); err != nil {
		t.Fatal(err)
	}
	signal.ArrivedAt = store.EnvironmentTime(signal.ArrivedAt)
	request := SignalRequest{Signal: signal, Environment: f.environment, Repositories: []Repository{{Name: "conveyor", Slug: "acme/conveyor"}}}
	work := Work{Store: cancelRefusingStore{FeedbackStore: f.st}, Forge: expiringForge{}, Now: f.clock}
	jobCtx, cancel := context.WithTimeout(f.ctx, 50*time.Millisecond)
	defer cancel()
	if err := work.ProcessSignal(jobCtx, request); err != nil {
		t.Fatal(err)
	}
	if jobCtx.Err() == nil {
		t.Fatal("the job's time did not run out during the forge call")
	}
	got := f.resolution(commit)
	if got.Resolved() || got.Reason != string(github.ForgeRequest) || !got.AttemptedAt.Equal(statusBase) || !got.NextAttemptAt.Equal(statusBase.Add(store.FeedbackResolutionRetry)) {
		t.Fatalf("resolution after the forge timeout=%+v, want forge_request and the 10-minute rule", got)
	}
	// A later signal inside the 10 minutes claims nothing.
	f.advance(5 * time.Minute)
	later := request
	later.Signal.ArrivedAt = f.clock()
	if claimed, err := f.st.ClaimFeedbackResolution(f.ctx, store.FeedbackVersionRef{EnvironmentID: f.environment.ID, Service: "api", FeedbackVersion: store.FeedbackVersion{Repository: "conveyor", Commit: commit[:12]}}, later.Signal.ArrivedAt, f.clock()); err != nil || claimed {
		t.Fatalf("a signal inside the 10 minutes claimed the version: claimed=%v err=%v", claimed, err)
	}
	// A comparison answer that arrives as the time runs out is saved too.
	head, base := sha("cd00cd00cd00"), sha("ef00ef00ef00")
	compareCtx, cancelCompare := context.WithTimeout(f.ctx, 50*time.Millisecond)
	defer cancelCompare()
	work.Forge = expiringForge{answer: store.FeedbackCompareAhead}
	if err := work.CompareDirect(compareCtx, []store.FeedbackComparisonKey{{Repository: "acme/conveyor", Base: base, Head: head}, {Repository: "acme/conveyor", Base: head, Head: base}}); err != nil {
		t.Fatal(err)
	}
	if answer, ok := f.comparison(base, head); !ok || answer.Status != store.FeedbackCompareAhead {
		t.Fatalf("comparison after the timeout=%+v ok=%v", answer, ok)
	}
	// No second request started after the time ran out.
	if _, ok := f.comparison(head, base); ok {
		t.Fatal("a request started after the job's time ran out")
	}
}

// TestCompareDirectStopsOnARateLimitedBaseRead proves that a compare that
// answers 404 followed by a rate-limited read of the base commit stops the
// pass, as a rate-limited compare does, and stores nothing.
func TestCompareDirectStopsOnARateLimitedBaseRead(t *testing.T) {
	f := newWorkFixture(t)
	failing, later, head := sha("aa01"), sha("bb01"), sha("cc01")
	f.forge.chain("acme/conveyor", "", later, head)
	f.forge.fail["compare|acme/conveyor|"+failing+"|"+head] = github.StatusError(404, errors.New("not found"))
	f.forge.fail["acme/conveyor|"+failing] = fmt.Errorf("read commit: %w", github.RateLimitedError(statusBase.Add(time.Hour), errors.New("rate limited")))
	keys := []store.FeedbackComparisonKey{{Repository: "acme/conveyor", Base: failing, Head: head}, {Repository: "acme/conveyor", Base: later, Head: head}}
	if err := f.work().CompareDirect(f.ctx, keys); !errors.Is(err, ErrRateLimited) || !isRateLimited(err) {
		t.Fatalf("err=%v, want ErrRateLimited from the base read", err)
	}
	if reads, compares := f.forge.calls(); reads != 1 || compares != 1 {
		t.Fatalf("reads=%d compares=%d, want the pass to stop after the rate-limited read", reads, compares)
	}
	for _, base := range []string{failing, later} {
		if got, ok := f.comparison(base, head); ok {
			t.Fatalf("stored %+v", got)
		}
	}
	if logs := strings.Join(f.logs, "\n"); !strings.Contains(logs, "category=forge_rate_limited") {
		t.Fatalf("logs=%s", logs)
	}
}

// TestCompareDirectKeepsAmbiguous422Pending covers component-production-
// feedback invariant 16 and req-production-feedback AC-3.2: when a compare
// answers 404 and the read of the base commit answers an ambiguous 422, the
// pair is not stored as missing and stays eligible. Only GitHub's no-commit
// answer stores missing.
func TestCompareDirectKeepsAmbiguous422Pending(t *testing.T) {
	f := newWorkFixture(t)
	head, base := sha("ab01"), sha("ab02")
	f.forge.chain("acme/conveyor", "", head)
	key := store.FeedbackComparisonKey{Repository: "acme/conveyor", Base: base, Head: head}
	f.forge.fail["compare|acme/conveyor|"+base+"|"+head] = github.StatusError(404, errors.New("not found"))
	for name, lookup := range map[string]error{
		"an ambiguous 422":        fmt.Errorf("read commit: %w", github.StatusError(422, errors.New("GitHub REST status 422: Validation Failed"))),
		"an abuse-protection 422": fmt.Errorf("read commit: %w", github.RateLimitedError(statusBase.Add(time.Hour), errors.New("GitHub REST status 422: abuse detection"))),
	} {
		f.forge.fail["acme/conveyor|"+base] = lookup
		// A rate-limited base read stops the pass; an ambiguous 422 does not.
		if err := f.work().CompareDirect(f.ctx, []store.FeedbackComparisonKey{key}); errors.Is(err, ErrRateLimited) != isRateLimited(lookup) || (err != nil && !errors.Is(err, ErrRateLimited)) {
			t.Fatalf("%s: err=%v", name, err)
		}
		if got, ok := f.comparison(base, head); ok {
			t.Fatalf("%s: the pair was stored as %+v", name, got)
		}
		// The next pass may admit the pair again once its lease is over.
		f.advance(store.FeedbackAdmissionLease + time.Second)
	}
	// GitHub's no-commit answer for the base proves it missing.
	f.forge.fail["acme/conveyor|"+base] = github.CommitNotFoundError(base)
	if err := f.work().CompareDirect(f.ctx, []store.FeedbackComparisonKey{key}); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.comparison(base, head); !ok || got.Status != store.FeedbackCompareMissing {
		t.Fatalf("after the no-commit answer=%+v ok=%v", got, ok)
	}
}
