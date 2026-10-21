package singlestore

// Requirement insight inputs and headline storage on SingleStore
// (feature-requirement-insights RI-2, RI-5; DEC-87 (2)).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

var _ store.RequirementInsightStore = (*Store)(nil)

// ReadRequirementInsightInputs answers in at most eight set-based queries
// without correlated subqueries, whatever the serving-task count: the
// requirement, its current version, the requirement's add/remove events in
// (task, at, id) order, the serving task rows, their dependency edges, their
// design add/remove events, the attached documents that exist, and their
// earliest merged and closed transition times. The folds are the task map's
// and the task context's: store.LatestRequirementServing and
// store.ActiveTaskContextReferences.
func (s *Store) ReadRequirementInsightInputs(ctx context.Context, requirementID string) (store.RequirementInsightInputs, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	requirement, err := scanRequirement(documentRow(ctx, s.db, requirementSelect+` WHERE workspace_id=? AND id=?`, ws, requirementID), requirementID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (requirement.Archived || requirement.CurrentVersion <= 0)) {
		return store.RequirementInsightInputs{}, store.RequirementInsightNotFound(requirementID)
	}
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	version, err := scanRequirementVersion(documentRow(ctx, s.db, requirementVersionSelect+` WHERE workspace_id=? AND requirement_id=? AND version=?`, ws, requirementID, requirement.CurrentVersion), requirementID, requirement.CurrentVersion)
	if errors.Is(err, store.ErrNotFound) {
		return store.RequirementInsightInputs{}, store.RequirementInsightNotFound(requirementID)
	}
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}

	attachmentEvents, err := s.requirementInsightEvents(ctx, `SELECT id, task_id, kind, payload_json, at FROM events
WHERE workspace_id=? AND task_id IS NOT NULL AND kind IN (?,?) AND JSON_EXTRACT_STRING(payload_json,'id')=?
ORDER BY task_id, at, id`, ws, store.TaskContextRequirementAdded, store.TaskContextRequirementRemoved, requirementID)
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	serving := store.LatestRequirementServing(attachmentEvents, requirementID)
	if len(serving) == 0 {
		return store.AssembleRequirementInsightInputs(requirement, version, nil, nil, nil, nil, nil, nil), nil
	}
	ids := make([]string, len(serving))
	for i, item := range serving {
		ids[i] = item.TaskID
	}
	marks, idArgs := taskMapMarks(len(ids)), taskMapArgs(ids)
	scoped := func(extra ...any) []any { return append(append([]any{ws}, idArgs...), extra...) }

	loaded, err := s.scanTaskMapRows(ctx, `SELECT `+taskColumns+` FROM tasks WHERE workspace_id=? AND id IN (`+marks+`) ORDER BY id`, scoped()...)
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	tasks := make(map[string]core.Task, len(loaded))
	for _, task := range loaded {
		tasks[task.ID] = task
	}

	edgeRows, err := documentRows(ctx, s.db, `SELECT task_id, depends_on_task_id FROM task_dependencies
WHERE workspace_id=? AND task_id IN (`+marks+`) ORDER BY task_id, depends_on_task_id`, scoped()...)
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	dependencies := map[string][]string{}
	for edgeRows.Next() {
		var taskID, dependencyID string
		if err = edgeRows.Scan(&taskID, &dependencyID); err != nil {
			edgeRows.Close()
			return store.RequirementInsightInputs{}, err
		}
		dependencies[taskID] = append(dependencies[taskID], dependencyID)
	}
	err = edgeRows.Err()
	edgeRows.Close()
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}

	designEvents, err := s.requirementInsightEvents(ctx, `SELECT id, task_id, kind, payload_json, at FROM events
WHERE workspace_id=? AND task_id IN (`+marks+`) AND kind IN (?,?)
ORDER BY task_id, at, id`, scoped(store.TaskContextDesignAdded, store.TaskContextDesignRemoved)...)
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	byTask := map[string][]core.Event{}
	for _, event := range designEvents {
		byTask[event.TaskID] = append(byTask[event.TaskID], event)
	}
	attached := map[string][]string{}
	candidateSet := map[string]bool{}
	for taskID, events := range byTask {
		attached[taskID] = store.ActiveDesignAttachments(events)
		for _, id := range attached[taskID] {
			candidateSet[id] = true
		}
	}
	existing := map[string]bool{}
	if len(candidateSet) > 0 {
		candidates := make([]string, 0, len(candidateSet))
		for id := range candidateSet {
			candidates = append(candidates, id)
		}
		sort.Strings(candidates)
		designRows, err := documentRows(ctx, s.db, `SELECT id FROM system_designs WHERE workspace_id=? AND id IN (`+taskMapMarks(len(candidates))+`)`, append([]any{ws}, taskMapArgs(candidates)...)...)
		if err != nil {
			return store.RequirementInsightInputs{}, err
		}
		for designRows.Next() {
			var id string
			if err = designRows.Scan(&id); err != nil {
				designRows.Close()
				return store.RequirementInsightInputs{}, err
			}
			existing[id] = true
		}
		err = designRows.Err()
		designRows.Close()
		if err != nil {
			return store.RequirementInsightInputs{}, err
		}
	}
	designs := map[string][]string{}
	for taskID, ids := range attached {
		for _, id := range ids {
			if existing[id] {
				designs[taskID] = append(designs[taskID], id)
			}
		}
	}

	terminalRows, err := documentRows(ctx, s.db, `SELECT task_id, JSON_EXTRACT_STRING(payload_json,'to') AS target, MIN(at) FROM events
WHERE workspace_id=? AND task_id IN (`+marks+`) AND kind='task.state_changed'
  AND JSON_EXTRACT_STRING(payload_json,'to') IN ('merged','closed')
GROUP BY task_id, target`, scoped()...)
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	merged, closed := map[string]time.Time{}, map[string]time.Time{}
	for terminalRows.Next() {
		var taskID, target string
		var at time.Time
		if err = terminalRows.Scan(&taskID, &target, &at); err != nil {
			terminalRows.Close()
			return store.RequirementInsightInputs{}, err
		}
		switch target {
		case string(core.TaskMerged):
			merged[taskID] = at.UTC()
		case string(core.TaskClosed):
			closed[taskID] = at.UTC()
		}
	}
	err = terminalRows.Err()
	terminalRows.Close()
	if err != nil {
		return store.RequirementInsightInputs{}, err
	}
	return store.AssembleRequirementInsightInputs(requirement, version, serving, tasks, dependencies, designs, merged, closed), nil
}

