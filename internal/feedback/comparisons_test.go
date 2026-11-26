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
	signal := store.FeedbackSignal{EnvironmentID: f.environment.ID, KeyID: f.key.ID, ArrivedAt: f.clock(), NextSignalSeconds: 60, Services: []store.FeedbackSignalService{{Name: "api", Repository: "conveyor", Versions: versions}}}
	if err := f.st.RecordFeedbackSignal(f.ctx, signal); err != nil {
		f.t.Fatal(err)
	}
	signal.ArrivedAt = store.EnvironmentTime(signal.ArrivedAt)
	request := SignalRequest{Signal: signal, Environment: f.environment, Repositories: []Repository{{Name: "conveyor", Slug: "acme/conveyor"}, {Name: "app", Slug: "acme/app"}}}
	if err := f.work().ProcessSignal(f.ctx, request); err != nil {
		f.t.Fatal(err)
	}
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
	if err := f.work().CompareDirect(f.ctx, keys); err != nil {
		t.Fatal(err)
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
	heads, err := f.st.ListFeedbackBranchHeads(f.ctx)
	if err != nil || len(heads) != 1 || heads[0].Commit != head || heads[0].Branch != "main" || !heads[0].ObservedAt.Equal(statusBase) {
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
