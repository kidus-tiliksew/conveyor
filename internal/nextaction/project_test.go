package nextaction

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestProjectRowsAndLabel(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	cases := []struct {
		name        string
		in          Input
		actor, kind string
		stall       string
		secondary   string
	}{
		{name: "row1", in: Input{Task: core.Task{State: core.TaskMerged}, Now: now}, actor: ActorNone, kind: KindTerminal},
		{name: "row2", in: Input{Task: core.Task{State: core.TaskParked}, Markers: Markers{ParkCommand: "triage.park"}, Now: now}, actor: ActorOperator, kind: KindParked},
		{name: "row3", in: Input{Task: core.Task{State: core.TaskParked}, Markers: Markers{ParkCommand: "dispatch.fail_final"}, Now: now}, actor: ActorOperator, kind: KindDispatchExhausted},
		{name: "row4 plan", in: gateInput(core.GateKindPlanApproval, now), actor: ActorOperator, kind: KindPlanApproval},
		{name: "row4 revision", in: gateInput(core.GateKindPlanRevision, now), actor: ActorOperator, kind: KindPlanRevision},
		{name: "row4 merge", in: gateInput(core.GateKindMergeApproval, now), actor: ActorOperator, kind: KindMergeApproval},
		{name: "row4 bounce", in: gateInput(core.GateKindReviewBounceLimit, now), actor: ActorOperator, kind: KindReviewBounceLimit},
		{name: "row4 job", in: gateInput(core.GateKindJobFailure, now), actor: ActorOperator, kind: KindJobFailure},
		{name: "row5", in: conflictInput(true, now), actor: ActorOperator, kind: KindMergeConflict, secondary: KindMergeExecution},
		{name: "row5 pre", in: conflictInput(false, now), actor: ActorFactory, kind: KindRetrying},
		{name: "row6", in: Input{Task: core.Task{State: core.TaskApproved, MergeApproval: true}, Now: now}, actor: ActorOperator, kind: KindMergeExecution},
		{name: "row7", in: Input{Task: core.Task{State: core.TaskApproved}, Markers: Markers{ForgeFailure: true}, Now: now}, actor: ActorFactory, kind: KindMerging},
		{name: "row8", in: waitingAndQuestion(now), actor: ActorOperator, kind: KindDocumentProposal, secondary: KindAgentQuestion},
		{name: "row9-10", in: questionAndVerify(now), actor: ActorOperator, kind: KindAgentQuestion, secondary: KindVerificationDisposition},
		{name: "row11-12", in: Input{Task: core.Task{State: core.TaskRunning}, Markers: Markers{ForgeFailure: true, ReviewRecovery: true}, Now: now}, actor: ActorOperator, kind: KindForgeFailure, secondary: KindReviewRecovery},
		{name: "row13-14", in: unsatisfiableAndSuppressed(now), actor: ActorOperator, kind: KindDependencyUnsatisfiable, secondary: KindStalled},
		{name: "row14 timed_out", in: timedOut(now), actor: ActorOperator, kind: KindStalled, stall: StallTimedOut},
		{name: "row14-15", in: suppressedAndBlocked(now), actor: ActorOperator, kind: KindStalled, secondary: KindBlocked},
		{name: "row15-17", in: blockedClaimable(now), actor: ActorDependency, kind: KindBlocked, secondary: KindQueued},
		{name: "row16-17", in: claimedAndQueued(now), actor: ActorAgent, kind: KindWorking, secondary: KindQueued},
		{name: "ar6", in: scheduled(now, later), actor: ActorFactory, kind: KindRetrying},
		{name: "ar6 exhausted", in: recoveryExhausted(now), actor: ActorOperator, kind: KindStalled, stall: StallAutomaticRecoveryExhausted},
		{name: "verify fallback", in: verifyRefused(now), actor: ActorOperator, kind: KindStalled, stall: StallUnclaimable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Project(tc.in)
			if got.Actor != tc.actor || got.Kind != tc.kind {
				t.Fatalf("actor/kind=%s/%s want %s/%s", got.Actor, got.Kind, tc.actor, tc.kind)
			}
			if got.Label == "" {
				t.Fatal("missing label")
			}
			entry, ok := Lookup(got.Kind, got.Details.StallReason, got.Details.ReleaseOutcome, got.Details.GateKind)
			if !ok || got.Label != entry.Label {
				t.Fatalf("label=%q entry=%q ok=%v", got.Label, entry.Label, ok)
			}
			if CompactFrom(got).Label != got.Label {
				t.Fatal("compact label drifted")
			}
			if tc.stall != "" && got.Details.StallReason != tc.stall {
				t.Fatalf("stall=%s", got.Details.StallReason)
			}
			if tc.secondary != "" && !hasSecondary(got, tc.secondary) {
				t.Fatalf("secondary=%v want %s", got.Details.Secondary, tc.secondary)
			}
		})
	}
}