// requirementInsightEvents reads (id, task_id, kind, payload_json, at) rows.
func (s *Store) requirementInsightEvents(ctx context.Context, query string, args ...any) ([]core.Event, error) {
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Event
	for rows.Next() {
		var event core.Event
		if err = rows.Scan(&event.ID, &event.TaskID, &event.Kind, &event.Payload, &event.At); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

const requirementInsightHeadlineColumns = `workspace_id,requirement_id,fingerprint,headline,model,tokens_in,tokens_out,generated_by,generated_at`

type requirementInsightScanner interface{ Scan(...any) error }

func scanRequirementInsightHeadline(row requirementInsightScanner) (store.RequirementInsightHeadline, error) {
	var headline store.RequirementInsightHeadline
	err := row.Scan(&headline.WorkspaceID, &headline.RequirementID, &headline.Fingerprint, &headline.Headline, &headline.Model, &headline.TokensIn, &headline.TokensOut, &headline.GeneratedBy, &headline.GeneratedAt)
	headline.GeneratedAt = headline.GeneratedAt.UTC()
	return headline, err
}

// GetRequirementInsightHeadline reads the bound workspace's stored headline.
func (s *Store) GetRequirementInsightHeadline(ctx context.Context, requirementID string) (store.RequirementInsightHeadline, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return store.RequirementInsightHeadline{}, err
	}
	headline, err := scanRequirementInsightHeadline(documentRow(ctx, s.db, `SELECT `+requirementInsightHeadlineColumns+` FROM requirement_insight_headlines WHERE workspace_id=? AND requirement_id=?`, ws, requirementID))
	if err != nil {
		return store.RequirementInsightHeadline{}, notFound(err, "requirement insight headline %s", requirementID)
	}
	return headline, nil
}

// RecordRequirementInsightHeadline locks the headline's own key rather than a
// conveyor_locks row: a lock row on another partition can be released
// before a predecessor's headline row is visible (component-persistence).
//
// One transaction first inserts the row. Its primary key decides the race
// for an absent row: a concurrent insert of the same key waits on the
// uncommitted row, so success means this caller created the row and holds
// its lock. A duplicate key rolls back only that statement and proves the
// row exists; the same transaction then selects it FOR UPDATE, compares
// fingerprints on the locked row, and replaces it or refuses with
// ErrInsightHeadlineCurrent, rolling back with nothing written. Affected-row
// counts are never read. The concurrent integration tests exercise the
// duplicate-key path.
func (s *Store) RecordRequirementInsightHeadline(ctx context.Context, headline store.RequirementInsightHeadline) (store.RequirementInsightHeadline, error) {
	if err := store.ValidateRequirementInsightHeadline(headline); err != nil {
		return store.RequirementInsightHeadline{}, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.RequirementInsightHeadline{}, err
	}
	headline.WorkspaceID = ws
	headline.GeneratedAt = time.Now().UTC().Truncate(time.Microsecond)
	values := []any{headline.Fingerprint, headline.Headline, headline.Model, headline.TokensIn, headline.TokensOut, headline.GeneratedBy, headline.GeneratedAt}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO requirement_insight_headlines (`+requirementInsightHeadlineColumns+`) VALUES (?,?,?,?,?,?,?,?,?)`, append([]any{ws, headline.RequirementID}, values...)...)
		var driver *mysql.MySQLError
		if err == nil || !errors.As(err, &driver) || driver.Number != 1062 {
			return err
		}
		current, err := scanRequirementInsightHeadline(tx.QueryRowContext(ctx, `SELECT `+requirementInsightHeadlineColumns+` FROM requirement_insight_headlines WHERE workspace_id=? AND requirement_id=? FOR UPDATE`, ws, headline.RequirementID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("requirement insight headline %s: duplicate key without a visible row", headline.RequirementID)
		}
		if err != nil {
			return err
		}
		if current.Fingerprint == headline.Fingerprint {
			return store.ErrInsightHeadlineCurrent
		}
		_, err = tx.ExecContext(ctx, `UPDATE requirement_insight_headlines SET fingerprint=?, headline=?, model=?, tokens_in=?, tokens_out=?, generated_by=?, generated_at=? WHERE workspace_id=? AND requirement_id=?`, append(values, ws, headline.RequirementID)...)
		return err
	})
	if err != nil {
		return store.RequirementInsightHeadline{}, err
	}
	return headline, nil
}
