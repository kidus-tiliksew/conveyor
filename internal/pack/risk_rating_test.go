package pack

import (
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestReviewRoleStatesNoRating: the review role asks for no risk tier in any
// form. A risk agent rates the change, and a review verdict that sends
// risk_tier or risk_reason is refused (req-risk-based-review AC-5.7;
// feature-risk-based-review RR-14).
func TestReviewRoleStatesNoRating(t *testing.T) {
	bundle, err := Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	role, err := bundle.Role(core.StageReview)
	if err != nil {
		t.Fatal(err)
	}
	for _, served := range []string{role, MCPReviewRole(role), InProcessReviewRole(role)} {
		for _, forbidden := range []string{"risk_tier", "risk_reason", "Risk rating", "conveyor:risk-rating", "Rate the head"} {
			if strings.Contains(served, forbidden) {
				t.Fatalf("review role carries %q", forbidden)
			}
		}
	}
	if !strings.HasSuffix(role, "omit the assessment.\n") {
		t.Fatalf("review role ends %q", role[len(role)-40:])
	}
}