func TestRefusedChoices(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	claimed := Project(Input{Task: core.Task{ID: "t", State: core.TaskParked}, Markers: Markers{ParkCommand: "dispatch.fail_final"}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderClaimed}}, Now: now, Capabilities: allCaps()})
	if hasChoice(claimed, "recover") || !hasRefusal(claimed, "recover") {
		t.Fatalf("claimed recover choices=%v refusals=%v", claimed.Choices, claimed.Details.Refusals)
	}
	revision := Project(Input{Task: core.Task{ID: "t", State: core.TaskAwaiting, GateKind: core.GateKindPlanRevision}, GateSpecVersion: 1, LatestSpec: &SpecRef{Version: 1}, Now: now})
	if hasChoice(revision, "approve") || !hasRefusal(revision, "approve") {
		t.Fatalf("plan revision approve choices=%v refusals=%v", revision.Choices, revision.Details.Refusals)
	}
	superseded := Project(Input{Task: core.Task{ID: "t", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}, GateSpecVersion: 1, LatestSpec: &SpecRef{Version: 2}, Now: now})
	if hasChoice(superseded, "approve") || !hasRefusal(superseded, "approve") {
		t.Fatalf("superseded choices=%v refusals=%v", superseded.Choices, superseded.Details.Refusals)
	}
	stalled := Project(timedOut(now))
	if hasChoice(stalled, "change_policy") || !hasRefusal(stalled, "change_policy") {
		t.Fatalf("policy choices=%v refusals=%v", stalled.Choices, stalled.Details.Refusals)
	}
	allowed := Project(Input{Task: core.Task{ID: "t", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}, GateSpecVersion: 1, LatestSpec: &SpecRef{Version: 1}, Now: now, Capabilities: map[core.Capability]bool{core.CapabilityOperateGates: true}})
	choice, ok := choiceByID(allowed, "approve")
	if !ok || !choice.Allowed {
		t.Fatalf("allowed=%v ok=%v", choice, ok)
	}
	denied := Project(Input{Task: core.Task{ID: "t", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}, GateSpecVersion: 1, LatestSpec: &SpecRef{Version: 1}, Now: now})
	choice, ok = choiceByID(denied, "approve")
	if !ok || choice.Allowed {
		t.Fatalf("denied allowed=%v", choice.Allowed)
	}
}

func TestCatalogueCoverage(t *testing.T) {
	for _, kind := range Kinds() {
		entry, ok := Lookup(kind, "", "", "")
		if !ok || entry.Label == "" || entry.Summary == nil {
			t.Fatalf("kind %s missing catalogue", kind)
		}
	}
	for _, reason := range StallReasons() {
		entry, ok := Lookup(KindStalled, reason, "", "")
		if !ok || entry.Label == "" || entry.Summary == nil {
			t.Fatalf("stall %s missing", reason)
		}
	}
	entry, ok := Lookup(KindPlanApproval, "", "", "")
	if !ok || entry.RunGate == nil || entry.RunGate.Kind != "spec" {
		t.Fatal("plan_approval run gate drifted")
	}
	for _, outcome := range ReleaseOutcomes() {
		entry, ok := Lookup("", "", outcome, "")
		if !ok || entry.Label == "" || entry.Summary == nil {
			t.Fatalf("release %s missing", outcome)
		}
	}
	for _, gate := range GateKinds() {
		entry, ok := Lookup(string(gate), "", "", string(gate))
		if !ok || entry.Label == "" || entry.RunGate == nil || entry.RunGate.Label == "" {
			t.Fatalf("gate %s missing run gate", gate)
		}
	}
}

