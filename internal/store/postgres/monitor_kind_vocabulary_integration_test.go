package postgres

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// A SignalKind added in Go without extending the 040/069 kind CHECKs fails
// only at insert time in production (the lineaged_merge escape from PR #279).
// Exercise every Go-valid kind against the live constraints so the two
// vocabularies cannot drift apart again.
func TestMonitorKindVocabularyMatchesConstraintsIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	workspace := "postgres-kinds-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.Actor{ID: "operator", Role: core.ActorHuman})
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	kinds := []monitor.SignalKind{monitor.PostMergeFailure, monitor.DirectPush, monitor.ExternalPRMerge, monitor.LineagedMerge, monitor.Revert}
	for _, kind := range kinds {
		if !kind.Valid() {
			t.Fatalf("test vocabulary lists invalid kind %q; update the list alongside monitor.SignalKind", kind)
		}
		if _, _, err := st.Observe(ctx, monitor.Observation{
			WorkspaceID: workspace, Repository: "conveyor", Kind: kind,
			OccurrenceID: "vocab:" + string(kind), SourceURL: "https://example.test/" + string(kind),
			CommitSHA: "0000000000000000000000000000000000000000", ObservedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("observation kind %q rejected by the database constraint: %v", kind, err)
		}
	}
}
