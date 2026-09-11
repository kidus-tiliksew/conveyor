package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// StartOverTaskCommand commits cancellation, dismissals, successor context and
// dispatch intent together (req-task-lifecycle-and-queue REQ-7).
func (s *Store) StartOverTaskCommand(ctx context.Context, lease taskops.TaskLease, r core.TaskStartOverRequest) (core.TaskStartOverResult, error) {
	var result core.TaskStartOverResult
	if !lease.ValidForCommand(r.TaskID, string(core.TaskStartOver)) {
		return result, fmt.Errorf("task start over requires a valid taskops lease")
	}
	if err := r.Validate(); err != nil {
		return result, err
	}
	var noteErr error
	ctx, noteErr = store.WithDocumentDismissalNote(ctx, r.Note)
	if noteErr != nil {
		return result, noteErr
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ws := documentWorkspace(ctx)
		// Match dependency writers: task operation before the workspace graph lock.
		if err := s.lockTaskOperation(ctx, tx, ws, r.TaskID); err != nil {
			return err
		}
		if err := lockDependencyEdgesTx(ctx, tx, ws); err != nil {
			return err
		}
		if err := lockKey(ctx, tx, "document-corpus:"+ws); err != nil {
			return err
		}
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE workspace_id=? AND id=? FOR UPDATE`, ws, r.TaskID).Scan(&locked); err != nil {
			return notFound(err, "task %s", r.TaskID)
		}
		old, err := getTaskRow(ctx, tx, r.TaskID)
		if err != nil {
			return err
		}
		var successorID, reason, note string
		err = tx.QueryRowContext(ctx, `SELECT successor_id,reason,note FROM task_start_overs WHERE workspace_id=? AND task_id=? AND request_id=?`, ws, r.TaskID, r.RequestID).Scan(&successorID, &reason, &note)
		if err == nil {
			if reason != r.Reason || note != r.Note {
				return store.ErrStartOverRequestConflict
			}
			successor, err := getTaskRow(ctx, tx, successorID)
			if err != nil {
				return err
			}
			result = core.TaskStartOverResult{Task: old, Successor: successor}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if core.TaskTerminal(old.State) {
			return store.ErrTaskTerminal
		}
		if _, err := core.TransitionTask(old.State, core.TaskStartOver); err != nil {
			return err
		}
		type proposal struct {
			id      string
			version int
			design  bool
		}
		var proposals []proposal
		for _, design := range []bool{false, true} {
			query := `SELECT v.requirement_id,v.version FROM requirement_versions v WHERE v.workspace_id=? AND v.origin_task_id=? AND v.origin='implementation' AND NOT v.confirmed AND NOT v.retired ORDER BY v.requirement_id,v.version`
			if design {
				query = `SELECT v.document_id,v.version FROM system_design_versions v WHERE v.workspace_id=? AND v.origin_task_id=? AND v.origin='implementation_deliberation' AND NOT v.confirmed AND NOT v.dismissed ORDER BY v.document_id,v.version`
			}
			rows, err := tx.QueryContext(ctx, query, ws, r.TaskID)
			if err != nil {
				return err
			}
			for rows.Next() {
				p := proposal{design: design}
				if err := rows.Scan(&p.id, &p.version); err != nil {
					rows.Close()
					return err
				}
				proposals = append(proposals, p)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		if len(proposals) > 0 && !r.CanConfirmDocuments {
			return store.ErrStartOverConfirmDocuments
		}
		rows, err := tx.QueryContext(ctx, `SELECT kind,payload_json FROM events WHERE workspace_id=? AND task_id=? ORDER BY id`, ws, r.TaskID)
		if err != nil {
			return err
		}
		var events []core.Event
		for rows.Next() {
			var event core.Event
			if err := rows.Scan(&event.Kind, &event.Payload); err != nil {
				rows.Close()
				return err
			}
			events = append(events, event)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		requirements, pinned := store.ActiveTaskContextReferences(events)
		attached := store.TaskContextInput{}
		for id := range requirements {
			attached.RequirementIDs = append(attached.RequirementIDs, id)
		}
		for id := range pinned {
			attached.DesignIDs = append(attached.DesignIDs, id)
		}
		var dependencies []string
		rows, err = tx.QueryContext(ctx, `SELECT d.id FROM task_dependencies e JOIN tasks d ON d.workspace_id=e.workspace_id AND d.id=e.depends_on_task_id WHERE e.workspace_id=? AND e.task_id=? AND d.state NOT IN ('merged','closed') ORDER BY d.id`, ws, r.TaskID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			dependencies = append(dependencies, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		var plan string
		var version int
		err = tx.QueryRowContext(ctx, `SELECT s.version,s.content FROM task_specs s JOIN tasks t ON t.id=s.task_id WHERE t.workspace_id=? AND s.task_id=? AND s.approved ORDER BY s.version DESC LIMIT 1`, ws, r.TaskID).Scan(&version, &plan)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		next := store.StartOverSuccessor(old, r)
		intervention := core.Intervention{TaskID: old.ID, Action: core.InterventionCancel, ReasonCode: r.Reason, Comment: r.Note, At: next.CreatedAt}
		if err := s.cancelTaskTx(ctx, tx, normalizeIntervention(ctx, intervention)); err != nil {
			return err
		}
		for _, p := range proposals {
			var err error
			if p.design {
				_, _, err = dismissSystemDesignVersionTx(ctx, tx, p.id, p.version)
			} else {
				_, _, err = dismissRequirementVersionTx(ctx, tx, p.id, p.version)
			}
			if err != nil && !store.StartOverDismissalDecided(err) {
				return err
			}
		}
		if err := s.createTaskTx(ctx, tx, next, dependencies, attached, pinned); err != nil {
			return err
		}
		if version > 0 {
			content := store.StartOverPlanContent(old.ID, version, plan)
			artifact, err := prepareArtifact(ctx, core.Artifact{Name: "previous-approved-plan.md", ContentType: "text/markdown", TaskID: next.ID, Role: core.ArtifactRoleTaskContext, CreatedAt: next.CreatedAt}, content)
			if err != nil {
				return err
			}
			if err := insertArtifactTx(ctx, tx, artifact, content); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET superseded_by=? WHERE workspace_id=? AND id=?`, next.ID, ws, old.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_start_overs(workspace_id,task_id,request_id,successor_id,reason,note,created_at) VALUES(?,?,?,?,?,?,?)`, ws, old.ID, r.RequestID, next.ID, r.Reason, r.Note, next.CreatedAt); err != nil {
			return err
		}
		if err := taskEvent(ctx, tx, core.Event{TaskID: old.ID, Kind: "task.started_over", At: next.CreatedAt, Payload: core.JSONPayload(map[string]any{"request_id": r.RequestID, "reason": r.Reason, "note": r.Note, "successor_id": next.ID})}); err != nil {
			return err
		}
		old.State = core.TaskClosed
		old.NextStage = ""
		old.RecoveryStage = ""
		old.SupersededBy = next.ID
		result = core.TaskStartOverResult{Task: old, Successor: next, Created: true}
		return nil
	})
	return result, err
}
