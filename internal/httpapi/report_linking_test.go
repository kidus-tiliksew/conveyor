package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

const (
	linkingResolved = "abcdef0000000000000000000000000000000000"
	linkingChange   = "c4a0000000000000000000000000000000000000"
)

// linkingForge resolves abcdef0 in acme/app and answers line reads. It
// counts every call. While gate is open, a call waits for it.
type linkingForge struct {
	mu        sync.Mutex
	reads     int
	lineReads int
	compares  int
	// readFailure fails every commit read, and compareFailure every
	// comparison, when set.
	readFailure    error
	compareFailure error
	answer         github.LineChange
	failure        error
	gate           chan struct{}
	entered        chan struct{}
	// commits answers commit reads by slug with a full commit, and
	// readsBySlug counts the reads of each slug. statuses answers
	// comparisons by base|head.
	commits     map[string]string
	readsBySlug map[string]int
	statuses    map[string]string
	// lineSlugs records the slug of each line read.
	lineSlugs []string
}

func newLinkingForge() *linkingForge {
	return &linkingForge{answer: github.LineChange{FileFound: true, Commit: linkingChange, PullRequests: []int{}}, entered: make(chan struct{}, 64)}
}

func (f *linkingForge) wait() {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		f.entered <- struct{}{}
		<-gate
	}
}

func (f *linkingForge) ReadCommit(_ context.Context, slug, ref string) (string, error) {
	f.wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readsBySlug == nil {
		f.readsBySlug = map[string]int{}
	}
	f.readsBySlug[slug]++
	if f.readFailure != nil {
		return "", f.readFailure
	}
	if slug == "acme/app" && strings.HasPrefix(linkingResolved, ref) {
		return linkingResolved, nil
	}
	if commit := f.commits[slug]; commit != "" && strings.HasPrefix(commit, ref) {
		return commit, nil
	}
	return "", github.CommitNotFoundError(ref)
}

func (f *linkingForge) Compare(_ context.Context, _, base, head string, _ int) (feedback.CompareResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compares++
	if f.compareFailure != nil {
		return feedback.CompareResult{}, f.compareFailure
	}
	if status := f.statuses[base+"|"+head]; status != "" {
		return feedback.CompareResult{Status: status}, nil
	}
	return feedback.CompareResult{}, errors.New("this forge stub compares nothing")
}

// calls returns the commit reads, line reads, and comparisons so far.
func (f *linkingForge) calls() [3]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return [3]int{f.reads, f.lineReads, f.compares}
}

func (f *linkingForge) set(apply func(*linkingForge)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	apply(f)
}

func (f *linkingForge) ReadLineChange(_ context.Context, slug, commit, path string, line int) (github.LineChange, error) {
	f.wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lineReads++
	f.lineSlugs = append(f.lineSlugs, slug)
	if !strings.HasPrefix(slug, "acme/") || commit != linkingResolved || path != "cmd/main.go" || line != 12 {
		return github.LineChange{}, errors.New("unexpected line read")
	}
	if f.failure != nil {
		return github.LineChange{}, f.failure
	}
	return f.answer, nil
}

func (f *linkingForge) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads, f.lineReads
}

// linkingHarness is the report harness with a GitHub slug for the
// workspace's repository and the linking forge.
type linkingHarness struct {
	*feedbackHarness
	forge *linkingForge
}

func newLinkingHarness(t *testing.T) *linkingHarness {
	t.Helper()
	h := &linkingHarness{feedbackHarness: newFeedbackHarness(t), forge: newLinkingForge()}
	current, err := h.st.WorkspaceConfig(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.UpdateWorkspaceConfig(h.ctx, current.Version, &config.Config{Workspace: "alpha", Repos: []config.Repo{{Name: "app", URL: "https://example.test/app", Base: "main", GitHub: "acme/app"}}}); err != nil {
		t.Fatal(err)
	}
	h.server.feedbackForge = h.forge
	return h
}

func (h *linkingHarness) settle() {
	h.t.Helper()
	h.server.feedbackWorker().wait()
}

// lineEntry is an entry that the line rule applies to, and service adds a
// service so the timing rule applies too.
func lineEntry(key string, more ...string) string {
	return reportEntryJSON(key, append([]string{`"file":"cmd/main.go","line":12,"commit":"abcdef0"`}, more...)...)
}

// mergedTask creates a merged task of app whose latest merge event names a
// merge commit and a pull request.
func (h *linkingHarness) mergedTask(merge string, pull int) string {
	h.t.Helper()
	id := core.NewTaskID() + "-" + merge[:4]
	if err := h.st.CreateTask(h.ctx, core.Task{ID: id, Workspace: "alpha", Repo: "app", Title: "Merged", Body: "Merged.", State: core.TaskMerged, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.AppendEvent(h.ctx, core.Event{TaskID: id, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "pull_request": pull, "merge_commit_sha": merge})}); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *linkingHarness) links(reportID string) map[string]store.FeedbackReportLink {
	h.t.Helper()
	links, err := h.st.ListFeedbackReportLinks(h.ctx, reportID)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]store.FeedbackReportLink{}
	for _, link := range links {
		out[link.TaskID] = link
	}
	return out
}

