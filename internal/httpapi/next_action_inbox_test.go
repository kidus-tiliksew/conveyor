package httpapi

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

func TestInboxFoldsDocumentProposalTasks(t *testing.T) {
	task := core.Task{ID: "folded", State: core.TaskRunning}
	projected := map[string]nextaction.NextAction{
		task.ID: {Actor: nextaction.ActorOperator, Kind: nextaction.KindDocumentProposal, Judgment: true},
	}
	items := inboxTaskItems([]core.Task{task}, projected, map[string]time.Time{}, map[string]bool{}, map[string]bool{task.ID: true})
	if len(items) != 0 {
		t.Fatalf("folded task still listed: %+v", items)
	}
}
