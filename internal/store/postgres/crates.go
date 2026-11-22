package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

var _ store.CrateStore = (*Store)(nil)

// crateColumns reads one crate row. The date reads back as YYYY-MM-DD text.
const crateColumns = `id, name, to_char(target_date, 'YYYY-MM-DD'), state, created_by, created_at, closed_at`

func scanCrate(row pgx.Row, workspaceID string) (store.Crate, error) {
	var crate store.Crate
	var targetDate pgtype.Text
	var state string
	var createdAt time.Time
	var closedAt pgtype.Timestamptz
	if err := row.Scan(&crate.ID, &crate.Name, &targetDate, &state, &crate.CreatedBy, &createdAt, &closedAt); err != nil {
		return store.Crate{}, err
	}
	crate.Workspace = workspaceID
	crate.State = store.CrateState(state)
	crate.CreatedAt = createdAt.UTC()
	if targetDate.Valid {
		date := targetDate.String
		crate.TargetDate = &date
	}
	if closedAt.Valid {
		at := closedAt.Time.UTC()
		crate.ClosedAt = &at
	}
	crate.RequirementIDs = []string{}
	return crate, nil
}

// CreateCrate implements store.CrateStore. The requirement validation, the
// record, its declared list, and its crate.created event share one
// transaction, so a refusal records nothing (component-crates invariants 4,
// 5, and 8; req-crates AC-1.1, AC-1.3).
func (s *Store) CreateCrate(ctx context.Context, input store.CrateInput) (store.Crate, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CreateCrate")
	actor, err := store.RequireActor(ctx)
	if err != nil {
		return store.Crate{}, err
	}
	if input, err = store.NormalizeCrateInput(input); err != nil {
		return store.Crate{}, err
	}
	workspaceID := workspace(ctx)
	crate := store.Crate{
		ID: store.NewCrateID(), Workspace: workspaceID, Name: input.Name, TargetDate: input.TargetDate, State: store.CrateOpen,
		CreatedBy: actor.ID, CreatedAt: store.CrateTime(time.Now()), RequirementIDs: append([]string{}, input.RequirementIDs...),
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := validateTaskContextTx(ctx, tx, workspaceID, store.TaskContextInput{RequirementIDs: input.RequirementIDs}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO crates (workspace_id, id, name, target_date, state, created_by, created_at)
VALUES ($1, $2, $3, $4::text::date, 'open', $5, $6)`, workspaceID, crate.ID, crate.Name, crate.TargetDate, crate.CreatedBy, crate.CreatedAt); err != nil {
			return err
		}
		if err := replaceCrateRequirementsTx(ctx, tx, workspaceID, crate.ID, crate.RequirementIDs); err != nil {
			return err
		}
		_, err := insertWorkspaceEventRow(ctx, q, db.InsertWorkspaceEventParams{
			WorkspaceID: workspaceID, Kind: store.CrateCreatedEvent, ActorID: actor.ID, ActorRole: string(actor.Role),
			PayloadJson: store.CrateCreatedPayload(crate), At: timestamp(crate.CreatedAt),
		})
		return err
	})
	if err != nil {
		return store.Crate{}, err
	}
	store.ApplyCrateCounts(&crate, nil)
	return crate, nil
}

// UpdateCrate implements store.CrateStore. The crate row lock serializes the
// edit with a join and with intake, which read the crate FOR SHARE, so a
// closed crate never gains a task (component-crates invariant 6).
func (s *Store) UpdateCrate(ctx context.Context, id string, patch store.CratePatch) (store.Crate, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "UpdateCrate")
	actor, err := store.RequireActor(ctx)
	if err != nil {
		return store.Crate{}, err
	}
	if patch, err = store.NormalizeCratePatch(patch); err != nil {
		return store.Crate{}, err
	}
	workspaceID := workspace(ctx)
	id = strings.TrimSpace(id)
	var result store.Crate
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		current, err := scanCrate(tx.QueryRow(ctx, `SELECT `+crateColumns+` FROM crates WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, id), workspaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &store.CrateNotFoundError{ID: id}
		}
		if err != nil {
			return err
		}
		if current.RequirementIDs, err = crateRequirementIDsTx(ctx, tx, workspaceID, id); err != nil {
			return err
		}
		if patch.RequirementIDs != nil {
			if _, err := validateTaskContextTx(ctx, tx, workspaceID, store.TaskContextInput{RequirementIDs: *patch.RequirementIDs}); err != nil {
				return err
			}
		}
		now := time.Now()
		next, kind, payload := store.ApplyCratePatch(current, patch, now)
		if kind != "" {
			if _, err := tx.Exec(ctx, `UPDATE crates SET name=$3, target_date=$4::text::date, state=$5, closed_at=$6
WHERE workspace_id=$1 AND id=$2`, workspaceID, id, next.Name, next.TargetDate, string(next.State), next.ClosedAt); err != nil {
				return err
			}
			if patch.RequirementIDs != nil {
				if err := replaceCrateRequirementsTx(ctx, tx, workspaceID, id, next.RequirementIDs); err != nil {
					return err
				}
			}
			if _, err := insertWorkspaceEventRow(ctx, q, db.InsertWorkspaceEventParams{
				WorkspaceID: workspaceID, Kind: kind, ActorID: actor.ID, ActorRole: string(actor.Role),
				PayloadJson: payload, At: timestamp(store.CrateTime(now)),
			}); err != nil {
				return err
			}
		}
		states, err := crateMemberStatesTx(ctx, tx, workspaceID, id)
		if err != nil {
			return err
		}
		store.ApplyCrateCounts(&next, states)
		result = next
		return nil
	})
	if err != nil {
		return store.Crate{}, err
	}
	return result, nil
}

