package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const worktreeCleanupCompletedEvent = "worktree.cleanup_completed"

type worktreeCleanupStatus struct {
	Terminal  bool `json:"terminal"`
	Completed bool `json:"completed"`
}

type worktreeCleanupRecord struct {
	Repository      string   `json:"repository"`
	Branch          string   `json:"branch"`
	Worktree        string   `json:"worktree"`
	BranchResult    string   `json:"branch_result"`
	Path            string   `json:"path"`
	ProcessWarnings []string `json:"process_warnings,omitempty"`
}

type worktreeCleanupReceipt struct {
	Completed bool `json:"completed"`
	Recorded  bool `json:"recorded"`
}

func (s *Server) getWorktreeCleanupStatus(w http.ResponseWriter, r *http.Request) {
	task, authorized, err := s.authorizedWorktreeCleanupTask(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !authorized {
		http.NotFound(w, r)
		return
	}
	completed, err := s.Store.CountEvents(r.Context(), task.ID, worktreeCleanupCompletedEvent)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, worktreeCleanupStatus{Terminal: core.TaskTerminal(task.State), Completed: completed != 0})
}

func (s *Server) recordWorktreeCleanup(w http.ResponseWriter, r *http.Request) {
	var request worktreeCleanupRecord
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateWorktreeCleanupRecord(request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_worktree_cleanup", "message": err.Error()})
		return
	}
	task, authorized, err := s.authorizedWorktreeCleanupTask(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !authorized {
		http.NotFound(w, r)
		return
	}
	if request.Repository != task.Repo || request.Branch != task.Branch {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "worktree_cleanup_mismatch", "message": "cleanup repository and branch must match the task"})
		return
	}
	if !core.TaskTerminal(task.State) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "task_not_terminal", "message": "worktree cleanup requires a merged or closed task"})
		return
	}
	receipt := worktreeCleanupReceipt{Completed: true}
	err = s.Store.WithTaskSideEffectLock(r.Context(), task.ID, func(lockedCtx context.Context) error {
		current, getErr := s.Store.GetTask(lockedCtx, task.ID)
		if getErr != nil {
			return getErr
		}
		if !core.TaskTerminal(current.State) || current.Repo != request.Repository || current.Branch != request.Branch {
			return errWorktreeCleanupStateChanged
		}
		completed, countErr := s.Store.CountEvents(lockedCtx, task.ID, worktreeCleanupCompletedEvent)
		if countErr != nil {
			return countErr
		}
		if completed != 0 {
			return nil
		}
		if appendErr := s.Store.AppendEvent(lockedCtx, core.Event{
			TaskID: task.ID,
			Kind:   worktreeCleanupCompletedEvent,
			Payload: core.JSONPayload(map[string]any{
				"workspace": task.Workspace, "repository": request.Repository, "branch": request.Branch,
				"worktree": request.Worktree, "branch_result": request.BranchResult, "path": request.Path,
				"process_warnings": request.ProcessWarnings,
			}),
		}); appendErr != nil {
			return appendErr
		}
		receipt.Recorded = true
		return nil
	})
	if errors.Is(err, errWorktreeCleanupStateChanged) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "worktree_cleanup_state_changed", "message": "task cleanup state changed before completion could be recorded"})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

var errWorktreeCleanupStateChanged = errors.New("worktree cleanup state changed")

func validateWorktreeCleanupRecord(record worktreeCleanupRecord) error {
	switch record.Worktree {
	case "removed", "pruned", "skipped":
	default:
		return errors.New("cleanup worktree result must be removed, pruned, or skipped")
	}
	switch record.BranchResult {
	case "retained", "absent":
	default:
		return errors.New("cleanup branch result must be retained or absent")
	}
	if strings.TrimSpace(record.Path) == "" {
		return errors.New("cleanup path is required")
	}
	for _, warning := range record.ProcessWarnings {
		if strings.TrimSpace(warning) == "" {
			return errors.New("cleanup process warnings must not contain blank entries")
		}
	}
	return nil
}

func (s *Server) authorizedWorktreeCleanupTask(r *http.Request) (core.Task, bool, error) {
	task, err := s.Store.GetTask(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return core.Task{}, false, nil
		}
		return core.Task{}, false, err
	}
	actor := store.ActorFromContext(r.Context())
	if strings.TrimSpace(actor.ID) == "" || (actor.Role != core.ActorUser && actor.Role != core.ActorWorker) {
		return core.Task{}, false, nil
	}
	events, err := s.Store.ListEvents(r.Context(), task.ID)
	if err != nil {
		return core.Task{}, false, err
	}
	for _, event := range events {
		if event.Kind == "work_order.claimed" && event.ActorID == actor.ID && event.ActorRole == actor.Role {
			return task, true, nil
		}
	}
	return core.Task{}, false, nil
}
