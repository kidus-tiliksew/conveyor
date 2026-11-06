package monitor_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

const (
	tipWindow = time.Hour
	tipSlug   = "acme/conveyor"
)

// tipEpoch is when each scenario's forge starts. Scenarios move their clock
// forward from it, so a window read is measured against the poll's own time.
var tipEpoch = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

type forgeCommit struct {
	sha     string
	parents []string
	date    time.Time
	message string
	files   []string
	hints   string
	checks  string
	pull    int
}

// tipForge is a scripted GitHub holding a commit graph. It answers the reads a
// pass makes the way GitHub does: lists newest first, comparisons oldest first
// and paged by commit, and a 404 for a comparison naming an unknown commit.
type tipForge struct {
	t        *testing.T
	commits  map[string]*forgeCommit
	head     string
	readErr  error
	hintsErr error
	requests []string
}

func newTipForge(t *testing.T) *tipForge {
	return &tipForge{t: t, commits: map[string]*forgeCommit{}}
}

// push adds a commit on top of the branch head, as its new head.
func (f *tipForge) push(sha string, date time.Time, files ...string) *forgeCommit {
	parents := []string{}
	if f.head != "" {
		parents = append(parents, f.head)
	}
	commit := &forgeCommit{sha: sha, parents: parents, date: date, message: "manual change", files: files}
	f.commits[sha] = commit
	f.head = sha
	return commit
}

func (f *tipForge) ancestors(sha string) map[string]struct{} {
	seen := map[string]struct{}{}
	pending := []string{sha}
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		commit, known := f.commits[next]
		if _, visited := seen[next]; visited || !known {
			continue
		}
		seen[next] = struct{}{}
		pending = append(pending, commit.parents...)
	}
	return seen
}

func commitJSON(commit *forgeCommit) map[string]any {
	parents := make([]map[string]string, 0, len(commit.parents))
	for _, parent := range commit.parents {
		parents = append(parents, map[string]string{"sha": parent})
	}
	return map[string]any{
		"sha": commit.sha, "html_url": "https://github.com/" + tipSlug + "/commit/" + commit.sha, "parents": parents,
		"commit": map[string]any{"message": commit.message, "committer": map[string]any{"date": commit.date.UTC().Format(time.RFC3339)}},
	}
}

func arg(args []string, name string) string {
	for _, value := range args {
		if rest, found := strings.CutPrefix(value, name+"="); found {
			return rest
		}
	}
	return ""
}

func (f *tipForge) run(_ context.Context, args ...string) ([]byte, error) {
	endpoint := strings.TrimPrefix(args[3], "repos/"+tipSlug+"/")
	f.requests = append(f.requests, endpoint)
	if strings.HasPrefix(endpoint, "contents/") {
		if f.hintsErr != nil {
			return nil, f.hintsErr
		}
		commit := f.commits[arg(args, "ref")]
		if commit == nil || commit.hints == "" {
			return nil, githubtrigger.StatusError(http.StatusNotFound, errors.New("GitHub REST status 404: Not Found"))
		}
		return json.Marshal(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(commit.hints))})
	}
	if f.readErr != nil {
		return nil, f.readErr
	}
	switch {
	case endpoint == "commits":
		return f.window(args)
	case strings.HasPrefix(endpoint, "compare/"):
		return f.compare(args, strings.TrimPrefix(endpoint, "compare/"))
	case strings.HasSuffix(endpoint, "/check-runs"):
		commit := f.commits[strings.Split(endpoint, "/")[1]]
		if commit == nil || commit.checks == "" {
			return []byte(`{"check_runs":[]}`), nil
		}
		return []byte(commit.checks), nil
	case strings.HasSuffix(endpoint, "/pulls"):
		commit := f.commits[strings.Split(endpoint, "/")[1]]
		if commit == nil || commit.pull == 0 {
			return []byte(`[]`), nil
		}
		return json.Marshal([]map[string]any{forgePull(commit)})
	case strings.HasPrefix(endpoint, "pulls/"):
		number, _ := strconv.Atoi(strings.TrimPrefix(endpoint, "pulls/"))
		for _, commit := range f.commits {
			if commit.pull == number {
				return json.Marshal(forgePull(commit))
			}
		}
	case strings.HasPrefix(endpoint, "commits/"):
		name := strings.TrimPrefix(endpoint, "commits/")
		if name == "main" {
			name = f.head
		}
		if commit := f.commits[name]; commit != nil {
			return json.Marshal(commitJSON(commit))
		}
	}
	f.t.Errorf("unscripted forge read: %v", args)
	return nil, fmt.Errorf("unscripted forge read: %v", args)
}