// GetCrate implements store.CrateStore.
func (s *Store) GetCrate(ctx context.Context, id string) (store.CrateRecord, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetCrate")
	workspaceID := workspace(ctx)
	id = strings.TrimSpace(id)
	crate, err := scanCrate(s.boundary.QueryRow(ctx, `SELECT `+crateColumns+` FROM crates WHERE workspace_id=$1 AND id=$2`, workspaceID, id), workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CrateRecord{}, &store.CrateNotFoundError{ID: id}
	}
	if err != nil {
		return store.CrateRecord{}, err
	}
	if crate.RequirementIDs, err = crateRequirementIDsTx(ctx, s.boundary, workspaceID, id); err != nil {
		return store.CrateRecord{}, err
	}
	rows, err := s.boundary.Query(ctx, `SELECT id, state FROM tasks WHERE workspace_id=$1 AND crate_id=$2 ORDER BY created_at, id`, workspaceID, id)
	if err != nil {
		return store.CrateRecord{}, err
	}
	defer rows.Close()
	record := store.CrateRecord{TaskIDs: []string{}}
	var states []core.TaskState
	for rows.Next() {
		var taskID, state string
		if err := rows.Scan(&taskID, &state); err != nil {
			return store.CrateRecord{}, err
		}
		record.TaskIDs = append(record.TaskIDs, taskID)
		states = append(states, core.TaskState(state))
	}
	if err := rows.Err(); err != nil {
		return store.CrateRecord{}, err
	}
	store.ApplyCrateCounts(&crate, states)
	record.Crate = crate
	return record, nil
}

