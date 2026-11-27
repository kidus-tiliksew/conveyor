package reports

import (
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
	shaD = "dddddddddddddddddddddddddddddddddddddddd"
	shaE = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	shaF = "ffffffffffffffffffffffffffffffffffffffff"
	shaG = "0123456789abcdef0123456789abcdef01234567"
)

var base = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func lineReport() store.FeedbackReport {
	return store.FeedbackReport{ID: "rpt-261127-000001", EnvironmentID: "env-1", File: "cmd/main.go", Line: 12,
		First: store.FeedbackReportVersion{Commit: "abc1234"}, FirstEntrySeen: base, CreatedAt: base}
}

func lineRead(answer store.FeedbackReportLineAnswer) *store.FeedbackReportLineRead {
	return &store.FeedbackReportLineRead{ReportID: "rpt-261127-000001", Repository: "app", ResolvedCommit: shaD, File: "cmd/main.go", Line: 12, Answer: answer, AnsweredAt: base}
}

// TestLineApplies covers when the line rule applies: a file, a line, and a
// commit or a version (component-feedback-reports invariant 12).
func TestLineApplies(t *testing.T) {
	report := lineReport()
	if !LineApplies(report) {
		t.Fatal("a file, a line, and a commit must apply")
	}
	report.First = store.FeedbackReportVersion{Version: "1.2.3"}
	if !LineApplies(report) {
		t.Fatal("a file, a line, and a version must apply")
	}
	report.First = store.FeedbackReportVersion{Repository: "app"}
	if LineApplies(report) {
		t.Fatal("a repository alone must not apply")
	}
	report = lineReport()
	report.File, report.Line = "", 0
	if LineApplies(report) {
		t.Fatal("no file must not apply")
	}
	if outcome := Line(LineInput{Report: report}); !outcome.Ended || len(outcome.Links) != 0 || len(outcome.Notes) != 0 {
		t.Fatalf("a rule that does not apply: %+v, want ended with nothing", outcome)
	}
}

// TestLineMatches covers the matches of invariant 12: the merge commit, a
// merged pull request number of the latest merge event, the repository
// scope, and the evidence (req-production-feedback AC-5.1).
func TestLineMatches(t *testing.T) {
	merged := []store.FeedbackMergedTask{
		{TaskID: "t-commit", Repository: "app", GitHubRepository: "acme/app", MergeCommit: shaA},
		{TaskID: "t-pull", Repository: "app", GitHubRepository: "acme/app", MergeCommit: shaB, PullRequest: 42},
		{TaskID: "t-pull-no-commit", Repository: "app", GitHubRepository: "acme/app", PullRequest: 43},
		{TaskID: "t-other-repo", Repository: "web", GitHubRepository: "acme/web", MergeCommit: shaA, PullRequest: 42},
		{TaskID: "t-unrelated", Repository: "app", GitHubRepository: "acme/app", MergeCommit: shaC, PullRequest: 9},
		{TaskID: "t-no-pull", Repository: "app", GitHubRepository: "acme/app", MergeCommit: shaC},
	}
	outcome := Line(LineInput{Report: lineReport(), Read: lineRead(store.FeedbackReportLineAnswer{FileFound: true, Commit: shaA, PullRequests: []int{42, 43}}), Slug: "Acme/App", Merged: merged})
	if !outcome.Ended || len(outcome.Notes) != 0 {
		t.Fatalf("outcome %+v, want ended with no note", outcome)
	}
	got := []string{}
	for _, link := range outcome.Links {
		got = append(got, link.TaskID)
		if len(link.Evidence) != 1 {
			t.Fatalf("link %+v, want one piece of evidence", link)
		}
		evidence := link.Evidence[0]
		if evidence.Kind != store.FeedbackEvidenceLine || evidence.File != "cmd/main.go" || evidence.Line != 12 || evidence.Commit != shaA || evidence.Deploy != nil || evidence.ElapsedMinutes != nil {
			t.Fatalf("evidence %+v, want the file, the line, and the changing commit", evidence)
		}
	}
	want := []string{"t-commit", "t-pull", "t-pull-no-commit"}
	if len(got) != len(want) {
		t.Fatalf("links %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("links %v, want %v", got, want)
		}
	}
	// No task matches: the rule ends with no link and no note.
	none := Line(LineInput{Report: lineReport(), Read: lineRead(store.FeedbackReportLineAnswer{FileFound: true, Commit: shaD, PullRequests: []int{}}), Slug: "acme/app", Merged: merged})
	if !none.Ended || len(none.Links) != 0 || len(none.Notes) != 0 {
		t.Fatalf("no match: %+v, want ended with nothing", none)
	}
}

