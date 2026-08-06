package store_test

import (
	"context"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemoryPlanningBundleConformance(t *testing.T) {
	storetest.RunPlanningBundleConformance(t, func(t *testing.T) (store.Store, context.Context, string) {
		workspace := "bundle-" + core.NewTaskID()
		return store.NewMemory(), store.WithWorkspace(t.Context(), workspace), workspace
	})
}
