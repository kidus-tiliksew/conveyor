package core

import (
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// TestValidateRiskReason covers the one-line reason rules that a person's
// raise and a risk order's completion share (req-risk-based-review AC-6.1,
// AC-12.2).
func TestValidateRiskReason(t *testing.T) {
	for _, tc := range []struct {
		reason string
		ok     bool
	}{
		{"small change", true},
		{strings.Repeat("x", MaxRiskReasonRunes), true},
		{"", false},
		{" \t", false},
		{"two\nlines", false},
		{"carriage\rreturn", false},
		{"line\u2028separator", false},
		{"nul\x00byte", false},
		{strings.Repeat("x", MaxRiskReasonRunes+1), false},
	} {
		if err := ValidateRiskReason("reason", tc.reason); (err == nil) != tc.ok {
			t.Fatalf("reason=%q err=%v, want ok=%t", tc.reason, err, tc.ok)
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
