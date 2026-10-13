package dispatch_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testsupport/mergelineagefixture"
)

// Monitor lineage of a Conveyor merge after the pull-request head moved
// (component-submission-merge "Observed merges"; component-monitor-drift).
func TestMonitorLineagesConveyorMergeAfterHeadMoved(t *testing.T) {
	mergelineagefixture.RunHeadMoved(t, memoryStore)
}

func TestMonitorKeepsUnapprovedHeadMergeExternal(t *testing.T) {
	mergelineagefixture.RunUnapprovedHead(t, memoryStore)
}

func TestMonitorKeepsStaleApprovalMergeExternal(t *testing.T) {
	mergelineagefixture.RunStaleApproval(t, memoryStore)
}

func TestMonitorLineagesConveyorMergeAfterHeadMovedPostgresIntegration(t *testing.T) {
	raw := disposablePostgresURL(t, "Merge lineage after head move")
	open := func(t *testing.T) store.Store { return postgresSchemaBackend(t, raw, "lineage_") }
	t.Run("head-moved", func(t *testing.T) { mergelineagefixture.RunHeadMoved(t, open) })
	t.Run("unapproved-head", func(t *testing.T) { mergelineagefixture.RunUnapprovedHead(t, open) })
	t.Run("stale-approval", func(t *testing.T) { mergelineagefixture.RunStaleApproval(t, open) })
}

func memoryStore(*testing.T) store.Store { return store.NewMemory() }
