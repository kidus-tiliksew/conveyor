package store_test

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemoryTaskContextQueueRepinConformance(t *testing.T) {
	workspace := "memory-repin"
	cfg := &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Timeout: time.Hour},
			"review":    {Execution: config.ExecutionMCP, Timeout: time.Hour},
		}},
	}
	storetest.RunTaskContextQueueRepinConformance(t, store.NewMemoryWithConfig(cfg), store.WithWorkspace(t.Context(), workspace), cfg)
}
