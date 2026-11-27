package storetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runFeedbackReportLinking holds every backend to the storage rules of the
// linking work: the once-a-minute claim, the stored line read, the end of
// linking with its links, notes, and event, the watcher signals that named a
// service, and the pull request of the latest merge event
// (component-feedback-reports invariants 11 to 16 and 20;
// feature-production-feedback-listening FR-2; req-production-feedback AC-5.1
// to AC-5.6, AC-7.2, and AC-11.1). Each case starts from a fresh workspace.
func runFeedbackReportLinking(t *testing.T, factory Factory) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Fixture)
	}{
		{"ClaimOncePerMinute", runFeedbackLinkingClaim},
		{"ClaimAtTheSixtySecondBoundary", runFeedbackLinkingBoundary},
		{"TwoWorkersOneMinute", runFeedbackLinkingTwoWorkers},
		{"LineReadKept", runFeedbackLinkingLineRead},
		{"FinishLinks", runFeedbackLinkingFinish},
		{"FinishAfterLaterEntries", runFeedbackLinkingLaterEntries},
		{"LinkIDsSkipStoredIDs", runFeedbackLinkingLinkIDs},
		{"ServiceSignals", runFeedbackLinkingServiceSignals},
		{"MergePullRequest", runFeedbackLinkingMergePullRequest},
		{"MergeRepository", runFeedbackLinkingMergeRepository},
		{"MergeEventShapes", runFeedbackLinkingMergeEventShapes},
		{"MalformedEvidenceRefused", runFeedbackLinkingMalformedEvidence},
		{"LineProgressResumes", runFeedbackLinkingLineProgress},
		{"LineProgressOneInput", runFeedbackLinkingLineProgressInput},
		{"TimingResultKept", runFeedbackLinkingTimingResult},
		{"ProgressAfterTheEnd", runFeedbackLinkingProgressAfterEnd},
		{"WritesNeedTheCurrentClaim", runFeedbackLinkingWritesNeedTheClaim},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := factory.fresh(t, nil)
			if x.SeedFeedbackReportLink == nil || x.WorkspaceEvents == nil {
				t.Fatal("conformance fixture must provide SeedFeedbackReportLink and WorkspaceEvents for the linking cases")
			}
			c.run(t, x)
		})
	}
}

// linkingReport stores one report and returns it.
func linkingReport(t *testing.T, x Fixture, key string, more func(*store.FeedbackReportEntry)) (feedbackEnvironment, store.FeedbackReport) {
	t.Helper()
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	entry := reportEntry(key)
	if more != nil {
		more(&entry)
	}
	e.sendReports(t, 0, entry)
	return e, e.reportByKey(t, key)
}

func claimLinking(t *testing.T, x Fixture, reportID string, now time.Time) (store.FeedbackReport, bool) {
	t.Helper()
	report, claimed, err := x.Backend.ClaimFeedbackReportLinking(x.Context, reportID, now)
	requireOK(t, err)
	return report, claimed
}

// claimedAt claims one linking attempt of a report at now and returns now,
// the claim time an attempt passes with each of its writes.
func claimedAt(t *testing.T, x Fixture, reportID string, now time.Time) time.Time {
	t.Helper()
	if _, ok := claimLinking(t, x, reportID, now); !ok {
		t.Fatalf("the claim of %s at %v was refused", reportID, now)
	}
	return now
}

func requireAttempt(t *testing.T, e feedbackEnvironment, key string, want time.Time) {
	t.Helper()
	report := e.reportByKey(t, key)
	if report.LinkingLastAttemptAt == nil || !report.LinkingLastAttemptAt.Equal(want) {
		t.Fatalf("stored last attempt=%v, want %v", report.LinkingLastAttemptAt, want)
	}
}

func runFeedbackLinkingClaim(t *testing.T, x Fixture) {
	e, report := linkingReport(t, x, "crash", nil)
	if report.LinkingLastAttemptAt != nil || report.LinkingEndedAt != nil {
		t.Fatalf("a new report=%+v, want no attempt and linking not ended", report)
	}
	first := feedbackTime(time.Minute)
	claimed, ok := claimLinking(t, x, report.ID, first)
	if !ok || claimed.ID != report.ID || claimed.LinkingLastAttemptAt == nil || !claimed.LinkingLastAttemptAt.Equal(first) {
		t.Fatalf("first claim=%+v ok=%v", claimed, ok)
	}
	requireAttempt(t, e, "crash", first)
	// A claim within the minute writes nothing.
	if _, ok := claimLinking(t, x, report.ID, first.Add(30*time.Second)); ok {
		t.Fatal("a claim 30 seconds later succeeded")
	}
	requireAttempt(t, e, "crash", first)
	// The claim does not change what ingest stored.
	after := e.reportByKey(t, "crash")
	after.LinkingLastAttemptAt = nil
	requireReport(t, after, report)
	// A report the workspace does not hold.
	if _, _, err := x.Backend.ClaimFeedbackReportLinking(x.Context, "rpt-000000-000000", first); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown report err=%v, want not found", err)
	}
	// A report whose linking ended is never claimed again.
	finished, err := x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: report.ID, EndedAt: first.Add(2 * time.Minute), AttemptAt: first})
	requireOK(t, err)
	if !finished {
		t.Fatal("the end of linking wrote nothing")
	}
	if _, ok := claimLinking(t, x, report.ID, first.Add(time.Hour)); ok {
		t.Fatal("a report whose linking ended was claimed")
	}
	requireAttempt(t, e, "crash", first)
}

// runFeedbackLinkingBoundary proves that the claim compares the exact attempt
// time with the stored last attempt: an attempt one nanosecond or one
// microsecond under 60 seconds after it is refused, and one at 60 seconds
// succeeds. The stored time is the attempt cut to the microsecond.
func runFeedbackLinkingBoundary(t *testing.T, x Fixture) {
	e, report := linkingReport(t, x, "crash", nil)
	// An attempt time with nanoseconds is stored cut to the microsecond.
	first := feedbackTime(time.Minute).Add(789 * time.Nanosecond)
	stored := store.EnvironmentTime(first)
	if _, ok := claimLinking(t, x, report.ID, first); !ok {
		t.Fatal("first claim refused")
	}
	requireAttempt(t, e, "crash", stored)
	for _, under := range []time.Duration{time.Microsecond, time.Nanosecond} {
		if _, ok := claimLinking(t, x, report.ID, stored.Add(time.Minute-under)); ok {
			t.Fatalf("a claim %v under 60 seconds after the stored attempt succeeded", under)
		}
		requireAttempt(t, e, "crash", stored)
	}
	second := stored.Add(time.Minute)
	if _, ok := claimLinking(t, x, report.ID, second); !ok {
		t.Fatal("a claim at 60 seconds was refused")
	}
	requireAttempt(t, e, "crash", second)
}

// runFeedbackLinkingTwoWorkers proves that of two workers that claim one
// report in the same minute only one succeeds, both when they race and when
// they come one after the other.
func runFeedbackLinkingTwoWorkers(t *testing.T, x Fixture) {
	e, report := linkingReport(t, x, "crash", nil)
	now := feedbackTime(time.Minute)
	var wg sync.WaitGroup
	results := make([]bool, 2)
	failures := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i], failures[i] = x.Backend.ClaimFeedbackReportLinking(x.Context, report.ID, now.Add(time.Duration(i)*time.Second))
		}(i)
	}
	wg.Wait()
	for _, err := range failures {
		requireOK(t, err)
	}
	if results[0] == results[1] {
		t.Fatalf("racing claims=%v, want exactly one", results)
	}
	winner := now
	if results[1] {
		winner = now.Add(time.Second)
	}
	requireAttempt(t, e, "crash", winner)
	if _, ok := claimLinking(t, x, report.ID, winner.Add(59*time.Second)); ok {
		t.Fatal("a third claim within the minute succeeded")
	}
}

