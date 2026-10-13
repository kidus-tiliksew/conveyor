package core

import (
	"strings"
	"testing"
)

func TestVerifyReviewCompletionBindsCurrentHead(t *testing.T) {
	task := Task{ID: "task", ReviewedHeadSHA: "head"}
	if !VerifyReviewReady(task, nil) {
		t.Fatal("toggle-off review changed")
	}
	task.SetupContract.VerifyStage = true
	for _, order := range []WorkOrder{{}, {TaskID: "task", Stage: StageVerify, State: WorkOrderQueued, HeadSHA: "head"}, {TaskID: "task", Stage: StageVerify, State: WorkOrderCompleted, HeadSHA: "old"}, {TaskID: "other", Stage: StageVerify, State: WorkOrderCompleted, HeadSHA: "head"}} {
		if VerifyReviewReady(task, []WorkOrder{order}) {
			t.Fatalf("accepted incomplete or wrong revision: %+v", order)
		}
	}
	order := WorkOrder{TaskID: "task", Stage: StageVerify, State: WorkOrderCompleted, HeadSHA: "head", VerificationContextID: "sealed"}
	if !VerifyReviewReady(task, []WorkOrder{order}) {
		t.Fatal("current completion not recognized")
	}
	task.ApprovalStale = true
	task.RefreshHeadSHA = "new"
	if VerifyReviewReady(task, []WorkOrder{order}) {
		t.Fatal("refresh bypassed verify")
	}
}

func TestVerifyUsesOrdinaryLifecycleEdges(t *testing.T) {
	for _, tc := range []struct {
		from    WorkOrderState
		command WorkOrderCommand
		to      WorkOrderState
	}{
		{"", WorkOrderCmdCreate, WorkOrderQueued}, {WorkOrderQueued, WorkOrderCmdClaim, WorkOrderClaimed},
		{WorkOrderClaimed, WorkOrderCmdSubmitVerification, WorkOrderCompleted},
		{WorkOrderClaimed, WorkOrderCmdRenew, WorkOrderClaimed}, {WorkOrderClaimed, WorkOrderCmdRelease, WorkOrderQueued},
		{WorkOrderClaimed, WorkOrderCmdExpire, WorkOrderQueued}, {WorkOrderClaimed, WorkOrderCmdTimeout, WorkOrderTimedOut},
		{WorkOrderQueued, WorkOrderCmdMarkStale, WorkOrderStale}, {WorkOrderStale, WorkOrderCmdRedispatch, WorkOrderQueued},
		{WorkOrderTimedOut, WorkOrderCmdRecover, WorkOrderQueued}, {WorkOrderClaimed, WorkOrderCmdCancel, WorkOrderCancelled},
	} {
		got, err := TransitionWorkOrder(tc.from, tc.command)
		if err != nil || got != tc.to {
			t.Fatalf("%s %s: %s %v", tc.from, tc.command, got, err)
		}
	}
	if !ValidWorkOrderStage(StageVerify) || ValidWorkOrderStage(StageMerge) {
		t.Fatal("invalid executor vocabulary")
	}
}

func TestValidateVerifyDispatchNamesEachCondition(t *testing.T) {
	order := WorkOrder{ID: "verify-1", TaskID: "task", Stage: StageVerify, HeadSHA: "head"}
	if err := ValidateVerifyDispatch(Task{ID: "task", NextStage: StageVerify, ReviewedHeadSHA: "head"}, order); err == nil || !strings.Contains(err.Error(), "verify stage is off in task task's frozen policy") {
		t.Fatalf("off err=%v", err)
	}
	task := Task{ID: "task", NextStage: StageImplement, ReviewedHeadSHA: "head"}
	task.SetupContract.VerifyStage = true
	if err := ValidateVerifyDispatch(task, order); err == nil || !strings.Contains(err.Error(), "next_stage is implement, not verify") {
		t.Fatalf("next_stage err=%v", err)
	}
	task.NextStage = StageVerify
	blank := order
	blank.HeadSHA = ""
	if err := ValidateVerifyDispatch(task, blank); err == nil || !strings.Contains(err.Error(), "the order has no head") {
		t.Fatalf("missing head err=%v", err)
	}
	mismatch := order
	mismatch.HeadSHA = "other"
	if err := ValidateVerifyDispatch(task, mismatch); err == nil || !strings.Contains(err.Error(), "order head other differs from the verify head head") {
		t.Fatalf("mismatch err=%v", err)
	}
	task.ApprovalStale = true
	task.RefreshHeadSHA = "refresh"
	if err := ValidateVerifyDispatch(task, mismatch); err == nil || !strings.Contains(err.Error(), "differs from the verify head refresh (the refresh head, because approval_stale is set)") {
		t.Fatalf("refresh err=%v", err)
	}
	if err := ValidateVerifyDispatch(task, WorkOrder{ID: "impl", Stage: StageImplement}); err != nil {
		t.Fatalf("non-verify err=%v", err)
	}
}
