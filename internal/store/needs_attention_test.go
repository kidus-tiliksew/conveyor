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