func (h *linkingHarness) notes(reportID string) []store.FeedbackReportNote {
	h.t.Helper()
	notes, err := h.st.ListFeedbackReportNotes(h.ctx, reportID)
	if err != nil {
		h.t.Fatal(err)
	}
	return notes
}

// TestFeedbackReportAnswersBeforeForgeCall proves that the report route
// answers 202 before any forge call and before any link is worked out: the
// forge holds every call until the answer is read, and the linking job then
// runs, reads the line, links the task, and ends linking
// (component-feedback-reports invariant 15; req-production-feedback AC-5.1).
func TestFeedbackReportAnswersBeforeForgeCall(t *testing.T) {
	h := newLinkingHarness(t)
	task := h.mergedTask(linkingChange, 7)
	gate := make(chan struct{})
	h.forge.mu.Lock()
	h.forge.gate = gate
	h.forge.mu.Unlock()
	response := h.acceptReport(reportBody(lineEntry("crash")))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d", response.Code)
	}
	report := h.reportByKey("crash")
	if report.LinkingEndedAt != nil {
		t.Fatal("linking ended before the answer")
	}
	// The job is held at its first forge call, which came after the answer.
	select {
	case <-h.forge.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the linking job made no forge call")
	}
	if links := h.links(report.ID); len(links) != 0 {
		t.Fatalf("links before the forge answered=%+v", links)
	}
	close(gate)
	h.settle()
	reads, lineReads := h.forge.counts()
	if reads != 1 || lineReads != 1 {
		t.Fatalf("commit reads=%d line reads=%d, want 1 and 1", reads, lineReads)
	}
	links := h.links(report.ID)
	if link, ok := links[task]; len(links) != 1 || !ok || link.State != store.FeedbackReportLinkPossible || len(link.Evidence) != 1 || link.Evidence[0].Commit != linkingChange {
		t.Fatalf("links=%+v, want a possible link to %s with the line evidence", links, task)
	}
	if after := h.reportByKey("crash"); after.LinkingEndedAt == nil {
		t.Fatal("linking did not end")
	}
}

// TestFeedbackReportLinkingOncePerMinute proves that a report gets at most one
// linking attempt a minute, that a saved line read is never asked again, and
// that the timing rule waits for a watcher signal that names the service and
// arrived after the report (component-feedback-reports invariant 15).
func TestFeedbackReportLinkingOncePerMinute(t *testing.T) {
	h := newLinkingHarness(t)
	// A watcher is listening and named only another service.
	h.accept(`{"next_signal_seconds":3600,"services":[{"name":"web","versions":[{"version":"1"}]}]}`)
	h.settle()
	h.acceptReport(reportBody(lineEntry("crash", `"service":"api"`)))
	h.settle()
	report := h.reportByKey("crash")
	if _, lineReads := h.forge.counts(); lineReads != 1 {
		t.Fatalf("line reads=%d, want 1", lineReads)
	}
	read, stored, err := h.st.ReadFeedbackReportLineRead(h.ctx, report.ID)
	if err != nil || !stored || read.Answer.Commit != linkingChange {
		t.Fatalf("stored read=%+v stored=%v err=%v", read, stored, err)
	}
	if report.LinkingEndedAt != nil {
		t.Fatal("linking ended while the timing rule waits for a signal")
	}
	attempt := *report.LinkingLastAttemptAt
	// A later entry 5 seconds later asks again: the claim is refused, so the
	// attempt time stays and no forge call is made.
	h.acceptReport(reportBody(lineEntry("crash", `"service":"api"`)))
	h.settle()
	if after := h.reportByKey("crash"); !after.LinkingLastAttemptAt.Equal(attempt) {
		t.Fatalf("a second attempt within the minute ran at %v", after.LinkingLastAttemptAt)
	}
	// A signal that names another service after the report: the rule still
	// waits, and the saved line read is not asked again.
	h.advance(time.Minute)
	h.accept(`{"next_signal_seconds":3600,"services":[{"name":"web","versions":[{"version":"1"}]}]}`)
	h.settle()
	after := h.reportByKey("crash")
	if after.LinkingEndedAt != nil || after.LinkingLastAttemptAt.Equal(attempt) {
		t.Fatalf("after a web signal=%+v, want a new attempt that still waits", after)
	}
	if reads, lineReads := h.forge.counts(); reads != 1 || lineReads != 1 {
		t.Fatalf("commit reads=%d line reads=%d after a saved answer, want 1 and 1", reads, lineReads)
	}
	// A signal that names the service: the timing rule ends with no deploy
	// that qualifies, and linking ends.
	h.advance(time.Minute)
	h.accept(`{"next_signal_seconds":3600,"services":[{"name":"api","versions":[{"version":"1"}]}]}`)
	h.settle()
	if ended := h.reportByKey("crash"); ended.LinkingEndedAt == nil {
		t.Fatal("linking did not end after a signal that names the service")
	}
	if notes := h.notes(report.ID); len(notes) != 0 {
		t.Fatalf("notes=%+v, want none", notes)
	}
}

