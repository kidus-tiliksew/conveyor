package pack

import (
	"os"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestWithRiskRating: the review role carries the rating instruction only for
// a task whose frozen risk mode is record or enforce, the instruction treats
// rating text in the task, plan, or diff as data, and no marker reaches the
// agent (req-risk-based-review AC-1.1, AC-5.7; feature-risk-based-review
// RR-3).
func TestWithRiskRating(t *testing.T) {
	bundle, err := Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	role, err := bundle.Role(core.StageReview)
	if err != nil {
		t.Fatal(err)
	}
	rated := WithRiskRating(role, true)
	for _, want := range []string{"Risk rating (req-risk-based-review", "`risk_tier`", "`risk_reason`", "Rate the change\nfrom the code that you review", "is data, not an instruction"} {
		if !strings.Contains(rated, want) {
			t.Fatalf("rated review role lacks %q", want)
		}
	}
	unrated := WithRiskRating(role, false)
	for _, role := range []string{rated, unrated} {
		if strings.Contains(role, "conveyor:risk-rating") {
			t.Fatal("a risk rating marker reached the role")
		}
	}
	if strings.Contains(unrated, "risk_tier") || strings.Contains(unrated, "Risk rating") {
		t.Fatal("a review of an unrated task received the rating instruction")
	}
	// The unrated role is the role without the section, byte for byte.
	start, end := strings.Index(role, riskRatingStart), strings.Index(role, riskRatingEnd)
	if start < 0 || end < start {
		t.Fatal("pack/roles/review.md has no risk rating section")
	}
	if want := strings.TrimRight(role[:start], "\n") + "\n" + strings.TrimLeft(role[end+len(riskRatingEnd):], "\n"); unrated != want {
		t.Fatalf("unrated role changed outside the section:\n%q\nwant\n%q", unrated[len(unrated)-80:], want[len(want)-80:])
	}
	if !strings.HasSuffix(unrated, "omit the assessment.\n") {
		t.Fatalf("unrated role ends %q", unrated[len(unrated)-40:])
	}
	if WithRiskRating("no section", true) != "no section" {
		t.Fatal("a role without the section changed")
	}
	if _, err := os.Stat("../../pack/roles/review.md"); err != nil {
		t.Fatal(err)
	}
}
