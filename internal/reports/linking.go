package reports

import (
	"slices"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// NotCheckedReason is the reason of a not_checked note for a rule that waited
// with no reason it could name.
const NotCheckedReason = "no_answer"

// Decide combines the answers of the two rules at now
// (component-feedback-reports invariants 11 and 15). It answers the end of
// linking and true when both rules have ended. When
// store.FeedbackReportLinkingDeadline has passed since the report was first
// stored, compared exactly with now, each rule that has not ended ends with
// the note not_checked, naming the rule and the reason it waited. Otherwise
// it answers false, and linking goes on. The links of the two rules are
// merged by task, line evidence first, and the notes keep the order line
// then timing.
func Decide(report store.FeedbackReport, line, timing Outcome, now time.Time) (store.FeedbackReportLinking, bool) {
	expired := !now.Before(report.CreatedAt.Add(store.FeedbackReportLinkingDeadline))
	rules := []struct {
		name    string
		outcome Outcome
	}{{store.FeedbackRuleLine, line}, {store.FeedbackRuleTiming, timing}}
	linking := store.FeedbackReportLinking{ReportID: report.ID, EndedAt: now}
	evidence := map[string][]store.FeedbackReportEvidence{}
	order := []string{}
	for _, rule := range rules {
		if !rule.outcome.Ended {
			if !expired {
				return store.FeedbackReportLinking{}, false
			}
			reason := rule.outcome.Waiting
			if reason == "" {
				reason = NotCheckedReason
			}
			linking.Notes = append(linking.Notes, store.FeedbackReportNote{Kind: store.FeedbackNoteNotChecked, Rule: rule.name, Reason: reason})
			continue
		}
		for _, link := range rule.outcome.Links {
			if _, seen := evidence[link.TaskID]; !seen {
				order = append(order, link.TaskID)
			}
			evidence[link.TaskID] = append(evidence[link.TaskID], link.Evidence...)
		}
		linking.Notes = append(linking.Notes, rule.outcome.Notes...)
	}
	slices.Sort(order)
	for _, taskID := range order {
		linking.Links = append(linking.Links, store.FeedbackReportLinkCandidate{TaskID: taskID, Evidence: evidence[taskID]})
	}
	return linking, true
}
