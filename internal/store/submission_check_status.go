package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// Check-status ledger events (component-submission-merge, Public surfaces).
// Each one's payload names the head it was read at in head_sha, so an event
// of an older head never holds a new one.
const (
	EventCheckPending     = "merge.check_pending"
	EventCheckHold        = "merge.check_hold"
	EventCheckHoldCleared = "merge.check_hold_cleared"
	EventCheckOverride    = "merge.check_override"
)

// ErrCheckStatusHeadRequired refuses a check-status deadline with no head.
var ErrCheckStatusHeadRequired = errors.New("check-status deadline requires a head")

// CheckStatusEventKinds are the kinds FoldCheckStatus reads.
var CheckStatusEventKinds = []string{EventCheckPending, EventCheckHold, EventCheckHoldCleared}

// CheckStatusHead is the head a task's check status is read at: the approved
// head, or the reviewed head of a task approved before it recorded one.
func CheckStatusHead(task core.Task) string {
	if task.ApprovedHeadSHA != "" {
		return task.ApprovedHeadSHA
	}
	return task.ReviewedHeadSHA
}

type checkStatusPayload struct {
	HeadSHA    string                `json:"head_sha"`
	Reason     string                `json:"reason"`
	ReadFailed bool                  `json:"read_failed"`
	Failure    string                `json:"read_failure"`
	Remedy     string                `json:"remedy"`
	Checks     []nextaction.CheckRef `json:"checks"`
}

// FoldCheckStatus folds a task's check-status events in (at, id) order for one
// head. A hold stands from its latest merge.check_hold until a later
// merge.check_hold_cleared; a later hold of that head replaces it, so the
// standing hold names what the last deciding read named. Waiting is what the
// latest merge.check_pending named.
func FoldCheckStatus(events []core.Event, head string) nextaction.CheckStatus {
	var out nextaction.CheckStatus
	if head == "" {
		return out
	}
	for _, event := range events {
		switch event.Kind {
		case EventCheckPending, EventCheckHold, EventCheckHoldCleared:
		default:
			continue
		}
		var payload checkStatusPayload
		if json.Unmarshal(event.Payload, &payload) != nil || payload.HeadSHA != head {
			continue
		}
		switch event.Kind {
		case EventCheckHold:
			out.Hold = &nextaction.CheckHold{Reason: payload.Reason, Checks: payload.Checks, ReadFailure: payload.Failure, Remedy: payload.Remedy}
		case EventCheckHoldCleared:
			out.Hold = nil
		case EventCheckPending:
			out.Waiting, out.ReadFailure = payload.Checks, ""
			if payload.ReadFailed {
				out.ReadFailure = payload.Failure
			}
		}
	}
	return out
}

// HoldsSilenceFallback reports whether the standing hold takes the task out of
// the set that waits on GitHub: a failed or stuck check names the operator, so
// nothing about it is GitHub's to report (req-quiet-idle-runtime AC-3.8). A
// refused read holds no check and stays watched, so the read that follows the
// operator's grant still happens.
func HoldsSilenceFallback(status nextaction.CheckStatus) bool {
	return status.Hold != nil && status.Hold.Reason != nextaction.CheckHoldPermission
}

// ReadCheckStatuses folds the recorded check status of each task at its own
// head in one event read. A task with no check-status event is absent.
func ReadCheckStatuses(ctx context.Context, st NextActionInputStore, tasks []core.Task) (map[string]nextaction.CheckStatus, error) {
	out := map[string]nextaction.CheckStatus{}
	if len(tasks) == 0 {
		return out, nil
	}
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
	}
	events, err := st.ListCheckStatusEvents(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		if recorded := events[task.ID]; len(recorded) > 0 {
			out[task.ID] = FoldCheckStatus(recorded, CheckStatusHead(task))
		}
	}
	return out, nil
}

func checkStatusEventKind(kind string) bool {
	for _, candidate := range CheckStatusEventKinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

func (m *memory) ListCheckStatusEvents(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	out := map[string][]core.Event{}
	for _, id := range taskIDs {
		task, ok := m.tasks[id]
		if !ok || (scoped && workspace != "" && task.Workspace != workspace) {
			continue
		}
		for _, event := range m.sortedEventsLocked(id) {
			if checkStatusEventKind(event.Kind) {
				out[id] = append(out[id], event)
			}
		}
	}
	return out, nil
}

func (m *memory) RecordCheckStatusDeadline(ctx context.Context, id, head string, deadline time.Time, event core.Event) (bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return false, err
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return false, ErrCheckStatusHeadRequired
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return false, fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if task.CheckStatusDeadline != nil && task.CheckStatusDeadlineHead == head {
		return false, nil
	}
	at := deadline.UTC()
	task.CheckStatusDeadline, task.CheckStatusDeadlineHead = &at, head
	m.tasks[id] = task
	event.TaskID = id
	m.appendEventLocked(ctx, event)
	return true, nil
}

func (m *memory) ClearCheckStatusDeadline(ctx context.Context, id string) (bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return false, fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if task.CheckStatusDeadline == nil {
		return false, nil
	}
	task.CheckStatusDeadline = nil
	m.tasks[id] = task
	return true, nil
}
