package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestBackendConflictSentinelsIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	task := phase61Task(workspace, core.NewTaskID(), core.TaskRunning, "")
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobRunning, StartedAt: time.Now().UTC()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); !errors.Is(err, store.ErrDispatchJobConflict) {
		t.Fatalf("duplicate job: %v", err)
	}
	doc := core.ReferenceDocument{ID: "ref-" + core.NewTaskID(), Name: "Backend contract"}
	version := core.ReferenceDocumentVersion{Filename: "contract.md", ContentType: "text/markdown", Content: "# Contract"}
	if _, _, err := st.CreateReferenceDocument(ctx, doc, version); err != nil {
		t.Fatal(err)
	}
	doc.ID = "ref-" + core.NewTaskID()
	doc.Name = "BACKEND CONTRACT"
	if _, _, err := st.CreateReferenceDocument(ctx, doc, version); !errors.Is(err, store.ErrReferenceDocumentNameConflict) {
		t.Fatalf("duplicate live name: %v", err)
	}
}

// TestUnmappedDriverFailuresLeaveAsStoreErrorsIntegration drives real
// unmapped PostgreSQL failures through public store methods: a foreign-key
// violation inside a transaction and a parameter the driver cannot encode on
// a direct read. Neither may expose a pgx or pgconn type, SQLSTATE, or driver
// message (component-persistence; DEC-38).
func TestUnmappedDriverFailuresLeaveAsStoreErrorsIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	parent := phase61Task(workspace, "unmapped-parent-"+core.NewTaskID(), core.TaskClosed, "")
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	other := workspace + "-other"
	otherCtx := store.WithWorkspace(context.Background(), other)
	if _, err := st.BootstrapWorkspaceConfig(otherCtx, &config.Config{Workspace: other, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	_, directErr := st.GetSystemDesignVersion(ctx, "design-"+core.NewTaskID(), 1<<40)
	for name, err := range map[string]error{
		"transaction foreign key": st.CreateTask(otherCtx, phase61Task(other, "unmapped-child-"+core.NewTaskID(), core.TaskClosed, parent.ID)),
		"direct read encoding":    directErr,
	} {
		if !errors.Is(err, store.ErrBackendOperation) {
			t.Fatalf("%s err=%v, want store.ErrBackendOperation", name, err)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) || carriesDriverError(err) {
			t.Fatalf("%s exposed a driver error: %#v", name, err)
		}
		for _, leaked := range []string{"SQLSTATE", "23503", "violates", "int4", "encode"} {
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("%s message %q leaks %q", name, err, leaked)
			}
		}
	}
	missing, err := st.GetRequirementVersion(ctx, "req-"+core.NewTaskID(), 1)
	if !errors.Is(err, store.ErrNotFound) || missing.Version != 0 {
		t.Fatalf("missing version err=%v", err)
	}
}
