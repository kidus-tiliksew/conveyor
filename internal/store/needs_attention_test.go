package store

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

func TestTaskNeedsAttentionRequiresOperator(t *testing.T) {
	if !TaskNeedsAttention(nextaction.NextAction{Actor: nextaction.ActorOperator, Kind: nextaction.KindParked}) {
		t.Fatal("operator parked task needs attention")
	}
	if TaskNeedsAttention(nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindWorking}) {
		t.Fatal("factory work counted as attention")
	}
	if TaskNeedsAttention(nextaction.NextAction{Actor: nextaction.ActorNone, Kind: nextaction.KindTerminal}) {
		t.Fatal("terminal task counted as attention")
	}
}

// Every actor=operator, judgment=false kind stays in caller attention and out
// of the inbox's decisions (req-operator-next-action AC-2.5, AC-2.7;
// component-next-action, Band membership).
func TestTaskIsInboxDecisionRequiresOperatorJudgment(t *testing.T) {
	for _, kind := range []string{nextaction.KindDispatchExhausted, nextaction.KindMergeConflict, nextaction.KindForgeFailure, nextaction.KindReviewRecovery, nextaction.KindStalled} {
		action := nextaction.NextAction{Actor: nextaction.ActorOperator, Kind: kind}
		if TaskIsInboxDecision(action) {
			t.Fatalf("%s without judgment counted as a decision", kind)
		}
		if !TaskNeedsAttention(action) {
			t.Fatalf("%s lost caller attention", kind)
		}
	}
	if !TaskIsInboxDecision(nextaction.NextAction{Actor: nextaction.ActorOperator, Kind: nextaction.KindParked, Judgment: true}) {
		t.Fatal("operator judgment is a decision")
	}
	for _, actor := range []string{nextaction.ActorAgent, nextaction.ActorFactory, nextaction.ActorDependency, nextaction.ActorNone} {
		if TaskIsInboxDecision(nextaction.NextAction{Actor: actor, Judgment: true}) {
			t.Fatalf("actor %s counted as a decision", actor)
		}
	}
}