func runFeedbackLinkingLineRead(t *testing.T, x Fixture) {
	_, report := linkingReport(t, x, "crash", func(entry *store.FeedbackReportEntry) {
		entry.File, entry.Line, entry.Commit = "cmd/main.go", 12, "abcdef0"
	})
	if _, found, err := x.Backend.ReadFeedbackReportLineRead(x.Context, report.ID); err != nil || found {
		t.Fatalf("a read before any answer found=%v err=%v", found, err)
	}
	answered := feedbackTime(time.Minute).Add(321 * time.Nanosecond)
	read := store.FeedbackReportLineRead{ReportID: report.ID, Repository: "app", ResolvedCommit: feedbackCommit("e5"), File: "cmd/main.go", Line: 12,
		Answer: store.FeedbackReportLineAnswer{FileFound: true, Commit: feedbackCommit("c4"), PullRequests: []int{9, 3, 9}}, AnsweredAt: answered}
	requireOK(t, x.Backend.RecordFeedbackReportLineRead(x.Context, read))
	got, found, err := x.Backend.ReadFeedbackReportLineRead(x.Context, report.ID)
	requireOK(t, err)
	want := read
	want.Answer.PullRequests, want.AnsweredAt = []int{3, 9}, store.EnvironmentTime(answered)
	if !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("stored read=%+v found=%v, want %+v", got, found, want)
	}
	// A stored answer is kept: a second answer changes nothing.
	other := read
	other.Answer = store.FeedbackReportLineAnswer{FileFound: false}
	other.AnsweredAt = answered.Add(time.Minute)
	requireOK(t, x.Backend.RecordFeedbackReportLineRead(x.Context, other))
	if again, _, err := x.Backend.ReadFeedbackReportLineRead(x.Context, report.ID); err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("after a second answer=%+v err=%v, want %+v", again, err, want)
	}
	// A missing-file answer is an answer too.
	_, missing := linkingReportIn(t, x, "missing-file")
	requireOK(t, x.Backend.RecordFeedbackReportLineRead(x.Context, store.FeedbackReportLineRead{ReportID: missing.ID, Repository: "app", ResolvedCommit: feedbackCommit("e5"), File: "gone.go", Line: 1, AnsweredAt: answered}))
	if stored, found, err := x.Backend.ReadFeedbackReportLineRead(x.Context, missing.ID); err != nil || !found || stored.Answer.FileFound || stored.Answer.Commit != "" || len(stored.Answer.PullRequests) != 0 {
		t.Fatalf("missing file read=%+v found=%v err=%v", stored, found, err)
	}
	// Refusals record nothing.
	if err := x.Backend.RecordFeedbackReportLineRead(x.Context, store.FeedbackReportLineRead{ReportID: "rpt-000000-000000", Repository: "app", ResolvedCommit: feedbackCommit("e5"), File: "a.go", Line: 1, AnsweredAt: answered}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown report err=%v, want not found", err)
	}
	if err := x.Backend.RecordFeedbackReportLineRead(x.Context, store.FeedbackReportLineRead{ReportID: report.ID, Repository: "app", ResolvedCommit: "abc", File: "a.go", Line: 1, AnsweredAt: answered}); err == nil {
		t.Fatal("a short resolved commit was accepted")
	}
}

// linkingReportIn stores one more report in a new environment.
func linkingReportIn(t *testing.T, x Fixture, key string) (feedbackEnvironment, store.FeedbackReport) {
	t.Helper()
	e := newFeedbackEnvironment(t, x, "Staging "+key, "watcher-"+key)
	e.sendReports(t, 0, reportEntry(key))
	return e, e.reportByKey(t, key)
}

func lineEvidence(commit string) store.FeedbackReportEvidence {
	return store.FeedbackReportEvidence{Kind: store.FeedbackEvidenceLine, File: "cmd/main.go", Line: 12, Commit: commit}
}

func timingEvidence(deployID string, elapsed int64, prior *time.Time) store.FeedbackReportEvidence {
	return store.FeedbackReportEvidence{Kind: store.FeedbackEvidenceTiming, Deploy: &store.FeedbackReportDeploy{ID: deployID, Service: "api",
		Version: store.FeedbackReportVersion{Version: "2.0"}, DeployedAt: feedbackTime(time.Minute).Add(7 * time.Microsecond), Source: store.FeedbackSourceSignal},
		ElapsedMinutes: &elapsed, PriorSignalAt: prior}
}

func linksByTask(t *testing.T, x Fixture, reportID string) map[string]store.FeedbackReportLink {
	t.Helper()
	links, err := x.Backend.ListFeedbackReportLinks(x.Context, reportID)
	requireOK(t, err)
	out := map[string]store.FeedbackReportLink{}
	for _, link := range links {
		out[link.TaskID] = link
	}
	return out
}

func linkedEvents(t *testing.T, x Fixture) []core.Event {
	t.Helper()
	events := []core.Event{}
	for _, event := range reportEvents(t, x) {
		if event.Kind == store.FeedbackReportLinkedEvent {
			events = append(events, event)
		}
	}
	return events
}

// runFeedbackLinkingFinish proves the end of linking: a new link is possible
// with the system as its creator, a confirmed link keeps its state and gains
// only evidence it lacks, a removed link is not restored and keeps its
// evidence, the notes keep their order, linking ends with one task-less
// linked event and one workspace notice, and a second end writes nothing
// (invariants 11, 15 to 17, and 20; AC-5.6, AC-7.2).
func runFeedbackLinkingFinish(t *testing.T, x Fixture) {
	e, report := linkingReport(t, x, "crash", func(entry *store.FeedbackReportEntry) {
		entry.Service, entry.File, entry.Line, entry.Commit = "api", "cmd/main.go", 12, "abcdef0"
	})
	confirmedAt := feedbackTime(30 * time.Second)
	removedAt := feedbackTime(40 * time.Second)
	held := lineEvidence(feedbackCommit("be"))
	x.SeedFeedbackReportLink(t, x.Context, store.FeedbackReportLink{ID: store.FeedbackReportLinkID(report.ID, 1), ReportID: report.ID, TaskID: "task-confirmed", State: store.FeedbackReportLinkConfirmed,
		Evidence: []store.FeedbackReportEvidence{held}, CreatedAt: confirmedAt, CreatedBy: "usr-operator", ConfirmedBy: "usr-operator", ConfirmedAt: &confirmedAt})
	x.SeedFeedbackReportLink(t, x.Context, store.FeedbackReportLink{ID: store.FeedbackReportLinkID(report.ID, 2), ReportID: report.ID, TaskID: "task-removed", State: store.FeedbackReportLinkRemoved,
		Evidence: []store.FeedbackReportEvidence{held}, CreatedAt: confirmedAt, CreatedBy: store.SystemActor().ID, RemovedBy: "usr-operator", RemovedAt: &removedAt})
	before := linksByTask(t, x, report.ID)
	prior := feedbackTime(-time.Hour).Add(3 * time.Microsecond)
	timing := timingEvidence("dep-1", 0, &prior)
	fresh := lineEvidence(feedbackCommit("f5"))
	notes := []store.FeedbackReportNote{
		{Kind: store.FeedbackNoteNoTaskDeploy, Deploy: timing.Deploy},
		{Kind: store.FeedbackNoteTooManyTasks, Deploy: timing.Deploy, TaskCount: 11},
		{Kind: store.FeedbackNoteNotChecked, Rule: store.FeedbackRuleLine, Reason: store.FeedbackReasonNoGitHubApp},
	}
	ended := feedbackTime(2 * time.Minute).Add(999 * time.Nanosecond)
	notices := x.Backend.Changes().Subscribe(x.Workspace, changes.Workspace)
	defer notices.Close()
	attemptAt := claimedAt(t, x, report.ID, feedbackTime(time.Minute))
	finished, err := x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: report.ID, EndedAt: ended, AttemptAt: attemptAt, Notes: notes, Links: []store.FeedbackReportLinkCandidate{
		{TaskID: "task-new", Evidence: []store.FeedbackReportEvidence{fresh, timing, fresh}},
		{TaskID: "task-confirmed", Evidence: []store.FeedbackReportEvidence{held, timing}},
		{TaskID: "task-removed", Evidence: []store.FeedbackReportEvidence{timing}},
	}})
	requireOK(t, err)
	if !finished {
		t.Fatal("the end of linking wrote nothing")
	}
	if n := awaitNotice(t, notices, "workspace notice after the end of linking"); n.Kind != changes.Workspace || n.Workspace != x.Workspace {
		t.Fatalf("notice=%+v", n)
	}
	settle(notices)
	links := linksByTask(t, x, report.ID)
	if len(links) != 3 {
		t.Fatalf("links=%+v, want three", links)
	}
	endedAt := store.EnvironmentTime(ended)
	created := links["task-new"]
	if created.State != store.FeedbackReportLinkPossible || created.CreatedBy != store.SystemActor().ID || !created.CreatedAt.Equal(endedAt) || created.ConfirmedAt != nil || created.RemovedAt != nil ||
		!reflect.DeepEqual(created.Evidence, []store.FeedbackReportEvidence{fresh, timing}) || !strings.HasPrefix(created.ID, "lnk:"+report.ID+":") || created.ID == before["task-confirmed"].ID || created.ID == before["task-removed"].ID {
		t.Fatalf("new link=%+v", created)
	}
	confirmed := links["task-confirmed"]
	wantConfirmed := before["task-confirmed"]
	wantConfirmed.Evidence = []store.FeedbackReportEvidence{held, timing}
	if !reflect.DeepEqual(confirmed, wantConfirmed) {
		t.Fatalf("confirmed link=%+v, want %+v", confirmed, wantConfirmed)
	}
	if removed := links["task-removed"]; !reflect.DeepEqual(removed, before["task-removed"]) {
		t.Fatalf("removed link=%+v, want it unchanged %+v", removed, before["task-removed"])
	}
	storedNotes, err := x.Backend.ListFeedbackReportNotes(x.Context, report.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(storedNotes, notes) {
		t.Fatalf("notes=%+v, want %+v", storedNotes, notes)
	}
	after := e.reportByKey(t, "crash")
	if after.LinkingEndedAt == nil || !after.LinkingEndedAt.Equal(endedAt) {
		t.Fatalf("linking ended at %v, want %v", after.LinkingEndedAt, endedAt)
	}
	events := linkedEvents(t, x)
	if len(events) != 1 {
		t.Fatalf("linked events=%+v, want one", events)
	}
	event := events[0]
	if event.TaskID != "" || event.ActorID != store.SystemActor().ID || event.ActorRole != core.ActorSystem {
		t.Fatalf("linked event=%+v, want a task-less event by the system", event)
	}
	var payload struct {
		ReportID      string   `json:"report_id"`
		EnvironmentID string   `json:"environment_id"`
		TaskIDs       []string `json:"task_ids"`
	}
	requireOK(t, json.Unmarshal(event.Payload, &payload))
	if payload.ReportID != report.ID || payload.EnvironmentID != report.EnvironmentID || !slices.Equal(payload.TaskIDs, []string{"task-confirmed", "task-new"}) {
		t.Fatalf("linked payload=%s", event.Payload)
	}
	for _, text := range []string{report.Key, report.Summary, "cmd/main.go"} {
		if strings.Contains(string(event.Payload), text) {
			t.Fatalf("linked payload %s holds report text %q", event.Payload, text)
		}
	}
	// A second end writes nothing: no link, note, event, or notice.
	finished, err = x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: report.ID, EndedAt: ended.Add(time.Hour), Notes: notes,
		Links: []store.FeedbackReportLinkCandidate{{TaskID: "task-late", Evidence: []store.FeedbackReportEvidence{fresh}}}})
	requireOK(t, err)
	if finished {
		t.Fatal("a second end of linking wrote")
	}
	expectQuiet(t, notices, "a second end of linking")
	if again := linksByTask(t, x, report.ID); !reflect.DeepEqual(again, links) {
		t.Fatalf("links after a second end=%+v", again)
	}
	if again, _ := x.Backend.ListFeedbackReportNotes(x.Context, report.ID); len(again) != len(notes) {
		t.Fatalf("notes after a second end=%+v", again)
	}
	if len(linkedEvents(t, x)) != 1 {
		t.Fatal("a second end appended an event")
	}
	if again := e.reportByKey(t, "crash"); !again.LinkingEndedAt.Equal(endedAt) {
		t.Fatalf("a second end moved the end to %v", again.LinkingEndedAt)
	}
	// An unknown report.
	if _, err := x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: "rpt-000000-000000", EndedAt: ended}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown report err=%v, want not found", err)
	}
}

