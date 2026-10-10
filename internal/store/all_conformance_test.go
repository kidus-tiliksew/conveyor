package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemoryConformance(t *testing.T) {
	storetest.RunAll(t, storetest.Factory{
		Capabilities: storetest.Capabilities{Identity: true, Membership: true, Tokens: true},
		New: func(t *testing.T, repos []config.Repo) storetest.Fixture {
			t.Helper()
			st := store.NewVolatileBackend()
			t.Cleanup(st.Close)
			workspace := "conformance-" + core.NewTaskID()
			ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor())
			cfg := &config.Config{Workspace: workspace, Repos: repos, Routing: config.Routing{Stages: map[string]config.StageRoute{
				"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour},
			}}}
			if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			return storetest.Fixture{Backend: st, Context: ctx, Workspace: workspace, Config: cfg, ArtifactRepairEvents: func(ctx context.Context) ([]core.Event, error) { return st.ListEvents(ctx, "") }, WorkspaceEvents: func(ctx context.Context, kindPrefix string) ([]core.Event, error) {
				// The volatile ledger keeps taskless events of every workspace
				// in one list. An event whose payload names another
				// workspace_id belongs to that workspace's ledger.
				ws, _ := store.WorkspaceFromContext(ctx)
				events, err := st.ListEvents(ctx, "")
				var out []core.Event
				for _, event := range events {
					var scope struct {
						WorkspaceID string `json:"workspace_id"`
					}
					if json.Unmarshal(event.Payload, &scope) == nil && scope.WorkspaceID != "" && scope.WorkspaceID != ws {
						continue
					}
					if strings.HasPrefix(event.Kind, kindPrefix) {
						out = append(out, event)
					}
				}
				return out, err
			}, SeedHistoricalLink: func(t *testing.T, _ context.Context, link core.LineageLink) {
				store.SeedLineageLinkForTest(t, st, link)
			}, SeedArtifact: func(t *testing.T, ctx context.Context, a core.Artifact, b []byte) {
				store.SeedArtifactMetadataForTest(t, st, ctx, a, b)
			}, SeedEvents: func(t *testing.T, ctx context.Context, base int64, events []core.Event) []core.Event {
				return store.SeedEventsForTest(t, st, ctx, base, events)
			}}
		},
	})
}
