package postgres

// Requirement insight inputs and headline storage on PostgreSQL
// (feature-requirement-insights RI-2, RI-5; DEC-87 (2)).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

var _ store.RequirementInsightStore = (*Store)(nil)

// requirementInsightTaskColumns is db.Task's full column set, body included:
// RI-3's citation scans read serving task bodies.
const requirementInsightTaskColumns = `t.supersedes, t.superseded_by, t.intake_operator_direction, t.id, t.workspace_id,
	t.source, t.title, t.body, t.class, t.escalation_level, t.repo_name, t.base_branch, t.branch,
	t.state, t.parent_task_id, t.created_at, t.updated_at, t.next_stage, t.recovery_stage, t.intake_key,
	t.mode, t.spec_approval, t.merge_approval, t.policy_version, t.setup_name, t.setup_contract, t.hold,
	t.reviewed_head_sha, t.approved_head_sha, t.approval_stale, t.refresh_baseline_sha, t.refresh_head_sha,
	t.refresh_review_scope, t.origin_spec_version, t.origin_sub_id, t.assignee_user_id, t.gate_kind`

// ReadRequirementInsightInputs answers in at most eight set-based queries
// through the translating boundary, whatever the serving-task count: the
// requirement, its current version, the latest add/remove attachment per
// task (DISTINCT ON in descending (at, id) order, the task map's rule), the
// serving task rows, their assignees, their dependency edges, their active
// design attachments joined to existing documents, and their earliest
// merged and closed transition times.
func (s *Store) ReadRequirementInsightInputs(ctx context.Context, requirementID string) (store.RequirementInsightInputs, error) {
	workspaceID := workspace(ctx)
	requirement, err := scanRequirement(s.boundary.QueryRow(ctx, requirementSelect+` WHERE workspace_id=$1 AND id=$2`, workspaceID, requirementID), requirementID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (requirement.Archived || requirement.CurrentVersion <= 0)) {
		return store.RequirementInsightInputs{}, store.RequirementInsightNotFound(requirementID)
	}
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	version, err := scanRequirementVersion(s.boundary.QueryRow(ctx, requirementVersionSelect+` WHERE workspace_id=$1 AND requirement_id=$2 AND version=$3`, workspaceID, requirementID, requirement.CurrentVersion), requirementID, requirement.CurrentVersion)
	if errors.Is(err, store.ErrNotFound) {
		return store.RequirementInsightInputs{}, store.RequirementInsightNotFound(requirementID)
	}
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}

	servingRows, err := s.boundary.Query(ctx, `SELECT latest.task_id, latest.at
FROM (
	SELECT DISTINCT ON (e.task_id) e.task_id, e.kind, e.at
	FROM events e
	JOIN tasks t ON t.workspace_id=e.workspace_id AND t.id=e.task_id
	WHERE e.workspace_id=$1
	  AND e.kind IN ('task.context_requirement_added','task.context_requirement_removed')
	  AND e.payload_json->>'id'=$2
	ORDER BY e.task_id, e.at DESC, e.id DESC
) latest
WHERE latest.kind='task.context_requirement_added'
ORDER BY latest.task_id`, workspaceID, requirementID)
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	var serving []store.RequirementInsightServing
	for servingRows.Next() {
		var item store.RequirementInsightServing
		if err = servingRows.Scan(&item.TaskID, &item.ServingSince); err != nil {
			servingRows.Close()
			return store.RequirementInsightInputs{}, err
		}
		serving = append(serving, item)
	}
	servingRows.Close()
	if err = servingRows.Err(); err != nil {
		return store.RequirementInsightInputs{}, err
	}
	if len(serving) == 0 {
		return store.AssembleRequirementInsightInputs(requirement, version, nil, nil, nil, nil, nil, nil), nil
	}
	ids := make([]string, len(serving))
	for i, item := range serving {
		ids[i] = item.TaskID
	}

	taskRows, err := s.boundary.Query(ctx, `SELECT `+requirementInsightTaskColumns+`
FROM tasks t WHERE t.workspace_id=$1 AND t.id=ANY($2::text[]) ORDER BY t.id`, workspaceID, ids)
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	persisted, err := pgx.CollectRows(taskRows, pgx.RowToStructByName[db.Task])
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	loaded := make([]core.Task, len(persisted))
	for i := range persisted {
		loaded[i] = taskFromDB(persisted[i])
	}
	if err = s.hydrateTaskAssignees(ctx, loaded); err != nil {
		return store.RequirementInsightInputs{}, err
	}
	tasks := make(map[string]core.Task, len(loaded))
	for _, task := range loaded {
		tasks[task.ID] = task
	}

	dependencies, err := s.requirementInsightPairs(ctx, `SELECT task_id, depends_on_task_id FROM task_dependencies
WHERE workspace_id=$1 AND task_id=ANY($2::text[])
ORDER BY task_id, depends_on_task_id`, workspaceID, ids)
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	// Designs resolve as store.TaskContextFromEvents resolves them: the
	// latest design add/remove per task and document is an add, and the
	// document exists in the workspace.
	designs, err := s.requirementInsightPairs(ctx, `SELECT latest.task_id, latest.design_id
FROM (
	SELECT DISTINCT ON (e.task_id, e.payload_json->>'id') e.task_id, e.payload_json->>'id' AS design_id, e.kind
	FROM events e
	WHERE e.workspace_id=$1 AND e.task_id=ANY($2::text[])
	  AND e.kind IN ('task.context_design_added','task.context_design_removed')
	ORDER BY e.task_id, e.payload_json->>'id', e.at DESC, e.id DESC
) latest
JOIN system_designs d ON d.workspace_id=$1 AND d.id=latest.design_id
WHERE latest.kind='task.context_design_added'
ORDER BY latest.task_id, latest.design_id`, workspaceID, ids)
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}

	terminalRows, err := s.boundary.Query(ctx, `SELECT e.task_id, e.payload_json->>'to', min(e.at)
FROM events e
WHERE e.workspace_id=$1 AND e.task_id=ANY($2::text[]) AND e.kind='task.state_changed'
  AND e.payload_json->>'to' IN ('merged','closed')
GROUP BY e.task_id, e.payload_json->>'to'`, workspaceID, ids)
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	merged, closed := map[string]time.Time{}, map[string]time.Time{}
	for terminalRows.Next() {
		var taskID, to string
		var at time.Time
		if err = terminalRows.Scan(&taskID, &to, &at); err != nil {
			terminalRows.Close()
			return store.RequirementInsightInputs{}, err
		}
		if to == string(core.TaskMerged) {
			merged[taskID] = at.UTC()
		} else {
			closed[taskID] = at.UTC()
		}
	}
	terminalRows.Close()
	if err = terminalRows.Err(); err != nil {
		return store.RequirementInsightInputs{}, err
	}
	return store.AssembleRequirementInsightInputs(requirement, version, serving, tasks, dependencies, designs, merged, closed), nil
}

