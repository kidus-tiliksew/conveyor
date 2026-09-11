package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func ValidatePullRequestCloseActor(ctx context.Context) error {
	actor := ActorFromContext(ctx)
	if !utf8.ValidString(actor.ID) || !utf8.ValidString(string(actor.Role)) || strings.ContainsRune(actor.ID, 0) || strings.ContainsRune(string(actor.Role), 0) {
		return fmt.Errorf("invalid pull request close audit actor")
	}
	return nil
}

func ValidatePullRequestCloseIntent(p core.PullRequestClose, task core.Task) error {
	if p.WorkspaceID == "" || p.WorkspaceID != task.Workspace || p.TaskID != task.ID || task.State != core.TaskClosed || p.SuccessorID == "" || p.SuccessorID != task.SupersededBy || p.Branch != task.Branch || p.SuccessorBranch == "" || p.Repository == "" || p.Reason == "" || p.RestartingOperatorID == "" || p.ForgeAuthorClass != core.ForgeAuthorWorkspace || p.State != "queued" || p.Attempts != 0 || p.Number != 0 || p.URL != "" || p.ForgeErrorCategory != "" || p.Outcome != "" || p.LastError != "" {
		return fmt.Errorf("invalid pull request close intent for task %s", p.TaskID)
	}
	return nil
}

// ValidatePullRequestCloseUpdate is shared by the three persistence backends.
// Identity and supersession inputs are immutable; only bounded progress changes.
func ValidatePullRequestCloseUpdate(old, next core.PullRequestClose) error {
	if err := core.ValidatePullRequestCloseTransition(old.State, next.State); err != nil {
		return err
	}
	a, b := old, next
	a.State, b.State = "", ""
	a.Attempts, b.Attempts = 0, 0
	a.Number, b.Number = 0, 0
	a.URL, b.URL = "", ""
	a.ForgeErrorCategory, b.ForgeErrorCategory = "", ""
	a.Outcome, b.Outcome = "", ""
	a.LastError, b.LastError = "", ""
	if !reflect.DeepEqual(a, b) || next.Attempts < old.Attempts || next.Attempts > old.Attempts+1 || next.Attempts > core.PullRequestCloseMaxAttempts || (old.Number != 0 && (old.Number != next.Number || old.URL != next.URL)) {
		return fmt.Errorf("invalid pull request close progress")
	}
	if next.State == "closed" && (next.Number <= 0 || next.URL == "") {
		return fmt.Errorf("closed pull request requires forge confirmation")
	}
	if next.State == "failed" && next.ForgeErrorCategory == "" {
		return fmt.Errorf("failed closure requires error category")
	}
	return nil
}

func PullRequestCloseEvent(p core.PullRequestClose) core.Event {
	kind := "pull_request.close_retry"
	switch p.State {
	case "queued":
		kind = "pull_request.close_queued"
	case "closed":
		kind = "pull_request.closed"
	case "skipped":
		kind = "pull_request.close_skipped"
	case "failed":
		kind = "pull_request.close_failed"
	}
	return core.Event{TaskID: p.TaskID, Kind: kind, Payload: core.JSONPayload(p)}
}

func (m *memory) QueuePullRequestClose(ctx context.Context, p core.PullRequestClose) error {
	if err := ValidatePullRequestCloseActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, _ := WorkspaceFromContext(ctx)
	if ws != "" && ws != p.WorkspaceID {
		return ErrNotFound
	}
	task, ok := m.tasks[p.TaskID]
	if !ok {
		return ErrNotFound
	}
	if err := ValidatePullRequestCloseIntent(p, task); err != nil {
		return err
	}
	if m.pullRequestCloses == nil {
		m.pullRequestCloses = map[string]core.PullRequestClose{}
	}
	if _, ok := m.pullRequestCloses[p.TaskID]; ok {
		return nil
	}
	m.pullRequestCloses[p.TaskID] = p
	m.appendEventLocked(ctx, PullRequestCloseEvent(p))
	return nil
}
func (m *memory) GetPullRequestClose(ctx context.Context, id string) (core.PullRequestClose, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.pullRequestCloses[id]
	ws, _ := WorkspaceFromContext(ctx)
	if ok && ws != "" && p.WorkspaceID != ws {
		return core.PullRequestClose{}, false, nil
	}
	return p, ok, nil
}
func (m *memory) UpdatePullRequestClose(ctx context.Context, p core.PullRequestClose) error {
	if err := ValidatePullRequestCloseActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.pullRequestCloses[p.TaskID]
	if !ok {
		return ErrNotFound
	}
	ws, _ := WorkspaceFromContext(ctx)
	if ws != "" && old.WorkspaceID != ws {
		return ErrNotFound
	}
	if err := ValidatePullRequestCloseUpdate(old, p); err != nil {
		return err
	}
	m.pullRequestCloses[p.TaskID] = p
	m.appendEventLocked(ctx, PullRequestCloseEvent(p))
	return nil
}