// ListCrates implements store.CrateStore in three set-based queries: the
// crates, their declared lists, and the member states.
func (s *Store) ListCrates(ctx context.Context, query store.CrateListQuery) ([]store.Crate, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListCrates")
	workspaceID := workspace(ctx)
	rows, err := s.boundary.Query(ctx, `SELECT `+crateColumns+` FROM crates WHERE workspace_id=$1 AND ($2::text = '' OR state = $2::text)`, workspaceID, string(query.State))
	if err != nil {
		return nil, err
	}
	crates := []store.Crate{}
	for rows.Next() {
		crate, err := scanCrate(rows, workspaceID)
		if err != nil {
			rows.Close()
			return nil, err
		}
		crates = append(crates, crate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	requirementRows, err := s.boundary.Query(ctx, `SELECT crate_id, requirement_id FROM crate_requirements WHERE workspace_id=$1 ORDER BY crate_id, requirement_id`, workspaceID)
	if err != nil {
		return nil, err
	}
	requirements := map[string][]string{}
	for requirementRows.Next() {
		var crateID, requirementID string
		if err := requirementRows.Scan(&crateID, &requirementID); err != nil {
			requirementRows.Close()
			return nil, err
		}
		requirements[crateID] = append(requirements[crateID], requirementID)
	}
	requirementRows.Close()
	if err := requirementRows.Err(); err != nil {
		return nil, err
	}
	memberRows, err := s.boundary.Query(ctx, `SELECT crate_id, state FROM tasks WHERE workspace_id=$1 AND crate_id IS NOT NULL`, workspaceID)
	if err != nil {
		return nil, err
	}
	var members []store.CrateMember
	for memberRows.Next() {
		var member store.CrateMember
		var state string
		if err := memberRows.Scan(&member.CrateID, &state); err != nil {
			memberRows.Close()
			return nil, err
		}
		member.State = core.TaskState(state)
		members = append(members, member)
	}
	memberRows.Close()
	if err := memberRows.Err(); err != nil {
		return nil, err
	}
	for i := range crates {
		crates[i].RequirementIDs = append([]string{}, requirements[crates[i].ID]...)
	}
	store.ApplyCrateMemberCounts(crates, members)
	store.SortCrates(crates)
	return crates, nil
}

// ListCratesWithBriefings implements store.CrateStore in four set-based
// statements whatever the number of crates: the three of ListCrates, which
// run under this read label, and one briefing summary statement that joins
// on workspace and crate ID with the same state filter.
func (s *Store) ListCratesWithBriefings(ctx context.Context, query store.CrateListQuery) ([]store.CrateListRow, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListCratesWithBriefings")
	crates, err := s.ListCrates(ctx, query)
	if err != nil {
		return nil, err
	}
	rows, err := s.queries.ListCrateBriefingSummaries(ctx, db.ListCrateBriefingSummariesParams{WorkspaceID: workspace(ctx), State: string(query.State)})
	if err != nil {
		return nil, err
	}
	summaries := make(map[string]store.CrateBriefingSummary, len(rows))
	for _, row := range rows {
		summaries[row.CrateID] = store.CrateBriefingSummary{Headline: row.Headline, GeneratedAt: row.GeneratedAt.Time}
	}
	return store.CrateListRows(crates, summaries), nil
}

// CrateNames implements store.CrateStore.
func (s *Store) CrateNames(ctx context.Context, ids []string) (map[string]string, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CrateNames")
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT id, name FROM crates WHERE workspace_id=$1 AND id=ANY($2::text[])`, workspace(ctx), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// ListCrateMembers implements store.CrateStore in one query over the
// member tasks of the listed crates.
func (s *Store) ListCrateMembers(ctx context.Context, ids []string) (map[string][]store.CrateMemberTask, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListCrateMembers")
	out := make(map[string][]store.CrateMemberTask, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT crate_id, id, title, state FROM tasks WHERE workspace_id=$1 AND crate_id=ANY($2::text[]) ORDER BY crate_id, created_at, id`, workspace(ctx), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var crateID, state string
		var member store.CrateMemberTask
		if err := rows.Scan(&crateID, &member.ID, &member.Title, &state); err != nil {
			return nil, err
		}
		member.State = core.TaskState(state)
		out[crateID] = append(out[crateID], member)
	}
	return out, rows.Err()
}

// ListCrateMembersLimited implements store.CrateStore in one query that
// returns at most limit plus one rows. Migration 174's index on (workspace_id,
// crate_id, created_at, id) gives the query an ordered path, and the LIMIT
// bounds the rows whichever plan the engine picks (component-crates invariant
// 20; DEC-105(4)).
func (s *Store) ListCrateMembersLimited(ctx context.Context, crateID, excludeTaskID string, limit int) ([]store.CrateMemberTask, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListCrateMembersLimited")
	if err := store.CheckCrateMemberLimit(limit); err != nil {
		return nil, false, err
	}
	rows, err := s.boundary.Query(ctx, `SELECT id, title, state FROM tasks WHERE workspace_id=$1 AND crate_id=$2 AND id<>$3 ORDER BY created_at, id LIMIT $4`, workspace(ctx), crateID, excludeTaskID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	members := []store.CrateMemberTask{}
	for rows.Next() {
		var state string
		var member store.CrateMemberTask
		if err := rows.Scan(&member.ID, &member.Title, &state); err != nil {
			return nil, false, err
		}
		member.State = core.TaskState(state)
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(members) > limit {
		return members[:limit], true, nil
	}
	return members, false, nil
}

// ListCrateActivity implements store.CrateStore: the listed event kinds of
// the crate's current members, newest first by (at, id) (component-crates
// invariant 13).
func (s *Store) ListCrateActivity(ctx context.Context, id string, query store.CrateActivityQuery) (store.CrateActivityPage, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListCrateActivity")
	limit, cursor, err := store.NormalizeCrateActivityQuery(query)
	if err != nil {
		return store.CrateActivityPage{}, err
	}
	workspaceID := workspace(ctx)
	id = strings.TrimSpace(id)
	var exists bool
	if err := s.boundary.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM crates WHERE workspace_id=$1 AND id=$2)`, workspaceID, id).Scan(&exists); err != nil {
		return store.CrateActivityPage{}, err
	}
	if !exists {
		return store.CrateActivityPage{}, &store.CrateNotFoundError{ID: id}
	}
	var cursorAt pgtype.Timestamptz
	var cursorID int64
	if cursor != nil {
		cursorAt, cursorID = timestamp(cursor.At), cursor.ID
	}
	rows, err := s.boundary.Query(ctx, `SELECT e.id, e.task_id, e.job_id, e.kind, e.actor_id, e.actor_role, e.payload_json, e.at, e.workspace_id, t.title
FROM tasks t
JOIN events e ON e.workspace_id = t.workspace_id AND e.task_id = t.id
WHERE t.workspace_id = $1 AND t.crate_id = $2 AND e.kind = ANY($3::text[])
  AND ($4::timestamptz IS NULL OR e.at < $4 OR (e.at = $4 AND e.id < $5))
ORDER BY e.at DESC, e.id DESC
LIMIT $6`, workspaceID, id, store.CrateActivityKinds, cursorAt, cursorID, limit+1)
	if err != nil {
		return store.CrateActivityPage{}, err
	}
	defer rows.Close()
	var entries []store.CrateActivityEntry
	for rows.Next() {
		var event db.Event
		var title string
		if err := rows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.ActorID, &event.ActorRole, &event.PayloadJson, &event.At, &event.WorkspaceID, &title); err != nil {
			return store.CrateActivityPage{}, err
		}
		entries = append(entries, store.CrateActivityEntry{Event: eventFromDB(event), TaskTitle: title})
	}
	if err := rows.Err(); err != nil {
		return store.CrateActivityPage{}, err
	}
	return store.PageCrateActivity(entries, limit), nil
}

// SetTaskCrate implements store.CrateStore.
func (s *Store) SetTaskCrate(ctx context.Context, taskID, crateID string) (core.Task, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "SetTaskCrate")
	crateID, err := store.NormalizeJoinCrateID(crateID)
	if err != nil {
		return core.Task{}, err
	}
	task, _, err := s.moveTaskCrate(ctx, taskID, crateID)
	return task, err
}

// JoinTaskCrate implements store.CrateStore. changed is true only when the
// committed transaction wrote the task's crate.
func (s *Store) JoinTaskCrate(ctx context.Context, taskID, crateID string) (core.Task, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "JoinTaskCrate")
	crateID, err := store.NormalizeJoinCrateID(crateID)
	if err != nil {
		return core.Task{}, false, err
	}
	return s.moveTaskCrate(ctx, taskID, crateID)
}

// ClearTaskCrate implements store.CrateStore.
func (s *Store) ClearTaskCrate(ctx context.Context, taskID string) (core.Task, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ClearTaskCrate")
	task, _, err := s.moveTaskCrate(ctx, taskID, "")
	return task, err
}

// moveTaskCrate changes only tasks.crate_id and appends one task event with
// the actor, the crate joined, and the crate left. A move to the crate the
// task already holds changes nothing and records nothing (component-crates
// invariants 1, 2, and 8; req-crates AC-2.2, AC-2.3, AC-4.1).
// changed is set inside the transaction function and read only after the
// transaction commits, so a retried or rolled-back attempt never reports it.
func (s *Store) moveTaskCrate(ctx context.Context, taskID, crateID string) (core.Task, bool, error) {
	if _, err := store.RequireActor(ctx); err != nil {
		return core.Task{}, false, err
	}
	workspaceID := workspace(ctx)
	changed := false
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		changed = false
		var current pgtype.Text
		if err := tx.QueryRow(ctx, `SELECT crate_id FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, taskID).Scan(&current); err != nil {
			return notFound(err, "task %s", taskID)
		}
		if crateID != "" {
			if err := lockJoinableCrateTx(ctx, tx, workspaceID, crateID); err != nil {
				return err
			}
		}
		if current.String == crateID {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET crate_id=$3, updated_at=now() WHERE workspace_id=$1 AND id=$2`, workspaceID, taskID, nullableText(crateID)); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, store.TaskCrateMoveEvent(taskID, crateID, current.String, store.CrateTime(time.Now()))); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return core.Task{}, false, err
	}
	task, err := s.GetTask(ctx, taskID)
	if err != nil {
		return core.Task{}, false, err
	}
	return task, changed, nil
}

// lockJoinableCrateTx reads the crate FOR SHARE, which waits for a close
// that holds the row and then reads its committed state, and refuses a crate
// that is missing, in another workspace, or closed (component-crates
// invariants 3, 6, and 7).
func lockJoinableCrateTx(ctx context.Context, tx pgx.Tx, workspaceID, crateID string) error {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM crates WHERE workspace_id=$1 AND id=$2 FOR SHARE`, workspaceID, crateID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return &store.CrateNotFoundError{ID: crateID}
	}
	if err != nil {
		return err
	}
	if store.CrateState(state) != store.CrateOpen {
		return &store.CrateClosedError{ID: crateID}
	}
	return nil
}

type crateQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func crateRequirementIDsTx(ctx context.Context, q crateQuerier, workspaceID, crateID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT requirement_id FROM crate_requirements WHERE workspace_id=$1 AND crate_id=$2 ORDER BY requirement_id`, workspaceID, crateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func crateMemberStatesTx(ctx context.Context, tx pgx.Tx, workspaceID, crateID string) ([]core.TaskState, error) {
	rows, err := tx.Query(ctx, `SELECT state FROM tasks WHERE workspace_id=$1 AND crate_id=$2`, workspaceID, crateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var states []core.TaskState
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			return nil, err
		}
		states = append(states, core.TaskState(state))
	}
	return states, rows.Err()
}

func replaceCrateRequirementsTx(ctx context.Context, tx pgx.Tx, workspaceID, crateID string, ids []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM crate_requirements WHERE workspace_id=$1 AND crate_id=$2`, workspaceID, crateID); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO crate_requirements (workspace_id, crate_id, requirement_id)
SELECT $1, $2, requirement_id FROM unnest($3::text[]) AS wanted(requirement_id)`, workspaceID, crateID, ids)
	if err != nil {
		return fmt.Errorf("record crate requirements: %w", err)
	}
	return nil
}

// crateInsightTaskColumns is db.Task's column set for the insight read's
// member rows, the scanner the requirement insight read uses. The body is
// read as empty text: no part of the insight view or the next-action
// projection reads it.
var crateInsightTaskColumns = strings.Replace(requirementInsightTaskColumns, "t.body,", "''::text AS body,", 1)

// ReadCrateInsightInputs implements store.CrateStore in four set-based
// statements whatever the member count: the crate row, the current member
// rows, and then the assignees and the events of the listed kinds for the
// member IDs those rows hold. Each claim's
// stage comes from the order the claim records, or from the order row the
// claim names when the recorded payload carries no stage (component-crates
// invariant 22).
func (s *Store) ReadCrateInsightInputs(ctx context.Context, id string) (store.CrateInsightInputs, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadCrateInsightInputs")
	workspaceID := workspace(ctx)
	id = strings.TrimSpace(id)
	crate, err := scanCrate(s.boundary.QueryRow(ctx, `SELECT `+crateColumns+` FROM crates WHERE workspace_id=$1 AND id=$2`, workspaceID, id), workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CrateInsightInputs{}, &store.CrateNotFoundError{ID: id}
	}
	if err != nil {
		return store.CrateInsightInputs{}, err
	}
	taskRows, err := s.boundary.Query(ctx, `SELECT `+crateInsightTaskColumns+`
FROM tasks t WHERE t.workspace_id=$1 AND t.crate_id=$2 ORDER BY t.created_at, t.id`, workspaceID, id)
	if err != nil {
		return store.CrateInsightInputs{}, err
	}
	persisted, err := pgx.CollectRows(taskRows, pgx.RowToStructByName[db.Task])
	if err != nil {
		return store.CrateInsightInputs{}, err
	}
	if len(persisted) == 0 {
		return store.AssembleCrateInsightInputs(crate, nil, nil), nil
	}
	tasks := make([]core.Task, len(persisted))
	ids := make([]string, len(persisted))
	for i := range persisted {
		tasks[i] = taskFromDB(persisted[i])
		ids[i] = tasks[i].ID
	}
	store.RunCrateInsightMembersReadHook()
	// Every later statement reads the member IDs the member-row statement
	// returned, scoped by workspace, so a member that leaves the crate
	// meanwhile keeps its assignee and its events (invariant 22). Events
	// match by task ID, whatever crate an older membership event names.
	if err = s.hydrateTaskAssignees(ctx, tasks); err != nil {
		return store.CrateInsightInputs{}, err
	}
	eventRows, err := s.boundary.Query(ctx, `SELECT e.task_id, e.id, e.kind, e.at,
	CASE WHEN e.kind = 'work_order.claimed' THEN NULL ELSE e.payload_json END,
	CASE WHEN e.kind = 'work_order.claimed' THEN COALESCE(NULLIF(e.payload_json->>'stage', ''), w.stage, '') ELSE '' END
FROM events e
LEFT JOIN work_orders w ON e.kind = 'work_order.claimed' AND w.workspace_id = e.workspace_id AND w.id = e.payload_json->>'id'
WHERE e.workspace_id = $1 AND e.task_id = ANY($2::text[]) AND e.kind = ANY($3::text[])
ORDER BY e.task_id, e.at, e.id`, workspaceID, ids, store.CrateInsightEventKinds)
	if err != nil {
		return store.CrateInsightInputs{}, err
	}
	defer eventRows.Close()
	var events []store.CrateInsightEventRow
	for eventRows.Next() {
		var row store.CrateInsightEventRow
		var payload []byte
		var stage string
		if err = eventRows.Scan(&row.TaskID, &row.Event.ID, &row.Event.Kind, &row.Event.At, &payload, &stage); err != nil {
			return store.CrateInsightInputs{}, err
		}
		row.Event.Payload, row.Event.Stage = payload, core.Stage(stage)
		events = append(events, row)
	}
	if err = eventRows.Err(); err != nil {
		return store.CrateInsightInputs{}, err
	}
	return store.AssembleCrateInsightInputs(crate, tasks, events), nil
}
