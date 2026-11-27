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
	answer    github.LineChange
	failure   error
	gate      chan struct{}
	entered   chan struct{}
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
	if slug == "acme/app" && strings.HasPrefix(linkingResolved, ref) {
		return linkingResolved, nil
	}
	return "", github.CommitNotFoundError(ref)
}

func (f *linkingForge) Compare(context.Context, string, string, string, int) (feedback.CompareResult, error) {
	return feedback.CompareResult{}, errors.New("this forge stub compares nothing")
}

func (f *linkingForge) ReadLineChange(_ context.Context, slug, commit, path string, line int) (github.LineChange, error) {
	f.wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lineReads++
	if slug != "acme/app" || commit != linkingResolved || path != "cmd/main.go" || line != 12 {
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
	h.mu.Lock()
	// The admission lease of the failed read has passed, so this attempt
	// reads again and fails with the same category.
	h.now = report.CreatedAt.Add(store.FeedbackReportLinkingDeadline + store.FeedbackAdmissionLease + time.Second)
	h.mu.Unlock()
	h.server.enqueueFeedbackReportLinking("alpha", report.ID)
	h.settle()
	if ended := h.reportByKey("crash"); ended.LinkingEndedAt == nil {
		t.Fatal("linking did not end after 24 hours")
	}
	notes := h.notes(report.ID)
	if len(notes) != 1 || notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteNotChecked, Rule: store.FeedbackRuleLine, Reason: string(github.ForgeStatus)}) {
		t.Fatalf("notes=%+v, want not_checked for the line rule with the forge category", notes)
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