// runFeedbackLinkingLaterEntries proves that a later entry under the key,
// before and after the end of linking, changes no link, no note, and the
// first entry's first-seen time the rules read, and that a report that
// another environment holds is untouched (invariants 6 and 11; AC-5.6).
func runFeedbackLinkingLaterEntries(t *testing.T, x Fixture) {
	firstSeen := feedbackReportTimePtr(-time.Hour)
	e, report := linkingReport(t, x, "crash", func(entry *store.FeedbackReportEntry) { entry.FirstSeen = firstSeen })
	_, other := linkingReportIn(t, x, "other")
	earlier := reportEntry("crash")
	earlier.FirstSeen = feedbackReportTimePtr(-2 * time.Hour)
	e.sendReports(t, 10*time.Second, earlier)
	if updated := e.reportByKey(t, "crash"); !updated.FirstEntrySeen.Equal(report.FirstEntrySeen) || !updated.FirstSeen.Equal(*earlier.FirstSeen) {
		t.Fatalf("after an earlier entry=%+v, want the first entry's first-seen kept", updated)
	}
	if _, ok := claimLinking(t, x, report.ID, feedbackTime(time.Minute)); !ok {
		t.Fatal("claim refused")
	}
	finished, err := x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: report.ID, EndedAt: feedbackTime(2 * time.Minute), AttemptAt: feedbackTime(time.Minute),
		Links: []store.FeedbackReportLinkCandidate{{TaskID: "task-1", Evidence: []store.FeedbackReportEvidence{lineEvidence(feedbackCommit("01"))}}}})
	requireOK(t, err)
	if !finished {
		t.Fatal("the end of linking wrote nothing")
	}
	links := linksByTask(t, x, report.ID)
	e.sendReports(t, 20*time.Second, reportEntry("crash"))
	if again := linksByTask(t, x, report.ID); !reflect.DeepEqual(again, links) {
		t.Fatalf("a later entry changed links: %+v", again)
	}
	if updated := e.reportByKey(t, "crash"); updated.LinkingEndedAt == nil || updated.Count != 3 || !updated.FirstEntrySeen.Equal(report.FirstEntrySeen) {
		t.Fatalf("after the end and a later entry=%+v", updated)
	}
	if otherLinks, err := x.Backend.ListFeedbackReportLinks(x.Context, other.ID); err != nil || len(otherLinks) != 0 {
		t.Fatalf("another report's links=%+v err=%v", otherLinks, err)
	}
	if pending, err := x.Backend.ListPendingFeedbackReports(x.Context, e.id); err != nil || len(pending) != 0 {
		t.Fatalf("pending after the end=%+v err=%v", pending, err)
	}
}

// runFeedbackLinkingLinkIDs proves that a new link never takes an ID that a
// stored link of the report holds, whatever task that link names: the link
// IDs of a report are numbered under the report write lock, and a taken
// number is skipped.
func runFeedbackLinkingLinkIDs(t *testing.T, x Fixture) {
	e, report := linkingReport(t, x, "crash", nil)
	// A link stored with the first and third numbers, as another writer of
	// the report could have stored them.
	at := feedbackTime(30 * time.Second)
	for _, n := range []int{1, 3} {
		x.SeedFeedbackReportLink(t, x.Context, store.FeedbackReportLink{ID: store.FeedbackReportLinkID(report.ID, n), ReportID: report.ID, TaskID: "task-seeded-" + string(rune('0'+n)),
			State: store.FeedbackReportLinkConfirmed, Evidence: []store.FeedbackReportEvidence{}, CreatedAt: at, CreatedBy: "usr-operator", ConfirmedBy: "usr-operator", ConfirmedAt: &at})
	}
	finished, err := x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: report.ID, EndedAt: feedbackTime(time.Minute), AttemptAt: claimedAt(t, x, report.ID, feedbackTime(time.Minute)), Links: []store.FeedbackReportLinkCandidate{
		{TaskID: "task-a", Evidence: []store.FeedbackReportEvidence{lineEvidence(feedbackCommit("a"))}},
		{TaskID: "task-b", Evidence: []store.FeedbackReportEvidence{lineEvidence(feedbackCommit("b"))}},
	}})
	requireOK(t, err)
	if !finished {
		t.Fatal("the end of linking wrote nothing")
	}
	links := linksByTask(t, x, report.ID)
	if links["task-a"].ID != store.FeedbackReportLinkID(report.ID, 2) || links["task-b"].ID != store.FeedbackReportLinkID(report.ID, 4) {
		t.Fatalf("new link IDs a=%s b=%s, want numbers 2 and 4", links["task-a"].ID, links["task-b"].ID)
	}
	ids := map[string]bool{}
	for _, link := range links {
		if ids[link.ID] {
			t.Fatalf("two links share ID %s", link.ID)
		}
		ids[link.ID] = true
	}
	// Another report's first link number names that report, so it never
	// equals a link ID of this one.
	_, other := linkingReportIn(t, x, "other")
	finished, err = x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: other.ID, EndedAt: feedbackTime(time.Minute), AttemptAt: claimedAt(t, x, other.ID, feedbackTime(time.Minute)), Links: []store.FeedbackReportLinkCandidate{
		{TaskID: "task-a", Evidence: []store.FeedbackReportEvidence{lineEvidence(feedbackCommit("a"))}}}})
	requireOK(t, err)
	if otherLinks := linksByTask(t, x, other.ID); !finished || otherLinks["task-a"].ID != store.FeedbackReportLinkID(other.ID, 1) || ids[otherLinks["task-a"].ID] {
		t.Fatalf("other report links=%+v", otherLinks)
	}
	_ = e
}

