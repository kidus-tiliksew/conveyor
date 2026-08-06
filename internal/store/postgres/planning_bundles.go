package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

const planningBundleSelect = `SELECT id,session_id,title,documents,tasks,status,COALESCE(created_by,''),COALESCE(decided_by,''),created_at,decided_at,workspace_id FROM planning_bundles`

func (s *Store) CreatePlanningBundle(ctx context.Context, bundle core.PlanningBundle) (core.PlanningBundle, error) {
	bundle.Workspace = workspace(ctx)
	if err := store.ValidatePlanningBundleShape(&bundle); err != nil {
		return core.PlanningBundle{}, err
	}
	actor := store.ActorFromContext(ctx)
	bundle.CreatedBy = actor.ID
	bundle.Status = core.PlanningBundlePending
	if bundle.CreatedAt.IsZero() {
		bundle.CreatedAt = time.Now().UTC()
	}
	documents, _ := json.Marshal(bundle.Documents)
	tasks, _ := json.Marshal(bundle.Tasks)
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		for i := range bundle.Documents {
			doc := &bundle.Documents[i]
			switch doc.Kind {
			case core.PlanningBundleRequirement:
				var confirmed bool
				if err := tx.QueryRow(ctx, `SELECT r.title,v.confirmed FROM requirement_versions v JOIN requirements r ON r.workspace_id=v.workspace_id AND r.id=v.requirement_id WHERE v.workspace_id=$1 AND v.requirement_id=$2 AND v.version=$3`, bundle.Workspace, doc.ID, doc.Version).Scan(&doc.Title, &confirmed); err != nil {
					return notFound(err, "requirement %s version %d", doc.ID, doc.Version)
				}
				if confirmed {
					return fmt.Errorf("requirement %s version %d is not pending", doc.ID, doc.Version)
				}
			case core.PlanningBundleSystemDesign:
				var confirmed, dismissed bool
				if err := tx.QueryRow(ctx, `SELECT d.title,v.confirmed,v.dismissed FROM system_design_versions v JOIN system_designs d ON d.workspace_id=v.workspace_id AND d.id=v.document_id WHERE v.workspace_id=$1 AND v.document_id=$2 AND v.version=$3`, bundle.Workspace, doc.ID, doc.Version).Scan(&doc.Title, &confirmed, &dismissed); err != nil {
					return notFound(err, "system design %s version %d", doc.ID, doc.Version)
				}
				if confirmed || dismissed {
					return fmt.Errorf("system design %s version %d is not pending", doc.ID, doc.Version)
				}
			case core.PlanningBundleDecision:
				var status string
				if err := tx.QueryRow(ctx, `SELECT statement,status FROM decisions WHERE workspace_id=$1 AND id=$2`, bundle.Workspace, doc.ID).Scan(&doc.Title, &status); err != nil {
					return notFound(err, "decision %s", doc.ID)
				}
				if status != string(core.DecisionProposed) {
					return fmt.Errorf("decision %s is not pending", doc.ID)
				}
			}
			doc.Status = "pending"
		}
		documents, _ = json.Marshal(bundle.Documents)
		command, err := tx.Exec(ctx, `INSERT INTO planning_bundles (workspace_id,id,session_id,title,documents,tasks,status,created_by,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9) ON CONFLICT (workspace_id,id) DO NOTHING`, bundle.Workspace, bundle.ID, bundle.SessionID, bundle.Title, documents, tasks, string(bundle.Status), bundle.CreatedBy, bundle.CreatedAt)
		if err != nil {
			return err
		}
		if command.RowsAffected() == 0 {
			return nil
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: store.PlanningBundleFinalized, At: bundle.CreatedAt, Payload: core.JSONPayload(map[string]any{"bundle_id": bundle.ID, "session_id": bundle.SessionID, "documents": bundle.Documents})})
	})
	if err != nil {
		return core.PlanningBundle{}, err
	}
	return s.GetPlanningBundle(ctx, bundle.ID)
}

func scanPlanningBundle(row pgx.Row) (core.PlanningBundle, error) {
	var b core.PlanningBundle
	var docs, tasks []byte
	var status string
	var decided *time.Time
	err := row.Scan(&b.ID, &b.SessionID, &b.Title, &docs, &tasks, &status, &b.CreatedBy, &b.DecidedBy, &b.CreatedAt, &decided, &b.Workspace)
	if err != nil {
		return b, err
	}
	if err = json.Unmarshal(docs, &b.Documents); err != nil {
		return b, err
	}
	if err = json.Unmarshal(tasks, &b.Tasks); err != nil {
		return b, err
	}
	b.Status = core.PlanningBundleStatus(status)
	if decided != nil {
		b.DecidedAt = *decided
	}
	return b, nil
}

