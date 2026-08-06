package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type taskContextReadErrorStore struct{ store.Store }

func (s taskContextReadErrorStore) ListEvents(context.Context, string) ([]core.Event, error) {
	return nil, fmt.Errorf("context read unavailable")
}

type intakeRaceContextReadErrorStore struct {
	store.Store
	intakeLookups int
}

func (s *intakeRaceContextReadErrorStore) GetTaskByIntakeKey(ctx context.Context, key string) (core.Task, bool, error) {
	s.intakeLookups++
	if s.intakeLookups == 1 {
		return core.Task{}, false, nil
	}
	return s.Store.GetTaskByIntakeKey(ctx, key)
}

func (s *intakeRaceContextReadErrorStore) CreateTaskWithDependenciesAndContext(context.Context, core.Task, []string, store.TaskContextInput) error {
	return fmt.Errorf("duplicate intake key")
}

func (s *intakeRaceContextReadErrorStore) ListEvents(context.Context, string) ([]core.Event, error) {
	return nil, fmt.Errorf("context read unavailable")
}

func TestTaskIntakeRetryUsesCreateTimeContextAfterLaterEdits(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	for _, id := range []string{"req-original", "req-later"} {
		requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, core.RequirementVersion{
			Content: id, Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: id}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(st)
	server.Workspace, server.Repos = "demo", []string{"api"}
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Stable contextual task", nil }
	req := createTaskReq{Body: "same request", Repo: "api", RequirementIDs: []string{"req-original"}}
	first, err := server.createTaskRecord(ctx, req, "stable-context", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpdateTaskContext(ctx, first.Task.ID, store.TaskContextChange{
		Add:    store.TaskContextInput{RequirementIDs: []string{"req-later"}},
		Remove: store.TaskContextInput{RequirementIDs: []string{"req-original"}},
	}); err != nil {
		t.Fatal(err)
	}
	retry, err := server.createTaskRecord(ctx, req, "stable-context", "mcp")
	if err != nil || retry.Created || retry.Task.ID != first.Task.ID {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	if len(retry.Task.Context.Requirements) != 1 || retry.Task.Context.Requirements[0].ID != "req-later" {
		t.Fatalf("retry response did not retain live context: %+v", retry.Task.Context)
	}
	liveContextRequest := req
	liveContextRequest.RequirementIDs = []string{"req-later"}
	if _, err = server.createTaskRecord(ctx, liveContextRequest, "stable-context", "mcp"); err == nil || !strings.Contains(err.Error(), "different task") {
		t.Fatalf("live context incorrectly replaced intake authority: %v", err)
	}
}

func TestTaskIntakeRetriesPropagateContextReadErrors(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	server := NewServer(st)
	server.Workspace, server.Repos = "demo", []string{"api"}
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Retry context", nil }
	req := createTaskReq{Body: "same request", Repo: "api"}
	if _, err := server.createTaskRecord(ctx, req, "context-error", "mcp"); err != nil {
		t.Fatal(err)
	}

	server.Store = taskContextReadErrorStore{Store: st}
	if _, err := server.createTaskRecord(ctx, req, "context-error", "mcp"); err == nil || !strings.Contains(err.Error(), "context read unavailable") {
		t.Fatalf("normal retry context error=%v", err)
	}

	server.Store = &intakeRaceContextReadErrorStore{Store: st}
	if _, err := server.createTaskRecord(ctx, req, "context-error", "mcp"); err == nil || !strings.Contains(err.Error(), "context read unavailable") {
		t.Fatalf("concurrent retry context error=%v", err)
	}
}
