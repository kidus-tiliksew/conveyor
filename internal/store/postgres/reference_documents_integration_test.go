package postgres

import (
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestInsertEventRejectsTasklessEvent(t *testing.T) {
	err := insertEvent(t.Context(), nil, core.Event{Kind: "reference_document.created"})
	if err == nil || !strings.Contains(err.Error(), "use insertWorkspaceEvent") {
		t.Fatalf("task-less task-bound insertion error=%v", err)
	}
}

func TestPostgresReferenceDocumentConformance(t *testing.T) {
	st, ctx, _ := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	ctx = store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	storetest.RunReferenceDocumentConformance(t, st, ctx)
}

func TestReferenceDocumentVersionsCascadeWithDocument(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	documentID := "ref-" + core.NewTaskID()
	if _, _, err := st.CreateReferenceDocument(ctx,
		core.ReferenceDocument{ID: documentID, Name: "Cascade reference " + documentID},
		core.ReferenceDocumentVersion{Filename: "cascade.md", ContentType: "text/markdown", Content: "# Cascade"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM reference_documents WHERE workspace_id=$1 AND id=$2`, workspace, documentID); err != nil {
		t.Fatalf("delete reference document with version: %v", err)
	}
	var versions int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM reference_document_versions WHERE workspace_id=$1`, workspace).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Fatalf("reference versions after document delete=%d, want 0", versions)
	}
}