// TestFeedbackReportTimingWithoutWatcher proves the exception of invariant 15:
// when no watcher is listening for the environment, the timing rule does not
// wait for a signal.
func TestFeedbackReportTimingWithoutWatcher(t *testing.T) {
	h := newLinkingHarness(t)
	h.acceptReport(reportBody(reportEntryJSON("crash", `"service":"api"`)))
	h.settle()
	if report := h.reportByKey("crash"); report.LinkingEndedAt == nil {
		t.Fatal("linking did not end with no watcher listening")
	}
}

// TestFeedbackReportLineReadsShareTheAllowance proves that a line read is
// admitted against the workspace's 60 requests a minute: with the minute's
// allowance spent, the read waits and no forge line read runs, and a later
// attempt in the next minute reads (component-feedback-reports invariant 15;
// component-production-feedback invariant 28).
func TestFeedbackReportLineReadsShareTheAllowance(t *testing.T) {
	h := newLinkingHarness(t)
	keys := make([]store.FeedbackComparisonKey, store.FeedbackComparisonsPerMinute)
	for i := range keys {
		keys[i] = store.FeedbackComparisonKey{Repository: "acme/app", Base: readsCommit("a"), Head: fmt.Sprintf("%040x", i+1)}
	}
	admitted, err := h.st.AdmitFeedbackComparisons(store.WithActor(h.ctx, store.SystemActor()), keys, h.clock(), 0)
	if err != nil || len(admitted) != store.FeedbackComparisonsPerMinute {
		t.Fatalf("admitted=%d err=%v, want the whole allowance", len(admitted), err)
	}
	h.acceptReport(reportBody(lineEntry("crash")))
	h.settle()
	if _, lineReads := h.forge.counts(); lineReads != 0 {
		t.Fatalf("line reads=%d with the allowance spent, want 0", lineReads)
	}
	report := h.reportByKey("crash")
	if report.LinkingEndedAt != nil {
		t.Fatal("linking ended without the line read")
	}
	h.advance(time.Minute)
	h.server.enqueueFeedbackReportLinking("alpha", report.ID)
	h.settle()
	if _, lineReads := h.forge.counts(); lineReads != 1 {
		t.Fatalf("line reads=%d in the next minute, want 1", lineReads)
	}
	if after := h.reportByKey("crash"); after.LinkingEndedAt == nil {
		t.Fatal("linking did not end after the line read")
	}
}

// TestFeedbackReportLinkingExpiry proves that a rule that has no answer after
// 24 hours ends with not_checked and its reason, and that a forge failure
// stores no line read (component-feedback-reports invariant 15; State and
// failure).
func TestFeedbackReportLinkingExpiry(t *testing.T) {
	h := newLinkingHarness(t)
	h.forge.failure = github.StatusError(http.StatusBadGateway, errors.New("bad gateway"))
	h.acceptReport(reportBody(lineEntry("crash")))
	h.settle()
	report := h.reportByKey("crash")
	if report.LinkingEndedAt != nil {
		t.Fatal("linking ended after a forge failure")
	}
	if _, stored, err := h.st.ReadFeedbackReportLineRead(h.ctx, report.ID); err != nil || stored {
		t.Fatalf("a forge failure stored a line read: stored=%v err=%v", stored, err)
	}
	// One nanosecond short of 24 hours after the report was stored: still
	// waiting.
	h.mu.Lock()
	h.now = report.CreatedAt.Add(store.FeedbackReportLinkingDeadline - time.Nanosecond)
	h.mu.Unlock()
	h.server.enqueueFeedbackReportLinking("alpha", report.ID)
	h.settle()
	if again := h.reportByKey("crash"); again.LinkingEndedAt != nil {
		t.Fatal("linking ended before 24 hours")
	}
	// At exactly 24 hours the forge would answer, but the attempt sends it
	// nothing: the rule ends with not_checked and the reason the last
	// attempt named.
	h.forge.set(func(f *linkingForge) { f.failure = nil })
	before := h.forge.calls()
	h.setNow(report.CreatedAt.Add(store.FeedbackReportLinkingDeadline + store.FeedbackAdmissionLease))
	h.server.enqueueFeedbackReportLinking("alpha", report.ID)
	h.settle()
	if after := h.forge.calls(); after != before {
		t.Fatalf("forge calls at expiry %v, want %v unchanged", after, before)
	}
	if ended := h.reportByKey("crash"); ended.LinkingEndedAt == nil {
		t.Fatal("linking did not end after 24 hours")
	}
	notes := h.notes(report.ID)
	if len(notes) != 1 || notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteNotChecked, Rule: store.FeedbackRuleLine, Reason: string(github.ForgeStatus)}) {
		t.Fatalf("notes=%+v, want not_checked for the line rule with the forge category", notes)
	}
	if links := h.links(report.ID); len(links) != 0 {
		t.Fatalf("links=%+v, want none", links)
	}
}

