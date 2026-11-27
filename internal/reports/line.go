// Package reports holds the two linking rules of feedback reports as
// functions of stored records, with no I/O of their own
// (component-feedback-reports invariants 11 to 14;
// feature-production-feedback-listening FR-2). Each rule answers the tasks it
// links with their evidence, its notes, or what it still waits for. The
// background linking work reads the records, runs the rules, and writes
// their answers when both have ended.
package reports

import (
	"slices"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Outcome is the answer of one rule. Ended is true when the rule has its
// answer, and then Links and Notes are that answer. While Ended is false,
// Waiting names what the rule still waits for, as the reason of a later
// not_checked note, and Missing lists the stored comparisons it needs.
type Outcome struct {
	Ended   bool
	Links   []store.FeedbackReportLinkCandidate
	Notes   []store.FeedbackReportNote
	Waiting string
	Missing []store.FeedbackComparisonKey
}

// The reasons a rule waits that are not a forge failure category or an
// unresolved reason of component-production-feedback invariant 7.
const (
	// WaitingForSignal: no watcher signal that names the service arrived
	// after the report was first stored, and a watcher is listening.
	WaitingForSignal = "no_signal_after_report"
	// WaitingForComparisons: a stored comparison that the timing rule needs
	// is missing.
	WaitingForComparisons = "comparison_missing"
	// WaitingForResolution: a deploy's version is not resolved yet.
	WaitingForResolution = "version_pending"
)

// LineApplies reports whether the line rule applies to a report: its first
// entry states a file, a line, and a commit or a version
// (component-feedback-reports invariant 12).
func LineApplies(report store.FeedbackReport) bool {
	return report.File != "" && report.Line > 0 && (report.First.Commit != "" || report.First.Version != "")
}

// DefinitiveUnresolved reports whether an unresolved reason of
// component-production-feedback invariant 7 is final for the line rule. A
// pending resolution, a workspace without a usable GitHub App, and a forge
// failure prove nothing, so the rule waits for them
// (component-feedback-reports invariant 12; State and failure).
func DefinitiveUnresolved(reason string) bool {
	return slices.Contains(store.FeedbackDefinitiveUnresolved, reason)
}

// LineInput is what the line rule reads for one report.
type LineInput struct {
	Report store.FeedbackReport
	// Read is the stored line read of the report, or nil when none is
	// stored.
	Read *store.FeedbackReportLineRead
	// Unresolved is the reason the first entry's commit or version did not
	// resolve, when no read is stored, and empty when it resolved or was not
	// tried.
	Unresolved string
	// Slug is the canonical GitHub slug the line read was sent to: the slug
	// stored with the resolution. Empty proves no repository, and the rule
	// then links nothing.
	Slug string
	// Merged lists the merged tasks of the workspace. The rule keeps those
	// whose latest merge event records the read's GitHub repository.
	Merged []store.FeedbackMergedTask
}

// Line runs the line rule (component-feedback-reports invariant 12;
// req-production-feedback AC-5.1).
//
//   - A report the rule does not apply to ends with no link and no note.
//   - A stored read whose file GitHub did not find ends with the note
//     file_not_found.
//   - A stored read links, as possible, each merged task whose latest merge
//     event records the GitHub repository the read was sent to, and whose
//     merge commit from that event is the changing commit or whose pull
//     request number from that event is one of the merged pull requests
//     GitHub lists for that commit, with the file, the line, and the
//     changing commit as evidence. The repository is compared by GitHub
//     slug, never by workspace name, because a name can be pointed at
//     another repository and pull request numbers and commits repeat across
//     repositories and forks. A merge event that records no repository, and
//     a read with no stored slug, prove no repository and link nothing.
//   - Without a read, a definitive unresolved reason ends the rule with the
//     note commit_unresolved and the reason. Any other reason leaves the
//     rule waiting for it.
func Line(input LineInput) Outcome {
	report := input.Report
	if !LineApplies(report) {
		return Outcome{Ended: true}
	}
	if input.Read == nil {
		if DefinitiveUnresolved(input.Unresolved) {
			return Outcome{Ended: true, Notes: []store.FeedbackReportNote{{Kind: store.FeedbackNoteCommitUnresolved, Reason: input.Unresolved}}}
		}
		return Outcome{Waiting: input.Unresolved}
	}
	read := *input.Read
	if !read.Answer.FileFound {
		return Outcome{Ended: true, Notes: []store.FeedbackReportNote{{Kind: store.FeedbackNoteFileNotFound, File: read.File, Commit: read.ResolvedCommit}}}
	}
	evidence := store.FeedbackReportEvidence{Kind: store.FeedbackEvidenceLine, File: read.File, Line: read.Line, Commit: read.Answer.Commit}
	outcome := Outcome{Ended: true}
	seen := map[string]bool{}
	slug := store.FeedbackRepositorySlug(input.Slug)
	for _, task := range input.Merged {
		if slug == "" || task.GitHubRepository != slug || seen[task.TaskID] {
			continue
		}
		byCommit := task.MergeCommit != "" && task.MergeCommit == read.Answer.Commit
		byPull := task.PullRequest > 0 && slices.Contains(read.Answer.PullRequests, task.PullRequest)
		if !byCommit && !byPull {
			continue
		}
		seen[task.TaskID] = true
		outcome.Links = append(outcome.Links, store.FeedbackReportLinkCandidate{TaskID: task.TaskID, Evidence: []store.FeedbackReportEvidence{evidence}})
	}
	slices.SortFunc(outcome.Links, func(a, b store.FeedbackReportLinkCandidate) int { return compareStrings(a.TaskID, b.TaskID) })
	return outcome
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