// requirementInsightPairs reads (task_id, value) rows into per-task lists.
func (s *Store) requirementInsightPairs(ctx context.Context, query string, args ...any) (map[string][]string, error) {
	rows, err := s.boundary.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var taskID, value string
		if err = rows.Scan(&taskID, &value); err != nil {
			return nil, err
		}
		out[taskID] = append(out[taskID], value)
	}
	return out, rows.Err()
}

func requirementInsightHeadlineFromDB(row db.RequirementInsightHeadline) store.RequirementInsightHeadline {
	return store.RequirementInsightHeadline{
		WorkspaceID: row.WorkspaceID, RequirementID: row.RequirementID, Fingerprint: row.Fingerprint,
		Headline: row.Headline, Model: row.Model, TokensIn: row.TokensIn, TokensOut: row.TokensOut,
		GeneratedBy: row.GeneratedBy, GeneratedAt: row.GeneratedAt.Time.UTC(),
	}
}

// GetRequirementInsightHeadline reads the bound workspace's stored headline.
func (s *Store) GetRequirementInsightHeadline(ctx context.Context, requirementID string) (store.RequirementInsightHeadline, error) {
	row, err := s.queries.GetRequirementInsightHeadline(ctx, db.GetRequirementInsightHeadlineParams{WorkspaceID: workspace(ctx), RequirementID: requirementID})
	if err != nil {
		return store.RequirementInsightHeadline{}, notFound(err, "requirement insight headline %s", requirementID)
	}
	return requirementInsightHeadlineFromDB(row), nil
}

// RecordRequirementInsightHeadline compares and writes in one transaction.
// The transaction lock conveyor:requirement-insight-headline:<workspace>:<id>
// covers an absent row, and the existing row is also selected FOR UPDATE
// before the fingerprint comparison, so concurrent callers observe one
// winner. An equal fingerprint returns ErrInsightHeadlineCurrent and the
// rolled-back transaction writes nothing.
func (s *Store) RecordRequirementInsightHeadline(ctx context.Context, headline store.RequirementInsightHeadline) (store.RequirementInsightHeadline, error) {
	if err := store.ValidateRequirementInsightHeadline(headline); err != nil {
		return store.RequirementInsightHeadline{}, err
	}
	workspaceID := workspace(ctx)
	if workspaceID == "" {
		return store.RequirementInsightHeadline{}, store.ErrWorkspaceRequired
	}
	var stored store.RequirementInsightHeadline
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:requirement-insight-headline:"+workspaceID+":"+headline.RequirementID); err != nil {
			return err
		}
		current, err := q.LockRequirementInsightHeadline(ctx, db.LockRequirementInsightHeadlineParams{WorkspaceID: workspaceID, RequirementID: headline.RequirementID})
		switch {
		case err == nil && current.Fingerprint == headline.Fingerprint:
			return store.ErrInsightHeadlineCurrent
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("lock requirement insight headline %s: %w", headline.RequirementID, err)
		}
		row, err := q.UpsertRequirementInsightHeadline(ctx, db.UpsertRequirementInsightHeadlineParams{
			WorkspaceID: workspaceID, RequirementID: headline.RequirementID, Fingerprint: headline.Fingerprint,
			Headline: headline.Headline, Model: headline.Model, TokensIn: headline.TokensIn, TokensOut: headline.TokensOut,
			GeneratedBy: headline.GeneratedBy, GeneratedAt: timestamp(time.Now().UTC().Truncate(time.Microsecond)),
		})
		if err != nil {
			return err
		}
		stored = requirementInsightHeadlineFromDB(row)
		return nil
	})
	if err != nil {
		return store.RequirementInsightHeadline{}, err
	}
	return stored, nil
}
