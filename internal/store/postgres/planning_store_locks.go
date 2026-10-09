package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// WithPlanningSessionFinalization holds a workspace-scoped Postgres advisory
// lock across the active-state check, produced writes, transcript archival,
// and session finalization. AbandonPlanningSession takes the same lock, so only
// one terminal outcome can win across daemon instances.
func (s *Store) WithPlanningSessionFinalization(ctx context.Context, sessionID string, fn func(context.Context) error) error {
	return s.withPlanningSessionLock(ctx, sessionID, func(lockedCtx context.Context) error {
		session, err := scanPlanningSession(s.boundary.QueryRow(lockedCtx, planningSessionSelect+
			` WHERE workspace_id=$1 AND id=$2`, workspace(lockedCtx), sessionID), sessionID)
		if err != nil {
			return err
		}
		if session.Status != core.PlanningSessionActive {
			return fmt.Errorf(
				"planning session %s is %s and cannot produce an artifact", sessionID, session.Status)
		}
		return fn(lockedCtx)
	})
}

func (s *Store) WithPlanningSessionRun(ctx context.Context, sessionID string, fn func(context.Context) error) error {
	key := "conveyor:planning-session-run:" + workspace(ctx) + ":" + sessionID
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return translateDriverError(err)
	}
	conn := pooled.Hijack()
	var acquired bool
	if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext($1))", key).Scan(&acquired); err != nil {
		_ = conn.Close(ctx)
		return translateDriverError(err)
	}
	if !acquired {
		_ = conn.Close(ctx)
		return fmt.Errorf("%w: %s", store.ErrPlanningSessionRunConflict, sessionID)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock(hashtext($1))", key)
		_ = conn.Close(unlockCtx)
	}()
	return fn(ctx)
}

func (s *Store) withPlanningSessionLock(ctx context.Context, sessionID string, fn func(context.Context) error) error {
	key := "conveyor:planning-session:" + workspace(ctx) + ":" + sessionID
	return s.withDetachedAdvisoryLock(ctx, key, fn)
}
