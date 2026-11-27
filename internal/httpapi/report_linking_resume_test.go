package httpapi

import (
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
	if after.LineProgress == nil || after.LineProgress.Commit != linkingResolved || len(after.LineProgress.Candidates) != 1 {
		t.Fatalf("progress=%+v, want the resolution stored", after.LineProgress)
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
			h := newLinkingHarness(t)
			accepted := h.clock().Add(feedbackSignalWindow)
			// The store keeps signal times to the microsecond.
			predecessor := accepted.Add(-10 * time.Minute).Truncate(time.Microsecond)
			h.recordSignal(predecessor, "1")
			h.recordSignal(accepted.Add(5*time.Minute), order...)
			merge := readsCommit("c2")
			task := h.mergedTask(merge, 21)
			h.storeComparisons(merge, map[string]bool{"1": false, "2": true, "3": false})
			h.acceptReport(reportBody(reportEntryJSON("boundary", `"service":"api"`)))
			h.settle()
			report := h.reportByKey("boundary")
			if !report.FirstEntrySeen.After(predecessor) || !report.FirstEntrySeen.Before(accepted.Add(5*time.Minute)) {
				t.Fatalf("first_seen %v is not strictly between the two signals", report.FirstEntrySeen)
			}
			if report.LinkingEndedAt == nil || report.TimingResult == nil {
				t.Fatalf("linking ended=%v timing result=%+v", report.LinkingEndedAt != nil, report.TimingResult)
			}
			check := func(stage string) {
				t.Helper()
				links := h.links(report.ID)
				link, linked := links[task]
				if len(links) != 1 || !linked || len(link.Evidence) != 1 {
					t.Fatalf("%s: links=%+v, want the merged task with one piece of evidence", stage, links)
				}
				evidence := link.Evidence[0]
				if evidence.Kind != store.FeedbackEvidenceTiming || evidence.Deploy == nil || evidence.Deploy.Version.Version != "2" || evidence.ElapsedMinutes == nil || *evidence.ElapsedMinutes != 0 ||
					evidence.PriorSignalAt == nil || !evidence.PriorSignalAt.Equal(predecessor) {
					t.Fatalf("%s: evidence=%+v deploy=%+v, want version 2 with 0 minutes and the prior signal", stage, evidence, evidence.Deploy)
				}
				notes := h.notes(report.ID)
				if len(notes) != 1 || notes[0].Kind != store.FeedbackNoteNoTaskDeploy || notes[0].Deploy == nil || notes[0].Deploy.Version.Version != "3" ||
					!notes[0].Deploy.DeployedAt.Equal(evidence.Deploy.DeployedAt) {
					t.Fatalf("%s: notes=%+v, want no_task_deploy for version 3 at the same boundary", stage, notes)
				}
			}
			check("first attempt")
			if result := report.TimingResult; len(result.Links) != 1 || len(result.Notes) != 1 || result.Links[0].DeployID == result.Notes[0].DeployID ||
				result.Links[0].PriorSignalAt == nil || !result.Links[0].PriorSignalAt.Equal(predecessor) {
				t.Fatalf("timing result=%+v, want one link and one note on two deploys", result)
			}
			want := h.finalState(report.ID)
			h.freshWorker()
			h.advance(time.Minute + time.Second)
			h.attempt(report.ID)
			check("fresh worker")
			if got := h.finalState(report.ID); got != want {
				t.Fatalf("fresh worker final=%s\nwant %s", got, want)
			}
		})
	}
}
