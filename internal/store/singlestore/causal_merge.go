package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) ResolveCausalSystemDesignMerge(ctx context.Context, documentID, repository, commitSHA string, causalEventID int64, driftID string, matchingPaths []string, recordConsulted bool) (monitor.SystemDesignMergeJudgment, error) {
	if causalEventID <= 0 || commitSHA == "" {
		return monitor.SystemDesignMergeJudgment{}, nil
	}
	var result monitor.SystemDesignMergeJudgment
	// req-260802-72fc68 AC-1.1: absent causal history produces no judgment.
	// The corpus writer rejects missing workspaces before this scoped read.
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var causalTaskID string
		var causalAt time.Time
		if err := documentRow(ctx, tx, `SELECT causal.task_id,causal.at FROM events causal
			JOIN tasks causal_task ON causal_task.workspace_id=causal.workspace_id AND causal_task.id=causal.task_id
			WHERE causal.workspace_id=? AND causal.id=? AND causal_task.repo_name=?
			AND causal.kind IN ('merge.confirmed','merge.reconciled')
			AND JSON_EXTRACT_STRING(causal.payload_json,'head_sha')=?`, documentWorkspace(ctx), causalEventID, repository, commitSHA).Scan(&causalTaskID, &causalAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		result.CausalEventValid = true
		mergeKey := "conveyor:design-merge-judgment:" + documentID + ":" + strconv.FormatInt(causalEventID, 10)
		if err := lockKey(ctx, tx, documentWorkspace(ctx)+":"+mergeKey); err != nil {
			return err
		}
		var eventID int64
		var version int
		var confirmed bool
		proposalErr := documentRow(ctx, tx, `SELECT proposal.id,CAST(JSON_EXTRACT_STRING(proposal.payload_json,'version') AS SIGNED),version.confirmed
			FROM events proposal
			JOIN system_design_versions version ON version.workspace_id=proposal.workspace_id
			 AND version.document_id=JSON_EXTRACT_STRING(proposal.payload_json,'document_id')
			 AND version.version=CAST(JSON_EXTRACT_STRING(proposal.payload_json,'version') AS SIGNED)
			WHERE proposal.workspace_id=? AND proposal.id<? AND proposal.kind='system_design.version_proposed'
			 AND JSON_EXTRACT_STRING(proposal.payload_json,'document_id')=? AND JSON_EXTRACT_STRING(proposal.payload_json,'origin_task_id')=?
			ORDER BY proposal.id DESC LIMIT 1`, documentWorkspace(ctx), causalEventID, documentID, causalTaskID).Scan(&eventID, &version, &confirmed)
		if proposalErr != nil && !errors.Is(proposalErr, sql.ErrNoRows) {
			return proposalErr
		}
		if proposalErr == nil {
			var existingDrift, existingStatus string
			existingErr := documentRow(ctx, tx, `SELECT JSON_EXTRACT_STRING(payload_json,'drift_id'),JSON_EXTRACT_STRING(payload_json,'proposal_status')
				FROM monitor_activity WHERE workspace_id=? AND kind='system_design.drift_suppressed'
					 AND JSON_EXTRACT_STRING(payload_json,'proposal_event_id')=? ORDER BY id LIMIT 1`, documentWorkspace(ctx), strconv.FormatInt(eventID, 10)).Scan(&existingDrift, &existingStatus)
			if existingErr == nil {
				if existingDrift == driftID {
					result.Proposal = monitor.ProposalSuppression{EventID: eventID, Status: existingStatus}
					return nil
				}
			} else if !errors.Is(existingErr, sql.ErrNoRows) {
				return existingErr
			} else {
				status := "pending"
				if confirmed {
					status = "confirmed"
				}
				payload, marshalErr := json.Marshal(map[string]any{"document_id": documentID, "drift_id": driftID, "merge_event_id": causalEventID, "proposal_event_id": eventID, "proposal_status": status, "proposal_version": version, "matching_paths": matchingPaths})
				if marshalErr != nil {
					return marshalErr
				}
				if _, insertErr := documentExec(ctx, tx, `INSERT INTO monitor_activity(workspace_id,kind,payload_json) VALUES(?,'system_design.drift_suppressed',?)`, documentWorkspace(ctx), string(payload)); insertErr != nil {
					return insertErr
				}
				result.Proposal = monitor.ProposalSuppression{EventID: eventID, Status: status}
				return nil
			}
		}

		var contextKind string
		var attachedVersion int
		contextErr := documentRow(ctx, tx, `SELECT kind,COALESCE(CAST(JSON_EXTRACT_STRING(payload_json,'version') AS SIGNED),0)
			FROM events WHERE workspace_id=? AND task_id=? AND id<?
				AND kind IN ('task.context_design_added','task.context_design_removed')
				AND JSON_EXTRACT_STRING(payload_json,'id')=? ORDER BY id DESC LIMIT 1`, documentWorkspace(ctx), causalTaskID, causalEventID, documentID).Scan(&contextKind, &attachedVersion)
		if contextErr != nil && !errors.Is(contextErr, sql.ErrNoRows) {
			return contextErr
		}
		if contextErr == nil && contextKind == store.TaskContextDesignAdded {
			result.AttachedVersion = attachedVersion
		}
		if !recordConsulted || result.AttachedVersion == 0 {
			return nil
		}
		var exists bool
		if err := documentRow(ctx, tx, `SELECT EXISTS(SELECT 1 FROM events WHERE workspace_id=?
			AND kind='system_design.consulted' AND JSON_EXTRACT_STRING(payload_json,'document_id')=?
			AND JSON_EXTRACT_STRING(payload_json,'merge_event_id')=?)`, documentWorkspace(ctx), documentID, strconv.FormatInt(causalEventID, 10)).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if err := insertWorkspaceEvent(ctx, tx, core.Event{Kind: "system_design.consulted", At: causalAt, Payload: core.JSONPayload(map[string]any{
				"workspace_id": documentWorkspace(ctx), "document_id": documentID, "version": result.AttachedVersion,
				"delivery_task_id": causalTaskID, "merge_event_id": causalEventID, "merge_head_sha": commitSHA,
				"matching_paths": matchingPaths, "consultation": "delivery_no_revision",
			})}); err != nil {
				return err
			}
		}
		result.Consulted = true
		return nil
	})
	return result, err
}
