package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) ProposeTaskContext(ctx context.Context, input core.TaskContextProposalInput) (core.TaskContextProposal, bool, error) {
	return s.proposeTaskContext(ctx, input, false)
}

func (s *Store) proposeTaskContext(ctx context.Context, input core.TaskContextProposalInput, legacyCompatibility bool) (core.TaskContextProposal, bool, error) {
	input.TaskID, input.TargetID, input.Justification = strings.TrimSpace(input.TaskID), strings.TrimSpace(input.TargetID), strings.TrimSpace(input.Justification)
	if !input.Valid() {
		return core.TaskContextProposal{}, false, fmt.Errorf("invalid task context proposal")
	}
	var proposal core.TaskContextProposal
	var suppressed bool
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), input.TaskID).Scan(&state); err != nil {
			return notFound(err, "task %s", input.TaskID)
		}
		if core.TaskTerminal(core.TaskState(state)) && !legacyCompatibility {
			return store.ErrTaskTerminal
		}
		rows, err := q.ListEvents(ctx, db.ListEventsParams{TaskID: pgtype.Text{String: input.TaskID, Valid: true}, WorkspaceID: workspace(ctx)})
		if err != nil {
			return err
		}
		events := make([]core.Event, len(rows))
		for i := range rows {
			events[i] = eventFromDB(rows[i])
		}
		activeRequirements, activeDesigns := store.ActiveTaskContextReferences(events)
		if input.TargetKind == core.TaskContextProposalRequirement && activeRequirements[input.TargetID] ||
			input.TargetKind == core.TaskContextProposalSystemDesign && activeDesigns[input.TargetID] > 0 {
			suppressed = true
			return nil
		}
		if existing, getErr := getTaskContextProposalTx(ctx, tx, input.TaskID, input.TargetKind, input.TargetID, true); getErr == nil {
			if existing.State == core.TaskContextProposalProposed {
				proposal, suppressed = existing, true
				return nil
			}
			return fmt.Errorf("%w: cannot repropose a %s proposal", store.ErrTaskContextProposalTransition, existing.State)
		} else if !errors.Is(getErr, pgx.ErrNoRows) {
			return getErr
		}
		var title, requirementSlug string
		var current *int
		var targetErr error
		if input.TargetKind == core.TaskContextProposalRequirement {
			targetErr = tx.QueryRow(ctx, `SELECT title,slug,current_version FROM requirements WHERE workspace_id=$1 AND id=$2 FOR SHARE`, workspace(ctx), input.TargetID).Scan(&title, &requirementSlug, &current)
		} else {
			targetErr = tx.QueryRow(ctx, `SELECT title,current_version FROM system_designs WHERE workspace_id=$1 AND id=$2 FOR SHARE`, workspace(ctx), input.TargetID).Scan(&title, &current)
		}
		if targetErr != nil {
			kind := string(input.TargetKind)
			if input.TargetKind == core.TaskContextProposalSystemDesign {
				kind = "system design"
			}
			if errors.Is(targetErr, pgx.ErrNoRows) {
				return &store.TaskContextReferenceError{Kind: kind, ID: input.TargetID, Reason: "was not found in this workspace"}
			}
			return targetErr
		}
		if (current == nil || *current <= 0) && !legacyCompatibility {
			kind := string(input.TargetKind)
			if input.TargetKind == core.TaskContextProposalSystemDesign {
				kind = "system design"
			}
			return &store.TaskContextReferenceError{Kind: kind, ID: input.TargetID, Reason: "has no confirmed version"}
		}
		actor, now := store.ActorFromContext(ctx), time.Now().UTC()
		eventKind := "task.context_proposed"
		payload := map[string]any{"target_kind": input.TargetKind, "target_id": input.TargetID, "source": input.Source, "justification": input.Justification}
		if input.TargetKind == core.TaskContextProposalRequirement && legacyCompatibility {
			eventKind, payload["requirement_id"] = "task.requirement_suggested", input.TargetID
			payload["requirement_slug"], payload["requirement_title"] = requirementSlug, title
		}
		eventID, err := insertEventWithID(ctx, q, core.Event{TaskID: input.TaskID, Kind: eventKind, At: now, Payload: core.JSONPayload(payload)})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO task_context_proposals
			(workspace_id,task_id,target_kind,target_id,target_title,state,source,justification,created_by_event_id,proposed_by,created_at,updated_at)
			VALUES ($1,$2,$3,$4,$5,'proposed',$6,$7,$8,$9,$10,$10)`, workspace(ctx), input.TaskID, input.TargetKind, input.TargetID, title, input.Source, input.Justification, eventID, actor.ID, now)
		if err != nil {
			return err
		}
		proposal, err = getTaskContextProposalTx(ctx, tx, input.TaskID, input.TargetKind, input.TargetID, true)
		return err
	})
	return proposal, suppressed, err
}

func (s *Store) ConfirmTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string) (core.TaskContextProposal, error) {
	return s.transitionTaskContextProposal(ctx, taskID, kind, targetID, core.TaskContextProposalConfirmed, false)
}
func (s *Store) DismissTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string) (core.TaskContextProposal, error) {
	return s.transitionTaskContextProposal(ctx, taskID, kind, targetID, core.TaskContextProposalDismissed, false)
}

func (s *Store) transitionTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string, target core.TaskContextProposalState, legacyCompatibility bool) (core.TaskContextProposal, error) {
	var proposal core.TaskContextProposal
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var taskState string
		if err := tx.QueryRow(ctx, `SELECT state FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), taskID).Scan(&taskState); err != nil {
			return notFound(err, "task %s", taskID)
		}
		if core.TaskTerminal(core.TaskState(taskState)) && !legacyCompatibility {
			return store.ErrTaskTerminal
		}
		var err error
		proposal, err = getTaskContextProposalTx(ctx, tx, taskID, kind, strings.TrimSpace(targetID), true)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("task context proposal: %w", store.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if proposal.State == target {
			return nil
		}
		if proposal.State != core.TaskContextProposalProposed {
			return fmt.Errorf("%w: cannot transition %s proposal to %s", store.ErrTaskContextProposalTransition, proposal.State, target)
		}
		actor, now := store.ActorFromContext(ctx), time.Now().UTC()
		eventKind := "task.context_proposal_dismissed"
		if target == core.TaskContextProposalConfirmed {
			eventKind = "task.context_proposal_confirmed"
		}
		payload := map[string]any{"target_kind": kind, "target_id": targetID}
		if kind == core.TaskContextProposalRequirement && legacyCompatibility {
			payload["requirement_id"] = targetID
			if target == core.TaskContextProposalConfirmed {
				eventKind = "requirement.serves_confirmed"
			} else {
				eventKind = "requirement.serves_dismissed"
			}
		}
		eventID, err := insertEventWithID(ctx, q, core.Event{TaskID: taskID, Kind: eventKind, At: now, Payload: core.JSONPayload(payload)})
		if err != nil {
			return err
		}
		if target == core.TaskContextProposalConfirmed {
			rows, listErr := q.ListEvents(ctx, db.ListEventsParams{TaskID: pgtype.Text{String: taskID, Valid: true}, WorkspaceID: workspace(ctx)})
			if listErr != nil {
				return listErr
			}
			events := make([]core.Event, len(rows))
			for i := range rows {
				events[i] = eventFromDB(rows[i])
			}
			activeRequirements, activeDesigns := store.ActiveTaskContextReferences(events)
			if kind == core.TaskContextProposalRequirement {
				if !activeRequirements[targetID] {
					if err = insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: store.TaskContextRequirementAdded, At: now, Payload: core.JSONPayload(map[string]any{"id": targetID})}); err != nil {
						return err
					}
				}
			} else {
				if activeDesigns[targetID] == 0 {
					var version *int
					if err = tx.QueryRow(ctx, `SELECT current_version FROM system_designs WHERE workspace_id=$1 AND id=$2 FOR SHARE`, workspace(ctx), targetID).Scan(&version); err != nil {
						return err
					}
					if version == nil || *version <= 0 {
						return &store.TaskContextReferenceError{Kind: "system design", ID: targetID, Reason: "has no confirmed version"}
					}
					if err = insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: store.TaskContextDesignAdded, At: now, Payload: core.JSONPayload(map[string]any{"id": targetID, "version": *version})}); err != nil {
						return err
					}
				}
			}
		}
		_, err = tx.Exec(ctx, `UPDATE task_context_proposals SET state=$5,decision_event_id=$6,decided_by=$7,updated_at=$8
			WHERE workspace_id=$1 AND task_id=$2 AND target_kind=$3 AND target_id=$4`, workspace(ctx), taskID, kind, targetID, target, eventID, actor.ID, now)
		if err != nil {
			return err
		}
		proposal.State, proposal.DecisionEventID, proposal.DecidedBy, proposal.UpdatedAt = target, eventID, actor.ID, now
		return nil
	})
	return proposal, err
}

