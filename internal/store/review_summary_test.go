package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestValidateReviewSummary pins SU-2's validation boundaries directly,
// including four-byte characters, which count once (feature-review-round-summary
// SU-2; req-review-gates-evidence AC-10.2).
func TestValidateReviewSummary(t *testing.T) {
	settled := []core.Event{{Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{
		"review_round": 2, "verdict": "changes_requested",
		"reviews": []map[string]any{{"review_seat": 1, "verdict": "changes_requested"}, {"review_seat": 2, "verdict": "approve"}, {"review_seat": 3, "verdict": "changes_requested"}},
	})}}
	valid := func() ReviewSummaryRequest {
		return ReviewSummaryRequest{TaskID: "task", ReviewRound: 2, RequestID: "request", Summary: "Summary.", Findings: []ReviewSummaryFinding{
			{Severity: ReviewSummarySeverityBlocking, Problem: "Problem.", Seats: []int{1, 3}},
		}}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ReviewSummaryRequest)
		events []core.Event
		want   error
		text   string
	}{
		{name: "valid", mutate: func(*ReviewSummaryRequest) {}},
		{name: "four-byte summary at the bound", mutate: func(r *ReviewSummaryRequest) { r.Summary = strings.Repeat("😀", ReviewSummaryMaxSummary) }},
		{name: "four-byte summary over the bound", mutate: func(r *ReviewSummaryRequest) { r.Summary = strings.Repeat("😀", ReviewSummaryMaxSummary+1) }, want: ErrReviewSummaryInvalid, text: "summary must be 1 to 4000 characters"},
		{name: "four-byte problem at the bound", mutate: func(r *ReviewSummaryRequest) { r.Findings[0].Problem = strings.Repeat("😀", ReviewSummaryMaxProblem) }},
		{name: "four-byte location over the bound", mutate: func(r *ReviewSummaryRequest) {
			r.Findings[0].Location = strings.Repeat("😀", ReviewSummaryMaxLocation+1)
		}, want: ErrReviewSummaryInvalid, text: "location"},
		{name: "four-byte model at the bound", mutate: func(r *ReviewSummaryRequest) { r.Author.Model = strings.Repeat("😀", ReviewSummaryMaxModel) }},
		{name: "unsettled history", events: []core.Event{}, mutate: func(*ReviewSummaryRequest) {}, want: ErrReviewSummaryRoundNotSettled},
		{name: "other round settled", mutate: func(r *ReviewSummaryRequest) { r.ReviewRound = 1 }, want: ErrReviewSummaryRoundNotSettled},
		{name: "round zero", mutate: func(r *ReviewSummaryRequest) { r.ReviewRound = 0 }, want: ErrReviewSummaryRoundNotSettled},
		{name: "uncited seats named in order", mutate: func(r *ReviewSummaryRequest) { r.Findings = nil }, want: ErrReviewSummaryInvalid, text: "uncited seats: 1, 3"},
		{name: "unknown seat", mutate: func(r *ReviewSummaryRequest) { r.Findings[0].Seats = []int{1, 3, 4} }, want: ErrReviewSummaryInvalid, text: "seat 4"},
		{name: "duplicate seat", mutate: func(r *ReviewSummaryRequest) { r.Findings[0].Seats = []int{1, 3, 1} }, want: ErrReviewSummaryInvalid, text: "repeats seat 1"},
		{name: "unknown severity", mutate: func(r *ReviewSummaryRequest) { r.Findings[0].Severity = "BLOCKING" }, want: ErrReviewSummaryInvalid, text: "severity"},
		{name: "blank request id", mutate: func(r *ReviewSummaryRequest) { r.RequestID = "" }, want: ErrReviewSummaryInvalid, text: "request_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := valid()
			tc.mutate(&request)
			events := settled
			if tc.events != nil {
				events = tc.events
			}
			err := ValidateReviewSummary(request, events)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("err=%v, want %v containing %q", err, tc.want, tc.text)
			}
		})
	}
}