// runFeedbackLinkingServiceSignals proves the read of the watcher signals
// that named a service: scoped by environment and service, GitHub
// deployment reports left out, ties at one arrival ordered by report number,
// and strict bounds on both sides (invariants 13 and 15).
func runFeedbackLinkingServiceSignals(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	other := newFeedbackEnvironment(t, x, "Staging", "staging-watcher")
	e.send(t, 0, feedbackService("api", "@1"))
	e.send(t, 10*time.Second, feedbackService("web", "@1"))
	// Two signals at one arrival: report numbers order them.
	e.send(t, 20*time.Second, feedbackService("api", "@2"))
	e.send(t, 20*time.Second, feedbackService("api", "@2"), feedbackService("web", "@1"))
	app := feedbackService("app", "@1")
	app.Repository = "app"
	e.send(t, 25*time.Second, app)
	e.report(t, e.github(30*time.Second, "app", feedbackCommit("a")), true)
	other.send(t, 40*time.Second, feedbackService("api", "@9"))
	find := func(query store.FeedbackServiceSignalQuery) (store.FeedbackBoundary, bool) {
		t.Helper()
		found, ok, err := x.Backend.FindFeedbackServiceSignal(x.Context, query)
		requireOK(t, err)
		return found, ok
	}
	deploysAt := func(service string, at time.Time) []store.FeedbackDeploy {
		out := []store.FeedbackDeploy{}
		for _, deploy := range e.deploys(t) {
			if deploy.Service == service && deploy.DeployedAt.Equal(at) {
				out = append(out, deploy)
			}
		}
		return out
	}
	tied := deploysAt("api", feedbackTime(20*time.Second))
	if len(tied) != 1 {
		t.Fatalf("api deploys at 20 seconds=%+v", tied)
	}
	// Before the first tied signal: the signal at 0 seconds.
	before := tied[0].Start()
	if found, ok := find(store.FeedbackServiceSignalQuery{EnvironmentID: e.id, Service: "api", Before: &before}); !ok || !found.At.Equal(feedbackTime(0)) {
		t.Fatalf("before the tied deploy=%+v ok=%v, want the signal at 0", found, ok)
	}
	// Before a place after both tied signals: the later of the two.
	late := store.FeedbackBoundary{At: feedbackTime(20 * time.Second), Report: before.Report + 100}
	found, ok := find(store.FeedbackServiceSignalQuery{EnvironmentID: e.id, Service: "api", Before: &late})
	if !ok || !found.At.Equal(feedbackTime(20*time.Second)) || found.Report <= before.Report {
		t.Fatalf("before a later place=%+v ok=%v, want the second tied signal", found, ok)
	}
	// After: strictly later arrivals only, and the oldest of them.
	at0 := feedbackTime(0)
	if found, ok := find(store.FeedbackServiceSignalQuery{EnvironmentID: e.id, Service: "api", After: &at0}); !ok || !found.At.Equal(feedbackTime(20*time.Second)) || found.Report != before.Report {
		t.Fatalf("after 0 seconds=%+v ok=%v, want the first signal at 20 seconds", found, ok)
	}
	at20 := feedbackTime(20 * time.Second)
	if found, ok := find(store.FeedbackServiceSignalQuery{EnvironmentID: e.id, Service: "api", After: &at20}); ok {
		t.Fatalf("after 20 seconds=%+v, want none in this environment", found)
	}
	// A GitHub deployment report of service app is never found.
	at25 := feedbackTime(25 * time.Second)
	if found, ok := find(store.FeedbackServiceSignalQuery{EnvironmentID: e.id, Service: "app", After: &at25}); ok {
		t.Fatalf("after 25 seconds for app=%+v, want the GitHub report left out", found)
	}
	github := deploysAt("app", feedbackTime(30*time.Second))
	if len(github) != 1 {
		t.Fatalf("GitHub deploys=%+v", github)
	}
	githubPlace := github[0].Start()
	if found, ok := find(store.FeedbackServiceSignalQuery{EnvironmentID: e.id, Service: "app", Before: &githubPlace}); !ok || !found.At.Equal(at25) {
		t.Fatalf("before the GitHub deploy=%+v ok=%v, want the watcher signal at 25 seconds", found, ok)
	}
	// Another service, another environment, and a bad query.
	if found, ok := find(store.FeedbackServiceSignalQuery{EnvironmentID: e.id, Service: "worker", After: &at0}); ok {
		t.Fatalf("a service no signal named=%+v", found)
	}
	if found, ok := find(store.FeedbackServiceSignalQuery{EnvironmentID: other.id, Service: "api", After: &at0}); !ok || !found.At.Equal(feedbackTime(40*time.Second)) {
		t.Fatalf("the other environment=%+v ok=%v", found, ok)
	}
	if _, _, err := x.Backend.FindFeedbackServiceSignal(x.Context, store.FeedbackServiceSignalQuery{EnvironmentID: e.id, Service: "api"}); err == nil {
		t.Fatal("a query with neither bound was accepted")
	}
}

// runFeedbackLinkingMergePullRequest proves that the merged-task read returns
// the pull request of the latest merge event, also when that event holds no
// usable merge commit, and never an older event's (invariant 12).
func runFeedbackLinkingMergePullRequest(t *testing.T, x Fixture) {
	event := func(kind, sha string, pull any, at time.Time) core.Event {
		payload := map[string]any{"repository": "acme/conveyor", "head_sha": "head", "merge_commit_sha": sha}
		if pull != nil {
			payload["pull_request"] = pull
		}
		return core.Event{Kind: kind, At: at, Payload: core.JSONPayload(payload)}
	}
	latest := feedbackMergedTask(t, x, "app", "",
		event("merge.confirmed", feedbackCommit("01d"), 5, feedbackTime(0)),
		event("merge.reconciled", "not-a-commit", 9, feedbackTime(time.Minute)))
	text := feedbackMergedTask(t, x, "app", "", event("merge.confirmed", feedbackCommit("7e"), "12", feedbackTime(0)))
	none := feedbackMergedTask(t, x, "app", "",
		event("merge.confirmed", feedbackCommit("f1"), 4, feedbackTime(0)),
		event("merge.confirmed", feedbackCommit("0e"), nil, feedbackTime(time.Minute)))
	merged, err := x.Backend.ListFeedbackMergedTasks(x.Context, store.FeedbackMergedTaskQuery{Repository: "app"})
	requireOK(t, err)
	got := map[string]store.FeedbackMergedTask{}
	for _, task := range merged {
		got[task.TaskID] = task
	}
	if task := got[latest.ID]; task.PullRequest != 9 || task.MergeCommit != "" {
		t.Fatalf("latest event without a commit=%+v, want pull request 9 and no commit", task)
	}
	if task := got[text.ID]; task.PullRequest != 12 || task.MergeCommit != feedbackCommit("7e") {
		t.Fatalf("a pull request as text=%+v", task)
	}
	if task := got[none.ID]; task.PullRequest != 0 || task.MergeCommit != feedbackCommit("0e") {
		t.Fatalf("a latest event without a pull request=%+v, want 0", task)
	}
}