// TestFeedbackReportLinkingNotes proves the notes of the line rule that the
// job writes: file_not_found for a missing file, saved before it is used,
// and commit_unresolved with the reason for a commit no repository holds.
func TestFeedbackReportLinkingNotes(t *testing.T) {
	h := newLinkingHarness(t)
	h.forge.answer = github.LineChange{FileFound: false}
	h.acceptReport(reportBody(lineEntry("missing")))
	h.settle()
	missing := h.reportByKey("missing")
	if notes := h.notes(missing.ID); missing.LinkingEndedAt == nil || len(notes) != 1 || notes[0].Kind != store.FeedbackNoteFileNotFound || notes[0].Commit != linkingResolved || notes[0].File != "cmd/main.go" {
		t.Fatalf("missing file: report=%+v notes=%+v", missing, notes)
	}
	if read, stored, err := h.st.ReadFeedbackReportLineRead(h.ctx, missing.ID); err != nil || !stored || read.Answer.FileFound {
		t.Fatalf("missing-file read=%+v stored=%v err=%v", read, stored, err)
	}
	h.acceptReport(reportBody(reportEntryJSON("unknown", `"file":"cmd/main.go","line":12,"commit":"1234567"`)))
	h.settle()
	unknown := h.reportByKey("unknown")
	if notes := h.notes(unknown.ID); unknown.LinkingEndedAt == nil || len(notes) != 1 || notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteCommitUnresolved, Reason: store.FeedbackReasonNoMatch}) {
		t.Fatalf("unknown commit: report=%+v notes=%+v", unknown, notes)
	}
}

// TestFeedbackReportLinkingLogs proves that a failed attempt logs the report
// by ID with a failure class and no text of the report or of the failure
// (component-feedback-reports invariant 29).
func TestFeedbackReportLinkingLogs(t *testing.T) {
	if got := feedbackReportErrorClass(github.StatusError(http.StatusBadGateway, errors.New("cmd/main.go secret"))); got != string(github.ForgeStatus) {
		t.Fatalf("forge class=%q", got)
	}
	if got := feedbackReportErrorClass(errors.New("pq: value cmd/main.go")); got != "store_failure" {
		t.Fatalf("store class=%q", got)
	}
	if got := feedbackReportErrorClass(store.ErrRetryable); got != "store_retryable" {
		t.Fatalf("retryable class=%q", got)
	}
	if got := feedbackReportErrorClass(feedback.ErrNoGitHubApp); got != store.FeedbackReasonNoGitHubApp {
		t.Fatalf("no App class=%q", got)
	}
}

func (h *linkingHarness) setNow(at time.Time) {
	h.mu.Lock()
	h.now = at
	h.mu.Unlock()
}

// timingHistory records two deploys of service api, versions 1 and 2, 10 and
// 5 minutes before the harness clock, both resolved in app, and a merged
// task of app whose comparisons with both versions are not stored. The
// signals asked for 10 seconds, so no watcher is listening when a report
// arrives at the clock.
func (h *linkingHarness) timingHistory() {
	h.t.Helper()
	system := store.WithActor(h.ctx, store.SystemActor())
	for i, version := range []string{"1", "2"} {
		at := h.clock().Add(time.Duration(i-2) * 5 * time.Minute)
		signal := store.FeedbackSignal{EnvironmentID: h.environment.ID, KeyID: h.key.ID, ArrivedAt: at, NextSignalSeconds: 10,
			Services: []store.FeedbackSignalService{{Name: "api", Versions: []store.FeedbackSignalVersion{{Version: version}}}}}
		if err := h.st.RecordFeedbackSignal(system, signal); err != nil {
			h.t.Fatal(err)
		}
		resolution := store.FeedbackResolution{FeedbackVersionRef: store.FeedbackVersionRef{EnvironmentID: h.environment.ID, Service: "api", FeedbackVersion: store.FeedbackVersion{Version: version}},
			ResolvedRepository: "app", ResolvedCommit: readsCommit("a" + version), AttemptedAt: at}
		if err := h.st.RecordFeedbackResolution(system, resolution); err != nil {
			h.t.Fatal(err)
		}
	}
	h.mergedTask(readsCommit("c3"), 9)
}

