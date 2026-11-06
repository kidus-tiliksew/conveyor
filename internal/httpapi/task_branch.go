package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) attachTaskBranch(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Branch string `json:"branch"`
	}
	if err := decodeBoundedJSON(w, r, taskRouteBodyLimit, &request); err != nil {
		writeBodyDecodeError(w, err)
		return
	}
	task, err := s.Store.AttachTaskBranch(r.Context(), chi.URLParam(r, "id"), request.Branch)
	if err != nil {
		writeTaskBranchAttachError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func writeTaskBranchAttachError(w http.ResponseWriter, r *http.Request, err error) {
	code, status, otherTaskID := classifyTaskBranchAttachError(err)
	if status == http.StatusNotFound {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if code == "" {
		writeStoreFailure(w, r, err)
		return
	}
	body := map[string]string{"error": code, "message": err.Error()}
	if otherTaskID != "" {
		body["other_task_id"] = otherTaskID
	}
	writeJSON(w, status, body)
}

func classifyTaskBranchAttachError(err error) (code string, status int, otherTaskID string) {
	var inUse *store.BranchInUseError
	switch {
	case errors.Is(err, store.ErrInvalidBranch):
		return "invalid_branch", http.StatusBadRequest, ""
	case errors.Is(err, store.ErrTaskTerminal):
		return "task_terminal", http.StatusConflict, ""
	case errors.Is(err, store.ErrWorkOrderClaimed):
		return "work_order_claimed", http.StatusConflict, ""
	case errors.Is(err, store.ErrPullRequestRecorded):
		return "pull_request_recorded", http.StatusConflict, ""
	case errors.Is(err, store.ErrBranchNotAttachable):
		return "branch_not_attachable", http.StatusConflict, ""
	case errors.As(err, &inUse):
		return "branch_in_use", http.StatusConflict, inUse.OtherTaskID
	case errors.Is(err, store.ErrTaskBranchConflict):
		return "branch_in_use", http.StatusConflict, ""
	case errors.Is(err, store.ErrNotFound):
		return "", http.StatusNotFound, ""
	default:
		return "", 0, ""
	}
}

func taskBranchAttachMCPError(err error) error {
	code, status, otherTaskID := classifyTaskBranchAttachError(err)
	if code == "" {
		return err
	}
	if status == http.StatusNotFound {
		return err
	}
	if otherTaskID != "" {
		return fmt.Errorf("branch_in_use: branch already belongs to task %s", otherTaskID)
	}
	return fmt.Errorf("%s: %s", code, err.Error())
}