func gateInput(kind core.GateKind, now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskAwaiting, GateKind: kind}, Now: now}
}

func conflictInput(exhausted bool, now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskApproved, MergeApproval: true}, MergeConflict: MergeConflict{Open: true, Exhausted: exhausted, NextRetryAt: now.Add(time.Minute)}, Now: now}
}

func waitingAndQuestion(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, ClaimWaiting: []ProposalRef{{Tier: "requirement", ID: "req", Version: 1}}, Markers: Markers{CheckpointDecision: true}, Orders: []core.WorkOrder{{ID: "q", State: core.WorkOrderQueued}}, Now: now}
}

func questionAndVerify(now time.Time) Input {
	hold := now.Add(time.Hour)
	return Input{Task: core.Task{State: core.TaskRunning}, Markers: Markers{CheckpointDecision: true, VerificationDisposition: true, VerificationHoldDeadline: &hold}, Orders: []core.WorkOrder{{ID: "q", State: core.WorkOrderQueued, Stage: core.StageVerify}}, Now: now}
}

func unsatisfiableAndSuppressed(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued, UnsatisfiableTaskIDs: []string{"dep"}, RetrySuppressed: true}}, Now: now}
}

func timedOut(now time.Time) Input {
	return Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderTimedOut}}, Now: now}
}

func suppressedAndBlocked(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued, RetrySuppressed: true, BlockingTaskIDs: []string{"b"}}}, Blockers: []BlockerRef{{TaskID: "b", Title: "B"}}, Now: now}
}

func blockedClaimable(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "blocked", State: core.WorkOrderQueued, BlockingTaskIDs: []string{"b"}, Claimable: true}, {ID: "free", State: core.WorkOrderQueued, Claimable: true}}, Now: now}
}

func claimedAndQueued(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "c", State: core.WorkOrderClaimed}, {ID: "q", State: core.WorkOrderQueued, Claimable: true}}, Now: now}
}

func scheduled(now, later time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued, Claimable: true}}, Recovery: map[string]AutomaticRecovery{"o": {NextAt: &later}}, Now: now}
}

func recoveryExhausted(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued}}, Recovery: map[string]AutomaticRecovery{"o": {Exhausted: true}}, Now: now}
}

func verifyRefused(now time.Time) Input {
	return Input{Task: core.Task{ID: "t", State: core.TaskRunning, NextStage: core.StageImplement, SetupContract: core.Task{}.SetupContract}, Orders: []core.WorkOrder{{ID: "v", State: core.WorkOrderQueued, Stage: core.StageVerify, HeadSHA: "abc"}}, Now: now}
}

func allCaps() map[core.Capability]bool {
	return map[core.Capability]bool{core.CapabilityOperateGates: true, core.CapabilityRecoverWork: true, core.CapabilityRequestChanges: true, core.CapabilityConfirmDocuments: true}
}

func hasSecondary(na NextAction, kind string) bool {
	for _, item := range na.Details.Secondary {
		if item.Kind == kind {
			return true
		}
	}
	return false
}

func hasChoice(na NextAction, id string) bool {
	_, ok := choiceByID(na, id)
	return ok
}

func hasRefusal(na NextAction, id string) bool {
	for _, item := range na.Details.Refusals {
		if item.Choice == id && item.Text != "" {
			return true
		}
	}
	return false
}

func choiceByID(na NextAction, id string) (Choice, bool) {
	for _, choice := range na.Choices {
		if choice.ID == id {
			return choice, true
		}
	}
	return Choice{}, false
}
