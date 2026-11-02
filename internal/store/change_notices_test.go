package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestVolatileStagedInstallPublishesOnlyWhenInstalled: attachment intake
// stages its rows on a copy, so its task notices are published only when the
// copy is installed, and a failed intake publishes none
// (component-persistence, Change notices).
func TestVolatileStagedInstallPublishesOnlyWhenInstalled(t *testing.T) {
	st := NewVolatileBackend()
	ctx := WithActor(WithWorkspace(context.Background(), "ws"), SystemActor())
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: "ws", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, CreatedAt: time.Now().UTC()}
	notices := st.Changes().SubscribeTask("ws", id)
	defer notices.Close()
	bad := ArtifactUpload{Name: "bad.png", ContentType: "image/png", Content: []byte("not a png")}
	if err := st.CreateTaskWithAttachments(ctx, task, nil, TaskContextInput{}, []ArtifactUpload{bad}); err == nil {
		t.Fatal("invalid attachment accepted")
	}
	select {
	case n := <-notices.C():
		t.Fatalf("failed intake published %+v", n)
	default:
	}
	good := ArtifactUpload{Name: "notes.txt", ContentType: "text/plain", Content: []byte("notes")}
	if err := st.CreateTaskWithAttachments(ctx, task, nil, TaskContextInput{}, []ArtifactUpload{good}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-notices.C():
		if n.Kind != changes.Task || n.TaskID != id {
			t.Fatalf("notice=%+v", n)
		}
	default:
		t.Fatal("installed intake published no task notice")
	}
	if _, err := st.GetTask(ctx, id); errors.Is(err, ErrNotFound) {
		t.Fatal("task not installed")
	}
}
