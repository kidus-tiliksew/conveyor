package pack

import (
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestDeltaReviewRoleInstruction: the review role holds the four delta round
// instructions in one marked section. A delta seat reads them without the
// markers, and every other review reads the role without the section, byte
// for byte (feature-delta-review-rounds DR-5; req-review-gates-evidence
// AC-12.4, AC-12.5).
func TestDeltaReviewRoleInstruction(t *testing.T) {
	bundle, err := Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	role, err := bundle.Role(core.StageReview)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(role, deltaRoundStart) != 1 || strings.Count(role, deltaRoundEnd) != 1 {
		t.Fatal("review role does not mark exactly one delta round section")
	}
	start, end := strings.Index(role, deltaRoundStart), strings.Index(role, deltaRoundEnd)
	section := role[start+len(deltaRoundStart) : end]
	delta, full := WithDeltaRound(role, true), WithDeltaRound(role, false)
	for _, instruction := range []string{
		"a complete review panel already judged the baseline commit",
		"Judge the changes from the baseline commit to the reviewed head.",
		"Read `bounce_history`. For each earlier finding, state whether these",
		"Request changes for each earlier finding that is still open.",
		"request changes for each defect that these changes introduce, in any",
		"Fill each whole-task assessment from the reviewed head",
	} {
		if !strings.Contains(delta, instruction) {
			t.Fatalf("delta role lacks %q", instruction)
		}
		if strings.Contains(full, instruction) {
			t.Fatalf("full role carries %q", instruction)
		}
	}
	for _, served := range []string{delta, full, MCPReviewRole(delta), InProcessReviewRole(full)} {
		if strings.Contains(served, "conveyor:delta-round") {
			t.Fatal("a served role carries a delta round marker")
		}
	}
	if full != role[:start]+role[end+len(deltaRoundEnd):] || delta != role[:start]+section+role[end+len(deltaRoundEnd):] {
		t.Fatal("the delta round section changed text outside its markers")
	}
	if WithDeltaRound("no section", true) != "no section" || WithDeltaRound(deltaRoundStart+"open", false) != deltaRoundStart+"open" {
		t.Fatal("a role without a complete section changed")
	}
}
