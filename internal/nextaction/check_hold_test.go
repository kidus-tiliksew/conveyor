package nextaction

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func checkHoldInput(hold *CheckHold, mergeApproval bool) Input {
	return Input{
		Task:        core.Task{ID: "held", State: core.TaskApproved, MergeApproval: mergeApproval},
		CheckStatus: CheckStatus{Hold: hold},
		Now:         time.Now().UTC(),
		Capabilities: map[core.Capability]bool{
			core.CapabilityOperateGates: true, core.CapabilityRequestChanges: true,
		},
	}
}

// component-next-action row 8; req-operator-next-action AC-1.5.
func TestCheckHoldNamesTheOperatorAndEachCheck(t *testing.T) {
	checks := []CheckRef{
		{Name: "e2e", State: "failed", URL: "https://github.com/acme/app/runs/2"},
		{Name: "deploy", State: "pending", URL: "https://github.com/acme/app/runs/3"},
	}
	na := Project(checkHoldInput(&CheckHold{Reason: CheckHoldFailed, Checks: checks}, false))
	if na.Actor != ActorOperator || na.Kind != KindCheckHold || !na.Judgment {
		t.Fatalf("next action = %+v", na)
	}
	for _, name := range []string{"e2e (failed)", "deploy (pending)"} {
		if !strings.Contains(na.Summary, name) {
			t.Errorf("summary %q does not name %s", na.Summary, name)
		}
	}
	if len(na.Details.Checks) != 2 || na.Details.Checks[0].URL != checks[0].URL || na.Details.CheckHoldReason != CheckHoldFailed {
		t.Fatalf("details = %+v", na.Details)
	}
	var ids []string
	for _, choice := range na.Choices {
		ids = append(ids, choice.ID)
	}
	if strings.Join(ids, ",") != "merge,request_changes,close" {
		t.Fatalf("choices = %v", ids)
	}
	if len(na.Details.Secondary) != 1 || na.Details.Secondary[0].Kind != KindMerging {
		t.Fatalf("secondary = %+v", na.Details.Secondary)
	}
}

// A refused read needs a grant, not a judgment about the change, and names the
// workspace-settings remedy (req-review-gates-evidence AC-1.7).
func TestCheckHoldOfARefusedReadNeedsNoJudgment(t *testing.T) {
	na := Project(checkHoldInput(&CheckHold{Reason: CheckHoldPermission, ReadFailure: "refused", Remedy: "Open workspace settings."}, false))
	if na.Actor != ActorOperator || na.Kind != KindCheckHold || na.Judgment {
		t.Fatalf("next action = %+v", na)
	}
	if na.Label != "Grant Checks read" || !strings.Contains(na.Question, "workspace settings") {
		t.Fatalf("label=%q question=%q", na.Label, na.Question)
	}
	if na.Details.CheckRemedy != "Open workspace settings." || na.Details.CheckReadFailure != "refused" {
		t.Fatalf("details = %+v", na.Details)
	}
	if len(na.Choices) == 0 || na.Choices[0].ID != "merge" {
		t.Fatalf("choices = %+v", na.Choices)
	}
}

// A stuck check reads differently from a failed one, links each stuck check on
// GitHub in details.checks, and offers the merge act (req-review-gates-evidence
// AC-1.9; req-operator-next-action AC-1.5).
func TestStuckCheckHoldReadsDifferentlyAndOffersTheMerge(t *testing.T) {
	url := "https://github.com/acme/app/runs/3"
	checks := []CheckRef{{Name: "deploy", State: "pending", URL: url}}
	failed := Project(checkHoldInput(&CheckHold{Reason: CheckHoldFailed, Checks: checks}, false))
	na := Project(checkHoldInput(&CheckHold{Reason: CheckHoldStuck, Checks: checks}, false))
	if na.Actor != ActorOperator || na.Kind != KindCheckHold || !na.Judgment || na.Details.CheckHoldReason != CheckHoldStuck || na.Details.ReasonCode != CheckHoldStuck {
		t.Fatalf("next action = %+v", na)
	}
	if na.Label == failed.Label || na.Summary == failed.Summary || na.Question == failed.Question || na.IfIgnored == failed.IfIgnored {
		t.Fatalf("stuck reads as failed: %q / %q", na.Summary, failed.Summary)
	}
	for _, text := range []string{na.Summary, na.Question} {
		if !strings.Contains(text, "deploy (pending)") {
			t.Errorf("%q does not name the stuck check", text)
		}
	}
	if !strings.Contains(na.Question, "approved head") || !strings.Contains(na.IfIgnored, "approved and unmerged") {
		t.Fatalf("question=%q ignored=%q", na.Question, na.IfIgnored)
	}
	if len(na.Details.Checks) != 1 || na.Details.Checks[0].URL != url {
		t.Fatalf("details.checks = %+v, want each stuck check linked", na.Details.Checks)
	}
	if len(na.Choices) == 0 || na.Choices[0].ID != "merge" {
		t.Fatalf("choices = %+v, want the merge act", na.Choices)
	}
	if !strings.Contains(na.Choices[0].Effect, "without reading its checks") {
		t.Fatalf("merge effect = %q", na.Choices[0].Effect)
	}
}

