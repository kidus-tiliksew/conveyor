package store

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// ErrPlanningSessionRunConflict is stable transport-facing classification for
// a second message submitted while one planning run still owns the session.
var ErrPlanningSessionRunConflict = errors.New("planning session run already in progress")

func (m *memory) WithPlanningSessionFinalization(ctx context.Context, sessionID string, fn func(context.Context) error) error {
	return m.withPlanningSessionLock(ctx, sessionID, func(lockedCtx context.Context) error {
		m.mu.RLock()
		session, ok := m.planningSessions[memoryScopedKey{
			workspace: workspaceOrDefault(lockedCtx, ""),
			id:        sessionID,
		}]
		m.mu.RUnlock()
		if !ok {
			return fmt.Errorf("planning session %s not found", sessionID)
		}
		if session.Status != core.PlanningSessionActive {
			return fmt.Errorf(
				"planning session %s is %s and cannot produce an artifact", sessionID, session.Status)
		}
		return fn(lockedCtx)
	})
}

func (m *memory) WithPlanningSessionRun(ctx context.Context, sessionID string, fn func(context.Context) error) error {
	workspace, _ := WorkspaceFromContext(ctx)
	value, _ := m.taskLocks.LoadOrStore("planning-session-run/"+workspace+"/"+sessionID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	if !lock.TryLock() {
		return fmt.Errorf("%w: %s", ErrPlanningSessionRunConflict, sessionID)
	}
	defer lock.Unlock()
	return fn(ctx)
}

func (m *memory) withPlanningSessionLock(ctx context.Context, sessionID string, fn func(context.Context) error) error {
	workspace, _ := WorkspaceFromContext(ctx)
	value, _ := m.taskLocks.LoadOrStore("planning-session/"+workspace+"/"+sessionID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	return fn(ctx)
}
