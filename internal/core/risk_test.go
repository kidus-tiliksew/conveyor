package core

import (
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// TestValidateStatedRisk covers the one-line reason and closed tier rules a
// verdict and a raise share (req-risk-based-review AC-5.2, AC-6.1;
// component-risk-review invariant 1).
func TestValidateStatedRisk(t *testing.T) {
	for _, tc := range []struct {
		tier, reason string
		ok           bool
	}{
		{"", "", true},
		{"low", "small change", true},
		{"high", strings.Repeat("x", MaxRiskReasonRunes), true},
		{"", "reason without a tier", false},
		{"low", "", false},
		{"low", " \t", false},
		{"low", "two\nlines", false},
		{"low", "carriage\rreturn", false},
		{"low", "line separator", false},
		{"low", "nul\x00byte", false},
		{"low", strings.Repeat("x", MaxRiskReasonRunes+1), false},
		{"critical", "unknown tier", false},
		{"Low", "wrong case", false},
	} {
		if err := ValidateStatedRisk("risk_tier", "risk_reason", tc.tier, tc.reason); (err == nil) != tc.ok {
			t.Fatalf("tier=%q reason=%q err=%v, want ok=%t", tc.tier, tc.reason, err, tc.ok)
		}
	}
}

// TestRiskTierOf: the tier is the highest recorded rating and the earliest
// rating at that tier set it; no rating means no tier (AC-5.5).
func TestRiskTierOf(t *testing.T) {
	if _, ok := RiskTierOf(nil); ok {
		t.Fatal("no rating produced a tier")
	}
	setter, ok := RiskTierOf([]RiskRating{
		{Seq: 1, Tier: riskpolicy.TierLow, Source: RiskSourcePathRule},
		{Seq: 2, Tier: riskpolicy.TierMedium, Source: RiskSourceReviewer, ReviewSeat: 1},
		{Seq: 3, Tier: riskpolicy.TierMedium, Source: RiskSourceReviewer, ReviewSeat: 2},
		{Seq: 4, Tier: riskpolicy.TierLow, Source: RiskSourceReviewer, ReviewSeat: 3},
		{Seq: 5, Tier: "unknown"},
	})
	if !ok || setter.Seq != 2 || setter.Tier != riskpolicy.TierMedium {
		t.Fatalf("setter=%+v ok=%t", setter, ok)
	}
}