// A deadline read that failed names the failure and what the last read found
// (req-review-gates-evidence AC-1.9).
func TestStuckCheckHoldOfAFailedReadNamesTheFailure(t *testing.T) {
	failure := "The check-status read failed."
	bare := Project(checkHoldInput(&CheckHold{Reason: CheckHoldStuck, ReadFailure: failure}, false))
	if bare.Actor != ActorOperator || !bare.Judgment || bare.Details.CheckReadFailure != failure || bare.Details.CheckHoldReason != CheckHoldStuck {
		t.Fatalf("next action = %+v", bare)
	}
	if !strings.Contains(bare.Summary, "could not read") || strings.Contains(bare.Summary, "named") {
		t.Fatalf("summary = %q", bare.Summary)
	}
	named := Project(checkHoldInput(&CheckHold{Reason: CheckHoldStuck, ReadFailure: failure, Checks: []CheckRef{{Name: "deploy", State: "pending"}}}, false))
	if !strings.Contains(named.Summary, "deploy (pending)") || !strings.Contains(named.Question, "deploy (pending)") {
		t.Fatalf("summary=%q question=%q", named.Summary, named.Question)
	}
	if len(named.Choices) == 0 || named.Choices[0].ID != "merge" {
		t.Fatalf("choices = %+v", named.Choices)
	}
}

func TestCheckHoldAppliesOnlyWithTheMergeGateOff(t *testing.T) {
	na := Project(checkHoldInput(&CheckHold{Reason: CheckHoldFailed}, true))
	if na.Kind != KindMergeExecution {
		t.Fatalf("kind = %s", na.Kind)
	}
}

// A task waiting on a pending check stays with the factory and names what it
// waits on (req-review-gates-evidence AC-1.5).
func TestMergingNamesWhatItWaitsOn(t *testing.T) {
	in := checkHoldInput(nil, false)
	in.CheckStatus.Waiting = []CheckRef{{Name: "deploy", State: "pending"}}
	na := Project(in)
	if na.Actor != ActorFactory || na.Kind != KindMerging || na.Judgment || len(na.Details.Checks) != 1 {
		t.Fatalf("next action = %+v", na)
	}
	in.CheckStatus = CheckStatus{ReadFailure: "The check-status read failed."}
	if na = Project(in); na.Kind != KindMerging || na.Details.CheckReadFailure == "" {
		t.Fatalf("next action = %+v", na)
	}
}

// A task whose last read went unanswered says so, before and after its retries
// end, and stays with the factory (req-review-gates-evidence AC-1.7).
func TestMergingSaysTheReadWentUnanswered(t *testing.T) {
	in := checkHoldInput(nil, false)
	in.CheckStatus = CheckStatus{ReadFailure: "The check-status read failed."}
	na := Project(in)
	if na.Actor != ActorFactory || na.Kind != KindMerging || na.Judgment || na.Details.ReasonCode != CheckReadFailed || !strings.Contains(na.Summary, "reading it again") {
		t.Fatalf("next action = %+v", na)
	}
	in.CheckStatus.RetriesExhausted = true
	na = Project(in)
	if na.Actor != ActorFactory || na.Kind != KindMerging || na.Details.ReasonCode != CheckReadRetriesExhausted || !strings.Contains(na.Summary, "retries ended") || !strings.Contains(na.IfIgnored, "deadline") {
		t.Fatalf("next action = %+v", na)
	}
}
