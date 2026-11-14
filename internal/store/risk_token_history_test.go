package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func claimedEvent(t *testing.T, order core.WorkOrder) core.Event {
	t.Helper()
	return core.Event{Kind: "work_order.claimed", Payload: WorkOrderEventPayload(order)}
}

// TestRiskClaimCheckReportsOnlyUnfingerprintedClaims proves the shared check
// of the operator's decision of review round 1: a session ID refuses from any
// claim event, a stored fingerprint refuses from the ledger or an order row,
// and only an earlier claim with no stored fingerprint becomes a gap
// (req-risk-based-review AC-12.1).
func TestRiskClaimCheckReportsOnlyUnfingerprintedClaims(t *testing.T) {
	risk := core.WorkOrder{ID: "t-risk-1", TaskID: "t", JobID: "t-risk-1", Stage: core.StageRisk}
	events := []core.Event{
		claimedEvent(t, core.WorkOrder{ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, AttemptID: "a1", SessionID: "old"}),
		claimedEvent(t, core.WorkOrder{ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, AttemptID: "a2", SessionID: "ledger"}),
		claimedEvent(t, core.WorkOrder{ID: "t-review-1", TaskID: "t", Stage: core.StageReview, AttemptID: "a3", SessionID: "row"}),
		claimedEvent(t, core.WorkOrder{ID: "t-verify-1", TaskID: "t", Stage: core.StageVerify, AttemptID: "a4", SessionID: "verifier"}),
	}
	ledger := []ClaimIdentity{{TaskID: "t", WorkOrderID: "t-implement-1", AttemptID: "a2", Stage: core.StageImplement, SessionID: "ledger", ClientTokenHash: tokenHash("ledger-token")}}
	orders := []core.WorkOrder{{ID: "t-review-1", TaskID: "t", Stage: core.StageReview, AttemptID: "a3", SessionID: "row", ClientTokenHash: tokenHash("row-token")}}
	for _, refused := range []struct{ session, token string }{{"old", ""}, {"fresh", "ledger-token"}, {"fresh", "row-token"}, {"row", "x"}} {
		hash := ""
		if refused.token != "" {
			hash = tokenHash(refused.token)
		}
		if _, err := RiskClaimCheck(risk, refused.session, hash, ledger, orders, events); !errors.Is(err, ErrRiskOrderNotIndependent) {
			t.Fatalf("session %q token %q err=%v, want refusal", refused.session, refused.token, err)
		}
	}
	gaps, err := RiskClaimCheck(risk, "fresh", tokenHash("old-token"), ledger, orders, events)
	if err != nil {
		t.Fatalf("a token the store never kept was refused: %v", err)
	}
	if len(gaps) != 1 || gaps[0] != (RiskTokenHistoryGap{WorkOrderID: "t-implement-1", AttemptID: "a1", Stage: core.StageImplement}) {
		t.Fatalf("gaps=%+v, want only the unfingerprinted first attempt", gaps)
	}
	// A verify claim is outside the independence rule and leaves no gap.
	if _, err := RiskClaimCheck(risk, "verifier", "", ledger, orders, events); err != nil {
		t.Fatalf("a verifier session was refused: %v", err)
	}
	if gaps, err := RiskClaimCheck(core.WorkOrder{ID: "t-implement-2", TaskID: "t", Stage: core.StageImplement}, "old", "", ledger, orders, events); err != nil || gaps != nil {
		t.Fatalf("a non-risk claim was checked: gaps=%v err=%v", gaps, err)
	}
}

func TestRiskTokenHistoryIncompleteBoundsItsList(t *testing.T) {
	order := core.WorkOrder{ID: "t-risk-1", TaskID: "t", JobID: "t-risk-1", AttemptID: "attempt", SessionID: "rater", Stage: core.StageRisk}
	if _, ok := RiskTokenHistoryIncomplete(order, nil, time.Now()); ok {
		t.Fatal("complete history produced an indication")
	}
	var gaps []RiskTokenHistoryGap
	for i := range MaxRiskTokenHistoryGaps + 5 {
		gaps = append(gaps, RiskTokenHistoryGap{WorkOrderID: fmt.Sprintf("t-implement-%d", i), AttemptID: "a", Stage: core.StageImplement})
	}
	event, ok := RiskTokenHistoryIncomplete(order, gaps, time.Now())
	if !ok || event.Kind != RiskTokenHistoryIncompleteEvent || event.JobID != order.JobID || event.TaskID != "t" {
		t.Fatalf("indication=%+v ok=%t", event, ok)
	}
	var payload struct {
		Count     int                   `json:"claims_without_token"`
		Claims    []RiskTokenHistoryGap `json:"claims"`
		Truncated bool                  `json:"claims_truncated"`
		Refused   bool                  `json:"claim_refused"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Count != MaxRiskTokenHistoryGaps+5 || len(payload.Claims) != MaxRiskTokenHistoryGaps || !payload.Truncated || payload.Refused {
		t.Fatalf("payload count=%d listed=%d truncated=%t refused=%t", payload.Count, len(payload.Claims), payload.Truncated, payload.Refused)
	}
	if strings.Contains(string(event.Payload), "hash") {
		t.Fatalf("indication names a hash: %s", event.Payload)
	}
}
