package storetest

import (
	"context"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func setAutomaticRecoveryBound(t *testing.T, st store.Store, ctx context.Context, bound int) func() {
	t.Helper()
	updater, ok := st.(interface {
		WorkspaceConfig(context.Context) (config.VersionedDocument, error)
		UpdateWorkspaceConfig(context.Context, int64, *config.Config) (config.UpdateReceipt, error)
	})
	if !ok {
		t.Fatal("store does not expose workspace config")
	}
	current, err := updater.WorkspaceConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	previous := current.Document.Recovery
	window := previous.WindowText
	if window == "" {
		window = config.DefaultRecoveryWindowText
	}
	write := func(version int64, recovery config.RecoveryPolicy) {
		t.Helper()
		next := &config.Config{Workspace: current.Document.Workspace, MaxBounces: current.Document.MaxBounces, WorkOrderQueueTimeoutText: current.Document.WorkOrderQueueTimeoutText, Recovery: recovery}
		if _, err := updater.UpdateWorkspaceConfig(ctx, version, next); err != nil {
			t.Fatal(err)
		}
	}
	write(current.Version, config.RecoveryPolicy{AutomaticRecoveries: bound, WindowText: window})
	return func() {
		record, err := updater.WorkspaceConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		write(record.Version, previous)
	}
}
