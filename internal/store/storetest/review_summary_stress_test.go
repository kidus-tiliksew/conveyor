package storetest

import (
	"os"
	"testing"
)

// The ReviewSummary stress cases run CONVEYOR_REVIEW_SUMMARY_STRESS_ROUNDS
// repetitions, 50 when it is unset (component-testing-strategy,
// Configuration).
func TestRepeatSummaryStressRunsConfiguredRounds(t *testing.T) {
	count := func() int {
		n := 0
		repeatSummaryStress(t, func() { n++ })
		return n
	}
	t.Setenv(reviewSummaryStressRoundsEnv, "7")
	if got := count(); got != 7 {
		t.Fatalf("%s=7 ran %d repetitions", reviewSummaryStressRoundsEnv, got)
	}
	if err := os.Unsetenv(reviewSummaryStressRoundsEnv); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 50 {
		t.Fatalf("unset %s ran %d repetitions, want 50", reviewSummaryStressRoundsEnv, got)
	}
}

func TestReviewSummaryStressRoundsRejectsInvalidValues(t *testing.T) {
	for _, raw := range []string{"", "0", "-3", "ten", "1.5", " 50"} {
		lookup := func(string) (string, bool) { return raw, true }
		if rounds, err := reviewSummaryStressRounds(lookup); err == nil {
			t.Errorf("%s=%q gave %d rounds, want an error", reviewSummaryStressRoundsEnv, raw, rounds)
		}
	}
}