// TestFeedbackReportLinkingCallPreconditions proves each check that comes
// before a call of a linking attempt: a commit read and a line read to
// GitHub, and a request for a comparison. A call is made only when the
// report's linking has not ended, the 24 hours have not passed at the
// attempt's time, the attempt holds the once-a-minute claim, the workspace
// admission admits it, and no retry time GitHub returned is ahead. A rate
// limit stops the other calls of the attempt, and what is unfinished stays
// pending (component-feedback-reports invariant 15;
// component-production-feedback invariant 28).
func TestFeedbackReportLinkingCallPreconditions(t *testing.T) {
	t.Run("all checks pass: every call is made", func(t *testing.T) {
		h := newLinkingHarness(t)
		h.timingHistory()
		h.forge.set(func(f *linkingForge) {
			f.failure = github.StatusError(http.StatusBadGateway, errors.New("bad gateway"))
		})
		h.acceptReport(reportBody(lineEntry("crash", `"service":"api"`)))
		h.settle()
		if calls := h.forge.calls(); calls[0] != 1 || calls[1] != 1 || calls[2] == 0 {
			t.Fatalf("calls=%v, want a commit read, a line read, and comparisons", calls)
		}
	})
	t.Run("linking ended: no call", func(t *testing.T) {
		h := newLinkingHarness(t)
		h.timingHistory()
		h.acceptReport(reportBody(reportEntryJSON("plain")))
		h.settle()
		report := h.reportByKey("plain")
		if report.LinkingEndedAt == nil {
			t.Fatal("linking of a report no rule applies to did not end")
		}
		before := h.forge.calls()
		h.advance(time.Hour)
		h.server.enqueueFeedbackReportLinking("alpha", report.ID)
		h.settle()
		if after := h.forge.calls(); after != before {
			t.Fatalf("calls after the end %v, want %v", after, before)
		}
	})
	t.Run("expired: no call and no comparison request, stored answers used", func(t *testing.T) {
		h := newLinkingHarness(t)
		h.timingHistory()
		task := h.mergedTask(linkingChange, 7)
		h.acceptReport(reportBody(lineEntry("crash", `"service":"api"`)))
		h.settle()
		report := h.reportByKey("crash")
		if _, stored, _ := h.st.ReadFeedbackReportLineRead(h.ctx, report.ID); !stored {
			t.Fatal("the line read was not stored")
		}
		before := h.forge.calls()
		h.setNow(report.CreatedAt.Add(store.FeedbackReportLinkingDeadline))
		h.server.enqueueFeedbackReportLinking("alpha", report.ID)
		h.settle()
		if after := h.forge.calls(); after != before {
			t.Fatalf("calls at expiry %v, want %v unchanged: no read and no comparison request", after, before)
		}
		ended := h.reportByKey("crash")
		notes := h.notes(report.ID)
		if ended.LinkingEndedAt == nil || len(notes) != 1 || notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteNotChecked, Rule: store.FeedbackRuleTiming, Reason: "comparison_missing"}) {
			t.Fatalf("report=%+v notes=%+v, want not_checked for the timing rule", ended, notes)
		}
		if links := h.links(report.ID); len(links) != 1 || links[task].State != store.FeedbackReportLinkPossible {
			t.Fatalf("links=%+v, want the stored line read's link", links)
		}
	})
	t.Run("claim within the minute: no call", func(t *testing.T) {
		h := newLinkingHarness(t)
		h.timingHistory()
		h.forge.set(func(f *linkingForge) {
			f.failure = github.StatusError(http.StatusBadGateway, errors.New("bad gateway"))
		})
		h.acceptReport(reportBody(lineEntry("crash", `"service":"api"`)))
		h.settle()
		report := h.reportByKey("crash")
		before := h.forge.calls()
		h.advance(59 * time.Second)
		h.server.enqueueFeedbackReportLinking("alpha", report.ID)
		h.settle()
		if after := h.forge.calls(); after != before {
			t.Fatalf("calls within the minute %v, want %v", after, before)
		}
	})
	t.Run("admission refuses: no commit read and no line read", func(t *testing.T) {
		h := newLinkingHarness(t)
		keys := make([]store.FeedbackComparisonKey, store.FeedbackComparisonsPerMinute)
		for i := range keys {
			keys[i] = store.FeedbackComparisonKey{Repository: "acme/app", Base: readsCommit("a"), Head: fmt.Sprintf("%040x", i+1)}
		}
		if admitted, err := h.st.AdmitFeedbackComparisons(store.WithActor(h.ctx, store.SystemActor()), keys, h.clock(), 0); err != nil || len(admitted) != len(keys) {
			t.Fatalf("admitted=%d err=%v", len(admitted), err)
		}
		h.acceptReport(reportBody(lineEntry("crash")))
		h.settle()
		if calls := h.forge.calls(); calls != [3]int{} {
			t.Fatalf("calls with the allowance spent %v, want none", calls)
		}
	})
	t.Run("a rate-limited commit read stops the line read and the comparison request", func(t *testing.T) {
		h := newLinkingHarness(t)
		h.timingHistory()
		h.forge.set(func(f *linkingForge) {
			f.readFailure = github.RateLimitedError(time.Time{}, errors.New("rate limit"))
		})
		h.acceptReport(reportBody(lineEntry("crash", `"service":"api"`)))
		h.settle()
		if calls := h.forge.calls(); calls != [3]int{1, 0, 0} {
			t.Fatalf("calls=%v, want one commit read only", calls)
		}
	})
	t.Run("a rate-limited comparison holds the next attempt", func(t *testing.T) {
		h := newLinkingHarness(t)
		h.timingHistory()
		h.forge.set(func(f *linkingForge) {
			f.compareFailure = github.RateLimitedError(h.clock().Add(10*time.Minute), errors.New("rate limit"))
		})
		h.acceptReport(reportBody(reportEntryJSON("timing", `"service":"api"`)))
		h.settle()
		report := h.reportByKey("timing")
		first := h.forge.calls()
		if first[2] != 1 {
			t.Fatalf("calls=%v, want one comparison that GitHub rate-limited", first)
		}
		h.advance(5 * time.Minute)
		h.server.enqueueFeedbackReportLinking("alpha", report.ID)
		h.settle()
		if after := h.forge.calls(); after != first {
			t.Fatalf("calls before the retry time %v, want %v: no comparison request", after, first)
		}
		h.advance(5*time.Minute + time.Second)
		h.forge.set(func(f *linkingForge) { f.compareFailure = nil })
		h.server.enqueueFeedbackReportLinking("alpha", report.ID)
		h.settle()
		if after := h.forge.calls(); after[2] <= first[2] {
			t.Fatalf("calls after the retry time %v, want more comparisons", after)
		}
	})
}

