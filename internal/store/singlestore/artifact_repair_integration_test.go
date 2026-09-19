package singlestore

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

func TestArtifactRepairDryRunDoesNotPersistLocksIntegration(t *testing.T) {
	st := integrationStore(t)
	ws := "repair-preview-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), ws), store.Actor{ID: "user:preview", Role: core.ActorUser})
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateArtifact(ctx, core.Artifact{Name: "legacy", ContentType: "application/binary"}, testimage.JPEG("preview"))
	if err != nil {
		t.Fatal(err)
	}
	var before, after int
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conveyor_locks`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RepairArtifactMetadata(ctx, store.ArtifactRepairRequest{ArtifactID: a.ID, ExpectedOldContentType: a.ContentType, NewContentType: "image/jpeg", RequestID: "preview", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conveyor_locks`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("dry-run persisted locks: %d -> %d", before, after)
	}
}