// runFeedbackLinkingMergeRepository proves that the merged-task read returns
// the GitHub repository of the latest merge event, in canonical form, from
// the same event as the merge commit and the pull request number. It never
// returns an older event's repository, and a latest event with no usable
// repository returns none. The configured name is returned as it is, so a
// name pointed at another repository later does not change the merge
// event's repository (component-feedback-reports invariants 12 and 14).
func runFeedbackLinkingMergeRepository(t *testing.T, x Fixture) {
	event := func(repository any, sha string, at time.Time) core.Event {
		payload := map[string]any{"head_sha": "head", "merge_commit_sha": sha, "pull_request": 42}
		if repository != nil {
			payload["repository"] = repository
		}
		return core.Event{Kind: "merge.confirmed", At: at, Payload: core.JSONPayload(payload)}
	}
	old := feedbackMergedTask(t, x, "app", "", event("Acme/Old", feedbackCommit("a1"), feedbackTime(0)))
	moved := feedbackMergedTask(t, x, "app", "",
		event("acme/old", feedbackCommit("a2"), feedbackTime(0)),
		event("acme/new", feedbackCommit("a3"), feedbackTime(time.Minute)))
	missing := feedbackMergedTask(t, x, "app", "",
		event("acme/old", feedbackCommit("a4"), feedbackTime(0)),
		event(nil, feedbackCommit("a5"), feedbackTime(time.Minute)))
	bad := feedbackMergedTask(t, x, "app", "", event("not a slug", feedbackCommit("a6"), feedbackTime(0)))
	number := feedbackMergedTask(t, x, "app", "", event(7, feedbackCommit("a7"), feedbackTime(0)))
	quoted := feedbackMergedTask(t, x, "app", "", event(`"acme/app"`, feedbackCommit("a9"), feedbackTime(0)))
	object := feedbackMergedTask(t, x, "app", "", event(map[string]any{"slug": "acme/old"}, feedbackCommit("a8"), feedbackTime(0)))
	merged, err := x.Backend.ListFeedbackMergedTasks(x.Context, store.FeedbackMergedTaskQuery{Repository: "app"})
	requireOK(t, err)
	got := map[string]store.FeedbackMergedTask{}
	for _, task := range merged {
		got[task.TaskID] = task
	}
	for _, c := range []struct {
		name, id, want, commit string
	}{
		{"canonical slug", old.ID, "acme/old", feedbackCommit("a1")},
		{"latest event wins", moved.ID, "acme/new", feedbackCommit("a3")},
		{"latest event without a repository", missing.ID, "", feedbackCommit("a5")},
		{"not a slug", bad.ID, "", feedbackCommit("a6")},
		{"not text", number.ID, "", feedbackCommit("a7")},
		{"a quoted slug inside the string", quoted.ID, "", feedbackCommit("a9")},
		{"an object", object.ID, "", feedbackCommit("a8")},
	} {
		task := got[c.id]
		if task.GitHubRepository != c.want || task.MergeCommit != c.commit || task.PullRequest != 42 || task.Repository != "app" {
			t.Fatalf("%s: task=%+v, want repository %q with commit %s", c.name, task, c.want, c.commit)
		}
	}
}

// runFeedbackLinkingMergeEventShapes proves, for each member of a merge
// event that linking uses as evidence, that every backend decodes it once
// and gives the same answer: the repository is exactly one owner/name slug,
// the commit a full commit ID, and the pull request number a positive whole
// number. Every malformed shape proves nothing and answers the zero value
// (component-feedback-reports invariants 12 and 13).
func runFeedbackLinkingMergeEventShapes(t *testing.T, x Fixture) {
	good := map[string]json.RawMessage{
		"repository":       json.RawMessage(`"acme/app"`),
		"merge_commit_sha": json.RawMessage(`"` + feedbackCommit("b0") + `"`),
		"pull_request":     json.RawMessage(`42`),
	}
	type shape struct {
		name   string
		member string
		raw    string // "" removes the member
		want   any
	}
	shapes := []shape{
		{"repository: a quoted slug inside the string", "repository", `"\"acme/app\""`, ""},
		{"repository: a space inside", "repository", `"acme /app"`, ""},
		{"repository: padding", "repository", `" acme/app"`, ""},
		{"repository: three parts", "repository", `"acme/app/x"`, ""},
		{"repository: no owner", "repository", `"/app"`, ""},
		{"repository: empty", "repository", `""`, ""},
		{"repository: null", "repository", `null`, ""},
		{"repository: missing", "repository", "", ""},
		{"repository: an array", "repository", `["acme/app"]`, ""},
		{"repository: a number", "repository", `12`, ""},
		{"repository: upper case is canonical", "repository", `"Acme/App"`, "acme/app"},
		{"commit: a quoted commit inside the string", "merge_commit_sha", `"\"` + feedbackCommit("b1") + `\""`, ""},
		{"commit: padding", "merge_commit_sha", `" ` + feedbackCommit("b2") + `"`, ""},
		{"commit: short", "merge_commit_sha", `"abc1234"`, ""},
		{"commit: forty digits as a number", "merge_commit_sha", strings.Repeat("1", 40), ""},
		{"commit: null", "merge_commit_sha", `null`, ""},
		{"commit: missing", "merge_commit_sha", "", ""},
		{"commit: upper case is canonical", "merge_commit_sha", `"` + strings.ToUpper(feedbackCommit("b3")) + `"`, feedbackCommit("b3")},
		{"pull request: a digit string", "pull_request", `"42"`, 42},
		{"pull request: a quoted number inside the string", "pull_request", `"\"42\""`, 0},
		{"pull request: a padded string", "pull_request", `" 42"`, 0},
		{"pull request: a leading zero", "pull_request", `"042"`, 0},
		{"pull request: a negative number", "pull_request", `-3`, 0},
		{"pull request: zero", "pull_request", `0`, 0},
		{"pull request: a fraction", "pull_request", `42.5`, 0},
		{"pull request: a whole number with a fraction part", "pull_request", `42.0`, 42},
		{"pull request: an exponent", "pull_request", `4.2e1`, 42},
		{"pull request: too large", "pull_request", `1e20`, 0},
		{"pull request: a boolean", "pull_request", `true`, 0},
		{"pull request: null", "pull_request", `null`, 0},
		{"pull request: missing", "pull_request", "", 0},
	}
	ids := map[string]string{}
	for i, sh := range shapes {
		payload := map[string]json.RawMessage{}
		for key, value := range good {
			payload[key] = value
		}
		if sh.raw == "" {
			delete(payload, sh.member)
		} else {
			payload[sh.member] = json.RawMessage(sh.raw)
		}
		raw, err := json.Marshal(payload)
		requireOK(t, err)
		task := feedbackMergedTask(t, x, "shapes", "", core.Event{Kind: "merge.confirmed", At: feedbackTime(time.Duration(i) * time.Second), Payload: raw})
		ids[sh.name] = task.ID
	}
	merged, err := x.Backend.ListFeedbackMergedTasks(x.Context, store.FeedbackMergedTaskQuery{Repository: "shapes"})
	requireOK(t, err)
	got := map[string]store.FeedbackMergedTask{}
	for _, task := range merged {
		got[task.TaskID] = task
	}
	for _, sh := range shapes {
		task, found := got[ids[sh.name]]
		if !found {
			t.Fatalf("%s: task not read", sh.name)
		}
		wantRepository, wantCommit, wantPull := "acme/app", feedbackCommit("b0"), 42
		switch sh.member {
		case "repository":
			wantRepository = sh.want.(string)
		case "merge_commit_sha":
			wantCommit = sh.want.(string)
		case "pull_request":
			wantPull = sh.want.(int)
		}
		if task.GitHubRepository != wantRepository || task.MergeCommit != wantCommit || task.PullRequest != wantPull {
			t.Fatalf("%s: repository=%q commit=%q pull=%d, want %q %q %d", sh.name, task.GitHubRepository, task.MergeCommit, task.PullRequest, wantRepository, wantCommit, wantPull)
		}
	}
}

