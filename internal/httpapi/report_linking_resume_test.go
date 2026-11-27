package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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

// freshWorker replaces the background worker with a new one that holds no
// memory: no queue, no retry hold, and no remembered reason. A linking
// attempt then continues from the store alone, as after a restart.
func (h *linkingHarness) freshWorker() {
	h.t.Helper()
	h.settle()
	h.server.feedbackWorkOnce = sync.Once{}
	h.server.feedbackWork = nil
}

// attempt queues one linking attempt of the report and waits for it.
func (h *linkingHarness) attempt(reportID string) {
	h.t.Helper()
	h.server.enqueueFeedbackReportLinking("alpha", reportID)
	h.settle()
}

func (h *linkingHarness) reportByID(reportID string) store.FeedbackReport {
	h.t.Helper()
	for _, report := range h.reports() {
		if report.ID == reportID {
			return report
		}
	}
	h.t.Fatalf("no report %s", reportID)
	return store.FeedbackReport{}
}

// finalState renders what the end of linking wrote for a report: each link's
// task, state, and evidence, the notes, and the linked events with the tasks
// they name. Two runs that wrote the same render the same.
func (h *linkingHarness) finalState(reportID string) string {
	h.t.Helper()
	type link struct {
		Task     string
		State    string
		Evidence []store.FeedbackReportEvidence
	}
	links := []link{}
	stored, err := h.st.ListFeedbackReportLinks(h.ctx, reportID)
	if err != nil {
		h.t.Fatal(err)
	}
	// Task and deploy IDs are drawn at random in each run, so a task is named
	// by the merge-commit suffix of its fixture ID and a deploy by its other
	// members.
	canonical := func(id string) string { return id[strings.LastIndex(id, "-")+1:] }
	for _, l := range stored {
		for i := range l.Evidence {
			if l.Evidence[i].Deploy != nil {
				deploy := *l.Evidence[i].Deploy
				deploy.ID = ""
				l.Evidence[i].Deploy = &deploy
			}
		}
		links = append(links, link{Task: canonical(l.TaskID), State: l.State, Evidence: l.Evidence})
	}
	slices.SortFunc(links, func(a, b link) int { return compareTaskIDs(a.Task, b.Task) })
	events, err := h.st.ListEvents(h.ctx, "")
	if err != nil {
		h.t.Fatal(err)
	}
	linked := []string{}
	for _, event := range events {
		if event.Kind != store.FeedbackReportLinkedEvent {
			continue
		}
		var payload struct {
			ReportID string   `json:"report_id"`
			TaskIDs  []string `json:"task_ids"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			h.t.Fatal(err)
		}
		if payload.ReportID == reportID {
			tasks := []string{}
			for _, id := range payload.TaskIDs {
				tasks = append(tasks, canonical(id))
			}
			slices.Sort(tasks)
			linked = append(linked, fmt.Sprint(tasks))
		}
	}
	notes := h.notes(reportID)
	for i := range notes {
		if notes[i].Deploy != nil {
			deploy := *notes[i].Deploy
			deploy.ID = ""
			notes[i].Deploy = &deploy
		}
	}
	raw, err := json.Marshal([]any{links, notes, linked})
	if err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

func compareTaskIDs(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// TestFeedbackReportResolveAcrossTheAllowance proves review round 2 finding
// 1: with 61 candidate repositories, the first attempt spends the whole
// allowance on 60 candidate reads and stores each answer. A fresh worker in
// the next minute reads only the one candidate left, stores the unique
// match, and completes the line read with the next slot. No repository is
// read twice (req-production-feedback AC-5.1; component-feedback-reports v2
// invariants 12 and 15).
func TestFeedbackReportResolveAcrossTheAllowance(t *testing.T) {
	setup := func(t *testing.T) (*linkingHarness, store.FeedbackReport, string) {
		h := newLinkingHarness(t)
		repos := []config.Repo{}
		for i := range 61 {
			name := fmt.Sprintf("r%02d", i)
			repos = append(repos, config.Repo{Name: name, URL: "https://example.test/" + name, Base: "main", GitHub: "acme/" + name})
		}
		current, err := h.st.WorkspaceConfig(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.st.UpdateWorkspaceConfig(h.ctx, current.Version, &config.Config{Workspace: "alpha", Repos: repos}); err != nil {
			t.Fatal(err)
		}
		h.forge.set(func(f *linkingForge) { f.commits = map[string]string{"acme/r37": linkingResolved} })
		task := core.NewTaskID() + "-r37"
		if err := h.st.CreateTask(h.ctx, core.Task{ID: task, Workspace: "alpha", Repo: "r37", Title: "Merged", Body: "Merged.", State: core.TaskMerged, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := h.st.AppendEvent(h.ctx, core.Event{TaskID: task, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "acme/r37", "pull_request": 5, "merge_commit_sha": linkingChange})}); err != nil {
			t.Fatal(err)
		}
		h.acceptReport(reportBody(lineEntry("crash")))
		h.settle()
		report := h.reportByKey("crash")
		calls := h.forge.calls()
		if calls[0] != store.FeedbackComparisonsPerMinute || calls[1] != 0 || report.LinkingEndedAt != nil {
			t.Fatalf("first attempt calls=%v ended=%v, want 60 commit reads and no line read", calls, report.LinkingEndedAt != nil)
		}
		if report.LineProgress == nil || len(report.LineProgress.Candidates) != store.FeedbackComparisonsPerMinute || report.LineProgress.Ended() {
			t.Fatalf("stored progress=%+v, want 60 candidate answers", report.LineProgress)
		}
		return h, report, task
	}
	t.Run("a fresh worker finishes resolution and the line read", func(t *testing.T) {
		h, report, task := setup(t)
		h.freshWorker()
		h.advance(time.Minute + time.Second)
		h.attempt(report.ID)
		calls := h.forge.calls()
		if calls[0] != 61 || calls[1] != 1 || !slices.Equal(h.forge.lineSlugs, []string{"acme/r37"}) {
			t.Fatalf("calls=%v line slugs=%v, want 61 commit reads and one line read of acme/r37", calls, h.forge.lineSlugs)
		}
		for slug, reads := range h.forge.readsBySlug {
			if reads != 1 {
				t.Fatalf("%s was read %d times", slug, reads)
			}
		}
		ended := h.reportByID(report.ID)
		if ended.LinkingEndedAt == nil || ended.LineProgress.Repository != "r37" || ended.LineProgress.Slug != "acme/r37" || ended.LineProgress.Commit != linkingResolved {
			t.Fatalf("report=%+v progress=%+v", ended, ended.LineProgress)
		}
		if links := h.links(report.ID); len(links) != 1 || links[task].State != store.FeedbackReportLinkPossible {
			t.Fatalf("links=%+v", links)
		}
	})
	t.Run("expiry in the middle of resolution sends nothing", func(t *testing.T) {
		h, report, _ := setup(t)
		h.freshWorker()
		before := h.forge.calls()
		h.setNow(report.CreatedAt.Add(store.FeedbackReportLinkingDeadline))
		h.attempt(report.ID)
		if after := h.forge.calls(); after != before {
			t.Fatalf("calls at expiry %v, want %v", after, before)
		}
		notes := h.notes(report.ID)
		if len(notes) != 1 || notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteNotChecked, Rule: store.FeedbackRuleLine, Reason: feedbackWaitingLineUnanswered}) {
			t.Fatalf("notes=%+v", notes)
		}
	})
}

// TestFeedbackReportUnresolvedSurvivesAFreshWorker proves review round 2
// finding 2: after no_match while a watcher keeps the timing rule waiting,
// a fresh worker sends nothing for the line rule, and at expiry it ends the
// rule with commit_unresolved, not not_checked.
func TestFeedbackReportUnresolvedSurvivesAFreshWorker(t *testing.T) {
	h := newLinkingHarness(t)
	h.accept(`{"next_signal_seconds":86400,"services":[{"name":"web","versions":[{"version":"1"}]}]}`)
	h.settle()
	h.acceptReport(reportBody(reportEntryJSON("crash", `"file":"cmd/main.go","line":12,"commit":"1234567","service":"api"`)))
	h.settle()
	report := h.reportByKey("crash")
	first := h.forge.calls()
	if first != [3]int{1, 0, 0} || report.LinkingEndedAt != nil || report.LineProgress == nil || report.LineProgress.Unresolved != store.FeedbackReasonNoMatch {
		t.Fatalf("calls=%v report=%+v progress=%+v, want one read, no_match stored, and linking pending", first, report, report.LineProgress)
	}
	h.freshWorker()
	h.advance(time.Minute + time.Second)
	h.attempt(report.ID)
	if calls := h.forge.calls(); calls != first {
		t.Fatalf("calls after a fresh worker %v, want %v", calls, first)
	}
	h.freshWorker()
	h.setNow(report.CreatedAt.Add(store.FeedbackReportLinkingDeadline))
	h.attempt(report.ID)
	if calls := h.forge.calls(); calls != first {
		t.Fatalf("calls at expiry %v, want %v", calls, first)
	}
	ended := h.reportByID(report.ID)
	notes := h.notes(report.ID)
	if ended.LinkingEndedAt == nil || len(notes) != 2 ||
		notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteCommitUnresolved, Reason: store.FeedbackReasonNoMatch}) ||
		notes[1] != (store.FeedbackReportNote{Kind: store.FeedbackNoteNotChecked, Rule: store.FeedbackRuleTiming, Reason: "no_signal_after_report"}) {
		t.Fatalf("report=%+v notes=%+v", ended, notes)
	}
}

// TestFeedbackReportRetryableFailuresStoreNothing proves that a retryable
// failure leaves both progress fields as they were: a failed commit read
// stores no candidate answer, and a failed line read after a stored
// resolution stores no line read and changes nothing else.
func TestFeedbackReportRetryableFailuresStoreNothing(t *testing.T) {
	h := newLinkingHarness(t)
	h.forge.set(func(f *linkingForge) {
		f.readFailure = github.StatusError(http.StatusBadGateway, errors.New("bad gateway"))
	})
	h.acceptReport(reportBody(lineEntry("crash", `"service":"api"`)))
	h.settle()
	report := h.reportByKey("crash")
	if report.LineProgress != nil || report.LinkingEndedAt != nil {
		t.Fatalf("a failed commit read stored %+v", report.LineProgress)
	}
	// No watcher listens and no deploy qualifies, so the timing rule ends with
	// an empty answer, which is stored.
	if report.TimingResult == nil || len(report.TimingResult.Links) != 0 || len(report.TimingResult.Notes) != 0 {
		t.Fatalf("timing result=%+v, want an empty stored answer", report.TimingResult)
	}
	h.forge.set(func(f *linkingForge) {
		f.readFailure = nil
		f.failure = github.StatusError(http.StatusBadGateway, errors.New("bad gateway"))
	})
	h.freshWorker()
	h.advance(store.FeedbackAdmissionLease + time.Second)
	h.attempt(report.ID)
	after := h.reportByID(report.ID)
	// The resolution is final and drops the interim candidate answers.
	if after.LineProgress == nil || after.LineProgress.Commit != linkingResolved || len(after.LineProgress.Candidates) != 0 || after.LineProgress.Input != "" {
		t.Fatalf("progress=%+v, want the resolution stored alone", after.LineProgress)
	}
	if _, stored, _ := h.st.ReadFeedbackReportLineRead(h.ctx, report.ID); stored {
		t.Fatal("a failed line read stored an answer")
	}
	h.freshWorker()
	h.advance(store.FeedbackAdmissionLease + time.Second)
	h.attempt(report.ID)
	if again := h.reportByID(report.ID); !jsonEqual(t, again.LineProgress, after.LineProgress) || !jsonEqual(t, again.TimingResult, after.TimingResult) {
		t.Fatalf("a second failure changed the progress: %+v %+v", again.LineProgress, again.TimingResult)
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(left) == string(right)
}

// resumeSetup is a report that both rules apply to: the line read links the
// task merged at linkingChange, and the timing rule links the task merged at
// c3, which version 2 newly holds. No watcher listens.
func resumeSetup(t *testing.T, fileFound bool, lineFailure error) (*linkingHarness, store.FeedbackReport) {
	h := newLinkingHarness(t)
	h.timingHistory()
	h.mergedTask(linkingChange, 7)
	h.forge.set(func(f *linkingForge) {
		f.statuses = map[string]string{
			readsCommit("c3") + "|" + readsCommit("a2"): store.FeedbackCompareAhead, readsCommit("c3") + "|" + readsCommit("a1"): store.FeedbackCompareBehind,
			linkingChange + "|" + readsCommit("a2"): store.FeedbackCompareBehind, linkingChange + "|" + readsCommit("a1"): store.FeedbackCompareBehind,
		}
		if !fileFound {
			f.answer = github.LineChange{FileFound: false}
		}
		// The failure is set before the report is sent, so the first line
		// read already meets it. The first commit read is not affected.
		f.failure = lineFailure
	})
	h.acceptReport(reportBody(lineEntry("crash", `"service":"api"`)))
	return h, h.reportByKey("crash")
}

// TestFeedbackReportResumesFromTheStore is the table of intermediate results
// of both rules. Each case stops after one result is stored, starts a fresh
// worker with empty memory, and checks that no stored result is asked for
// again and that the end of linking writes the same links, notes, and linked
// event as an uninterrupted run (approved plan version 3).
func TestFeedbackReportResumesFromTheStore(t *testing.T) {
	uninterrupted := func(t *testing.T, fileFound bool) string {
		h, report := resumeSetup(t, fileFound, nil)
		h.settle()
		h.advance(time.Minute + time.Second)
		h.attempt(report.ID)
		if ended := h.reportByID(report.ID); ended.LinkingEndedAt == nil {
			t.Fatal("the uninterrupted run did not end")
		}
		if calls := h.forge.calls(); calls != [3]int{1, 1, 4} {
			t.Fatalf("uninterrupted calls=%v", calls)
		}
		return h.finalState(report.ID)
	}
	want := uninterrupted(t, true)
	wantMissing := uninterrupted(t, false)

	t.Run("after the resolution is stored", func(t *testing.T) {
		h, report := resumeSetup(t, true, github.StatusError(http.StatusBadGateway, errors.New("bad gateway")))
		h.settle()
		stored := h.reportByID(report.ID)
		if stored.LineProgress == nil || stored.LineProgress.Commit != linkingResolved || h.forge.calls() != [3]int{1, 1, 4} {
			t.Fatalf("progress=%+v calls=%v", stored.LineProgress, h.forge.calls())
		}
		h.forge.set(func(f *linkingForge) { f.failure = nil })
		h.freshWorker()
		h.advance(store.FeedbackAdmissionLease + time.Second)
		h.attempt(report.ID)
		if calls := h.forge.calls(); calls != [3]int{1, 2, 4} {
			t.Fatalf("calls=%v, want no second commit read and no second comparison", calls)
		}
		if got := h.finalState(report.ID); got != want {
			t.Fatalf("final=%s\nwant  %s", got, want)
		}
	})
	t.Run("after the line read and the comparisons are stored", func(t *testing.T) {
		h, report := resumeSetup(t, true, nil)
		h.settle()
		h.freshWorker()
		h.advance(time.Minute + time.Second)
		h.attempt(report.ID)
		if calls := h.forge.calls(); calls != [3]int{1, 1, 4} {
			t.Fatalf("calls=%v, want nothing asked twice", calls)
		}
		if got := h.finalState(report.ID); got != want {
			t.Fatalf("final=%s\nwant  %s", got, want)
		}
	})
	t.Run("after a missing file is stored", func(t *testing.T) {
		h, report := resumeSetup(t, false, nil)
		h.settle()
		if _, stored, _ := h.st.ReadFeedbackReportLineRead(h.ctx, report.ID); !stored {
			t.Fatal("the missing file was not stored")
		}
		h.freshWorker()
		h.advance(time.Minute + time.Second)
		h.attempt(report.ID)
		if calls := h.forge.calls(); calls != [3]int{1, 1, 4} {
			t.Fatalf("calls=%v, want nothing asked twice", calls)
		}
		if got := h.finalState(report.ID); got != wantMissing {
			t.Fatalf("final=%s\nwant  %s", got, wantMissing)
		}
	})
	t.Run("after the timing result is stored while the line rule waits", func(t *testing.T) {
		h, report := resumeSetup(t, true, github.StatusError(http.StatusBadGateway, errors.New("bad gateway")))
		h.settle()
		h.freshWorker()
		h.advance(store.FeedbackAdmissionLease + time.Second)
		h.attempt(report.ID)
		stored := h.reportByID(report.ID)
		if stored.TimingResult == nil || len(stored.TimingResult.Links) != 1 || stored.LinkingEndedAt != nil {
			t.Fatalf("timing result=%+v ended=%v", stored.TimingResult, stored.LinkingEndedAt != nil)
		}
		// A watcher now listens and names another service: working the timing
		// rule out again would wait, but the stored answer is reused.
		h.accept(`{"next_signal_seconds":86400,"services":[{"name":"web","versions":[{"version":"1"}]}]}`)
		h.settle()
		h.forge.set(func(f *linkingForge) { f.failure = nil })
		h.freshWorker()
		h.advance(store.FeedbackAdmissionLease + time.Second)
		h.attempt(report.ID)
		if calls := h.forge.calls(); calls[0] != 1 || calls[2] != 4 {
			t.Fatalf("calls=%v, want one commit read and four comparisons in all", calls)
		}
		if got := h.finalState(report.ID); got != want {
			t.Fatalf("final=%s\nwant  %s", got, want)
		}
	})
	t.Run("the timing result through later inputs", func(t *testing.T) {
		h, report := resumeSetup(t, true, github.StatusError(http.StatusBadGateway, errors.New("bad gateway")))
		h.settle()
		h.freshWorker()
		h.advance(store.FeedbackAdmissionLease + time.Second)
		h.attempt(report.ID)
		stored := h.reportByID(report.ID)
		if stored.TimingResult == nil || len(stored.TimingResult.Links) != 1 || stored.LinkingEndedAt != nil {
			t.Fatalf("timing result=%+v ended=%v", stored.TimingResult, stored.LinkingEndedAt != nil)
		}
		// Every input of the timing rule changes: a one-minute window, which
		// leaves first_seen outside it, a late signal that deploys version 3
		// and makes a watcher listen, a task merged after that, and a changed
		// version rule. Working the rule out again would differ; the stored
		// answer is reused.
		window := 1
		if _, err := h.st.UpdateEnvironment(h.ctx, h.environment.ID, store.EnvironmentPatch{TimingWindowMinutes: &window}); err != nil {
			t.Fatal(err)
		}
		h.recordSignal(h.clock(), "3")
		h.mergedTask(readsCommit("c4"), 11)
		h.setRule(tagRule("v{version}"))
		h.forge.set(func(f *linkingForge) { f.failure = nil })
		h.freshWorker()
		h.advance(store.FeedbackAdmissionLease + time.Second)
		h.attempt(report.ID)
		if calls := h.forge.calls(); calls[0] != 1 || calls[2] != 4 {
			t.Fatalf("calls=%v, want one commit read and four comparisons in all", calls)
		}
		if after := h.reportByID(report.ID); !jsonEqual(t, after.TimingResult, stored.TimingResult) {
			t.Fatalf("timing result=%+v, want %+v", after.TimingResult, stored.TimingResult)
		}
		if got := h.finalState(report.ID); got != want {
			t.Fatalf("final=%s\nwant  %s", got, want)
		}
	})
	t.Run("after linking ended", func(t *testing.T) {
		// No act removes a link yet. Linking never runs again for a report
		// whose linking ended, so no stored result can bring a removed link
		// back: a fresh worker asks for nothing and writes nothing.
		h, report := resumeSetup(t, true, nil)
		h.settle()
		h.advance(time.Minute + time.Second)
		h.attempt(report.ID)
		if h.reportByID(report.ID).LinkingEndedAt == nil {
			t.Fatal("linking did not end")
		}
		before, state := h.forge.calls(), h.finalState(report.ID)
		h.setRule(tagRule("v{version}"))
		h.setRepositories(map[string]string{"app": "acme/other"})
		h.freshWorker()
		h.advance(time.Minute + time.Second)
		h.attempt(report.ID)
		if calls := h.forge.calls(); calls != before {
			t.Fatalf("calls=%v, want %v", calls, before)
		}
		if got := h.finalState(report.ID); got != state {
			t.Fatalf("final=%s\nwant  %s", got, state)
		}
	})
	t.Run("expiry after the timing result is stored", func(t *testing.T) {
		h, report := resumeSetup(t, true, github.StatusError(http.StatusBadGateway, errors.New("bad gateway")))
		h.settle()
		h.freshWorker()
		h.advance(store.FeedbackAdmissionLease + time.Second)
		h.attempt(report.ID)
		h.forge.set(func(f *linkingForge) { f.failure = nil })
		before := h.forge.calls()
		h.freshWorker()
		h.setNow(report.CreatedAt.Add(store.FeedbackReportLinkingDeadline))
		h.attempt(report.ID)
		if after := h.forge.calls(); after != before {
			t.Fatalf("calls at expiry %v, want %v", after, before)
		}
		notes := h.notes(report.ID)
		links := h.links(report.ID)
		if len(notes) != 1 || notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteNotChecked, Rule: store.FeedbackRuleLine, Reason: feedbackWaitingLineUnanswered}) || len(links) != 1 {
			t.Fatalf("notes=%+v links=%+v, want the stored timing link and not_checked for the line rule", notes, links)
		}
	})
}

// TestFeedbackReportLineReadUsesTheResolvedSlug proves that the line read asks
// the GitHub repository that resolved the commit, by its stored slug, even
// after the configured name points at another repository.
func TestFeedbackReportLineReadUsesTheResolvedSlug(t *testing.T) {
	h := newLinkingHarness(t)
	h.forge.set(func(f *linkingForge) {
		f.failure = github.StatusError(http.StatusBadGateway, errors.New("bad gateway"))
	})
	h.acceptReport(reportBody(lineEntry("crash")))
	h.settle()
	report := h.reportByKey("crash")
	if report.LineProgress == nil || report.LineProgress.Slug != "acme/app" {
		t.Fatalf("progress=%+v, want the resolution stored with slug acme/app", report.LineProgress)
	}
	current, err := h.st.WorkspaceConfig(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.UpdateWorkspaceConfig(h.ctx, current.Version, &config.Config{Workspace: "alpha", Repos: []config.Repo{{Name: "app", URL: "https://example.test/app", Base: "main", GitHub: "acme/other"}}}); err != nil {
		t.Fatal(err)
	}
	h.forge.set(func(f *linkingForge) { f.failure = nil })
	h.freshWorker()
	h.advance(store.FeedbackAdmissionLease + time.Second)
	h.attempt(report.ID)
	if !slices.Equal(h.forge.lineSlugs, []string{"acme/app", "acme/app"}) || h.forge.calls()[0] != 1 {
		t.Fatalf("line slugs=%v calls=%v, want both line reads of acme/app and one commit read", h.forge.lineSlugs, h.forge.calls())
	}
}

// TestFeedbackReportQueuedComparisonsHonorTheHold proves that a comparison
// the linking work queued sends nothing while a retry time is ahead, even
// when the hold was recorded after it was queued, and that it stays pending.
// A comparison that a live-status read asks for is not held this way.
func TestFeedbackReportQueuedComparisonsHonorTheHold(t *testing.T) {
	h := newLinkingHarness(t)
	keys := []store.FeedbackComparisonKey{{Repository: "acme/app", Base: readsCommit("c3"), Head: readsCommit("a2")}}
	env := h.server.feedbackJobEnv()
	// The job is built as it is queued, before any hold exists. The hold is
	// recorded after that and before the job runs.
	job, ok := env.linkingReadJob("alpha", keys)
	if !ok {
		t.Fatal("no linking read job for a missing comparison")
	}
	env.backoff.recordRateLimit("alpha", github.RateLimitedError(h.clock().Add(5*time.Minute), errors.New("rate limit")), h.clock())
	runFeedbackJob(job)
	if calls := h.forge.calls(); calls[2] != 0 {
		t.Fatalf("calls=%v, want no comparison while held", calls)
	}
	// A job queued during the hold sends nothing either.
	env.requestLinkingAnswers("alpha", keys)
	h.settle()
	if calls := h.forge.calls(); calls[2] != 0 {
		t.Fatalf("calls=%v, want no comparison from a job queued during the hold", calls)
	}
	stored, err := h.st.ListFeedbackComparisons(h.ctx, keys)
	if err != nil || len(stored) != 0 {
		t.Fatalf("stored=%+v err=%v, want the comparison pending", stored, err)
	}
	h.server.requestFeedbackAnswers("alpha", feedback.Missing{Comparisons: keys})
	h.settle()
	if calls := h.forge.calls(); calls[2] != 1 {
		t.Fatalf("calls=%v, want the live-status read to compare", calls)
	}
	h.advance(5*time.Minute + time.Second)
	env.requestLinkingAnswers("alpha", []store.FeedbackComparisonKey{{Repository: "acme/app", Base: readsCommit("c3"), Head: readsCommit("a1")}})
	h.settle()
	if calls := h.forge.calls(); calls[2] != 2 {
		t.Fatalf("calls=%v, want the linking comparison after the retry time", calls)
	}
}

// recordSignal records an accepted watcher signal of the environment that
// names service api with the versions, and stores each version resolved in
// app at the commit readsCommit("a"+version).
func (h *linkingHarness) recordSignal(at time.Time, versions ...string) {
	h.t.Helper()
	system := store.WithActor(h.ctx, store.SystemActor())
	stated := []store.FeedbackSignalVersion{}
	for _, version := range versions {
		stated = append(stated, store.FeedbackSignalVersion{Version: version})
	}
	signal := store.FeedbackSignal{EnvironmentID: h.environment.ID, KeyID: h.key.ID, ArrivedAt: at, NextSignalSeconds: 10,
		Services: []store.FeedbackSignalService{{Name: "api", Versions: stated}}}
	if err := h.st.RecordFeedbackSignal(system, signal); err != nil {
		h.t.Fatal(err)
	}
	for _, version := range versions {
		resolution := store.FeedbackResolution{FeedbackVersionRef: store.FeedbackVersionRef{EnvironmentID: h.environment.ID, Service: "api", FeedbackVersion: store.FeedbackVersion{Version: version}},
			ResolvedRepository: "app", ResolvedCommit: readsCommit("a" + version), AttemptedAt: at}
		if err := h.st.RecordFeedbackResolution(system, resolution); err != nil {
			h.t.Fatal(err)
		}
	}
}

// storeComparisons stores, for a merge commit, whether each version's commit
// holds it.
func (h *linkingHarness) storeComparisons(merge string, held map[string]bool) {
	h.t.Helper()
	comparisons := []store.FeedbackComparison{}
	for version, contains := range held {
		status := store.FeedbackCompareBehind
		if contains {
			status = store.FeedbackCompareAhead
		}
		comparisons = append(comparisons, store.FeedbackComparison{FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: "acme/app", Base: merge, Head: readsCommit("a" + version)},
			Status: status, ComparedAt: h.clock()})
	}
	if err := h.st.RecordFeedbackComparisons(h.ctx, comparisons); err != nil {
		h.t.Fatal(err)
	}
}

// TestFeedbackReportTimingDecidesEveryDeployAtABoundary proves review round
// 3 finding 1: one accepted signal deploys versions 2 and 3 of api at one
// boundary, and first_seen falls strictly between that signal and the signal
// before it. Each of the two deploys gets the prior signal and is decided on
// its own: version 2 newly holds a merged task and links it with
// elapsed_minutes 0 and prior_signal_at, and version 3 newly holds none and
// gives the note no_task_deploy. The stored timing result names both
// deploys, and a fresh worker answers the same from it
// (req-production-feedback AC-5.2 to AC-5.4; component-feedback-reports
// invariants 13 and 14).
func TestFeedbackReportTimingDecidesEveryDeployAtABoundary(t *testing.T) {
	for _, order := range [][]string{{"2", "3"}, {"3", "2"}} {
		t.Run("versions "+strings.Join(order, " then "), func(t *testing.T) {
			// setup records the two signals and the merged task, and sends a
			// report that both rules apply to. lineFailure fails the line
			// read, so the line rule stays pending after the first attempt.
			setup := func(t *testing.T, lineFailure error) (*linkingHarness, store.FeedbackReport, string, time.Time) {
				h := newLinkingHarness(t)
				accepted := h.clock().Add(feedbackSignalWindow)
				// The store keeps signal times to the microsecond.
				predecessor := accepted.Add(-10 * time.Minute).Truncate(time.Microsecond)
				h.recordSignal(predecessor, "1")
				h.recordSignal(accepted.Add(5*time.Minute), order...)
				merge := readsCommit("c2")
				task := h.mergedTask(merge, 21)
				h.storeComparisons(merge, map[string]bool{"1": false, "2": true, "3": false})
				h.forge.set(func(f *linkingForge) { f.failure = lineFailure })
				h.acceptReport(reportBody(lineEntry("boundary", `"service":"api"`)))
				h.settle()
				report := h.reportByKey("boundary")
				if !report.FirstEntrySeen.After(predecessor) || !report.FirstEntrySeen.Before(accepted.Add(5*time.Minute)) {
					t.Fatalf("first_seen %v is not strictly between the two signals", report.FirstEntrySeen)
				}
				if result := report.TimingResult; result == nil || len(result.Links) != 1 || len(result.Notes) != 1 || result.Links[0].DeployID == result.Notes[0].DeployID ||
					result.Links[0].PriorSignalAt == nil || !result.Links[0].PriorSignalAt.Equal(predecessor) {
					t.Fatalf("timing result=%+v, want one link and one note on two deploys", result)
				}
				return h, report, task, predecessor
			}
			check := func(t *testing.T, h *linkingHarness, report store.FeedbackReport, task string, predecessor time.Time) {
				t.Helper()
				links := h.links(report.ID)
				link, linked := links[task]
				if len(links) != 1 || !linked || len(link.Evidence) != 1 {
					t.Fatalf("links=%+v, want the merged task with one piece of evidence", links)
				}
				evidence := link.Evidence[0]
				if evidence.Kind != store.FeedbackEvidenceTiming || evidence.Deploy == nil || evidence.Deploy.Version.Version != "2" || evidence.ElapsedMinutes == nil || *evidence.ElapsedMinutes != 0 ||
					evidence.PriorSignalAt == nil || !evidence.PriorSignalAt.Equal(predecessor) {
					t.Fatalf("evidence=%+v deploy=%+v, want version 2 with 0 minutes and the prior signal", evidence, evidence.Deploy)
				}
				notes := h.notes(report.ID)
				if len(notes) != 1 || notes[0].Kind != store.FeedbackNoteNoTaskDeploy || notes[0].Deploy == nil || notes[0].Deploy.Version.Version != "3" ||
					!notes[0].Deploy.DeployedAt.Equal(evidence.Deploy.DeployedAt) {
					t.Fatalf("notes=%+v, want no_task_deploy for version 3 at the same boundary", notes)
				}
			}
			// Uninterrupted, linking ends in the first attempt.
			h, report, task, predecessor := setup(t, nil)
			if h.reportByID(report.ID).LinkingEndedAt == nil {
				t.Fatal("the uninterrupted run did not end")
			}
			check(t, h, report, task, predecessor)
			want := h.finalState(report.ID)
			// Interrupted, the first attempt stores the timing result while
			// the line read fails. A fresh worker ends linking from the
			// stored timing result alone and writes the same.
			h, report, task, predecessor = setup(t, github.StatusError(http.StatusBadGateway, errors.New("bad gateway")))
			if pending := h.reportByID(report.ID); pending.LinkingEndedAt != nil || len(h.links(report.ID)) != 0 || len(h.notes(report.ID)) != 0 {
				t.Fatalf("ended=%v links=%v, want linking pending with nothing written", pending.LinkingEndedAt != nil, h.links(report.ID))
			}
			stored := h.reportByID(report.ID).TimingResult
			h.forge.set(func(f *linkingForge) { f.failure = nil })
			h.freshWorker()
			h.advance(store.FeedbackAdmissionLease + time.Second)
			h.attempt(report.ID)
			ended := h.reportByID(report.ID)
			if ended.LinkingEndedAt == nil || !jsonEqual(t, ended.TimingResult, stored) || h.forge.calls()[2] != 0 {
				t.Fatalf("ended=%v timing result=%+v calls=%v, want linking ended from the stored result", ended.LinkingEndedAt != nil, ended.TimingResult, h.forge.calls())
			}
			check(t, h, report, task, predecessor)
			if got := h.finalState(report.ID); got != want {
				t.Fatalf("fresh worker final=%s\nwant %s", got, want)
			}
		})
	}
}

// setRepositories replaces the workspace's repositories with names on GitHub
// slugs.
func (h *linkingHarness) setRepositories(slugs map[string]string) {
	h.t.Helper()
	repos := []config.Repo{}
	for name, slug := range slugs {
		repos = append(repos, config.Repo{Name: name, URL: "https://example.test/" + name, Base: "main", GitHub: slug})
	}
	slices.SortFunc(repos, func(a, b config.Repo) int { return compareTaskIDs(a.Name, b.Name) })
	current, err := h.st.WorkspaceConfig(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.UpdateWorkspaceConfig(h.ctx, current.Version, &config.Config{Workspace: "alpha", Repos: repos}); err != nil {
		h.t.Fatal(err)
	}
}

// setRule replaces the environment's version rule.
func (h *linkingHarness) setRule(rule *store.EnvironmentVersionRule) {
	h.t.Helper()
	if _, err := h.st.UpdateEnvironment(h.ctx, h.environment.ID, store.EnvironmentPatch{VersionRuleSet: true, VersionRule: rule}); err != nil {
		h.t.Fatal(err)
	}
}

func tagRule(text string) *store.EnvironmentVersionRule {
	return &store.EnvironmentVersionRule{Kind: store.EnvironmentTagTemplate, Text: text}
}

func (h *linkingHarness) readsOf(slug string) int {
	h.forge.mu.Lock()
	defer h.forge.mu.Unlock()
	return h.forge.readsBySlug[slug]
}

// TestFeedbackReportVersionRuleChangeDropsInterimAnswers proves review round
// 3 finding 2 for a report with a version only. Under the rule v{version},
// acme/app holds the tag and its answer is stored, while the read of
// acme/other fails. A manager then changes the rule to release-{version}:
// acme/app holds no such tag and acme/other does. A fresh worker drops the
// old answer at once, reads both repositories with the new tag, and resolves
// in acme/other. Combining the old match with the new one would have given
// ambiguous (req-production-feedback AC-5.1; component-feedback-reports
// invariant 12; component-production-feedback invariant 7).
func TestFeedbackReportVersionRuleChangeDropsInterimAnswers(t *testing.T) {
	h := newLinkingHarness(t)
	h.setRepositories(map[string]string{"app": "acme/app", "other": "acme/other"})
	h.mergedTask(linkingChange, 7)
	h.setRule(tagRule("v{version}"))
	oldTag := readsCommit("e1")
	h.forge.set(func(f *linkingForge) {
		f.refs = map[string]string{"acme/app|tags/v1.2": oldTag, "acme/other|tags/release-1.2": linkingResolved}
		f.readFailures = map[string]error{"acme/other": github.StatusError(http.StatusBadGateway, errors.New("bad gateway"))}
	})
	h.acceptReport(reportBody(reportEntryJSON("crash", `"file":"cmd/main.go","line":12,"version":"1.2"`)))
	h.settle()
	report := h.reportByKey("crash")
	before := report.LineProgress
	if before == nil || before.Ended() || len(before.Candidates) != 1 || before.Candidates["acme/app"] != oldTag || !store.FeedbackLineInputDigest(before.Input) {
		t.Fatalf("progress=%+v, want the old tag's answer for acme/app alone", before)
	}
	h.setRule(tagRule("release-{version}"))
	h.forge.set(func(f *linkingForge) { f.readFailures = nil })
	h.freshWorker()
	h.advance(store.FeedbackAdmissionLease + time.Second)
	h.attempt(report.ID)
	after := h.reportByID(report.ID)
	if after.LineProgress == nil || after.LineProgress.Repository != "other" || after.LineProgress.Slug != "acme/other" || after.LineProgress.Commit != linkingResolved {
		t.Fatalf("progress=%+v, want the resolution in acme/other", after.LineProgress)
	}
	if reads := h.readsOf("acme/app"); reads != 2 {
		t.Fatalf("acme/app reads=%d, want it read again with the new tag", reads)
	}
	if after.LinkingEndedAt == nil || !slices.Equal(h.forge.lineSlugs, []string{"acme/other"}) {
		t.Fatalf("ended=%v line slugs=%v, want one line read of acme/other", after.LinkingEndedAt != nil, h.forge.lineSlugs)
	}
	// The merged task belongs to app, so the line read in acme/other links
	// nothing, and no commit_unresolved note is written.
	if notes := h.notes(report.ID); len(notes) != 0 {
		t.Fatalf("notes=%+v, want no ambiguous note", notes)
	}
}

// TestFeedbackReportCandidatesStayWithinTheRepositories proves the bound of
// the stored candidate answers across repository configuration changes. The
// read of acme/blocked always fails, so resolving never ends. After each
// change a fresh worker runs one attempt: an answer of an unchanged slug is
// reused, and the stored answers name exactly the current candidates that
// were answered, never a slug that was repointed or removed
// (component-feedback-reports invariant 12; approved plan version 3).
func TestFeedbackReportCandidatesStayWithinTheRepositories(t *testing.T) {
	h := newLinkingHarness(t)
	h.forge.set(func(f *linkingForge) {
		f.readFailures = map[string]error{"acme/blocked": github.StatusError(http.StatusBadGateway, errors.New("bad gateway"))}
	})
	h.setRepositories(map[string]string{"app": "acme/app", "blocked": "acme/blocked"})
	h.acceptReport(reportBody(lineEntry("crash")))
	h.settle()
	report := h.reportByKey("crash")
	for i, step := range []struct {
		name   string
		repos  map[string]string
		stored map[string]string
		reads  map[string]int
	}{
		{"first attempt", nil, map[string]string{"acme/app": linkingResolved}, map[string]int{"acme/app": 1}},
		{"app repointed", map[string]string{"app": "acme/app2", "blocked": "acme/blocked"}, map[string]string{"acme/app2": store.FeedbackLineAbsent}, map[string]int{"acme/app": 1, "acme/app2": 1}},
		{"a slug added", map[string]string{"app": "acme/app2", "blocked": "acme/blocked", "extra": "acme/extra"},
			map[string]string{"acme/app2": store.FeedbackLineAbsent, "acme/extra": store.FeedbackLineAbsent}, map[string]int{"acme/app2": 1, "acme/extra": 1}},
		{"app removed, no read succeeds", map[string]string{"blocked": "acme/blocked", "extra": "acme/extra"}, map[string]string{"acme/extra": store.FeedbackLineAbsent}, map[string]int{"acme/app2": 1, "acme/extra": 1}},
		{"app back on acme/app", map[string]string{"app": "acme/app", "blocked": "acme/blocked", "extra": "acme/extra"},
			map[string]string{"acme/app": linkingResolved, "acme/extra": store.FeedbackLineAbsent}, map[string]int{"acme/app": 2, "acme/extra": 1}},
	} {
		if i > 0 {
			h.setRepositories(step.repos)
			h.freshWorker()
			h.advance(store.FeedbackAdmissionLease + time.Second)
			h.attempt(report.ID)
		}
		progress := h.reportByID(report.ID).LineProgress
		if progress == nil || progress.Ended() || fmt.Sprint(progress.Candidates) != fmt.Sprint(step.stored) {
			t.Fatalf("%s: progress=%+v, want answers %v", step.name, progress, step.stored)
		}
		for slug, want := range step.reads {
			if got := h.readsOf(slug); got != want {
				t.Fatalf("%s: %s reads=%d, want %d", step.name, slug, got, want)
			}
		}
	}
}

// TestFeedbackReportFinalResultsSurviveInputChanges proves that the results
// the design calls final are kept through every input change: the
// resolution, a definitive unresolved reason, and the line read's answer. A
// fresh worker reads none of them again after the version rule changes and
// the repositories are repointed or added (component-feedback-reports
// invariants 11, 12, and 15; approved plan version 3).
func TestFeedbackReportFinalResultsSurviveInputChanges(t *testing.T) {
	// change sets a rule that would read another tag, adds a repository,
	// and, with repoint, points app at another GitHub repository.
	change := func(h *linkingHarness, repoint bool) {
		h.setRule(tagRule("v{version}"))
		repos := map[string]string{"app": "acme/app", "extra": "acme/extra"}
		if repoint {
			repos["app"] = "acme/other"
		}
		h.setRepositories(repos)
		h.forge.set(func(f *linkingForge) {
			f.refs = map[string]string{"acme/other|tags/vabcdef0": readsCommit("e2"), "acme/extra|tags/vabcdef0": readsCommit("e3")}
		})
	}
	t.Run("the resolution", func(t *testing.T) {
		h := newLinkingHarness(t)
		h.mergedTask(linkingChange, 7)
		h.setRule(&store.EnvironmentVersionRule{Kind: store.EnvironmentCommitPattern, Text: "{commit}"})
		h.forge.set(func(f *linkingForge) {
			f.failure = github.StatusError(http.StatusBadGateway, errors.New("bad gateway"))
		})
		h.acceptReport(reportBody(reportEntryJSON("crash", `"file":"cmd/main.go","line":12,"version":"abcdef0"`)))
		h.settle()
		report := h.reportByKey("crash")
		if progress := report.LineProgress; progress == nil || progress.Slug != "acme/app" || progress.Commit != linkingResolved {
			t.Fatalf("progress=%+v, want the resolution in acme/app", progress)
		}
		change(h, true)
		h.forge.set(func(f *linkingForge) { f.failure = nil })
		h.freshWorker()
		h.advance(store.FeedbackAdmissionLease + time.Second)
		h.attempt(report.ID)
		ended := h.reportByID(report.ID)
		if calls := h.forge.calls(); calls[0] != 1 || ended.LinkingEndedAt == nil || len(h.links(report.ID)) != 1 ||
			!slices.Equal(h.forge.lineSlugs, []string{"acme/app", "acme/app"}) {
			t.Fatalf("calls=%v ended=%v line slugs=%v, want no second commit read and the line read of acme/app", calls, ended.LinkingEndedAt != nil, h.forge.lineSlugs)
		}
	})
	t.Run("a definitive unresolved reason", func(t *testing.T) {
		h := newLinkingHarness(t)
		h.accept(`{"next_signal_seconds":86400,"services":[{"name":"web","versions":[{"version":"1"}]}]}`)
		h.settle()
		h.acceptReport(reportBody(reportEntryJSON("crash", `"file":"cmd/main.go","line":12,"version":"abcdef0","service":"api"`)))
		h.settle()
		report := h.reportByKey("crash")
		if progress := report.LineProgress; progress == nil || progress.Unresolved != store.FeedbackReasonNoVersionRule || report.LinkingEndedAt != nil {
			t.Fatalf("progress=%+v ended=%v, want no_version_rule stored while the timing rule waits", progress, report.LinkingEndedAt != nil)
		}
		change(h, true)
		h.freshWorker()
		h.advance(time.Minute + time.Second)
		h.attempt(report.ID)
		if calls := h.forge.calls(); calls != [3]int{} {
			t.Fatalf("calls=%v, want none after the rule was set", calls)
		}
		h.freshWorker()
		h.setNow(report.CreatedAt.Add(store.FeedbackReportLinkingDeadline))
		h.attempt(report.ID)
		notes := h.notes(report.ID)
		if len(notes) == 0 || notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteCommitUnresolved, Reason: store.FeedbackReasonNoVersionRule}) {
			t.Fatalf("notes=%+v, want commit_unresolved no_version_rule", notes)
		}
	})
	t.Run("the line read's answer", func(t *testing.T) {
		h, report := resumeSetup(t, true, nil)
		h.settle()
		if _, stored, _ := h.st.ReadFeedbackReportLineRead(h.ctx, report.ID); !stored || h.reportByID(report.ID).LinkingEndedAt != nil {
			t.Fatal("want the line read stored and linking pending")
		}
		// The timing rule has not ended and compares in the configured
		// repository, so app keeps its GitHub repository here; the
		// repointed case is TestFeedbackReportLineReadUsesTheResolvedSlug.
		before := h.forge.calls()
		change(h, false)
		h.freshWorker()
		h.advance(time.Minute + time.Second)
		h.attempt(report.ID)
		if calls := h.forge.calls(); calls != before || h.reportByID(report.ID).LinkingEndedAt == nil {
			t.Fatalf("calls=%v before=%v, want no commit read or line read again and linking ended", calls, before)
		}
		if links := h.links(report.ID); len(links) != 2 {
			t.Fatalf("links=%+v, want the line link and the timing link", links)
		}
	})
}

// TestFeedbackReportOlderAttemptWritesNothing proves CodeRabbit finding
// 4723513198: an attempt that lost its claim to a later one, as when two
// server processes overlap, stops at its first write and writes nothing, so
// an answer it read with an input that has since changed can never be
// stored. The log names the stop as superseded, not as a failure
// (component-feedback-reports invariant 15; approved plan version 3).
func TestFeedbackReportOlderAttemptWritesNothing(t *testing.T) {
	h := newLinkingHarness(t)
	h.setRepositories(map[string]string{"app": "acme/app", "other": "acme/other"})
	h.setRule(tagRule("v{version}"))
	h.forge.set(func(f *linkingForge) {
		f.readFailure = github.StatusError(http.StatusBadGateway, errors.New("bad gateway"))
	})
	h.acceptReport(reportBody(reportEntryJSON("crash", `"file":"cmd/main.go","line":12,"version":"1.2"`)))
	h.settle()
	report := h.reportByKey("crash")
	if report.LineProgress != nil || report.LinkingLastAttemptAt == nil {
		t.Fatalf("progress=%+v attempt=%v, want a claimed attempt that stored nothing", report.LineProgress, report.LinkingLastAttemptAt)
	}
	// The older attempt claims; then another process claims a minute later
	// under a changed rule, before the older attempt writes.
	backend := h.server.feedbackJobEnv().store
	reportStore := backend.(store.ReportStore)
	h.advance(store.FeedbackAdmissionLease + time.Second)
	older := h.clock()
	claimed, ok, err := reportStore.ClaimFeedbackReportLinking(h.ctx, report.ID, older)
	if err != nil || !ok {
		t.Fatalf("older claim ok=%v err=%v", ok, err)
	}
	if _, ok, err := reportStore.ClaimFeedbackReportLinking(h.ctx, report.ID, older.Add(store.FeedbackReportLinkingRetry)); err != nil || !ok {
		t.Fatalf("newer claim ok=%v err=%v", ok, err)
	}
	h.forge.set(func(f *linkingForge) {
		f.readFailure = nil
		f.refs = map[string]string{"acme/app|tags/v1.2": readsCommit("e1")}
	})
	job := feedbackJob{workspace: "alpha", report: report.ID, env: h.server.feedbackJobEnv()}
	attempt := feedbackReportAttempt{job: job, reports: reportStore, feedback: backend.(store.FeedbackStore), report: claimed, now: older}
	attempt.forge = &feedbackAttemptForge{inner: h.forge, env: job.env, workspace: "alpha"}
	if err := attempt.run(h.ctx); !errors.Is(err, store.ErrFeedbackLinkingSuperseded) {
		t.Fatalf("older attempt err=%v, want superseded", err)
	}
	after := h.reportByID(report.ID)
	if after.LineProgress != nil || after.TimingResult != nil || after.LinkingEndedAt != nil {
		t.Fatalf("older attempt wrote progress=%+v timing=%+v ended=%v", after.LineProgress, after.TimingResult, after.LinkingEndedAt != nil)
	}
	if got := feedbackReportErrorClass(store.ErrFeedbackLinkingSuperseded); got != "superseded" {
		t.Fatalf("class=%q", got)
	}
}

// readyLimitedForge fails readiness with a rate limit, as a lazy forge does
// when GitHub rate-limits the installation token request.
type readyLimitedForge struct {
	*linkingForge
	until time.Time
}

func (f readyLimitedForge) Ready(context.Context) error {
	return github.RateLimitedError(f.until, errors.New("token rate limit"))
}

// TestFeedbackReportReadyRateLimitHolds proves CodeRabbit finding
// 4723513177: a rate limit met while getting the installation token holds
// the workspace's later linking calls, as a rate-limited call does
// (component-production-feedback invariant 28).
func TestFeedbackReportReadyRateLimitHolds(t *testing.T) {
	h := newLinkingHarness(t)
	env := h.server.feedbackJobEnv()
	until := h.clock().Add(5 * time.Minute)
	forge := &feedbackAttemptForge{inner: readyLimitedForge{linkingForge: h.forge, until: until}, env: env, workspace: "alpha"}
	if err := forge.Ready(h.ctx); github.ErrorCategory(err) != github.ForgeRateLimited {
		t.Fatalf("ready err=%v, want the rate limit", err)
	}
	if held, ok := env.backoff.forgeHold("alpha", h.clock()); !ok || !held.Equal(until) {
		t.Fatalf("hold=%v ok=%v, want the workspace held until %v", held, ok, until)
	}
}

// signalDuringReadStore records a watcher signal right after the timing rule
// read the deploy history, as a watcher can at any moment.
type signalDuringReadStore struct {
	store.FeedbackStore
	once   sync.Once
	record func()
}

func (s *signalDuringReadStore) ReadFeedbackInputs(ctx context.Context, environmentIDs []string) (store.FeedbackInputs, error) {
	inputs, err := s.FeedbackStore.ReadFeedbackInputs(ctx, environmentIDs)
	s.once.Do(s.record)
	return inputs, err
}

// TestFeedbackReportTimingReadsSignalsBeforeDeploys proves CodeRabbit
// finding 4723513184: the timing rule reads the watcher signals before the
// deploy history. A watcher listens, so the rule waits for a signal after
// the report. One arrives right after the rule read the deploy history, and
// it deploys a version. Read in the other order, the rule would count that
// signal but miss its deploy and store an empty answer. Here it has not seen
// the signal, keeps waiting, and stores nothing.
func TestFeedbackReportTimingReadsSignalsBeforeDeploys(t *testing.T) {
	h := newLinkingHarness(t)
	h.accept(`{"next_signal_seconds":86400,"services":[{"name":"api","versions":[{"version":"1"}]}]}`)
	h.settle()
	h.forge.set(func(f *linkingForge) {
		f.failure = github.StatusError(http.StatusBadGateway, errors.New("bad gateway"))
	})
	h.acceptReport(reportBody(lineEntry("late", `"service":"api"`)))
	h.settle()
	report := h.reportByKey("late")
	if report.TimingResult != nil || report.LinkingEndedAt != nil {
		t.Fatalf("timing result=%+v, want the rule waiting for a signal", report.TimingResult)
	}
	h.advance(store.FeedbackAdmissionLease + time.Second)
	backend := h.server.feedbackJobEnv().store
	system := store.WithActor(h.ctx, store.SystemActor())
	wrapped := &signalDuringReadStore{FeedbackStore: backend.(store.FeedbackStore), record: func() {
		signal := store.FeedbackSignal{EnvironmentID: h.environment.ID, KeyID: h.key.ID, ArrivedAt: h.clock(), NextSignalSeconds: 86400,
			Services: []store.FeedbackSignalService{{Name: "api", Versions: []store.FeedbackSignalVersion{{Version: "2"}}}}}
		if err := h.st.RecordFeedbackSignal(system, signal); err != nil {
			t.Error(err)
		}
	}}
	reportStore := backend.(store.ReportStore)
	now := h.clock()
	claimed, ok, err := reportStore.ClaimFeedbackReportLinking(h.ctx, report.ID, now)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	job := feedbackJob{workspace: "alpha", report: report.ID, env: h.server.feedbackJobEnv()}
	attempt := feedbackReportAttempt{job: job, reports: reportStore, feedback: wrapped, report: claimed, now: now}
	attempt.forge = &feedbackAttemptForge{inner: h.forge, env: job.env, workspace: "alpha"}
	environment, _, err := feedbackReportEnvironment(h.ctx, backend, report.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := feedbackRepositories(h.ctx, backend)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := attempt.timingRule(h.ctx, environment, repositories, nil)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := backend.(store.FeedbackStore).ReadFeedbackInputs(h.ctx, []string{report.EnvironmentID})
	if err != nil || len(inputs.Deploys[report.EnvironmentID]) != 2 {
		t.Fatalf("deploys=%+v err=%v, want the late signal's deploy recorded", inputs.Deploys[report.EnvironmentID], err)
	}
	if outcome.Ended || outcome.Waiting == "" {
		t.Fatalf("outcome=%+v, want the rule still waiting", outcome)
	}
	if stored := h.reportByID(report.ID); stored.TimingResult != nil {
		t.Fatalf("timing result=%+v, want none stored", stored.TimingResult)
	}
}

// mergedTaskIn creates a merged task under the configured name app whose
// latest merge event records the GitHub repository, the merge commit, and
// the pull request.
func (h *linkingHarness) mergedTaskIn(repository, merge string, pull int) string {
	h.t.Helper()
	id := core.NewTaskID() + "-" + merge[:4]
	if err := h.st.CreateTask(h.ctx, core.Task{ID: id, Workspace: "alpha", Repo: "app", Title: "Merged", Body: "Merged.", State: core.TaskMerged, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.AppendEvent(h.ctx, core.Event{TaskID: id, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": repository, "pull_request": pull, "merge_commit_sha": merge})}); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// TestFeedbackReportLineMatchesTheReadRepository proves review round 4's
// finding through the worker. The name app pointed at acme/old, where a task
// merged as pull request 42 with the changing commit. Then app was pointed
// at acme/new, the report's commit resolved there, and the line read in
// acme/new lists pull request 42 and the same commit, as a fork can. Only
// the tasks merged in acme/new link, both when the answer is fresh and when
// a fresh worker reads it back from the store (component-feedback-reports
// invariant 12; req-production-feedback AC-5.1).
func TestFeedbackReportLineMatchesTheReadRepository(t *testing.T) {
	setup := func(t *testing.T) (*linkingHarness, string, string, string) {
		h := newLinkingHarness(t)
		h.setRepositories(map[string]string{"app": "acme/old"})
		oldPull := h.mergedTaskIn("acme/old", readsCommit("e7"), 42)
		oldCommit := h.mergedTaskIn("acme/old", linkingChange, 41)
		h.setRepositories(map[string]string{"app": "acme/new"})
		newPull := h.mergedTaskIn("acme/new", readsCommit("e8"), 42)
		h.forge.set(func(f *linkingForge) {
			f.commits = map[string]string{"acme/new": linkingResolved}
			f.answer = github.LineChange{FileFound: true, Commit: linkingChange, PullRequests: []int{42}}
		})
		return h, oldPull, oldCommit, newPull
	}
	check := func(t *testing.T, h *linkingHarness, reportID, oldPull, oldCommit, newPull string) {
		t.Helper()
		links := h.links(reportID)
		if _, ok := links[newPull]; !ok || len(links) != 1 {
			t.Fatalf("links=%v, want only the acme/new task %s (not %s or %s)", links, newPull, oldPull, oldCommit)
		}
		if progress := h.reportByID(reportID).LineProgress; progress == nil || progress.Slug != "acme/new" {
			t.Fatalf("progress=%+v, want the resolution in acme/new", progress)
		}
	}
	t.Run("fresh answer", func(t *testing.T) {
		h, oldPull, oldCommit, newPull := setup(t)
		h.acceptReport(reportBody(lineEntry("crash")))
		h.settle()
		report := h.reportByKey("crash")
		if report.LinkingEndedAt == nil {
			t.Fatal("linking did not end")
		}
		check(t, h, report.ID, oldPull, oldCommit, newPull)
	})
	t.Run("stored answer read by a fresh worker", func(t *testing.T) {
		h, oldPull, oldCommit, newPull := setup(t)
		// A listening watcher keeps the timing rule waiting, so the first
		// attempt stores the line read and linking stays pending.
		h.accept(`{"next_signal_seconds":86400,"services":[{"name":"web","versions":[{"version":"1"}]}]}`)
		h.settle()
		h.acceptReport(reportBody(lineEntry("crash", `"service":"api"`)))
		h.settle()
		report := h.reportByKey("crash")
		if _, stored, _ := h.st.ReadFeedbackReportLineRead(h.ctx, report.ID); !stored || h.reportByID(report.ID).LinkingEndedAt != nil {
			t.Fatal("want the line read stored and linking pending")
		}
		h.freshWorker()
		h.setNow(report.CreatedAt.Add(store.FeedbackReportLinkingDeadline))
		h.attempt(report.ID)
		if h.reportByID(report.ID).LinkingEndedAt == nil || h.forge.calls()[1] != 1 {
			t.Fatalf("calls=%v, want linking ended from the stored read", h.forge.calls())
		}
		check(t, h, report.ID, oldPull, oldCommit, newPull)
	})
}

// TestFeedbackReportTimingCountsOnlyTheDeployRepository proves the timing
// rule's part of review round 4 through the worker: a task merged under the
// name app while it pointed at acme/old is not newly introduced by a deploy
// resolved after app points at acme/app, even when a stored comparison in
// acme/app says the new version holds its commit (component-feedback-reports
// invariants 13 and 14).
func TestFeedbackReportTimingCountsOnlyTheDeployRepository(t *testing.T) {
	h := newLinkingHarness(t)
	h.timingHistory()
	old := h.mergedTaskIn("acme/old", readsCommit("c5"), 5)
	h.storeComparisons(readsCommit("c5"), map[string]bool{"1": false, "2": true})
	h.storeComparisons(readsCommit("c3"), map[string]bool{"1": false, "2": true})
	h.acceptReport(reportBody(reportEntryJSON("crash", `"service":"api"`)))
	h.settle()
	report := h.reportByKey("crash")
	links := h.links(report.ID)
	if report.LinkingEndedAt == nil || len(links) != 1 {
		t.Fatalf("ended=%v links=%v, want one timing link", report.LinkingEndedAt != nil, links)
	}
	if _, linked := links[old]; linked {
		t.Fatalf("links=%v, the acme/old task was counted", links)
	}
	for _, link := range links {
		if !strings.HasSuffix(link.TaskID, "-"+readsCommit("c3")[:4]) {
			t.Fatalf("link %s, want the acme/app task merged at c3", link.TaskID)
		}
	}
}
