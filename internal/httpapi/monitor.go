package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
)

// CreateMonitorTask is the only monitor-to-task bridge. It deliberately calls
// the same durable intake path as REST and MCP, preserving setup freezing,
// gates, title generation, idempotency, and triage enqueueing (design-monitor-drift).
func (s *Server) CreateMonitorTask(ctx context.Context, request monitor.TaskRequest) (monitor.IntakeResult, error) {
	var result taskCreateResult
	if request.ReuseExistingByKey {
		if existing, found, err := s.Store.GetTaskByIntakeKey(ctx, request.IntakeKey); err != nil {
			return monitor.IntakeResult{}, err
		} else if found {
			result.Task = existing
		}
	}
	if result.Task.ID == "" {
		var err error
		result, err = s.createTaskRecord(ctx, createTaskReq{
			Body: request.Body, Repo: request.Repository, Source: request.Source,
		}, request.IntakeKey, request.Source)
		if err != nil && request.ReuseExistingByKey {
			// A different attempt may have won the unique intake-key race with a
			// different task body. The first task remains the durable authority.
			if existing, found, getErr := s.Store.GetTaskByIntakeKey(ctx, request.IntakeKey); getErr != nil {
				return monitor.IntakeResult{}, getErr
			} else if found {
				result = taskCreateResult{Task: existing}
				err = nil
			}
		}
		if err != nil {
			return monitor.IntakeResult{}, err
		}
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
		status := taskCreateStatus(err)
		if errors.Is(err, monitor.ErrUnknownRequirementID) {
			status = http.StatusUnprocessableEntity
		}
		http.Error(w, fmt.Sprintf("observe signal: %v", err), status)
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
		Outcome       string `json:"outcome"`
		RequirementID string `json:"requirement_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	driftID, err := url.PathUnescape(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "drift id is not valid path encoding", http.StatusBadRequest)
		return
	}
	drift, err := s.Monitor.Resolve(r.Context(), driftID, request.Outcome, request.RequirementID)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, monitor.ErrRequirementIDMissing) || errors.Is(err, monitor.ErrUnknownRequirementID) ||
			errors.Is(err, monitor.ErrRequirementIDInvalid) || errors.Is(err, monitor.ErrRequirementIDNotAllowed) {
			status = http.StatusUnprocessableEntity
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, drift)
}
