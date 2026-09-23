package core

import "testing"

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
