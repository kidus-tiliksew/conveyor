package singlestore

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testsupport/mergelineagefixture"
)

// SingleStore twin of TestMonitorLineagesConveyorMergeAfterHeadMoved
// (component-submission-merge "Observed merges"; component-monitor-drift).
func TestMonitorLineagesConveyorMergeAfterHeadMovedSingleStoreIntegration(t *testing.T) {
	open := func(t *testing.T) store.Store { return integrationStore(t) }
	t.Run("head-moved", func(t *testing.T) { mergelineagefixture.RunHeadMoved(t, open) })
	t.Run("unapproved-head", func(t *testing.T) { mergelineagefixture.RunUnapprovedHead(t, open) })
	t.Run("stale-approval", func(t *testing.T) { mergelineagefixture.RunStaleApproval(t, open) })
}
