package singlestore

// DEC-39; component-persistence. These projections follow PostgreSQL on
// populated workspaces as well as the empty startup case.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) SetTaskAssigneeCommand(ctx context.Context, lease taskops.TaskLease, id, assigneeUserID string) (core.Task, error) {
	if !lease.ValidForCommand(id, taskops.SetAssigneeCommand) {
		return core.Task{}, fmt.Errorf("task assignment requires a valid taskops lease")
	}
	assigneeUserID = strings.TrimSpace(assigneeUserID)
	err := s.taskTx(ctx, id, func(tx *sql.Tx) error {
		t, err := getTaskRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if assigneeUserID != "" {
			var active bool
			var role core.WorkspaceRole
			err := documentRow(ctx, tx, `SELECT u.status='active',b.role FROM workspace_role_bindings b JOIN users u ON u.id=b.user_id WHERE b.workspace_id=? AND b.user_id=?`, documentWorkspace(ctx), assigneeUserID).Scan(&active, &role)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && !active) {
				return fmt.Errorf("assignee %s is not an active member of workspace %s", assigneeUserID, documentWorkspace(ctx))
			}
			if err != nil {
				return err
			}
			if !core.RoleAllows(role, core.CapabilityClaimWork) {
				return fmt.Errorf("assignee %s role %s lacks %s capability in workspace %s", assigneeUserID, role, core.CapabilityClaimWork, documentWorkspace(ctx))
			}
		}
		current := ""
		if t.Assignee != nil {
			current = t.Assignee.UserID
		}
		if current == assigneeUserID {
			return nil
		}
		if err := taskWrite(ctx, tx, id, map[string]any{"assignee_user_id": nullString(assigneeUserID)}); err != nil {
			return err
		}
		kind := "task.assignee.set"
		if assigneeUserID == "" {
			kind = "task.assignee.cleared"
		}
		return taskEvent(ctx, tx, core.Event{TaskID: id, Kind: kind, Payload: core.JSONPayload(map[string]any{"assignee_user_id": assigneeUserID})})
	})
	if err != nil {
		return core.Task{}, err
	}
	return s.GetTask(ctx, id)
}