// TestFeedbackReportLineReadRetryLongerThanTheLease proves that a retry time
// GitHub returns for a line read holds every later request of the workspace
// until it passes, also after the admission lease of the read has ended
// (component-production-feedback invariant 28).
func TestFeedbackReportLineReadRetryLongerThanTheLease(t *testing.T) {
	h := newLinkingHarness(t)
	retryAt := h.clock().Add(5 * time.Minute)
	h.forge.set(func(f *linkingForge) { f.failure = github.RateLimitedError(retryAt, errors.New("rate limit")) })
	h.acceptReport(reportBody(lineEntry("crash")))
	h.settle()
	report := h.reportByKey("crash")
	first := h.forge.calls()
	if first != [3]int{1, 1, 0} {
		t.Fatalf("calls=%v, want a commit read and a line read", first)
	}
	if _, stored, _ := h.st.ReadFeedbackReportLineRead(h.ctx, report.ID); stored {
		t.Fatal("a rate-limited read stored an answer")
	}
	h.forge.set(func(f *linkingForge) { f.failure = nil })
	// After the 2-minute admission lease and before the retry time.
	h.setNow(retryAt.Add(-time.Nanosecond))
	if !h.clock().After(report.CreatedAt.Add(store.FeedbackAdmissionLease)) {
		t.Fatal("the test clock is not past the admission lease")
	}
	h.server.enqueueFeedbackReportLinking("alpha", report.ID)
	h.settle()
	if after := h.forge.calls(); after != first {
		t.Fatalf("calls before the retry time %v, want %v", after, first)
	}
	if again := h.reportByKey("crash"); again.LinkingEndedAt != nil {
		t.Fatal("linking ended while held")
	}
	h.setNow(retryAt.Add(time.Minute))
	h.server.enqueueFeedbackReportLinking("alpha", report.ID)
	h.settle()
	if after := h.forge.calls(); after[1] != 2 {
		t.Fatalf("calls after the retry time %v, want a second line read", after)
	}
	if ended := h.reportByKey("crash"); ended.LinkingEndedAt == nil {
		t.Fatal("linking did not end after the retry time")
	}
}

