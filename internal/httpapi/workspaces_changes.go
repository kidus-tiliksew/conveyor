package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// workspaceActivity is the data of one workspace change frame. TaskID is
// empty when the notice folded several tasks or came from the peer tail:
// some task of the workspace changed.
type workspaceActivity struct {
	TaskID string `json:"task_id,omitempty"`
}

// streamWorkspaceChanges serves GET /v1/workspaces/{workspace_id}/changes:
// ready once subscribed, then one activity frame per task notice in the
// workspace. It reads nothing from the store; the middleware's credential
// and workspace reads are its only statements (component-http-api,
// "Workspace change stream"; feature-quiet-idle-runtime, "Pushed task
// streams and `await_review`"; req-quiet-idle-runtime AC-1.2, AC-2.2).
func (s *Server) streamWorkspaceChanges(w http.ResponseWriter, r *http.Request) {
	workspace, _ := store.WorkspaceFromContext(r.Context())
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	wake := s.openWorkspaceWake(workspace)
	defer wake.close()
	if _, err := io.WriteString(w, "event: ready\ndata: {}\n\n"); err != nil {
		return
	}
	flusher.Flush()
	for {
		notice, ok := wake.wait(r.Context(), w, flusher)
		if !ok {
			return
		}
		if err := writeWorkspaceActivityFrame(w, notice.TaskID); err != nil {
			return
		}
		flusher.Flush()
	}
}

func writeWorkspaceActivityFrame(w io.Writer, taskID string) error {
	data, err := json.Marshal(workspaceActivity{TaskID: taskID})
	if err != nil {
		return err
	}
	frame := make([]byte, 0, len(data)+32)
	frame = append(frame, "event: activity\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	_, err = w.Write(frame)
	return err
}
