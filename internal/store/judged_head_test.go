package store

import (
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func judgedRound(round int, reason string, cancelled []int, reviews ...map[string]any) core.Event {
	return core.Event{Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"review_round": round, "reason_code": reason, "cancelled_seats": cancelled, "reviews": reviews})}
}

func judgedReview(order string, seat int, commit string) map[string]any {
	return map[string]any{"review_work_order_id": order, "review_seat": seat, "reviewed_commit_sha": commit}
}

func judgedSeats(round, count int) []core.WorkOrder {
	orders := make([]core.WorkOrder, 0, count)
	for seat := 1; seat <= count; seat++ {
		orders = append(orders, core.WorkOrder{ID: judgedOrderID(round, seat), Stage: core.StageReview, ReviewRound: round, ReviewSeat: seat})
	}
	return orders
}

func judgedOrderID(round, seat int) string {
	return "t-review-" + string(rune('0'+round)) + "-seat-" + string(rune('0'+seat))
}

// TestJudgedHead covers the judged head of AC-12.2 (feature-delta-review-rounds
// DR-2): a complete round of either verdict gives its commit; a fail-fast
// round gives one only when it cancelled no seat; a head mismatch, a round
// with a seat that never answered, a superseded round, and a malformed event
// give none; and selection takes the latest qualifying round in history.
func TestJudgedHead(t *testing.T) {
	r1 := judgedSeats(1, 2)
	r2 := judgedSeats(2, 2)
	approved := judgedRound(1, "approved", nil, judgedReview(r1[0].ID, 1, "h1"), judgedReview(r1[1].ID, 2, "h1"))
	bounced := judgedRound(1, "panel_changes_requested", nil, judgedReview(r1[0].ID, 1, "h1"), judgedReview(r1[1].ID, 2, "h1"))
	superseded := core.Event{Kind: "task.setup.changed", Payload: core.JSONPayload(map[string]any{"review_transition": map[string]any{"superseded_work_order_ids": []string{r2[0].ID, r2[1].ID}}})}
	for _, tc := range []struct {
		name   string
		events []core.Event
		orders []core.WorkOrder
		want   string
	}{
		{name: "CompleteApproval", events: []core.Event{approved}, orders: r1, want: "h1"},
		{name: "CompleteChangesRequested", events: []core.Event{bounced}, orders: r1, want: "h1"},
		{name: "FailFastWithoutCancellation", events: []core.Event{judgedRound(1, "panel_changes_requested", []int{}, judgedReview(r1[0].ID, 1, "h1"), judgedReview(r1[1].ID, 2, "h1"))}, orders: r1, want: "h1"},
		{name: "FailFastWithCancelledSeat", events: []core.Event{judgedRound(1, "panel_changes_requested", []int{2}, judgedReview(r1[0].ID, 1, "h1"))}, orders: r1},
		{name: "HeadMismatchSettlement", events: []core.Event{judgedRound(1, "review_head_mismatch", nil, judgedReview(r1[0].ID, 1, "h1"), judgedReview(r1[1].ID, 2, "h1"))}, orders: r1},
		{name: "SeatsOnDifferentCommits", events: []core.Event{judgedRound(1, "panel_changes_requested", nil, judgedReview(r1[0].ID, 1, "h1"), judgedReview(r1[1].ID, 2, "h2"))}, orders: r1},
		{name: "BlankCommit", events: []core.Event{judgedRound(1, "approved", nil, judgedReview(r1[0].ID, 1, ""), judgedReview(r1[1].ID, 2, ""))}, orders: r1},
		{name: "OpenedSeatWithoutVerdict", events: []core.Event{judgedRound(1, "approved", nil, judgedReview(r1[0].ID, 1, "h1"))}, orders: r1},
		{name: "SupersededRound", events: []core.Event{judgedRound(2, "panel_changes_requested", nil, judgedReview(r2[0].ID, 1, "h2"), judgedReview(r2[1].ID, 2, "h2")), superseded}, orders: r2},
		{name: "NoRound", events: []core.Event{{Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"reviewed_commit_sha": "h1"})}}, orders: r1},
		{name: "MalformedRound", events: []core.Event{{Kind: "review.round_completed", Payload: []byte(`not json`)}}, orders: r1},
		{name: "LatestQualifyingRound", events: []core.Event{bounced, judgedRound(2, "panel_changes_requested", nil, judgedReview(r2[0].ID, 1, "h2"), judgedReview(r2[1].ID, 2, "h2"))}, orders: append(append([]core.WorkOrder{}, r1...), r2...), want: "h2"},
		{name: "OlderRoundBehindCancelledRound", events: []core.Event{bounced, judgedRound(2, "panel_changes_requested", []int{2}, judgedReview(r2[0].ID, 1, "h2"))}, orders: append(append([]core.WorkOrder{}, r1...), r2...), want: "h1"},
		{name: "OlderRoundBehindSupersededRound", events: []core.Event{bounced, judgedRound(2, "panel_changes_requested", nil, judgedReview(r2[0].ID, 1, "h2"), judgedReview(r2[1].ID, 2, "h2")), superseded}, orders: append(append([]core.WorkOrder{}, r1...), r2...), want: "h1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := JudgedHead(tc.events, tc.orders)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("JudgedHead=(%q,%t), want %q", got, ok, tc.want)
			}
		})
	}
}