func (s *Store) GetPlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error) {
	b, err := scanPlanningBundle(s.pool.QueryRow(ctx, planningBundleSelect+` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.PlanningBundle{}, fmt.Errorf("planning bundle %s: %w", id, store.ErrNotFound)
	}
	return b, err
}

func (s *Store) ListPlanningBundles(ctx context.Context) ([]core.PlanningBundle, error) {
	rows, err := s.pool.Query(ctx, planningBundleSelect+` WHERE workspace_id=$1 ORDER BY created_at DESC,id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.PlanningBundle{}
	for rows.Next() {
		b, scanErr := scanPlanningBundle(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) ApprovePlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error) {
	var result core.PlanningBundle
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		bundle, err := scanPlanningBundle(tx.QueryRow(ctx, planningBundleSelect+` WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), id))
		if err != nil {
			return notFound(err, "planning bundle %s", id)
		}
		if bundle.Status == core.PlanningBundleApproved {
			result = bundle
			return nil
		}
		if bundle.Status != core.PlanningBundlePending {
			return fmt.Errorf("planning bundle %s is %s", id, bundle.Status)
		}
		if err = lockDependencyEdgesTx(ctx, tx, bundle.Workspace); err != nil {
			return err
		}
		byMember := map[string]core.PlanningBundleTask{}
		designVersions := map[string]map[string]int{}
		for _, member := range bundle.Tasks {
			byMember[member.MemberID] = member
			var exists bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE workspace_id=$1 AND id=$2)`, bundle.Workspace, member.CreatedTaskID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return fmt.Errorf("bundle task identity %s already exists", member.CreatedTaskID)
			}
			versions, validationErr := validateTaskContextTx(ctx, tx, bundle.Workspace, store.TaskContextInput{RequirementIDs: member.Context.RequirementIDs, DesignIDs: member.Context.DesignIDs})
			if validationErr != nil {
				return validationErr
			}
			designVersions[member.MemberID] = versions
		}
		now := time.Now().UTC()
		for _, member := range bundle.Tasks {
			task := core.Task{ID: member.CreatedTaskID, Workspace: bundle.Workspace, Source: "planning_bundle:" + bundle.ID + ":" + member.MemberID, IntakeKey: "planning-bundle:" + bundle.ID + ":" + member.MemberID, Title: member.Title, Body: member.Body, Level: core.L2, SpecApproval: member.SpecApproval, MergeApproval: member.MergeApproval, PolicyVersion: 1, SetupName: member.SetupName, SetupContract: member.SetupContract, Repo: member.Repo, BaseBranch: member.BaseBranch, Branch: gitx.BranchName(member.CreatedTaskID), State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: now}
			if _, err = q.InsertTask(ctx, taskInsertParams(task)); err != nil {
				return err
			}
			if _, err = insertEventWithID(ctx, q, core.Event{TaskID: task.ID, Kind: "task.created", Payload: core.JSONPayload(task), At: now}); err != nil {
				return err
			}
		}
		for _, member := range bundle.Tasks {
			for _, dep := range member.DependsOn {
				dependencyID := byMember[dep].CreatedTaskID
				if _, err = tx.Exec(ctx, `INSERT INTO task_dependencies (workspace_id,task_id,depends_on_task_id) VALUES ($1,$2,$3)`, bundle.Workspace, member.CreatedTaskID, dependencyID); err != nil {
					return err
				}
				if err = insertEvent(ctx, q, core.Event{TaskID: member.CreatedTaskID, Kind: "task.dependency_added", At: now, Payload: core.JSONPayload(map[string]string{"task_id": member.CreatedTaskID, "depends_on_task_id": dependencyID})}); err != nil {
					return err
				}
			}
			for _, reqID := range member.Context.RequirementIDs {
				if err = insertEvent(ctx, q, core.Event{TaskID: member.CreatedTaskID, Kind: store.TaskContextRequirementAdded, At: now, Payload: core.JSONPayload(map[string]any{"id": reqID})}); err != nil {
					return err
				}
			}
			for _, designID := range member.Context.DesignIDs {
				if err = insertEvent(ctx, q, core.Event{TaskID: member.CreatedTaskID, Kind: store.TaskContextDesignAdded, At: now, Payload: core.JSONPayload(map[string]any{"id": designID, "version": designVersions[member.MemberID][designID]})}); err != nil {
					return err
				}
			}
		}
		actor := store.ActorFromContext(ctx)
		if _, err = tx.Exec(ctx, `UPDATE planning_bundles SET status='approved',decided_by=NULLIF($3,''),decided_at=$4 WHERE workspace_id=$1 AND id=$2`, bundle.Workspace, bundle.ID, actor.ID, now); err != nil {
			return err
		}
		ids := make([]string, len(bundle.Tasks))
		for i := range bundle.Tasks {
			ids[i] = bundle.Tasks[i].CreatedTaskID
		}
		if err = insertWorkspaceEvent(ctx, q, core.Event{Kind: store.PlanningBundleApproved, At: now, Payload: core.JSONPayload(map[string]any{"bundle_id": bundle.ID, "created_task_ids": ids})}); err != nil {
			return err
		}
		for _, taskID := range ids {
			if _, err = s.enqueueTaskTx(ctx, tx, taskID, bundle.Workspace); err != nil {
				return err
			}
		}
		bundle.Status, bundle.DecidedBy, bundle.DecidedAt = core.PlanningBundleApproved, actor.ID, now
		result = bundle
		return nil
	})
	return result, err
}

func (s *Store) RejectPlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error) {
	var result core.PlanningBundle
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		bundle, err := scanPlanningBundle(tx.QueryRow(ctx, planningBundleSelect+` WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), id))
		if err != nil {
			return notFound(err, "planning bundle %s", id)
		}
		if bundle.Status == core.PlanningBundleRejected {
			result = bundle
			return nil
		}
		if bundle.Status != core.PlanningBundlePending {
			return fmt.Errorf("planning bundle %s is %s", id, bundle.Status)
		}
		now := time.Now().UTC()
		actor := store.ActorFromContext(ctx)
		if _, err = tx.Exec(ctx, `UPDATE planning_bundles SET status='rejected',decided_by=NULLIF($3,''),decided_at=$4 WHERE workspace_id=$1 AND id=$2`, bundle.Workspace, bundle.ID, actor.ID, now); err != nil {
			return err
		}
		if err = insertWorkspaceEvent(ctx, q, core.Event{Kind: store.PlanningBundleRejected, At: now, Payload: core.JSONPayload(map[string]any{"bundle_id": bundle.ID})}); err != nil {
			return err
		}
		bundle.Status, bundle.DecidedBy, bundle.DecidedAt = core.PlanningBundleRejected, actor.ID, now
		result = bundle
		return nil
	})
	return result, err
}
