package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
)

// CreateMonitorTask is the only monitor-to-task bridge. It deliberately calls
// the same durable intake path as REST and MCP, preserving setup freezing,
// gates, title generation, idempotency, and triage enqueueing (spec §21.45).
func (s *Server) CreateMonitorTask(ctx context.Context, request monitor.TaskRequest) (monitor.IntakeResult, error) {
	result, err := s.createTaskRecord(ctx, createTaskReq{
		Body: request.Body, Repo: request.Repository, Source: request.Source,
	}, request.IntakeKey, request.Source)
	if err != nil {
		return monitor.IntakeResult{}, err
	}
	eventKind := "monitor.task_reused"
	if result.Created {
		eventKind = "monitor.task_created"
	}
	_ = s.Store.AppendEvent(ctx, core.Event{
		TaskID: result.Task.ID, Kind: eventKind,
		Payload: core.JSONPayload(map[string]any{
			"workspace_id": result.Task.Workspace, "repository": result.Task.Repo,
			"source": result.Task.Source, "intake_key": request.IntakeKey,
		}),
	})
	if request.Hints != nil {
		_ = s.Store.AppendEvent(ctx, core.Event{
			TaskID: result.Task.ID, Kind: "repository_hints.loaded",
			Payload: core.JSONPayload(map[string]any{
				"path": request.Hints.Path, "revision": request.Hints.Revision,
				"fingerprint": request.Hints.Fingerprint, "authority": "advisory_only",
			}),
		})
	}
	return monitor.IntakeResult{Task: result.Task, Created: result.Created}, nil
}

func (s *Server) getMonitorStatus(w http.ResponseWriter, r *http.Request) {
	if s.Monitor == nil {
		http.Error(w, "monitor is not configured", http.StatusNotImplemented)
		return
	}
	status, err := s.Monitor.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) observeMonitorSignal(w http.ResponseWriter, r *http.Request) {
	if s.Monitor == nil {
		http.Error(w, "monitor is not configured", http.StatusNotImplemented)
		return
	}
	var observation monitor.Observation
	if err := json.NewDecoder(r.Body).Decode(&observation); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	record, err := s.Monitor.Process(r.Context(), observation)
	if err != nil {
		http.Error(w, fmt.Sprintf("observe signal: %v", err), taskCreateStatus(err))
		return
	}
	writeJSON(w, http.StatusAccepted, record)
}

func (s *Server) resolveMonitorDrift(w http.ResponseWriter, r *http.Request) {
	if s.Monitor == nil {
		http.Error(w, "monitor is not configured", http.StatusNotImplemented)
		return
	}
	var request struct {
		Outcome string `json:"outcome"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	drift, err := s.Monitor.Resolve(r.Context(), chi.URLParam(r, "id"), request.Outcome)
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "requirement_id is missing") {
			status = http.StatusUnprocessableEntity
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, drift)
}
