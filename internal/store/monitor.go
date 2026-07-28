package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
)

func (m *memory) AuditTask(ctx context.Context, taskID, kind string, payload map[string]any) error {
	return m.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: kind, Payload: core.JSONPayload(payload)})
}

func (m *memory) AuditMonitor(ctx context.Context, kind string, payload map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return ErrWorkspaceRequired
	}
	m.nextMonitorActivityID++
	m.monitorActivity[workspace] = append(m.monitorActivity[workspace], monitor.Activity{
		ID: m.nextMonitorActivityID, WorkspaceID: workspace, Kind: kind,
		Payload: payload, At: time.Now().UTC(),
	})
	return nil
}

func monitorKey(workspace, identity string) string { return workspace + "\x00" + identity }

func (m *memory) Observe(ctx context.Context, observation monitor.Observation) (monitor.ObservationRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, observation.WorkspaceID)
	if workspace == "" || workspace != observation.WorkspaceID {
		return monitor.ObservationRecord{}, false, ErrWorkspaceRequired
	}
	key := monitorKey(workspace, observation.Identity())
	if current, ok := m.monitorObservations[key]; ok {
		current.DeduplicatedCount++
		current.UpdatedAt = observation.ObservedAt
		current.State = "deduplicated"
		m.monitorObservations[key] = current
		return current, false, nil
	}
	record := monitor.ObservationRecord{
		Observation: observation, State: "observed",
		CreatedAt: observation.ObservedAt, UpdatedAt: observation.ObservedAt,
	}
	m.monitorObservations[key] = record
	return record, true, nil
}

func (m *memory) LinkTask(ctx context.Context, identity, taskID, outcome string) (monitor.ObservationRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return monitor.ObservationRecord{}, ErrWorkspaceRequired
	}
	key := monitorKey(workspace, identity)
	record, ok := m.monitorObservations[key]
	if !ok {
		return monitor.ObservationRecord{}, fmt.Errorf("monitor observation %s not found", identity)
	}
	if record.TaskID != "" && record.TaskID != taskID {
		return monitor.ObservationRecord{}, fmt.Errorf("monitor observation %s already links task %s", identity, record.TaskID)
	}
	record.TaskID, record.TaskOutcome, record.State, record.UpdatedAt = taskID, outcome, "task_linked", time.Now().UTC()
	m.monitorObservations[key] = record
	return record, nil
}

func (m *memory) RecordDrift(ctx context.Context, drift monitor.Drift) (monitor.Drift, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, drift.WorkspaceID)
	if workspace == "" || workspace != drift.WorkspaceID {
		return monitor.Drift{}, false, ErrWorkspaceRequired
	}
	key := monitorKey(workspace, drift.ID)
	if current, ok := m.monitorDrift[key]; ok {
		return current, false, nil
	}
	m.monitorDrift[key] = drift
	return drift, true, nil
}

func (m *memory) ResolveDrift(ctx context.Context, id, outcome string) (monitor.Drift, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return monitor.Drift{}, ErrWorkspaceRequired
	}
	key := monitorKey(workspace, id)
	drift, ok := m.monitorDrift[key]
	if !ok {
		return monitor.Drift{}, fmt.Errorf("drift %s not found", id)
	}
	outcome = strings.TrimSpace(outcome)
	if outcome != "requirements_amended" && outcome != "conflict_resolved" && outcome != "change_reverted" {
		return monitor.Drift{}, fmt.Errorf("unsupported audited reconciliation outcome %q", outcome)
	}
	if drift.ResolvedAt.IsZero() {
		drift.Outcome, drift.ResolvedAt = outcome, time.Now().UTC()
		m.monitorDrift[key] = drift
	}
	return drift, nil
}

func (m *memory) MonitorStatus(ctx context.Context, enabled bool, now time.Time) (monitor.Status, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return monitor.Status{}, ErrWorkspaceRequired
	}
	status := monitor.Status{
		WorkspaceID: workspace, Enabled: enabled,
		LastSuccessfulAt:   m.monitorLastSuccess[workspace],
		CurrentError:       m.monitorError[workspace],
		ForgeErrorCategory: m.monitorErrorCategory[workspace],
		BackoffUntil:       m.monitorBackoff[workspace],
	}
	status.Activity = append([]monitor.Activity(nil), m.monitorActivity[workspace]...)
	prefix := workspace + "\x00"
	for key, record := range m.monitorObservations {
		if strings.HasPrefix(key, prefix) {
			status.Observations = append(status.Observations, record)
		}
	}
	for key, drift := range m.monitorDrift {
		if strings.HasPrefix(key, prefix) && drift.ResolvedAt.IsZero() {
			status.Drift = append(status.Drift, drift)
			status.DriftCount++
			age := now.Sub(drift.DetectedAt)
			if age > status.OldestDriftAge {
				status.OldestDriftAge = age
			}
		}
	}
	sort.Slice(status.Observations, func(i, j int) bool { return status.Observations[i].CreatedAt.Before(status.Observations[j].CreatedAt) })
	sort.Slice(status.Drift, func(i, j int) bool { return status.Drift[i].DetectedAt.Before(status.Drift[j].DetectedAt) })
	return status, nil
}

func (m *memory) RecordMonitorSuccess(ctx context.Context, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return ErrWorkspaceRequired
	}
	if m.monitorLastSuccess == nil {
		m.monitorLastSuccess, m.monitorError, m.monitorErrorCategory, m.monitorBackoff =
			map[string]time.Time{}, map[string]string{}, map[string]string{}, map[string]time.Time{}
	}
	m.monitorLastSuccess[workspace] = at
	delete(m.monitorError, workspace)
	delete(m.monitorErrorCategory, workspace)
	delete(m.monitorBackoff, workspace)
	return nil
}

func (m *memory) RecordMonitorFailure(ctx context.Context, category, detail string, backoffUntil time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return ErrWorkspaceRequired
	}
	if m.monitorError == nil {
		m.monitorLastSuccess, m.monitorError, m.monitorErrorCategory, m.monitorBackoff =
			map[string]time.Time{}, map[string]string{}, map[string]string{}, map[string]time.Time{}
	}
	m.monitorError[workspace], m.monitorErrorCategory[workspace], m.monitorBackoff[workspace] = detail, category, backoffUntil
	return nil
}