// TestLineComparesGitHubRepositories is the line rule's part of review
// round 4: both matches ask whether a task belongs to the repository the line
// read was sent to, and answer with the GitHub repository of the task's
// latest merge event, never the configured name. The name app was pointed
// from acme/old to acme/new. The read in acme/new lists pull request 42 and
// the changing commit shaA, which a fork also holds (component-feedback-
// reports invariant 12; req-production-feedback AC-5.1).
func TestLineComparesGitHubRepositories(t *testing.T) {
	merged := []store.FeedbackMergedTask{
		// Merged in acme/old under the same name, with the colliding pull
		// request number, and with the shared commit.
		{TaskID: "t-old-pull", Repository: "app", GitHubRepository: "acme/old", MergeCommit: shaB, PullRequest: 42},
		{TaskID: "t-old-commit", Repository: "app", GitHubRepository: "acme/old", MergeCommit: shaA},
		// The same commit merged in a fork.
		{TaskID: "t-fork-commit", Repository: "fork", GitHubRepository: "someone/new", MergeCommit: shaA, PullRequest: 42},
		// A merge event that records no repository proves none.
		{TaskID: "t-unknown", Repository: "app", MergeCommit: shaA, PullRequest: 42},
		// Merged in acme/new: by pull request, by commit, and under another
		// configured name that points at acme/new.
		{TaskID: "t-new-pull", Repository: "app", GitHubRepository: "acme/new", MergeCommit: shaC, PullRequest: 42},
		{TaskID: "t-new-commit", Repository: "app", GitHubRepository: "acme/new", MergeCommit: shaA},
		{TaskID: "t-new-other-name", Repository: "new", GitHubRepository: "acme/new", PullRequest: 42},
	}
	read := lineRead(store.FeedbackReportLineAnswer{FileFound: true, Commit: shaA, PullRequests: []int{42}})
	for _, tc := range []struct {
		name string
		slug string
		want []string
	}{
		{"read in acme/new", "acme/new", []string{"t-new-commit", "t-new-other-name", "t-new-pull"}},
		{"read in acme/old", "Acme/Old", []string{"t-old-commit", "t-old-pull"}},
		{"no stored slug", "", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome := Line(LineInput{Report: lineReport(), Read: read, Slug: tc.slug, Merged: merged})
			if got := linkedTasks(outcome); !outcome.Ended || !slices.Equal(got, tc.want) {
				t.Fatalf("links %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLineNotes covers file_not_found, commit_unresolved for each definitive
// reason, and the wait for a pending resolution, a missing App, and each
// forge failure (component-feedback-reports invariant 12; State and failure).
func TestLineNotes(t *testing.T) {
	missing := Line(LineInput{Report: lineReport(), Read: lineRead(store.FeedbackReportLineAnswer{FileFound: false})})
	if !missing.Ended || len(missing.Links) != 0 || len(missing.Notes) != 1 || missing.Notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteFileNotFound, File: "cmd/main.go", Commit: shaD}) {
		t.Fatalf("missing file: %+v", missing)
	}
	for _, reason := range []string{store.FeedbackReasonNoMatch, store.FeedbackReasonAmbiguous, store.FeedbackReasonNoVersionRule, store.FeedbackReasonRuleMismatch, store.FeedbackReasonNoRepository} {
		outcome := Line(LineInput{Report: lineReport(), Unresolved: reason})
		if !outcome.Ended || len(outcome.Notes) != 1 || outcome.Notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteCommitUnresolved, Reason: reason}) {
			t.Fatalf("reason %s: %+v, want commit_unresolved with the reason", reason, outcome)
		}
	}
	for _, reason := range []string{store.FeedbackReasonPending, store.FeedbackReasonNoGitHubApp, "forge_request", "forge_status", "forge_response", "forge_rate_limited", "forge_permission"} {
		outcome := Line(LineInput{Report: lineReport(), Unresolved: reason})
		if outcome.Ended || outcome.Waiting != reason || len(outcome.Notes) != 0 {
			t.Fatalf("reason %s: %+v, want waiting with the reason", reason, outcome)
		}
	}
}

// TestDecide covers the end of linking: both rules ended, a rule that waits
// before and at 24 hours compared exactly, and the merge of the two rules'
// links by task (component-feedback-reports invariants 11 and 15).
func TestDecide(t *testing.T) {
	report := lineReport()
	lineLink := store.FeedbackReportLinkCandidate{TaskID: "t-1", Evidence: []store.FeedbackReportEvidence{{Kind: store.FeedbackEvidenceLine, File: "cmd/main.go", Line: 12, Commit: shaA}}}
	elapsed := int64(3)
	timingLink := store.FeedbackReportLinkCandidate{TaskID: "t-1", Evidence: []store.FeedbackReportEvidence{{Kind: store.FeedbackEvidenceTiming, ElapsedMinutes: &elapsed}}}
	line := Outcome{Ended: true, Links: []store.FeedbackReportLinkCandidate{lineLink}}
	timing := Outcome{Ended: true, Links: []store.FeedbackReportLinkCandidate{timingLink, {TaskID: "t-0"}}, Notes: []store.FeedbackReportNote{{Kind: store.FeedbackNoteNoTaskDeploy}}}
	linking, ended := Decide(report, line, timing, base.Add(time.Minute))
	if !ended || linking.ReportID != report.ID || !linking.EndedAt.Equal(base.Add(time.Minute)) || len(linking.Links) != 2 || linking.Links[0].TaskID != "t-0" || linking.Links[1].TaskID != "t-1" || len(linking.Links[1].Evidence) != 2 || len(linking.Notes) != 1 {
		t.Fatalf("linking %+v ended=%v", linking, ended)
	}
	waiting := Outcome{Waiting: store.FeedbackReasonNoGitHubApp}
	if _, ended := Decide(report, waiting, timing, base.Add(store.FeedbackReportLinkingDeadline-time.Nanosecond)); ended {
		t.Fatal("a waiting rule one nanosecond before 24 hours must not end")
	}
	linking, ended = Decide(report, waiting, Outcome{Waiting: WaitingForSignal}, base.Add(store.FeedbackReportLinkingDeadline))
	if !ended || len(linking.Links) != 0 || len(linking.Notes) != 2 ||
		linking.Notes[0] != (store.FeedbackReportNote{Kind: store.FeedbackNoteNotChecked, Rule: store.FeedbackRuleLine, Reason: store.FeedbackReasonNoGitHubApp}) ||
		linking.Notes[1] != (store.FeedbackReportNote{Kind: store.FeedbackNoteNotChecked, Rule: store.FeedbackRuleTiming, Reason: WaitingForSignal}) {
		t.Fatalf("at 24 hours: %+v ended=%v, want not_checked for both rules with their reasons", linking, ended)
	}
	// A rule that ended keeps its answer when the other ends as not_checked.
	linking, ended = Decide(report, line, Outcome{}, base.Add(25*time.Hour))
	if !ended || len(linking.Links) != 1 || len(linking.Notes) != 1 || linking.Notes[0].Reason != NotCheckedReason || linking.Notes[0].Rule != store.FeedbackRuleTiming {
		t.Fatalf("one rule ended: %+v", linking)
	}
}
