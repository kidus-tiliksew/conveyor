package store_test

import (
	"context"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemorySystemDesignDriftConformance(t *testing.T) {
	storetest.RunSystemDesignDriftConformance(t, func(t *testing.T) (store.Store, context.Context, string) {
		workspace := "memory-drift-" + core.NewTaskID()
		return store.NewMemory(), store.WithActor(store.WithWorkspace(t.Context(), workspace), store.Actor{ID: "operator", Role: core.ActorHuman}), workspace
	})
}
