package store

import (
	"encoding/json"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Delta review rounds (feature-delta-review-rounds DR-2 to DR-4;
// req-review-gates-evidence REQ-12, AC-12.2 to AC-12.6). A submission fixes
// the review scope and review baseline of the head it submits and records them
// with the submission's comparison, the pull_request.opened event. Every
// command that opens a verify order or a review round for that head reads the
// pair back through ReviewRoundDispatchComparison.

// Review scopes a submission comparison records. They are the values of the
// frozen later_round_review policy and of a delta order's review_scope.
const (
	ReviewScopeFull  = "full"
	ReviewScopeDelta = "delta"
)

// JudgedHead returns a task's judged head: the reviewed commit of the latest
// review.round_completed event whose round every opened seat answered on one
// commit (AC-12.2; feature-delta-review-rounds DR-2). A round is skipped when
//
//   - its cancelled_seats list is not empty (a fail-fast round that retired a
//     seat),
//   - it settled as review_head_mismatch, or its reviews name more than one
//     commit or a blank commit,
//   - a seat order of the round has no verdict in the event, or
//   - a policy handoff superseded one of its seat orders.
//
// A malformed event is skipped and never answers. The function reads events
// and seat orders only, so every backend reaches the same answer from its own
// reads. ok is false when no round qualifies.
func JudgedHead(events []core.Event, orders []core.WorkOrder) (head string, ok bool) {
	superseded := SupersededReviewWorkOrders(events)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "review.round_completed" {
			continue
		}
		var round struct {
			ReviewRound    int    `json:"review_round"`
			ReasonCode     string `json:"reason_code"`
			CancelledSeats []int  `json:"cancelled_seats"`
			Reviews        []struct {
				ReviewWorkOrderID string `json:"review_work_order_id"`
				ReviewSeat        int    `json:"review_seat"`
				ReviewedCommitSHA string `json:"reviewed_commit_sha"`
			} `json:"reviews"`
		}
		if json.Unmarshal(events[i].Payload, &round) != nil || len(round.CancelledSeats) != 0 || round.ReasonCode == "review_head_mismatch" || len(round.Reviews) == 0 {
			continue
		}
		commit := strings.TrimSpace(round.Reviews[0].ReviewedCommitSHA)
		answered := map[int]bool{}
		qualifies := commit != ""
		for _, review := range round.Reviews {
			if strings.TrimSpace(review.ReviewedCommitSHA) != commit || superseded[review.ReviewWorkOrderID] {
				qualifies = false
				break
			}
			answered[review.ReviewSeat] = true
		}
		if !qualifies {
			continue
		}
		// Every opened seat of the round must have answered; a round 0
		// legacy review has no seat orders to count.
		for _, order := range orders {
			if order.Stage != core.StageReview || order.ReviewRound != round.ReviewRound || round.ReviewRound == 0 {
				continue
			}
			if superseded[order.ID] || !answered[order.ReviewSeat] {
				qualifies = false
				break
			}
		}
		if qualifies {
			return commit, true
		}
	}
	return "", false
}

// SubmissionComparison is one recorded submission comparison. BaseSHA is the
// pull request's observed base commit and HeadSHA the submitted head; the
// changed paths, documentation gate, governance attachment, path minimums,
// and head risk order always read this pair (DR-5). ReviewScope and
// ReviewBaselineSHA are the review scope and baseline the submission fixed for
// the head (DR-3). A record written before the scope existed reads as full
// from its base commit, so no stored record needs a rewrite. Fixed reports
// whether the record names its scope itself.
type SubmissionComparison struct {
	BaseSHA           string
	HeadSHA           string
	ReviewScope       string
	ReviewBaselineSHA string
	Fixed             bool
}

// Delta reports a delta comparison from the judged head.
func (c SubmissionComparison) Delta() bool {
	return c.ReviewScope == ReviewScopeDelta
}

// decodeSubmissionComparison reads one pull_request.opened payload. A record
// without both commits answers nothing. An explicit full or delta record with
// a non-blank review_baseline_sha compares from that baseline, so a full head
// retried after the pull request base moved keeps the base it was first fixed
// at, while BaseSHA stays the base this record observed for the whole-change
// readers (DR-3, DR-5). A record without review data, a delta record without a
// baseline, and any other scope read as full from the record's base commit.
func decodeSubmissionComparison(event core.Event) (SubmissionComparison, bool) {
	if event.Kind != SubmissionComparisonEventKind {
		return SubmissionComparison{}, false
	}
	var target struct {
		Base     string  `json:"base_sha"`
		Head     string  `json:"head_sha"`
		Scope    *string `json:"review_scope"`
		Baseline string  `json:"review_baseline_sha"`
	}
	if json.Unmarshal(event.Payload, &target) != nil || target.Base == "" || target.Head == "" {
		return SubmissionComparison{}, false
	}
	comparison := SubmissionComparison{BaseSHA: target.Base, HeadSHA: target.Head, ReviewScope: ReviewScopeFull, ReviewBaselineSHA: target.Base}
	if target.Scope != nil {
		comparison.Fixed = true
		baseline := strings.TrimSpace(target.Baseline)
		switch {
		case *target.Scope == ReviewScopeDelta && baseline != "":
			comparison.ReviewScope, comparison.ReviewBaselineSHA = ReviewScopeDelta, baseline
		case *target.Scope == ReviewScopeFull && baseline != "":
			comparison.ReviewBaselineSHA = baseline
		}
	}
	return comparison, true
}

// LatestSubmissionComparison returns the newest recorded submission
// comparison that names both commits, with the review scope it fixed.
func LatestSubmissionComparison(events []core.Event) (SubmissionComparison, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if comparison, ok := decodeSubmissionComparison(events[i]); ok {
			return comparison, true
		}
	}
	return SubmissionComparison{}, false
}