func (s *Store) ListTaskContextProposals(ctx context.Context, taskID string, state core.TaskContextProposalState) ([]core.TaskContextProposal, error) {
	query := taskContextProposalSelect + ` WHERE workspace_id=$1`
	args := []any{workspace(ctx)}
	if taskID != "" {
		args = append(args, taskID)
		query += fmt.Sprintf(" AND task_id=$%d", len(args))
	}
	if state != "" {
		args = append(args, state)
		query += fmt.Sprintf(" AND state=$%d", len(args))
	}
	query += ` ORDER BY task_id,target_kind,target_id`
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.TaskContextProposal, 0)
	for rows.Next() {
		item, scanErr := scanTaskContextProposal(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	return out, rows.Err()
}

const taskContextProposalSelect = `SELECT workspace_id,task_id,target_kind,target_id,target_title,state,source,justification,
	created_by_event_id,COALESCE(decision_event_id,0),proposed_by,decided_by,created_at,updated_at FROM task_context_proposals`

type proposalScanner interface{ Scan(...any) error }

func scanTaskContextProposal(row proposalScanner) (core.TaskContextProposal, error) {
	var item core.TaskContextProposal
	var kind, state, source string
	err := row.Scan(&item.Workspace, &item.TaskID, &kind, &item.TargetID, &item.TargetTitle, &state, &source, &item.Justification,
		&item.CreatedByEventID, &item.DecisionEventID, &item.ProposedBy, &item.DecidedBy, &item.CreatedAt, &item.UpdatedAt)
	item.TargetKind, item.State, item.Source = core.TaskContextProposalTargetKind(kind), core.TaskContextProposalState(state), core.TaskContextProposalSource(source)
	return item, err
}
func getTaskContextProposalTx(ctx context.Context, tx pgx.Tx, taskID string, kind core.TaskContextProposalTargetKind, targetID string, lock bool) (core.TaskContextProposal, error) {
	query := taskContextProposalSelect + ` WHERE workspace_id=$1 AND task_id=$2 AND target_kind=$3 AND target_id=$4`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanTaskContextProposal(tx.QueryRow(ctx, query, workspace(ctx), taskID, kind, targetID))
}

func deleteProposedTaskContextTx(ctx context.Context, tx pgx.Tx, taskID string) error {
	_, err := tx.Exec(ctx, `DELETE FROM task_context_proposals
		WHERE workspace_id=$1 AND task_id=$2 AND state='proposed'`, workspace(ctx), taskID)
	return err
}