// runFeedbackLinkingMalformedEvidence proves that every backend refuses each
// malformed shape of the stored evidence that a linking rule reads, so none
// is ever stored and none can match: line progress with a repository that is
// not exactly one owner/name slug, a line answer whose parts disagree, and a
// timing result whose notes or links are incomplete. Nothing is written for
// any of them (component-feedback-reports invariants 12 to 15).
func runFeedbackLinkingMalformedEvidence(t *testing.T, x Fixture) {
	e, report := linkingReport(t, x, "crash", func(entry *store.FeedbackReportEntry) {
		entry.Service, entry.File, entry.Line, entry.Commit = "api", "cmd/main.go", 12, "abcdef0"
	})
	attemptAt := claimedAt(t, x, report.ID, feedbackTime(10*time.Second))
	input := feedbackLineInput("1")
	commit := feedbackCommit("c0")
	for _, step := range []store.FeedbackLineStep{
		{Candidate: `"acme/app"`, Answer: commit, Input: input, Keep: []string{`"acme/app"`}},
		{Candidate: "acme/app/x", Answer: commit, Input: input, Keep: []string{"acme/app/x"}},
		{Candidate: "acme /app", Answer: commit, Input: input, Keep: []string{"acme /app"}},
		{Candidate: "acme/app", Answer: commit, Input: input, Keep: []string{"acme/app", " acme/x"}},
		{Repository: "app", Slug: `"acme/app"`, Commit: commit},
		{Repository: "app", Slug: "acme/app/x", Commit: commit},
		{Repository: "app", Slug: "acme", Commit: commit},
		{Repository: "app", Slug: "acme/app", Commit: strings.ToUpper(commit)},
		{Repository: "", Slug: "acme/app", Commit: commit},
	} {
		if _, err := x.Backend.SaveFeedbackLineProgress(x.Context, report.ID, attemptAt, step); err == nil {
			t.Fatalf("line step %+v was stored", step)
		}
	}
	answered := feedbackTime(time.Minute)
	for name, answer := range map[string]store.FeedbackReportLineAnswer{
		"a found file without its commit":       {FileFound: true, PullRequests: []int{}},
		"a found file with a short commit":      {FileFound: true, Commit: "abc1234", PullRequests: []int{}},
		"a missing file with a commit":          {FileFound: false, Commit: commit, PullRequests: []int{}},
		"a missing file with pull requests":     {FileFound: false, PullRequests: []int{7}},
		"a pull request number that is not one": {FileFound: true, Commit: commit, PullRequests: []int{0}},
		"a negative pull request number":        {FileFound: true, Commit: commit, PullRequests: []int{-7}},
		"an upper-case commit":                  {FileFound: true, Commit: strings.ToUpper(commit), PullRequests: []int{}},
	} {
		read := store.FeedbackReportLineRead{ReportID: report.ID, Repository: "app", ResolvedCommit: commit, File: "cmd/main.go", Line: 12, Answer: answer, AnsweredAt: answered}
		if err := x.Backend.RecordFeedbackReportLineRead(x.Context, read); err == nil {
			t.Fatalf("%s: the line answer was stored", name)
		}
	}
	for name, result := range map[string]store.FeedbackTimingResult{
		"a link without its task":                  {Links: []store.FeedbackTimingLink{{DeployID: "dep-1"}}},
		"a link without its deploy":                {Links: []store.FeedbackTimingLink{{TaskID: "task-1"}}},
		"negative minutes":                         {Links: []store.FeedbackTimingLink{{TaskID: "task-1", DeployID: "dep-1", ElapsedMinutes: -1}}},
		"a note without its deploy":                {Notes: []store.FeedbackTimingNote{{Kind: store.FeedbackNoteNoTaskDeploy}}},
		"too_many_tasks without a count":           {Notes: []store.FeedbackTimingNote{{Kind: store.FeedbackNoteTooManyTasks, DeployID: "dep-1"}}},
		"no_task_deploy with a count":              {Notes: []store.FeedbackTimingNote{{Kind: store.FeedbackNoteNoTaskDeploy, DeployID: "dep-1", TaskCount: 3}}},
		"a note kind the timing rule never writes": {Notes: []store.FeedbackTimingNote{{Kind: store.FeedbackNoteFileNotFound, DeployID: "dep-1"}}},
	} {
		if _, err := x.Backend.SaveFeedbackTimingResult(x.Context, report.ID, attemptAt, result); err == nil {
			t.Fatalf("%s: the timing result was stored", name)
		}
	}
	stored := pendingReport(t, x, e, "crash")
	if stored.LineProgress != nil || stored.TimingResult != nil {
		t.Fatalf("malformed evidence was stored: %+v %+v", stored.LineProgress, stored.TimingResult)
	}
	if _, found, err := x.Backend.ReadFeedbackReportLineRead(x.Context, report.ID); err != nil || found {
		t.Fatalf("a malformed line answer was stored: found=%v err=%v", found, err)
	}
}

func pendingReport(t *testing.T, x Fixture, e feedbackEnvironment, key string) store.FeedbackReport {
	t.Helper()
	pending, err := x.Backend.ListPendingFeedbackReports(x.Context, e.id)
	requireOK(t, err)
	for _, report := range pending {
		if report.Key == key {
			return report
		}
	}
	t.Fatalf("no pending report of key %q", key)
	return store.FeedbackReport{}
}

// feedbackLineInput is a resolution input digest of the conformance cases.
func feedbackLineInput(seed string) string { return strings.Repeat(seed, 64)[:64] }

// runFeedbackLinkingLineProgress proves the stored progress of resolving, so
// a fresh worker resumes from the store alone: candidate answers are kept and
// never replaced, the resolution or the definitive reason is stored once and
// drops the interim answers, a retryable reason is refused, every read of the
// report returns the stored progress, no served form of the report holds it,
// and another report is untouched (approved plan version 3;
// component-feedback-reports invariants 12 and 15).
func runFeedbackLinkingLineProgress(t *testing.T, x Fixture) {
	e, report := linkingReport(t, x, "crash", func(entry *store.FeedbackReportEntry) {
		entry.File, entry.Line, entry.Commit = "cmd/main.go", 12, "abcdef0"
	})
	_, other := linkingReportIn(t, x, "other")
	if report.LineProgress != nil || report.TimingResult != nil {
		t.Fatalf("a new report holds progress %+v %+v", report.LineProgress, report.TimingResult)
	}
	attemptAt := claimedAt(t, x, report.ID, feedbackTime(-time.Minute))
	save := func(step store.FeedbackLineStep) store.FeedbackLineProgress {
		t.Helper()
		progress, err := x.Backend.SaveFeedbackLineProgress(x.Context, report.ID, attemptAt, step)
		requireOK(t, err)
		return progress
	}
	commit := feedbackCommit("c0")
	input := feedbackLineInput("a")
	keep := []string{"acme/app", "acme/docs"}
	save(store.FeedbackLineStep{Candidate: "acme/app", Answer: store.FeedbackLineAbsent, Input: input, Keep: keep})
	progress := save(store.FeedbackLineStep{Candidate: "acme/docs", Answer: commit, Input: input, Keep: keep})
	want := store.FeedbackLineProgress{Input: input, Candidates: map[string]string{"acme/app": store.FeedbackLineAbsent, "acme/docs": commit}}
	if !reflect.DeepEqual(progress, want) {
		t.Fatalf("progress=%+v, want %+v", progress, want)
	}
	// A second answer for a candidate is not stored.
	if again := save(store.FeedbackLineStep{Candidate: "acme/app", Answer: commit, Input: input, Keep: keep}); !reflect.DeepEqual(again, want) {
		t.Fatalf("a second answer replaced the first: %+v", again)
	}
	// Retryable reasons and malformed steps are refused and store nothing.
	for _, step := range []store.FeedbackLineStep{
		{Unresolved: store.FeedbackReasonPending},
		{Unresolved: "forge_status"},
		{Unresolved: store.FeedbackReasonNoGitHubApp},
		{Candidate: "acme/app", Answer: "abc", Input: input, Keep: keep},
		{Candidate: "Acme/App", Answer: store.FeedbackLineAbsent, Input: input, Keep: keep},
		{Repository: "docs", Slug: "acme/docs", Commit: "abc"},
		{Repository: "docs", Commit: feedbackCommit("c0")},
		{Candidate: "acme/app", Answer: store.FeedbackLineAbsent, Unresolved: store.FeedbackReasonNoMatch, Input: input, Keep: keep},
		// A candidate answer needs a digest input and names a current
		// candidate; only a candidate answer names an input.
		{Candidate: "acme/app", Answer: store.FeedbackLineAbsent, Keep: keep},
		{Candidate: "acme/app", Answer: store.FeedbackLineAbsent, Input: "tags/v1.2", Keep: keep},
		{Candidate: "acme/app", Answer: store.FeedbackLineAbsent, Input: strings.ToUpper(input), Keep: keep},
		{Candidate: "acme/app", Answer: store.FeedbackLineAbsent, Input: input, Keep: []string{"acme/docs"}},
		{Candidate: "acme/app", Answer: store.FeedbackLineAbsent, Input: input, Keep: []string{"acme/app", "Acme/Docs"}},
		{Unresolved: store.FeedbackReasonNoMatch, Input: input},
		{Repository: "docs", Slug: "acme/docs", Commit: commit, Keep: keep},
		{},
	} {
		if _, err := x.Backend.SaveFeedbackLineProgress(x.Context, report.ID, attemptAt, step); err == nil {
			t.Fatalf("step %+v was stored", step)
		}
	}
	if stored := pendingReport(t, x, e, "crash"); !reflect.DeepEqual(stored.LineProgress, &want) {
		t.Fatalf("stored progress after refusals=%+v, want %+v", stored.LineProgress, want)
	}
	// The resolution is stored once and drops the interim answers; a later
	// reason or answer changes nothing.
	progress = save(store.FeedbackLineStep{Repository: "docs", Slug: "Acme/Docs", Commit: commit})
	want = store.FeedbackLineProgress{Repository: "docs", Slug: "acme/docs", Commit: commit}
	if !reflect.DeepEqual(progress, want) {
		t.Fatalf("progress=%+v, want %+v", progress, want)
	}
	for _, step := range []store.FeedbackLineStep{{Unresolved: store.FeedbackReasonNoMatch}, {Repository: "app", Slug: "acme/app", Commit: feedbackCommit("c1")},
		{Candidate: "acme/web", Answer: store.FeedbackLineAbsent, Input: input, Keep: []string{"acme/web"}},
		{Candidate: "acme/web", Answer: store.FeedbackLineAbsent, Input: feedbackLineInput("b"), Keep: []string{"acme/web"}}} {
		if again := save(step); !reflect.DeepEqual(again, want) {
			t.Fatalf("step %+v changed the ended progress: %+v", step, again)
		}
	}
	// Every read of the report returns it, and the claim answers it too.
	claimed, ok := claimLinking(t, x, report.ID, feedbackTime(time.Minute))
	if !ok || !reflect.DeepEqual(claimed.LineProgress, &want) {
		t.Fatalf("claim progress=%+v ok=%v", claimed.LineProgress, ok)
	}
	if listed := e.reportByKey(t, "crash"); !reflect.DeepEqual(listed.LineProgress, &want) {
		t.Fatalf("listed progress=%+v", listed.LineProgress)
	}
	// No served form holds it.
	raw, err := json.Marshal(claimed)
	requireOK(t, err)
	for _, member := range []string{"line_progress", "timing_result", "candidates", commit} {
		if strings.Contains(string(raw), member) {
			t.Fatalf("the report's JSON %s holds %q", raw, member)
		}
	}
	// A definitive reason on another report.
	unresolved, err := x.Backend.SaveFeedbackLineProgress(x.Context, other.ID, claimedAt(t, x, other.ID, feedbackTime(10*time.Second)), store.FeedbackLineStep{Unresolved: store.FeedbackReasonNoMatch})
	requireOK(t, err)
	if unresolved.Unresolved != store.FeedbackReasonNoMatch || len(unresolved.Candidates) != 0 {
		t.Fatalf("other progress=%+v", unresolved)
	}
	if stored := pendingReport(t, x, e, "crash"); !reflect.DeepEqual(stored.LineProgress, &want) {
		t.Fatalf("another report's write changed this one: %+v", stored.LineProgress)
	}
	if _, err := x.Backend.SaveFeedbackLineProgress(x.Context, "rpt-000000-000000", attemptAt, store.FeedbackLineStep{Unresolved: store.FeedbackReasonNoMatch}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown report err=%v, want not found", err)
	}
}