func forgePull(commit *forgeCommit) map[string]any {
	return map[string]any{
		"number": commit.pull, "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", tipSlug, commit.pull),
		"merged_at": commit.date.UTC().Format(time.RFC3339), "merged_by": map[string]string{"login": "operator"},
		"merge_commit_sha": commit.sha, "head": map[string]string{"ref": "conveyor/task-lineaged", "sha": "pull-head-" + commit.sha},
	}
}

func (f *tipForge) window(args []string) ([]byte, error) {
	since, err := time.Parse(time.RFC3339, arg(args, "since"))
	if err != nil {
		return nil, err
	}
	var listed []*forgeCommit
	for sha := range f.ancestors(f.head) {
		if commit := f.commits[sha]; !commit.date.Before(since) {
			listed = append(listed, commit)
		}
	}
	sort.Slice(listed, func(i, j int) bool {
		if f.isAncestor(listed[i].sha, listed[j].sha) {
			return false
		}
		if f.isAncestor(listed[j].sha, listed[i].sha) {
			return true
		}
		return listed[i].sha < listed[j].sha
	})
	body := make([]map[string]any, 0, len(listed))
	for _, commit := range listed {
		body = append(body, commitJSON(commit))
	}
	return json.Marshal(body)
}

// isAncestor reports whether candidate is a strict ancestor of descendant.
func (f *tipForge) isAncestor(candidate, descendant string) bool {
	if candidate == descendant {
		return false
	}
	_, found := f.ancestors(descendant)[candidate]
	return found
}

func (f *tipForge) compare(args []string, spec string) ([]byte, error) {
	from, to, found := strings.Cut(spec, "...")
	if !found {
		return nil, fmt.Errorf("malformed comparison %q", spec)
	}
	if to == "main" {
		to = f.head
	}
	if f.commits[from] == nil || f.commits[to] == nil {
		return nil, githubtrigger.StatusError(http.StatusNotFound, errors.New("GitHub REST status 404: Not Found"))
	}
	// A comparison of a commit with its parent is the first-parent file read.
	if commit := f.commits[to]; len(commit.parents) > 0 && commit.parents[0] == from && arg(args, "per_page") == "" {
		files := make([]map[string]string, 0, len(commit.files))
		for _, name := range commit.files {
			files = append(files, map[string]string{"filename": name})
		}
		return json.Marshal(map[string]any{"status": "ahead", "files": files})
	}
	status := "diverged"
	switch {
	case from == to:
		status = "identical"
	case f.isAncestor(from, to):
		status = "ahead"
	case f.isAncestor(to, from):
		status = "behind"
	}
	if arg(args, "per_page") == "1" {
		return json.Marshal(map[string]string{"status": status})
	}
	base := f.ancestors(from)
	var listed []*forgeCommit
	for sha := range f.ancestors(to) {
		if _, reachable := base[sha]; !reachable {
			listed = append(listed, f.commits[sha])
		}
	}
	// Oldest first, as GitHub answers a comparison.
	sort.Slice(listed, func(i, j int) bool {
		if f.isAncestor(listed[i].sha, listed[j].sha) {
			return true
		}
		if f.isAncestor(listed[j].sha, listed[i].sha) {
			return false
		}
		return listed[i].sha < listed[j].sha
	})
	total := len(listed)
	perPage, _ := strconv.Atoi(arg(args, "per_page"))
	page, _ := strconv.Atoi(arg(args, "page"))
	start := min((page-1)*perPage, total)
	end := min(start+perPage, total)
	body := make([]map[string]any, 0, end-start)
	for _, commit := range listed[start:end] {
		body = append(body, commitJSON(commit))
	}
	return json.Marshal(map[string]any{"status": status, "total_commits": total, "commits": body})
}

// tipScenario drives real polls of a scripted forge through the real source and
// service against the memory backend.
type tipScenario struct {
	t       *testing.T
	forge   *tipForge
	service *monitor.Service
	st      store.Store
	ctx     context.Context
	now     time.Time
	design  string
	// inFlight is what MergeCommitState answers for a commit until it is
	// deleted. Nil keeps every commit an ordinary one.
	inFlight map[string]bool
	lineaged map[string]bool
}

func newTipScenario(t *testing.T) *tipScenario {
	t.Helper()
	service, st, ctx := testService(t)
	s := &tipScenario{t: t, forge: newTipForge(t), service: service, st: st, ctx: ctx, now: tipEpoch,
		inFlight: map[string]bool{}, lineaged: map[string]bool{}}
	s.design = s.governDocument("tip-governed", "conveyor", "internal/**")
	return s
}