func (s *Store) AttachSubmissionGovernance(ctx context.Context, taskID, repository string, changedPaths []string, attribution store.SubmissionGovernanceAttribution) ([]core.TaskDesignContext, error) {
	attached := []core.TaskDesignContext{}
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockTaskOperation(ctx, tx, documentWorkspace(ctx), taskID); err != nil {
			return err
		}
		t, err := getTaskRow(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if core.TaskTerminal(t.State) {
			return store.ErrTaskTerminal
		}
		events, err := documentTaskEvents(ctx, tx, taskID)
		if err != nil {
			return err
		}
		_, active := store.ActiveTaskContextReferences(events)
		rows, err := documentRows(ctx, tx, `SELECT d.id,d.title,d.category,v.version,v.content,v.governs FROM system_designs d JOIN system_design_versions v ON v.workspace_id=d.workspace_id AND v.document_id=d.id AND v.version=d.current_version WHERE d.workspace_id=? ORDER BY d.id`, documentWorkspace(ctx))
		if err != nil {
			return err
		}
		designs := []core.GovernanceDesignContext{}
		for rows.Next() {
			var d core.GovernanceDesignContext
			var governs []byte
			if err = rows.Scan(&d.ID, &d.Title, &d.Category, &d.Version, &d.Content, &governs); err != nil {
				rows.Close()
				return err
			}
			if err = json.Unmarshal(governs, &d.Governs); err != nil {
				rows.Close()
				return err
			}
			designs = append(designs, d)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, match := range core.ResolveGovernedDesigns(designs, repository, changedPaths) {
			if active[match.Design.ID] > 0 {
				continue
			}
			if err = taskEvent(ctx, tx, core.Event{TaskID: taskID, Kind: store.TaskContextDesignAdded, At: time.Now().UTC(), Payload: core.JSONPayload(map[string]any{"id": match.Design.ID, "version": match.Design.Version, "source": "submission_diff", "work_order_id": attribution.WorkOrderID, "session_id": attribution.SessionID, "matching_paths": match.MatchingPaths})}); err != nil {
				return err
			}
			active[match.Design.ID] = match.Design.Version
			attached = append(attached, core.TaskDesignContext{ID: match.Design.ID, Title: match.Design.Title, Version: match.Design.Version})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("attach submission governance: %w", err)
	}
	return attached, nil
}

func (s *Store) ListCallerAttentionTaskPage(ctx context.Context, q store.CallerAttentionQuery) (store.TaskPage, error) {
	if err := q.Validate(); err != nil {
		return store.TaskPage{}, err
	}
	tasks, err := s.ListTasksFiltered(ctx, store.TaskFilter{Assignee: q.UserID})
	if err != nil {
		return store.TaskPage{}, err
	}
	items, err := s.ListPendingProposals(ctx)
	if err != nil {
		return store.TaskPage{}, err
	}
	origins, pending := map[string]bool{}, map[string]bool{}
	for _, p := range items {
		if p.OriginType == "task" {
			origins[p.OriginID] = true
			if p.Tier == "task_context" {
				pending[p.OriginID] = true
			}
		}
	}
	ids := []string{}
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	if len(ids) == 0 {
		return store.TaskPage{Tasks: []core.Task{}}, nil
	}
	markers, err := s.ListActivityMarkersForTasks(ctx, ids)
	if err != nil {
		return store.TaskPage{}, err
	}
	byID := map[string]store.ActivityMarker{}
	for _, m := range markers {
		byID[m.TaskID] = m
	}
	matches := []core.Task{}
	for _, t := range tasks {
		if core.TaskTerminal(t.State) {
			continue
		}
		var parent bool
		if err = documentRow(ctx, s.db, `SELECT EXISTS(SELECT 1 FROM tasks WHERE workspace_id=? AND parent_task_id=?)`, documentWorkspace(ctx), t.ID).Scan(&parent); err != nil {
			return store.TaskPage{}, err
		}
		if parent {
			continue
		}
		authority := false
		if origins[t.ID] {
			if err = documentRow(ctx, s.db, `SELECT EXISTS(SELECT 1 FROM work_orders WHERE workspace_id=? AND task_id=? AND ((stage='implement' AND state='submitted') OR (stage='review' AND state IN ('queued','claimed','submitted'))))`, documentWorkspace(ctx), t.ID).Scan(&authority); err != nil {
				return store.TaskPage{}, err
			}
		}
		if store.TaskNeedsAttention(t, byID[t.ID], authority, pending[t.ID]) {
			matches = append(matches, t)
		}
	}
	page := store.TaskPage{Tasks: []core.Task{}, Total: len(matches)}
	if q.Offset < len(matches) {
		end := min(len(matches), q.Offset+q.Limit)
		page.Tasks = matches[q.Offset:end]
	}
	return page, nil
}

func (s *Store) ListCheckpointContextCandidates(ctx context.Context, requirementID string) ([]store.CheckpointContextCandidate, error) {
	rows, err := documentRows(ctx, s.db, `SELECT t.id,t.title,t.state FROM tasks t JOIN work_orders w ON w.workspace_id=t.workspace_id AND w.task_id=t.id WHERE t.workspace_id=? AND t.state NOT IN ('merged','closed') AND w.state='queued' AND w.last_attempt_outcome='released' AND w.last_failure_message='operator checkpoint reached' AND NOT EXISTS(SELECT 1 FROM work_orders newer WHERE newer.workspace_id=w.workspace_id AND newer.task_id=w.task_id AND (newer.created_at>w.created_at OR (newer.created_at=w.created_at AND newer.id>w.id))) ORDER BY t.id`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	out := []store.CheckpointContextCandidate{}
	for rows.Next() {
		var c store.CheckpointContextCandidate
		if err = rows.Scan(&c.ID, &c.Title, &c.State); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := []store.CheckpointContextCandidate{}
	for _, c := range out {
		events, err := documentTaskEvents(ctx, s.db, c.ID)
		if err != nil {
			return nil, err
		}
		requirements, _ := store.ActiveTaskContextReferences(events)
		if !requirements[requirementID] {
			result = append(result, c)
		}
	}
	return result, nil
}

func (s *Store) ReconcileBlueprintClosures(ctx context.Context) (int, error) {
	rows, err := documentRows(ctx, s.db, `SELECT p.id FROM tasks p WHERE p.workspace_id=? AND p.state='queued' AND EXISTS(SELECT 1 FROM tasks c WHERE c.workspace_id=p.workspace_id AND c.parent_task_id=p.id) AND NOT EXISTS(SELECT 1 FROM tasks c WHERE c.workspace_id=p.workspace_id AND c.parent_task_id=p.id AND c.state NOT IN ('merged','closed')) ORDER BY p.created_at,p.id`, documentWorkspace(ctx))
	if err != nil {
		return 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		closed := false
		err = s.withTx(ctx, func(tx *sql.Tx) error { var e error; closed, e = s.closeBlueprintParentTx(ctx, tx, id); return e })
		if err != nil {
			return n, err
		}
		if closed {
			n++
		}
	}
	return n, nil
}

func (s *Store) ReconcileQueuedTasks(ctx context.Context) (int, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockKey(ctx, tx, "queue-reconcile:"+ws); err != nil {
			return err
		}
		rows, err := documentRows(ctx, tx, `SELECT t.id FROM tasks t WHERE t.workspace_id=? AND t.state='queued' AND NOT(t.parent_task_id IS NULL AND t.next_stage='implement' AND EXISTS(SELECT 1 FROM tasks c WHERE c.workspace_id=t.workspace_id AND c.parent_task_id=t.id)) ORDER BY t.created_at,t.id`, ws)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			job, err := logqueue.Load(s2log.WithTx(ctx, tx), s.log, ws, logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), id))
			if err != nil {
				return err
			}
			if job.Active() {
				continue
			}
			inserted, err := s.enqueueTaskTx(ctx, tx, id, ws)
			if err != nil {
				return err
			}
			if !inserted {
				continue
			}
			if err = taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "dispatch.reconciled", Payload: core.JSONPayload(map[string]string{"reason": "missing durable queue job"})}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return n, err
	}
	tasks, err := s.ListTasksFiltered(ctx, store.TaskFilter{States: []core.TaskState{core.TaskRunning}})
	if err != nil {
		return n, err
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) {
			return tasks[i].ID < tasks[j].ID
		}
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})
	for _, t := range tasks {
		job, err := logqueue.Load(ctx, s.log, ws, logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), t.ID))
		if err != nil {
			return n, err
		}
		if job.State == logqueue.StateDiscarded && job.Attempt >= job.MaxAttempts {
			_, err = taskops.New(s).Perform(ctx, t.ID, taskops.Command{Kind: core.TaskDispatchFailFinal, RecoveryStage: t.NextStage, ProjectStages: true, FailureMessage: "the queue discarded the dispatch job after its final attempt", Attempt: job.Attempt, MaxAttempts: job.MaxAttempts})
			if err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