// runFeedbackLinkingLineProgressInput proves that stored candidate answers
// hold one resolution input and stay within the current candidates through
// every change between two attempts. An answer read with another input, as
// after a version rule change, drops every stored answer before it is
// stored, so answers for two inputs are never combined. An answer saved with
// fewer current candidates, as after a repository was removed or repointed,
// drops the answers of the slugs no longer listed, so the stored answers
// never outnumber the candidates. A definitive reason after that is final
// (approved plan version 3; component-feedback-reports invariant 12;
// component-production-feedback invariant 7).
func runFeedbackLinkingLineProgressInput(t *testing.T, x Fixture) {
	e, report := linkingReport(t, x, "crash", func(entry *store.FeedbackReportEntry) {
		entry.File, entry.Line, entry.Version = "cmd/main.go", 12, "1.2"
	})
	attemptAt := claimedAt(t, x, report.ID, feedbackTime(10*time.Second))
	save := func(step store.FeedbackLineStep) store.FeedbackLineProgress {
		t.Helper()
		progress, err := x.Backend.SaveFeedbackLineProgress(x.Context, report.ID, attemptAt, step)
		requireOK(t, err)
		if stored := pendingReport(t, x, e, "crash"); !reflect.DeepEqual(stored.LineProgress, &progress) {
			t.Fatalf("stored=%+v, answered %+v", stored.LineProgress, progress)
		}
		return progress
	}
	oldInput, newInput := feedbackLineInput("1"), feedbackLineInput("2")
	match := feedbackCommit("c1")
	steps := []struct {
		name string
		step store.FeedbackLineStep
		want store.FeedbackLineProgress
	}{
		{"an answer under the old rule", store.FeedbackLineStep{Candidate: "acme/app", Answer: match, Input: oldInput, Keep: []string{"acme/app", "acme/other"}},
			store.FeedbackLineProgress{Input: oldInput, Candidates: map[string]string{"acme/app": match}}},
		{"an answer under a changed rule drops the old match", store.FeedbackLineStep{Candidate: "acme/other", Answer: feedbackCommit("c2"), Input: newInput, Keep: []string{"acme/app", "acme/other"}},
			store.FeedbackLineProgress{Input: newInput, Candidates: map[string]string{"acme/other": feedbackCommit("c2")}}},
		{"a slug added", store.FeedbackLineStep{Candidate: "acme/extra", Answer: store.FeedbackLineAbsent, Input: newInput, Keep: []string{"acme/app", "acme/extra", "acme/other"}},
			store.FeedbackLineProgress{Input: newInput, Candidates: map[string]string{"acme/other": feedbackCommit("c2"), "acme/extra": store.FeedbackLineAbsent}}},
		{"a slug repointed", store.FeedbackLineStep{Candidate: "acme/app2", Answer: store.FeedbackLineAbsent, Input: newInput, Keep: []string{"acme/app2", "acme/extra"}},
			store.FeedbackLineProgress{Input: newInput, Candidates: map[string]string{"acme/extra": store.FeedbackLineAbsent, "acme/app2": store.FeedbackLineAbsent}}},
		{"a slug removed, with an answer already stored", store.FeedbackLineStep{Candidate: "acme/app2", Answer: match, Input: newInput, Keep: []string{"acme/app2"}},
			store.FeedbackLineProgress{Input: newInput, Candidates: map[string]string{"acme/app2": store.FeedbackLineAbsent}}},
		{"a step with no answer drops the answers of a removed slug", store.FeedbackLineStep{Input: newInput, Keep: []string{"acme/blocked"}},
			store.FeedbackLineProgress{Input: newInput}},
		{"an answer for the input after the drop", store.FeedbackLineStep{Candidate: "acme/app2", Answer: store.FeedbackLineAbsent, Input: newInput, Keep: []string{"acme/app2", "acme/blocked"}},
			store.FeedbackLineProgress{Input: newInput, Candidates: map[string]string{"acme/app2": store.FeedbackLineAbsent}}},
		{"a step with no answer under a changed rule drops every answer", store.FeedbackLineStep{Input: oldInput, Keep: []string{"acme/app2", "acme/blocked"}},
			store.FeedbackLineProgress{Input: oldInput}},
		{"back to the old rule reads again from nothing", store.FeedbackLineStep{Candidate: "acme/app2", Answer: match, Input: oldInput, Keep: []string{"acme/app2"}},
			store.FeedbackLineProgress{Input: oldInput, Candidates: map[string]string{"acme/app2": match}}},
		{"a definitive reason is final", store.FeedbackLineStep{Unresolved: store.FeedbackReasonAmbiguous},
			store.FeedbackLineProgress{Unresolved: store.FeedbackReasonAmbiguous}},
		{"an answer after the end changes nothing", store.FeedbackLineStep{Candidate: "acme/app", Answer: match, Input: newInput, Keep: []string{"acme/app"}},
			store.FeedbackLineProgress{Unresolved: store.FeedbackReasonAmbiguous}},
		{"a step with no answer after the end changes nothing", store.FeedbackLineStep{Input: newInput, Keep: []string{"acme/app"}},
			store.FeedbackLineProgress{Unresolved: store.FeedbackReasonAmbiguous}},
	}
	for _, s := range steps {
		got := save(s.step)
		if !reflect.DeepEqual(got, s.want) {
			t.Fatalf("%s: progress=%+v, want %+v", s.name, got, s.want)
		}
		if len(got.Candidates) > len(s.step.Keep) {
			t.Fatalf("%s: %d answers for %d candidates", s.name, len(got.Candidates), len(s.step.Keep))
		}
	}
}