func (s *tipScenario) governDocument(id, repository, glob string) string {
	s.t.Helper()
	content := fmt.Sprintf("# %s\n\n```conveyor:governs\n- repo: %s\n  paths:\n    - %s\n```", id, repository, glob)
	design, version, err := s.st.CreateSystemDesign(s.ctx, core.SystemDesign{ID: id, Title: id, Category: "Component design"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
	if err != nil {
		s.t.Fatal(err)
	}
	if _, _, err = s.st.ConfirmSystemDesignVersion(s.ctx, design.ID, version.Version); err != nil {
		s.t.Fatal(err)
	}
	return design.ID
}

func (s *tipScenario) poll() error {
	s.t.Helper()
	store := s.service.Store
	source := monitor.GitHubSource{
		WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: tipSlug, Base: "main", Run: s.forge.run,
		ResolveTask: func(_ context.Context, ref string, _ int) (string, bool, error) {
			return "lineaged-task", ref == "conveyor/task-lineaged", nil
		},
		ReconcileMerged: func(_ context.Context, _ string, pull githubtrigger.PullRequest) (bool, error) {
			return s.lineaged[pull.MergeCommitSHA], nil
		},
		MergeCommitState: func(_ context.Context, _, sha string, _ time.Time) (monitor.MergeState, string, error) {
			if s.inFlight[sha] {
				return monitor.MergeInFlight, "merging-task", nil
			}
			return monitor.MergeNone, "", nil
		},
		OnSuppressed: func(ctx context.Context, payload map[string]any) error {
			return store.AuditMonitor(ctx, "monitor.suppressed", payload)
		},
		OnWindowFallback: func(ctx context.Context, payload map[string]any) error {
			return store.AuditMonitor(ctx, "monitor.window_fallback", payload)
		},
		LoadHints: func(ctx context.Context, revision string) (*monitor.HintContext, error) {
			return monitor.FetchGitHubHints(ctx, tipSlug, revision, s.forge.run)
		},
	}
	poller := monitor.Poller{Service: s.service, Source: source, Repository: "conveyor", StartupWindow: tipWindow, Attempts: 1,
		Now: func() time.Time { return s.now }}
	return poller.Poll(s.ctx)
}

func (s *tipScenario) mustPoll() {
	s.t.Helper()
	if err := s.poll(); err != nil {
		s.t.Fatal(err)
	}
}

func (s *tipScenario) tip() string {
	s.t.Helper()
	tip, err := s.service.Store.MonitorTip(s.ctx, "conveyor")
	if err != nil {
		s.t.Fatal(err)
	}
	return tip
}

func (s *tipScenario) status() monitor.Status {
	s.t.Helper()
	status, err := s.service.Status(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	return status
}

// seedTip stores a tip the way a clean pass does: a base commit dated before the
// window and one poll of the quiet repository.
func (s *tipScenario) seedTip() {
	s.t.Helper()
	s.forge.push("base", s.now.Add(-48*time.Hour), "internal/base.go")
	s.mustPoll()
	if s.tip() != "base" {
		s.t.Fatalf("seed tip=%q", s.tip())
	}
}

// assertFiledOnce proves sha ended with its observation linked to the one filed
// task, its repository drift, and its document drift.
func (s *tipScenario) assertFiledOnce(sha string) {
	s.t.Helper()
	status := s.status()
	tasks, err := s.st.ListTasks(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	if len(tasks) != 1 {
		s.t.Fatalf("tasks=%d want one: %+v", len(tasks), tasks)
	}
	linked := false
	for _, observation := range status.Observations {
		if observation.CommitSHA == sha && observation.TaskID == tasks[0].ID {
			linked = true
		}
	}
	if !linked {
		s.t.Fatalf("%s has no observation linked to the filed task: %+v", sha, status.Observations)
	}
	identity := "direct_push:conveyor:" + sha
	var repository, document bool
	for _, drift := range status.Drift {
		repository = repository || drift.ID == identity
		document = document || drift.ID == "design:"+s.design+":"+identity
	}
	if !repository || !document {
		s.t.Fatalf("%s drift repository=%t document=%t: %+v", sha, repository, document, status.Drift)
	}
}

func activityPayloads(status monitor.Status, kind string) []map[string]any {
	var payloads []map[string]any
	for _, activity := range status.Activity {
		if activity.Kind == kind {
			payloads = append(payloads, activity.Payload)
		}
	}
	return payloads
}

// A read failure, then a clean pass after the commit has aged out of the window.
func TestReadFailureOutlastingTheWindowStillFiles(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	s.forge.push("direct", s.now.Add(time.Minute), "internal/direct.go")
	s.now = s.now.Add(2 * time.Minute)
	s.forge.readErr = githubtrigger.StatusError(http.StatusBadGateway, errors.New("bad gateway"))
	if err := s.poll(); err == nil {
		t.Fatal("a failed read reported success")
	}
	if s.tip() != "base" || s.status().CurrentError == "" {
		t.Fatalf("tip=%q error=%q", s.tip(), s.status().CurrentError)
	}
	s.forge.readErr = nil
	s.now = s.now.Add(3 * tipWindow)
	s.mustPoll()
	s.assertFiledOnce("direct")
	if s.tip() != "direct" || s.status().CurrentError != "" {
		t.Fatalf("tip=%q error=%q", s.tip(), s.status().CurrentError)
	}
}

// An intake failure leaves the row with no task. The clean pass after the window
// reads the commit again and files it, and the tip waits for that pass.
func TestIntakeFailureOutlastingTheWindowStillFiles(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	s.forge.push("direct", s.now.Add(time.Minute), "internal/direct.go")
	s.now = s.now.Add(2 * time.Minute)
	errIntake := errors.New("intake unavailable")
	failing := failIntakeFor(s.service, errIntake, "monitor:direct_push:conveyor:direct")
	if err := s.poll(); !errors.Is(err, errIntake) {
		t.Fatalf("poll err=%v", err)
	}
	if s.tip() != "base" {
		t.Fatalf("a failed intake moved the tip to %q", s.tip())
	}
	*failing = false
	s.now = s.now.Add(3 * tipWindow)
	s.mustPoll()
	s.assertFiledOnce("direct")
	if s.tip() != "direct" {
		t.Fatalf("tip=%q", s.tip())
	}
}

func TestCommitDatedOlderThanTheWindowStillFiles(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	s.forge.push("old-dated", s.now.Add(-4*tipWindow), "internal/old.go")
	s.mustPoll()
	s.assertFiledOnce("old-dated")
	if s.tip() != "old-dated" {
		t.Fatalf("tip=%q", s.tip())
	}
}

func TestDaemonStartAfterDowntimeLongerThanTheWindowStillFiles(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	s.forge.push("first", s.now.Add(time.Minute), "internal/first.go")
	s.forge.push("second", s.now.Add(2*time.Minute), "internal/second.go")
	s.now = s.now.Add(30 * tipWindow)
	s.mustPoll()
	s.assertFiledOnce("first")
	s.assertFiledOnce("second")
	if s.tip() != "second" {
		t.Fatalf("tip=%q", s.tip())
	}
}

// A deferred commit holds the tip, while the pass records success and processes
// the commit above it. With no trigger until the window has passed, the next
// poll still reads the commit and files it.
func TestDeferredCommitWithNoTriggerUntilAfterTheWindowStillFiles(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	s.forge.push("deferred", s.now.Add(time.Minute), "internal/deferred.go")
	s.forge.push("above", s.now.Add(2*time.Minute), "internal/above.go")
	s.inFlight["deferred"] = true
	s.now = s.now.Add(3 * time.Minute)
	s.mustPoll()
	status := s.status()
	if s.tip() != "base" {
		t.Fatalf("a deferred commit moved the tip to %q", s.tip())
	}
	if status.CurrentError != "" || !status.LastSuccessfulAt.Equal(s.now) {
		t.Fatalf("a held pass recorded no success: %+v", status.Repositories)
	}
	suppressed := activityPayloads(status, "monitor.suppressed")
	if !slices.ContainsFunc(suppressed, func(payload map[string]any) bool {
		return payload["reason"] == "merge_in_flight" && payload["commit_sha"] == "deferred"
	}) {
		t.Fatalf("suppressed=%v", suppressed)
	}
	s.assertFiledOnce("above")

	delete(s.inFlight, "deferred")
	s.now = s.now.Add(3 * tipWindow)
	s.mustPoll()
	s.assertFiledOnce("deferred")
	if s.tip() != "above" {
		t.Fatalf("tip=%q", s.tip())
	}
}

// The push's own poll sees unfinished checks. The tip stays behind the lineaged
// commit, so the poll after its checks conclude reads the failure, and once the
// commit leaves the window the tip passes it.
func TestLineagedCommitFailedCheckIsObservedAfterThePushPoll(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	merge := s.forge.push("merge", s.now.Add(time.Minute), "internal/merge.go")
	merge.pull = 41
	merge.checks = `{"check_runs":[{"id":7,"name":"ci","conclusion":""}]}`
	s.lineaged["merge"] = true
	s.now = s.now.Add(2 * time.Minute)
	s.mustPoll()
	if observations := s.status().Observations; len(observations) != 0 {
		t.Fatalf("unfinished checks observed: %+v", observations)
	}
	if s.tip() != "base" || s.status().LastSuccessfulAt != s.now {
		t.Fatalf("tip=%q success=%s", s.tip(), s.status().LastSuccessfulAt)
	}

	merge.checks = `{"check_runs":[{"id":7,"name":"ci","html_url":"https://github.com/acme/conveyor/runs/7","conclusion":"failure","run_attempt":1}]}`
	s.now = s.now.Add(10 * time.Minute)
	s.mustPoll()
	observations := s.status().Observations
	if len(observations) != 1 || observations[0].OccurrenceID != "commit:merge:attempt:1" {
		t.Fatalf("observations=%+v", observations)
	}
	if s.tip() != "base" {
		t.Fatalf("a commit inside the window moved the tip to %q", s.tip())
	}

	s.now = s.now.Add(2 * tipWindow)
	s.mustPoll()
	if s.tip() != "merge" {
		t.Fatalf("tip=%q after the commit left the window", s.tip())
	}
}

// A repository that always has a lineaged commit inside its window still stores
// a tip from its first window read, at the oldest deferred commit's first parent.
func TestWindowReadStoresTipBehindTheOldestDeferredCommit(t *testing.T) {
	s := newTipScenario(t)
	s.forge.push("below-window", s.now.Add(-48*time.Hour), "internal/below.go")
	merge := s.forge.push("merge", s.now.Add(-time.Minute), "internal/merge.go")
	merge.pull = 41
	s.lineaged["merge"] = true
	s.mustPoll()
	if s.tip() != "below-window" {
		t.Fatalf("tip=%q", s.tip())
	}
	if !slices.Contains(s.forge.requests, "commits/main") {
		t.Fatalf("no head read in %v", s.forge.requests)
	}
}

// staleTipSource models a second pass finishing while this one runs.
type staleTipSource struct {
	store monitor.Store
}

func (s staleTipSource) Observations(ctx context.Context, from monitor.ReadFrom) (monitor.Read, error) {
	if advanced, err := s.store.AdvanceMonitorTip(ctx, "conveyor", from.Tip, "faster-pass"); err != nil || !advanced {
		return monitor.Read{}, fmt.Errorf("faster pass advanced=%t err=%v", advanced, err)
	}
	return monitor.Read{NextTip: "slower-pass"}, nil
}

func TestPassFinishingWithAStaleTipDoesNotMoveTheStoredTip(t *testing.T) {
	s := newTipScenario(t)
	poller := monitor.Poller{Service: s.service, Source: staleTipSource{store: s.service.Store}, Repository: "conveyor", StartupWindow: tipWindow,
		Now: func() time.Time { return s.now }}
	if err := poller.Poll(s.ctx); err != nil {
		t.Fatal(err)
	}
	if s.tip() != "faster-pass" {
		t.Fatalf("tip=%q", s.tip())
	}
	if status := s.status(); status.CurrentError != "" || !status.LastSuccessfulAt.Equal(s.now) {
		t.Fatalf("a lost advance is not a failed pass: %+v", status.Repositories)
	}
}

func TestDivergedForcePushFallsBackToTheWindowAndRecordsIt(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	s.forge.push("kept", s.now.Add(time.Minute), "internal/kept.go")
	s.now = s.now.Add(2 * time.Minute)
	s.mustPoll()
	if s.tip() != "kept" {
		t.Fatalf("tip=%q", s.tip())
	}
	// The force-push replaces "kept" with a commit on top of base.
	s.forge.head = "base"
	s.forge.push("rewritten", s.now.Add(time.Minute), "internal/rewritten.go")
	s.now = s.now.Add(2 * time.Minute)
	s.mustPoll()
	fallbacks := activityPayloads(s.status(), "monitor.window_fallback")
	if len(fallbacks) != 1 || fallbacks[0]["reason"] != "diverged" || fallbacks[0]["tip"] != "kept" || fallbacks[0]["repository"] != "conveyor" {
		t.Fatalf("fallbacks=%v", fallbacks)
	}
	s.assertFiledOnce("rewritten")
	if s.tip() != "rewritten" {
		t.Fatalf("tip=%q", s.tip())
	}
	// The stored tip is on the branch again, so the fallback isn't repeated.
	s.mustPoll()
	if fallbacks := activityPayloads(s.status(), "monitor.window_fallback"); len(fallbacks) != 1 {
		t.Fatalf("fallbacks=%v", fallbacks)
	}
}

// With no commit in the window, the fallback still moves the tip to the branch
// head, so a timed poll doesn't record the same fallback every minute.
func TestFallbackWithAQuietWindowStoresTheBranchHead(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	s.forge.push("kept", s.now.Add(time.Minute), "internal/kept.go")
	s.now = s.now.Add(2 * time.Minute)
	s.mustPoll()
	s.forge.head = "base"
	s.forge.push("rewritten", s.now.Add(-48*time.Hour), "internal/rewritten.go")
	s.mustPoll()
	s.mustPoll()
	if fallbacks := activityPayloads(s.status(), "monitor.window_fallback"); len(fallbacks) != 1 {
		t.Fatalf("fallbacks=%v", fallbacks)
	}
	if s.tip() != "rewritten" {
		t.Fatalf("tip=%q", s.tip())
	}
}

func TestTipGoneFallsBackToTheWindowAndRecordsIt(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	s.forge.push("gone", s.now.Add(time.Minute), "internal/gone.go")
	s.now = s.now.Add(2 * time.Minute)
	s.mustPoll()
	s.forge.head = "base"
	delete(s.forge.commits, "gone")
	s.forge.push("replacement", s.now.Add(time.Minute), "internal/replacement.go")
	s.now = s.now.Add(2 * time.Minute)
	s.mustPoll()
	fallbacks := activityPayloads(s.status(), "monitor.window_fallback")
	if len(fallbacks) != 1 || fallbacks[0]["reason"] != "tip_gone" {
		t.Fatalf("fallbacks=%v", fallbacks)
	}
	if s.tip() != "replacement" {
		t.Fatalf("tip=%q", s.tip())
	}
}

// Comparison pages answer oldest first. Every page is read, the classifier sees
// the branch head first, and every commit's merge history is judged.
func TestTipReadPagesTheWholeComparison(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	for i := range 230 {
		s.forge.push(fmt.Sprintf("commit-%03d", i), s.now.Add(time.Duration(i+1)*time.Second), "internal/file.go")
	}
	s.now = s.now.Add(time.Hour)
	s.mustPoll()
	pages := 0
	for _, request := range s.forge.requests {
		if request == "compare/base...main" {
			pages++
		}
	}
	if pages != 3 {
		t.Fatalf("comparison pages=%d", pages)
	}
	if observations := s.status().Observations; len(observations) != 230 {
		t.Fatalf("observations=%d", len(observations))
	}
	if s.tip() != "commit-229" {
		t.Fatalf("tip=%q", s.tip())
	}
}

func TestBrokenHintsProcessTheCommitWithoutHintsAndRecordWhy(t *testing.T) {
	for name, test := range map[string]struct{ document, reason string }{
		"parse failure":       {document: "version: 1\ntools: [shell]\n", reason: "parse advisory hints"},
		"another schema":      {document: "version: 2\n", reason: "unsupported advisory hint version 2"},
		"command interpreter": {document: "version: 1\nverification:\n  - name: run\n    argv: [bash, script]\n", reason: "command interpreter"},
		"shell-shaped argv":   {document: "version: 1\nverification:\n  - name: run\n    argv: [make, test;rm]\n", reason: "shell or injection-shaped argv"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTipScenario(t)
			s.seedTip()
			s.forge.push("direct", s.now.Add(time.Minute), "internal/direct.go").hints = test.document
			s.now = s.now.Add(2 * time.Minute)
			s.mustPoll()
			s.assertFiledOnce("direct")
			status := s.status()
			if status.CurrentError != "" || s.tip() != "direct" {
				t.Fatalf("error=%q tip=%q", status.CurrentError, s.tip())
			}
			if status.Observations[0].Hints != nil || !strings.Contains(status.Observations[0].Context[monitor.ContextHintsSkipped], test.reason) {
				t.Fatalf("observation=%+v", status.Observations[0])
			}
			events, err := s.st.ListEvents(s.ctx, status.Observations[0].TaskID)
			if err != nil {
				t.Fatal(err)
			}
			recorded := false
			for _, event := range events {
				var payload struct {
					Path, Revision, Reason string
				}
				if event.Kind == "repository_hints.skipped" && json.Unmarshal(event.Payload, &payload) == nil {
					recorded = payload.Path == ".conveyor/hints.yaml" && payload.Revision == "direct" && strings.Contains(payload.Reason, test.reason)
				}
			}
			if !recorded {
				t.Fatalf("no repository_hints.skipped naming %q in %+v", test.reason, events)
			}
			if !slices.ContainsFunc(activityPayloads(status, "monitor.suppressed"), func(payload map[string]any) bool {
				return payload["reason"] == "hints_unusable" && payload["commit_sha"] == "direct"
			}) {
				t.Fatalf("no activity for the skipped hints: %+v", status.Activity)
			}
		})
	}
}

// A commit that files nothing still records why its hints were skipped.
func TestBrokenHintsOnALineagedCommitAreRecordedWithoutATask(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	merge := s.forge.push("merge", s.now.Add(time.Minute), "internal/merge.go")
	merge.pull, merge.hints = 41, "version: 2\n"
	s.lineaged["merge"] = true
	s.now = s.now.Add(2 * time.Minute)
	s.mustPoll()
	status := s.status()
	if status.CurrentError != "" || len(status.Observations) != 0 {
		t.Fatalf("status=%+v", status)
	}
	if !slices.ContainsFunc(activityPayloads(status, "monitor.suppressed"), func(payload map[string]any) bool {
		return payload["reason"] == "hints_unusable" && payload["commit_sha"] == "merge"
	}) {
		t.Fatalf("activity=%+v", status.Activity)
	}
}

func TestForgeFailureReadingHintsFailsThePass(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	s.forge.push("direct", s.now.Add(time.Minute), "internal/direct.go").hints = "version: 1\n"
	s.now = s.now.Add(2 * time.Minute)
	s.forge.hintsErr = githubtrigger.RateLimitedError(s.now.Add(time.Hour), errors.New("GitHub REST status 403: API rate limit exceeded"))
	err := s.poll()
	if _, limited := githubtrigger.RateLimitRetryAt(err); !limited {
		t.Fatalf("poll err=%v", err)
	}
	status := s.status()
	if s.tip() != "base" || status.ForgeErrorCategory != string(githubtrigger.ForgeRateLimited) || len(status.Observations) != 0 {
		t.Fatalf("tip=%q status=%+v", s.tip(), status)
	}
}

func TestFileLimitPushFilesATaskAndRaisesDriftForEveryGoverningDocument(t *testing.T) {
	s := newTipScenario(t)
	unmatched := s.governDocument("tip-web", "conveyor", "web/**")
	foreign := s.governDocument("tip-foreign", "elsewhere", "internal/**")
	s.seedTip()
	s.forge.push("older-neighbour", s.now.Add(time.Minute), "internal/neighbour.go")
	files := make([]string, 300)
	for i := range files {
		files[i] = fmt.Sprintf("internal/generated/file-%03d.go", i)
	}
	s.forge.push("big", s.now.Add(2*time.Minute), files...)
	s.forge.push("newer-neighbour", s.now.Add(3*time.Minute), "internal/newer.go")
	s.now = s.now.Add(4 * time.Minute)
	s.mustPoll()
	s.assertFiledOnce("big")
	s.assertFiledOnce("older-neighbour")
	s.assertFiledOnce("newer-neighbour")
	status := s.status()
	if status.CurrentError != "" || s.tip() != "newer-neighbour" {
		t.Fatalf("error=%q tip=%q", status.CurrentError, s.tip())
	}
	identity := "direct_push:conveyor:big"
	drift := map[string]monitor.Drift{}
	for _, record := range status.Drift {
		drift[record.ID] = record
	}
	matched, foundMatched := drift["design:"+s.design+":"+identity]
	if !foundMatched || len(matched.MatchingPaths) != 300 {
		t.Fatalf("matching document drift=%+v", matched)
	}
	web, foundWeb := drift["design:"+unmatched+":"+identity]
	if !foundWeb || len(web.MatchingPaths) != 0 {
		t.Fatalf("a governing document with no listed path drift=%+v found=%t", web, foundWeb)
	}
	if _, found := drift["design:"+foreign+":"+identity]; found {
		t.Fatal("a document governing another repository raised drift")
	}
	if _, found := drift["design:"+unmatched+":direct_push:conveyor:older-neighbour"]; found {
		t.Fatal("a complete neighbour raised drift for a document it doesn't match")
	}
	for _, observation := range status.Observations {
		if observation.CommitSHA == "big" && observation.Context[monitor.ContextPathsIncomplete] != monitor.PathsFileLimit {
			t.Fatalf("observation=%+v", observation)
		}
	}
}

// The task's body comes from the observation that filed it, so the big push
// alone names its cut-off list there.
func TestFileLimitTaskBodySaysItsListIsCutOff(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	files := make([]string, 300)
	for i := range files {
		files[i] = fmt.Sprintf("internal/generated/file-%03d.go", i)
	}
	s.forge.push("big", s.now.Add(time.Minute), files...)
	s.now = s.now.Add(2 * time.Minute)
	s.mustPoll()
	tasks, err := s.st.ListTasks(s.ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	if !strings.Contains(tasks[0].Body, "cut off") || strings.Contains(tasks[0].Body, "propose") {
		t.Fatalf("body=%q", tasks[0].Body)
	}
}

func TestInvalidFilenameFilesTheCommitWithoutThatName(t *testing.T) {
	s := newTipScenario(t)
	unmatched := s.governDocument("tip-web", "conveyor", "web/**")
	s.seedTip()
	s.forge.push("direct", s.now.Add(time.Minute), "../outside.go", "internal/kept.go")
	s.now = s.now.Add(2 * time.Minute)
	s.mustPoll()
	s.assertFiledOnce("direct")
	status := s.status()
	observation := status.Observations[0]
	if slices.Contains(observation.ChangedPaths, "../outside.go") || !slices.Contains(observation.ChangedPaths, "internal/kept.go") ||
		observation.Context[monitor.ContextPathsIncomplete] != monitor.PathsInvalidFilename {
		t.Fatalf("observation=%+v", observation)
	}
	for _, drift := range status.Drift {
		if drift.SystemDesignID == unmatched {
			t.Fatalf("an unmatched document raised drift: %+v", drift)
		}
	}
	tasks, err := s.st.ListTasks(s.ctx)
	if err != nil || len(tasks) != 1 || !strings.Contains(tasks[0].Body, "leaves out") {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
}

// Only a comparison that lists no file at all is an empty direct push.
func TestDirectPushOfOnlyInvalidFilenamesIsNotSuppressedAsEmpty(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	s.forge.push("invalid-only", s.now.Add(time.Minute), "../outside.go")
	s.forge.push("empty", s.now.Add(2*time.Minute))
	s.now = s.now.Add(3 * time.Minute)
	s.mustPoll()
	status := s.status()
	if len(status.Observations) != 1 || status.Observations[0].CommitSHA != "invalid-only" {
		t.Fatalf("observations=%+v", status.Observations)
	}
	if !slices.ContainsFunc(activityPayloads(status, "monitor.suppressed"), func(payload map[string]any) bool {
		return payload["reason"] == "first_parent_empty" && payload["commit_sha"] == "empty"
	}) {
		t.Fatalf("activity=%+v", status.Activity)
	}
}

// A pass over one repository never clears another's error, and the top-level
// fields summarize the repositories (req-delivery-and-forge AC-4.5).
func TestStatusReportsPerRepositoryAndSummarizes(t *testing.T) {
	var status monitor.Status
	older, newer := tipEpoch.Add(-time.Hour), tipEpoch
	backoff := tipEpoch.Add(time.Minute)
	status.SetRepositories([]monitor.RepositoryStatus{
		{Repository: "web", LastSuccessfulAt: newer},
		{Repository: "docs"},
		{Repository: "worker", LastSuccessfulAt: older, CurrentError: "worker failed", ForgeErrorCategory: "forge_request", BackoffUntil: backoff.Add(time.Hour)},
		{Repository: "api", LastSuccessfulAt: newer, CurrentError: "api failed", ForgeErrorCategory: "forge_status", BackoffUntil: backoff},
	})
	names := make([]string, 0, 4)
	for _, row := range status.Repositories {
		names = append(names, row.Repository)
	}
	if strings.Join(names, ",") != "api,docs,web,worker" {
		t.Fatalf("names=%v", names)
	}
	if !status.LastSuccessfulAt.Equal(older) || status.CurrentError != "api failed" || status.ForgeErrorCategory != "forge_status" || !status.BackoffUntil.Equal(backoff) {
		t.Fatalf("summary=%+v", status)
	}
	var empty monitor.Status
	empty.SetRepositories(nil)
	if encoded, err := json.Marshal(empty.Repositories); err != nil || string(encoded) != "[]" {
		t.Fatalf("empty repositories=%s err=%v", encoded, err)
	}
}

// No pass reads a repository outside the monitor's scope, so its row can't hold
// the summary's error forever.
func TestStatusLeavesOutRepositoriesOutsideTheScope(t *testing.T) {
	s := newTipScenario(t)
	if err := s.service.Store.RecordMonitorFailure(s.ctx, "removed", "forge_status", "stale error", s.now); err != nil {
		t.Fatal(err)
	}
	if err := s.service.Store.RecordMonitorSuccess(s.ctx, "conveyor", s.now); err != nil {
		t.Fatal(err)
	}
	status := s.status()
	if status.CurrentError != "" || len(status.Repositories) != 1 || status.Repositories[0].Repository != "conveyor" {
		t.Fatalf("status=%+v", status.Repositories)
	}
}

// A comparison status GitHub doesn't document fails the read rather than guess.
func TestUnknownTipComparisonStatusFailsTheRead(t *testing.T) {
	s := newTipScenario(t)
	s.seedTip()
	run := s.forge.run
	source := monitor.GitHubSource{WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: tipSlug, Base: "main",
		Run: func(ctx context.Context, args ...string) ([]byte, error) {
			if strings.HasSuffix(args[3], "...main") {
				return []byte(`{"status":"sideways"}`), nil
			}
			return run(ctx, args...)
		}}
	_, err := source.Observations(s.ctx, monitor.ReadFrom{Since: s.now.Add(-tipWindow), Tip: "base"})
	if githubtrigger.ErrorCategory(err) != githubtrigger.ForgeResponse {
		t.Fatalf("err=%v", err)
	}
}
