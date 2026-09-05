package postgres

import (
	"errors"
	"testing"
	"time"

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