// useRepositories replaces the workspace's repositories: app on acme/app,
// other on acme/other, and docs with no GitHub slug.
func (h *linkingHarness) useRepositories() {
	h.t.Helper()
	current, err := h.st.WorkspaceConfig(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.UpdateWorkspaceConfig(h.ctx, current.Version, &config.Config{Workspace: "alpha", Repos: []config.Repo{
		{Name: "app", URL: "https://example.test/app", Base: "main", GitHub: "acme/app"},
		{Name: "other", URL: "https://example.test/other", Base: "main", GitHub: "acme/other"},
		{Name: "docs", URL: "https://example.test/docs", Base: "main"},
	}}); err != nil {
		h.t.Fatal(err)
	}
}

// ambiguousForge resolves abcdef0 in every repository.
type ambiguousForge struct{ *linkingForge }

func (f ambiguousForge) ReadCommit(ctx context.Context, _, ref string) (string, error) {
	return f.linkingForge.ReadCommit(ctx, "acme/app", ref)
}

// TestFeedbackReportResolveOutcomes is the table of the reasons
// feedback.Resolve answers for the first entry of a report, and what the line
// rule does with each: a resolved commit goes on to the line read, a
// definitive reason ends the rule with commit_unresolved, and every other
// reason leaves it unanswered with nothing stored (component-feedback-reports
// invariant 12; component-production-feedback invariant 7). The reason
// pending is never answered by Resolve; reports.TestLineNotes proves the rule
// waits for it.
func TestFeedbackReportResolveOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		entry    string
		rule     *store.EnvironmentVersionRule
		failure  error
		forge    func(*linkingForge) feedback.Forge
		resolved bool
		// note is the commit_unresolved reason, and waiting the reason the
		// unanswered rule names.
		note    string
		waiting string
	}{
		{name: "R01 resolved", entry: `"commit":"abcdef0"`, resolved: true},
		{name: "R02 no_repository: the named repository has no slug", entry: `"repository":"docs","commit":"abcdef0"`, note: store.FeedbackReasonNoRepository},
		{name: "R03 no_version_rule: a version and no rule", entry: `"version":"1.2.3"`, note: store.FeedbackReasonNoVersionRule},
		{name: "R04 version_rule_mismatch", entry: `"version":"1.2.3"`, rule: &store.EnvironmentVersionRule{Kind: store.EnvironmentCommitPattern, Text: "build-{commit}"}, note: store.FeedbackReasonRuleMismatch},
		{name: "R05 no_match: no repository holds the commit", entry: `"commit":"1234567"`, note: store.FeedbackReasonNoMatch},
		{name: "R06 ambiguous: two repositories hold the commit", entry: `"commit":"abcdef0"`, forge: func(f *linkingForge) feedback.Forge { return ambiguousForge{f} }, note: store.FeedbackReasonAmbiguous},
		{name: "R07 no_github_app", entry: `"commit":"abcdef0"`, failure: fmt.Errorf("%w: no installation", feedback.ErrNoGitHubApp), waiting: store.FeedbackReasonNoGitHubApp},
		{name: "R08 forge_status", entry: `"commit":"abcdef0"`, failure: github.StatusError(http.StatusBadGateway, errors.New("bad gateway")), waiting: string(github.ForgeStatus)},
		{name: "R09 forge_permission", entry: `"commit":"abcdef0"`, failure: github.PermissionError(errors.New("no access")), waiting: string(github.ForgePermission)},
		{name: "R10 forge_rate_limited", entry: `"commit":"abcdef0"`, failure: github.RateLimitedError(time.Time{}, errors.New("rate limit")), waiting: string(github.ForgeRateLimited)},
		{name: "R11 forge_request", entry: `"commit":"abcdef0"`, failure: &github.Error{Category: github.ForgeRequest, Err: errors.New("connection reset")}, waiting: string(github.ForgeRequest)},
		{name: "R12 forge_response", entry: `"commit":"abcdef0"`, failure: &github.Error{Category: github.ForgeResponse, Err: errors.New("malformed")}, waiting: string(github.ForgeResponse)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLinkingHarness(t)
			h.useRepositories()
			if tc.forge != nil {
				h.server.feedbackForge = tc.forge(h.forge)
			}
			if tc.rule != nil {
				if _, err := h.st.UpdateEnvironment(h.ctx, h.environment.ID, store.EnvironmentPatch{VersionRuleSet: true, VersionRule: tc.rule}); err != nil {
					t.Fatal(err)
				}
			}
			h.forge.set(func(f *linkingForge) { f.readFailure = tc.failure })
			h.acceptReport(reportBody(reportEntryJSON("crash", `"file":"cmd/main.go","line":12`, tc.entry)))
			h.settle()
			report := h.reportByKey("crash")
			_, stored, err := h.st.ReadFeedbackReportLineRead(h.ctx, report.ID)
			if err != nil {
				t.Fatal(err)
			}
			calls := h.forge.calls()
			switch {
			case tc.resolved:
				if calls[1] != 1 || !stored || report.LinkingEndedAt == nil {
					t.Fatalf("calls=%v stored=%v report=%+v, want the line read sent, stored, and linking ended", calls, stored, report)
				}
			case tc.note != "":
				notes := h.notes(report.ID)
				if calls[1] != 0 || stored || report.LinkingEndedAt == nil || len(notes) != 1 || notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteCommitUnresolved, Reason: tc.note}) {
					t.Fatalf("calls=%v stored=%v report=%+v notes=%+v, want commit_unresolved %s", calls, stored, report, notes, tc.note)
				}
			default:
				reason := h.server.feedbackWorker().backoff.lineReason("alpha/" + report.ID)
				if calls[1] != 0 || stored || report.LinkingEndedAt != nil || len(h.notes(report.ID)) != 0 || reason != tc.waiting {
					t.Fatalf("calls=%v stored=%v ended=%v reason=%q, want unanswered with %s and nothing stored", calls, stored, report.LinkingEndedAt != nil, reason, tc.waiting)
				}
			}
		})
	}
}