// runFeedbackLinkingTimingResult proves that the timing rule's answer is
// stored once and reused, also when it holds no link and no note.
func runFeedbackLinkingTimingResult(t *testing.T, x Fixture) {
	e, report := linkingReport(t, x, "crash", func(entry *store.FeedbackReportEntry) { entry.Service = "api" })
	attemptAt := claimedAt(t, x, report.ID, feedbackTime(10*time.Second))
	empty, err := x.Backend.SaveFeedbackTimingResult(x.Context, report.ID, attemptAt, store.FeedbackTimingResult{})
	requireOK(t, err)
	want := &store.FeedbackTimingResult{Links: []store.FeedbackTimingLink{}, Notes: []store.FeedbackTimingNote{}}
	if !reflect.DeepEqual(empty, want) {
		t.Fatalf("saved=%+v, want an empty result", empty)
	}
	prior := feedbackTime(-time.Hour)
	again, err := x.Backend.SaveFeedbackTimingResult(x.Context, report.ID, attemptAt, store.FeedbackTimingResult{
		Links: []store.FeedbackTimingLink{{TaskID: "task-1", DeployID: "dep-1", PriorSignalAt: &prior}}, Notes: []store.FeedbackTimingNote{{Kind: store.FeedbackNoteNoTaskDeploy, DeployID: "dep-2"}}})
	requireOK(t, err)
	if !reflect.DeepEqual(again, want) {
		t.Fatalf("a second result replaced the first: %+v", again)
	}
	if stored := pendingReport(t, x, e, "crash"); !reflect.DeepEqual(stored.TimingResult, want) || stored.LineProgress != nil {
		t.Fatalf("stored=%+v line=%+v", stored.TimingResult, stored.LineProgress)
	}
	// A result with links and notes on another report keeps every member.
	_, other := linkingReportIn(t, x, "other")
	full := store.FeedbackTimingResult{Links: []store.FeedbackTimingLink{{TaskID: "task-1", DeployID: "dep-1", ElapsedMinutes: 3}, {TaskID: "task-1", DeployID: "dep-3", PriorSignalAt: &prior}},
		Notes: []store.FeedbackTimingNote{{Kind: store.FeedbackNoteTooManyTasks, DeployID: "dep-2", TaskCount: 11}}}
	saved, err := x.Backend.SaveFeedbackTimingResult(x.Context, other.ID, claimedAt(t, x, other.ID, feedbackTime(10*time.Second)), full)
	requireOK(t, err)
	if !reflect.DeepEqual(saved, &full) {
		t.Fatalf("saved=%+v, want %+v", saved, full)
	}
	if _, err := x.Backend.SaveFeedbackTimingResult(x.Context, report.ID, attemptAt, store.FeedbackTimingResult{Notes: []store.FeedbackTimingNote{{Kind: store.FeedbackNoteNotChecked, DeployID: "dep"}}}); err == nil {
		t.Fatal("a not_checked timing note was accepted")
	}
	if _, err := x.Backend.SaveFeedbackTimingResult(x.Context, "rpt-000000-000000", attemptAt, store.FeedbackTimingResult{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown report err=%v, want not found", err)
	}
}

// runFeedbackLinkingProgressAfterEnd proves that progress and a line answer
// are not written after linking ended.
func runFeedbackLinkingProgressAfterEnd(t *testing.T, x Fixture) {
	e, report := linkingReport(t, x, "crash", nil)
	attemptAt := claimedAt(t, x, report.ID, feedbackTime(10*time.Second))
	finished, err := x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: report.ID, EndedAt: feedbackTime(time.Minute), AttemptAt: attemptAt})
	requireOK(t, err)
	if !finished {
		t.Fatal("the end of linking wrote nothing")
	}
	progress, err := x.Backend.SaveFeedbackLineProgress(x.Context, report.ID, attemptAt, store.FeedbackLineStep{Unresolved: store.FeedbackReasonNoMatch})
	requireOK(t, err)
	timing, err := x.Backend.SaveFeedbackTimingResult(x.Context, report.ID, attemptAt, store.FeedbackTimingResult{})
	requireOK(t, err)
	stored := e.reportByKey(t, "crash")
	if progress.Ended() || timing != nil || stored.LineProgress != nil || stored.TimingResult != nil {
		t.Fatalf("progress after the end: %+v %+v stored %+v %+v", progress, timing, stored.LineProgress, stored.TimingResult)
	}
	// A line answer that arrives after the end is refused and not stored.
	late := store.FeedbackReportLineRead{ReportID: report.ID, Repository: "app", ResolvedCommit: feedbackCommit("c0"), File: "cmd/main.go", Line: 12,
		Answer: store.FeedbackReportLineAnswer{FileFound: true, Commit: feedbackCommit("c1"), PullRequests: []int{}}, AnsweredAt: feedbackTime(2 * time.Minute)}
	if err := x.Backend.RecordFeedbackReportLineRead(x.Context, late); !errors.Is(err, store.ErrFeedbackLinkingSuperseded) {
		t.Fatalf("a line answer after the end: err=%v, want superseded", err)
	}
	if _, found, err := x.Backend.ReadFeedbackReportLineRead(x.Context, report.ID); err != nil || found {
		t.Fatalf("a line answer after the end was stored: found=%v err=%v", found, err)
	}
}

// runFeedbackLinkingWritesNeedTheClaim proves that only the attempt that
// holds a report's current claim writes its stored progress or ends its
// linking. An attempt that never claimed, and an older attempt after a later
// one claimed the report, as two server processes can overlap, write nothing
// and get ErrFeedbackLinkingSuperseded. The claim time is compared as the
// claim stored it, so an attempt time with nanoseconds still holds its own
// claim (component-feedback-reports invariant 15; approved plan version 3).
func runFeedbackLinkingWritesNeedTheClaim(t *testing.T, x Fixture) {
	e, report := linkingReport(t, x, "crash", func(entry *store.FeedbackReportEntry) {
		entry.Service, entry.File, entry.Line, entry.Version = "api", "cmd/main.go", 12, "1.2"
	})
	step := store.FeedbackLineStep{Candidate: "acme/app", Answer: store.FeedbackLineAbsent, Input: feedbackLineInput("1"), Keep: []string{"acme/app"}}
	superseded := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, store.ErrFeedbackLinkingSuperseded) {
			t.Fatalf("%s: err=%v, want superseded", name, err)
		}
	}
	unchanged := func(name string) {
		t.Helper()
		stored := pendingReport(t, x, e, "crash")
		if stored.LineProgress != nil || stored.TimingResult != nil || stored.LinkingEndedAt != nil {
			t.Fatalf("%s: stored progress %+v %+v ended %v, want nothing written", name, stored.LineProgress, stored.TimingResult, stored.LinkingEndedAt)
		}
	}
	never := feedbackTime(5 * time.Second)
	_, err := x.Backend.SaveFeedbackLineProgress(x.Context, report.ID, never, step)
	superseded("no claim, line progress", err)
	_, err = x.Backend.SaveFeedbackTimingResult(x.Context, report.ID, never, store.FeedbackTimingResult{})
	superseded("no claim, timing result", err)
	_, err = x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: report.ID, EndedAt: never, AttemptAt: never})
	superseded("no claim, finish", err)
	unchanged("no claim")

	older := claimedAt(t, x, report.ID, feedbackTime(10*time.Second).Add(123*time.Nanosecond))
	if _, err := x.Backend.SaveFeedbackLineProgress(x.Context, report.ID, older, step); err != nil {
		t.Fatalf("the attempt that holds the claim: %v", err)
	}
	newer := claimedAt(t, x, report.ID, older.Add(store.FeedbackReportLinkingRetry))
	changed := store.FeedbackLineStep{Candidate: "acme/app", Answer: feedbackCommit("c1"), Input: feedbackLineInput("2"), Keep: []string{"acme/app"}}
	_, err = x.Backend.SaveFeedbackLineProgress(x.Context, report.ID, older, changed)
	superseded("older attempt, line progress", err)
	_, err = x.Backend.SaveFeedbackLineProgress(x.Context, report.ID, older, store.FeedbackLineStep{Unresolved: store.FeedbackReasonNoMatch})
	superseded("older attempt, unresolved", err)
	_, err = x.Backend.SaveFeedbackTimingResult(x.Context, report.ID, older, store.FeedbackTimingResult{})
	superseded("older attempt, timing result", err)
	_, err = x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: report.ID, EndedAt: newer, AttemptAt: older})
	superseded("older attempt, finish", err)
	want := store.FeedbackLineProgress{Input: feedbackLineInput("1"), Candidates: map[string]string{"acme/app": store.FeedbackLineAbsent}}
	if stored := pendingReport(t, x, e, "crash"); !reflect.DeepEqual(stored.LineProgress, &want) || stored.TimingResult != nil {
		t.Fatalf("after the older attempt: %+v %+v, want only the first answer", stored.LineProgress, stored.TimingResult)
	}
	// The newer attempt writes, and ends linking.
	progress, err := x.Backend.SaveFeedbackLineProgress(x.Context, report.ID, newer, changed)
	requireOK(t, err)
	if progress.Input != feedbackLineInput("2") || progress.Candidates["acme/app"] != feedbackCommit("c1") {
		t.Fatalf("newer attempt progress=%+v", progress)
	}
	finished, err := x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: report.ID, EndedAt: newer.Add(time.Second), AttemptAt: newer})
	requireOK(t, err)
	if !finished {
		t.Fatal("the attempt that holds the claim did not end linking")
	}
}