// TestDeltaReviewSubmissionScope covers AC-12.3: delta only for a frozen
// delta policy, a judged head that differs from the head, and a comparison
// that reports the head ahead with nothing behind. The comparison is read only
// when every other condition holds, and its failure gives full.
func TestDeltaReviewSubmissionScope(t *testing.T) {
	read := func(ancestry SubmissionAncestry, err error, calls *int) func() (SubmissionAncestry, error) {
		return func() (SubmissionAncestry, error) { *calls++; return ancestry, err }
	}
	ahead := SubmissionAncestry{Status: "ahead"}
	for _, tc := range []struct {
		name, policy, judged, head string
		ancestry                   SubmissionAncestry
		err                        error
		wantScope, wantBaseline    string
		wantCalls                  int
	}{
		{name: "Delta", policy: ReviewScopeDelta, judged: "j", head: "h", ancestry: ahead, wantScope: ReviewScopeDelta, wantBaseline: "j", wantCalls: 1},
		{name: "PolicyAbsent", judged: "j", head: "h", ancestry: ahead, wantScope: ReviewScopeFull, wantBaseline: "b"},
		{name: "PolicyFull", policy: ReviewScopeFull, judged: "j", head: "h", ancestry: ahead, wantScope: ReviewScopeFull, wantBaseline: "b"},
		{name: "NoJudgedHead", policy: ReviewScopeDelta, head: "h", ancestry: ahead, wantScope: ReviewScopeFull, wantBaseline: "b"},
		{name: "EqualHead", policy: ReviewScopeDelta, judged: "h", head: "h", ancestry: ahead, wantScope: ReviewScopeFull, wantBaseline: "b"},
		{name: "Behind", policy: ReviewScopeDelta, judged: "j", head: "h", ancestry: SubmissionAncestry{Status: "behind", BehindBy: 2}, wantScope: ReviewScopeFull, wantBaseline: "b", wantCalls: 1},
		{name: "Diverged", policy: ReviewScopeDelta, judged: "j", head: "h", ancestry: SubmissionAncestry{Status: "diverged", BehindBy: 1}, wantScope: ReviewScopeFull, wantBaseline: "b", wantCalls: 1},
		{name: "Identical", policy: ReviewScopeDelta, judged: "j", head: "h", ancestry: SubmissionAncestry{Status: "identical"}, wantScope: ReviewScopeFull, wantBaseline: "b", wantCalls: 1},
		{name: "AheadWithCommitsBehind", policy: ReviewScopeDelta, judged: "j", head: "h", ancestry: SubmissionAncestry{Status: "ahead", BehindBy: 1}, wantScope: ReviewScopeFull, wantBaseline: "b", wantCalls: 1},
		{name: "ReadFails", policy: ReviewScopeDelta, judged: "j", head: "h", ancestry: ahead, err: errors.New("forge unavailable"), wantScope: ReviewScopeFull, wantBaseline: "b", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			scope, baseline := DecideSubmissionReviewScope(SubmissionReviewScopeInput{LaterRoundReview: tc.policy, JudgedHead: tc.judged, SubmittedHead: tc.head, BaseSHA: "b", Ancestry: read(tc.ancestry, tc.err, &calls)})
			if scope != tc.wantScope || baseline != tc.wantBaseline || calls != tc.wantCalls {
				t.Fatalf("scope=%q baseline=%q calls=%d, want %q %q %d", scope, baseline, calls, tc.wantScope, tc.wantBaseline, tc.wantCalls)
			}
		})
	}
}