// FixedHeadComparison returns the first recorded comparison of head that
// names its review scope. A replayed or partly retried submission of the same
// head keeps that scope, and a later policy change moves no scope already
// fixed (DR-3; AC-12.8).
func FixedHeadComparison(events []core.Event, head string) (SubmissionComparison, bool) {
	head = strings.TrimSpace(head)
	if head == "" {
		return SubmissionComparison{}, false
	}
	for _, event := range events {
		if comparison, ok := decodeSubmissionComparison(event); ok && comparison.Fixed && comparison.HeadSHA == head {
			return comparison, true
		}
	}
	return SubmissionComparison{}, false
}

// SubmissionAncestry is GitHub's comparison of a judged head with a submitted
// head: its status and the number of commits the submitted head is behind.
type SubmissionAncestry struct {
	Status   string
	BehindBy int
}

// SubmissionReviewScopeInput is what fixes an ordinary submission's review
// scope. Ancestry is called only when every other condition for delta holds;
// it reads GitHub's comparison of JudgedHead with SubmittedHead.
type SubmissionReviewScopeInput struct {
	LaterRoundReview string
	JudgedHead       string
	SubmittedHead    string
	BaseSHA          string
	Ancestry         func() (SubmissionAncestry, error)
}

// DecideSubmissionReviewScope fixes the review scope and baseline of an
// ordinary submission (AC-12.3; DR-3). The scope is delta from the judged head
// when the frozen policy is delta, a judged head exists and differs from the
// submitted head, and the comparison reports the submitted head ahead with no
// commit behind. Every other case is full from the observed base commit. A
// failed ancestry read gives full and never refuses the submission.
func DecideSubmissionReviewScope(in SubmissionReviewScopeInput) (scope, baseline string) {
	judged, head := strings.TrimSpace(in.JudgedHead), strings.TrimSpace(in.SubmittedHead)
	if in.LaterRoundReview != ReviewScopeDelta || judged == "" || head == "" || judged == head || in.Ancestry == nil {
		return ReviewScopeFull, in.BaseSHA
	}
	ancestry, err := in.Ancestry()
	if err != nil || ancestry.Status != "ahead" || ancestry.BehindBy != 0 {
		return ReviewScopeFull, in.BaseSHA
	}
	return ReviewScopeDelta, judged
}