// TestFeedbackReportLineReadOutcomes is the table of what the linking job
// does with each outcome of the line read: an answer and a missing file are
// stored, and every forge failure, an absent commit included, leaves the rule
// unanswered with its category and stores nothing. A rate-limited failure
// also keeps its retry time, or a minute when GitHub named none
// (component-feedback-reports invariants 12 and 15). TestReadLineChange maps
// each GitHub answer shape to one of these outcomes.
func TestFeedbackReportLineReadOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  github.LineChange
		failure error
		waiting string
		hold    time.Duration
	}{
		{name: "answer", answer: github.LineChange{FileFound: true, Commit: linkingChange, PullRequests: []int{}}},
		{name: "file_not_found", answer: github.LineChange{FileFound: false}},
		{name: "commit absent", failure: github.CommitNotFoundError(linkingResolved), waiting: string(github.ForgeStatus)},
		{name: "forge_status", failure: github.StatusError(http.StatusBadGateway, errors.New("bad gateway")), waiting: string(github.ForgeStatus)},
		{name: "forge_permission", failure: github.PermissionError(errors.New("no access")), waiting: string(github.ForgePermission)},
		{name: "forge_request", failure: &github.Error{Category: github.ForgeRequest, Err: errors.New("reset")}, waiting: string(github.ForgeRequest)},
		{name: "forge_response", failure: &github.Error{Category: github.ForgeResponse, Err: errors.New("partial")}, waiting: string(github.ForgeResponse)},
		{name: "forge_rate_limited with a retry time", failure: github.RateLimitedError(time.Date(2026, 11, 25, 12, 10, 0, 0, time.UTC), errors.New("rate limit")), waiting: string(github.ForgeRateLimited), hold: -1},
		{name: "forge_rate_limited with no retry time", failure: github.RateLimitedError(time.Time{}, errors.New("rate limit")), waiting: string(github.ForgeRateLimited), hold: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLinkingHarness(t)
			h.forge.set(func(f *linkingForge) { f.answer, f.failure = tc.answer, tc.failure })
			h.acceptReport(reportBody(lineEntry("crash")))
			h.settle()
			report := h.reportByKey("crash")
			read, stored, err := h.st.ReadFeedbackReportLineRead(h.ctx, report.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.failure == nil {
				if !stored || read.Answer.FileFound != tc.answer.FileFound || read.Answer.Commit != tc.answer.Commit || report.LinkingEndedAt == nil {
					t.Fatalf("stored=%v read=%+v ended=%v, want the answer stored and linking ended", stored, read, report.LinkingEndedAt != nil)
				}
				return
			}
			reason := h.server.feedbackWorker().backoff.lineReason("alpha/" + report.ID)
			if stored || report.LinkingEndedAt != nil || reason != tc.waiting {
				t.Fatalf("stored=%v ended=%v reason=%q, want unanswered with %s", stored, report.LinkingEndedAt != nil, reason, tc.waiting)
			}
			until, held := h.server.feedbackWorker().backoff.forgeHold("alpha", h.clock())
			switch {
			case tc.hold == 0 && held:
				t.Fatalf("a %s failure held the workspace until %v", tc.name, until)
			case tc.hold < 0:
				if retryAt, _ := github.RateLimitRetryAt(tc.failure); !held || !until.Equal(retryAt) {
					t.Fatalf("hold=%v held=%v, want GitHub's retry time %v", until, held, retryAt)
				}
			case tc.hold > 0 && (!held || !until.Equal(h.clock().Add(tc.hold))):
				t.Fatalf("hold=%v held=%v, want %v after now", until, held, tc.hold)
			}
		})
	}
}