// TestDeltaReviewComparisonRecords covers the recorded comparison: a record
// without a scope reads as full from its base, a delta record without a
// baseline reads as full, the latest record answers the round, the first
// fixed record of a head answers a replay, and the whole-change pair never
// moves (feature-delta-review-rounds DR-3 to DR-5).
func TestDeltaReviewComparisonRecords(t *testing.T) {
	opened := func(base, head string, fields map[string]any) core.Event {
		payload := map[string]any{"base_sha": base, "head_sha": head}
		for key, value := range fields {
			payload[key] = value
		}
		return core.Event{Kind: SubmissionComparisonEventKind, Payload: core.JSONPayload(payload)}
	}
	legacy := opened("b0", "h0", nil)
	if got, ok := LatestSubmissionComparison([]core.Event{legacy}); !ok || got.Fixed || got.Delta() || got.ReviewBaselineSHA != "b0" || got.ReviewScope != ReviewScopeFull {
		t.Fatalf("legacy record=%+v ok=%t", got, ok)
	}
	if got, _ := LatestSubmissionComparison([]core.Event{opened("b1", "h1", map[string]any{"review_scope": "delta"})}); got.Delta() || got.ReviewBaselineSHA != "b1" || !got.Fixed {
		t.Fatalf("delta record without baseline=%+v", got)
	}
	delta := opened("b1", "h1", map[string]any{"review_scope": "delta", "review_baseline_sha": "j1"})
	full := opened("b2", "h1", map[string]any{"review_scope": "full", "review_baseline_sha": "b2"})
	events := []core.Event{legacy, opened("b1", "h1", nil), delta, full}
	if got, ok := FixedHeadComparison(events, "h1"); !ok || !got.Delta() || got.ReviewBaselineSHA != "j1" {
		t.Fatalf("first fixed record=%+v ok=%t", got, ok)
	}
	if _, ok := FixedHeadComparison(events, "h0"); ok {
		t.Fatal("a record without a scope fixed none")
	}
	if got, _ := LatestSubmissionComparison(events); got.Delta() || got.BaseSHA != "b2" {
		t.Fatalf("latest record=%+v", got)
	}
	if base, head, ok := RecordedSubmissionComparison([]core.Event{delta}); !ok || base != "b1" || head != "h1" {
		t.Fatalf("whole-change pair=(%q,%q,%t)", base, head, ok)
	}
	if baseline, head, scope, recorded := ReviewRoundDispatchComparison(core.Task{}, []core.Event{delta}); !recorded || baseline != "j1" || head != "h1" || scope != ReviewScopeDelta {
		t.Fatalf("delta dispatch comparison=(%q,%q,%q,%t)", baseline, head, scope, recorded)
	}
	stale := core.Task{ApprovalStale: true, RefreshBaselineSHA: "approved", RefreshHeadSHA: "h1", RefreshReviewScope: "full"}
	if baseline, head, scope, recorded := ReviewRoundDispatchComparison(stale, []core.Event{delta}); recorded || baseline != "approved" || head != "h1" || scope != "full" {
		t.Fatalf("stale dispatch comparison=(%q,%q,%q,%t)", baseline, head, scope, recorded)
	}
}
