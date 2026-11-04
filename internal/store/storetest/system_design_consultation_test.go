package storetest

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestMemorySystemDesignConsultation runs the shared read-key cases against the
// volatile backend in this package. TestMemoryConformance runs them too, but
// its CI job is not verbose; the integration job that tests this package names
// each case in its log (DEC-72). This fixture has no event seed, so the
// historical-event case runs only under TestMemoryConformance.
func TestMemorySystemDesignConsultation(t *testing.T) {
	backend := store.NewVolatileBackend()
	t.Cleanup(backend.Close)
	workspace := "consultation-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor())
	if _, err := backend.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	RunSystemDesignConsultationConformance(t, Fixture{Backend: backend, Context: ctx, Workspace: workspace})
}
