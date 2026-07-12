package pipeline

import "testing"

func TestParseTriageAndReview(t *testing.T) {
	triage, err := ParseTriage("```conveyor:triage\n{\"class\":\"feature\",\"automatability\":0.8,\"route\":\"spec\",\"summary\":\"Needs a contract.\"}\n```")
	if err != nil || triage.Route != "spec" {
		t.Fatalf("triage=%+v err=%v", triage, err)
	}
	review, err := ParseReview("```conveyor:review\n{\"verdict\":\"changes_requested\",\"reason_code\":\"scope-creep\",\"summary\":\"Extra refactor\",\"feedback\":\"Remove it.\"}\n```")
	if err != nil || review.ReasonCode != "scope-creep" {
		t.Fatalf("review=%+v err=%v", review, err)
	}
}

func TestParseSpecValidatesMachineBlocks(t *testing.T) {
	valid := `# Change

## Intent
Ship it.

## Non-goals
Unrelated work.

` + "```conveyor:acceptance\n- id: AC-1\n  criterion: Tests pass\n  verify: test\n  ref: ./...\n```" + `

` + "```conveyor:decomposition\n- id: SUB-1\n  repo: api\n  summary: Add the contract\n  depends_on: []\n```"
	parsed, err := ParseSpec(valid)
	if err != nil || len(parsed.Acceptance) != 1 || len(parsed.Decomposition) != 1 {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	invalid := `# Change

## Intent
Ship it.

## Non-goals
Nothing.

` + "```conveyor:acceptance\n- id: AC-1\n  criterion: Tests pass\n  verify: test\n```" + `

` + "```conveyor:decomposition\n- id: SUB-1\n  repo: api\n  summary: Add it\n  depends_on: [SUB-404]\n```"
	if _, err := ParseSpec(invalid); err == nil {
		t.Fatal("accepted an unknown decomposition dependency")
	}
}
