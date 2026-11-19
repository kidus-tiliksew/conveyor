package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var _ store.CrateStore = (*Store)(nil)

// crateColumns reads one crate row. The date reads back as YYYY-MM-DD text.
const crateColumns = `id,name,DATE_FORMAT(target_date,'%Y-%m-%d'),state,created_by,created_at,closed_at`

// crateStateKey names the claim row whose holder records a crate's state. A
// commit spanning partitions can release a lock row before the rows it wrote
// on other partitions are visible, so a join reads the state the closing
// transaction committed on the claim row itself, atomically with the release
// (component-persistence, SingleStore claimed records; component-crates
// invariant 6).
func crateStateKey(workspaceID, crateID string) string {
	return "crate-state:" + workspaceID + ":" + crateID
}

func scanCrate(row interface{ Scan(...any) error }, workspaceID string) (store.Crate, error) {
	var crate store.Crate
	var targetDate sql.NullString
	var state string
	var createdAt time.Time
	var closedAt sql.NullTime
	if err := row.Scan(&crate.ID, &crate.Name, &targetDate, &state, &crate.CreatedBy, &createdAt, &closedAt); err != nil {
		return store.Crate{}, err
	}
	crate.Workspace = workspaceID
	crate.State = store.CrateState(state)
	crate.CreatedAt = store.CrateTime(createdAt)
	if targetDate.Valid {
		date := targetDate.String
		crate.TargetDate = &date
	}
	if closedAt.Valid {
		at := store.CrateTime(closedAt.Time)
		crate.ClosedAt = &at
	}
	crate.RequirementIDs = []string{}
	return crate, nil
}

// CreateCrate implements store.CrateStore. The requirement validation, the
// record, its declared list, its state claim, and its crate.created event
// share one transaction, so a refusal records nothing (component-crates
// invariants 4, 5, and 8).
func (s *Store) CreateCrate(ctx context.Context, input store.CrateInput) (store.Crate, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CreateCrate")
	actor, err := store.RequireActor(ctx)
	if err != nil {
		return store.Crate{}, err
	}
	if input, err = store.NormalizeCrateInput(input); err != nil {
		return store.Crate{}, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.Crate{}, err
	}
	crate := store.Crate{
		ID: store.NewCrateID(), Workspace: ws, Name: input.Name, TargetDate: input.TargetDate, State: store.CrateOpen,
		CreatedBy: actor.ID, CreatedAt: store.CrateTime(time.Now()), RequirementIDs: append([]string{}, input.RequirementIDs...),
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := validateTaskContextTx(ctx, tx, ws, store.TaskContextInput{RequirementIDs: input.RequirementIDs}); err != nil {
			return err
		}
		key := crateStateKey(ws, crate.ID)
		if _, err := claimKey(ctx, tx, key); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO crates (workspace_id,id,name,target_date,state,created_by,created_at) VALUES (?,?,?,?,?,?,?)`,
			ws, crate.ID, crate.Name, nullableDate(crate.TargetDate), string(crate.State), crate.CreatedBy, crate.CreatedAt); err != nil {
			return err
		}
		if err := replaceCrateRequirementsTx(ctx, tx, ws, crate.ID, crate.RequirementIDs); err != nil {
			return err
		}
		if err := setClaim(ctx, tx, key, string(store.CrateOpen)); err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: store.CrateCreatedEvent, At: crate.CreatedAt, Payload: store.CrateCreatedPayload(crate)})
	})
	if err != nil {
		return store.Crate{}, err
	}
	store.ApplyCrateCounts(&crate, nil)
	return crate, nil
}

// UpdateCrate implements store.CrateStore. It holds the crate's state claim,
// and a changed state commits on that claim row, so a join or an intake that
// takes the claim next reads the new state (component-crates invariant 6).
func (s *Store) UpdateCrate(ctx context.Context, id string, patch store.CratePatch) (store.Crate, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "UpdateCrate")
	if _, err := store.RequireActor(ctx); err != nil {
		return store.Crate{}, err
	}
	patch, err := store.NormalizeCratePatch(patch)
	if err != nil {
		return store.Crate{}, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.Crate{}, err
	}
	id = strings.TrimSpace(id)
	var result store.Crate
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		key := crateStateKey(ws, id)
		holder, err := claimKey(ctx, tx, key)
		if err != nil {
			return err
		}
		current, err := scanCrate(tx.QueryRowContext(ctx, `SELECT `+crateColumns+` FROM crates WHERE workspace_id=? AND id=? FOR UPDATE`, ws, id), ws)
		if errors.Is(err, sql.ErrNoRows) {
			return &store.CrateNotFoundError{ID: id}
		}
		if err != nil {
			return err
		}
		if holder == string(store.CrateOpen) || holder == string(store.CrateClosed) {
			current.State = store.CrateState(holder)
		}
		if current.RequirementIDs, err = crateRequirementIDsTx(ctx, tx, ws, id); err != nil {
			return err
		}
		if patch.RequirementIDs != nil {
			if _, err := validateTaskContextTx(ctx, tx, ws, store.TaskContextInput{RequirementIDs: *patch.RequirementIDs}); err != nil {
				return err
			}
		}
		now := time.Now()
		next, kind, payload := store.ApplyCratePatch(current, patch, now)
		if kind != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE crates SET name=?, target_date=?, state=?, closed_at=? WHERE workspace_id=? AND id=?`,
				next.Name, nullableDate(next.TargetDate), string(next.State), nullableTime(next.ClosedAt), ws, id); err != nil {
				return err
			}
			if patch.RequirementIDs != nil {
				if err := replaceCrateRequirementsTx(ctx, tx, ws, id, next.RequirementIDs); err != nil {
					return err
				}
			}
			if err := setClaim(ctx, tx, key, string(next.State)); err != nil {
				return err
			}
			if err := insertWorkspaceEvent(ctx, tx, core.Event{Kind: kind, At: store.CrateTime(now), Payload: payload}); err != nil {
				return err
			}
		} else if holder == "" {
			if err := setClaim(ctx, tx, key, string(current.State)); err != nil {
				return err
			}
		}
		states, err := crateMemberStatesTx(ctx, tx, ws, id)
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
	ws, err := workspace(ctx)
	if err != nil {
		return store.CrateRecord{}, err
	}
	id = strings.TrimSpace(id)
	crate, err := scanCrate(documentRow(ctx, s.db, `SELECT `+crateColumns+` FROM crates WHERE workspace_id=? AND id=?`, ws, id), ws)
	if errors.Is(err, sql.ErrNoRows) {
		return store.CrateRecord{}, &store.CrateNotFoundError{ID: id}
	}
	if err != nil {
		return store.CrateRecord{}, err
	}
	requirementRows, err := documentRows(ctx, s.db, `SELECT requirement_id FROM crate_requirements WHERE workspace_id=? AND crate_id=? ORDER BY requirement_id`, ws, id)
	if err != nil {
		return store.CrateRecord{}, err
	}
	for requirementRows.Next() {
		var requirementID string
		if err := requirementRows.Scan(&requirementID); err != nil {
			requirementRows.Close()
			return store.CrateRecord{}, err
		}
		crate.RequirementIDs = append(crate.RequirementIDs, requirementID)
	}
	requirementRows.Close()
	if err := requirementRows.Err(); err != nil {
		return store.CrateRecord{}, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT id,state FROM tasks WHERE workspace_id=? AND crate_id=? ORDER BY created_at,id`, ws, id)
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
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT `+crateColumns+` FROM crates WHERE workspace_id=? AND (?='' OR state=?)`, ws, string(query.State), string(query.State))
	if err != nil {
		return nil, err
	}
	crates := []store.Crate{}
	for rows.Next() {
		crate, err := scanCrate(rows, ws)
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
	requirementRows, err := documentRows(ctx, s.db, `SELECT crate_id,requirement_id FROM crate_requirements WHERE workspace_id=? ORDER BY crate_id,requirement_id`, ws)
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
	memberRows, err := documentRows(ctx, s.db, `SELECT crate_id,state FROM tasks WHERE workspace_id=? AND crate_id IS NOT NULL`, ws)
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

// CrateNames implements store.CrateStore.
func (s *Store) CrateNames(ctx context.Context, ids []string) (map[string]string, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CrateNames")
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT id,name FROM crates WHERE workspace_id=? AND id IN (`+taskMapMarks(len(ids))+`)`, append([]any{ws}, taskMapArgs(ids)...)...)
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
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT crate_id,id,title,state FROM tasks WHERE workspace_id=? AND crate_id IN (`+taskMapMarks(len(ids))+`) ORDER BY crate_id,created_at,id`, append([]any{ws}, taskMapArgs(ids)...)...)
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
// returns at most limit plus one rows. Migration 0047's key on (workspace_id,
// crate_id, created_at, id) gives the query an ordered path, and the LIMIT
// bounds the rows whichever plan the engine picks (component-crates invariant
// 20; DEC-105(4)).
func (s *Store) ListCrateMembersLimited(ctx context.Context, crateID, excludeTaskID string, limit int) ([]store.CrateMemberTask, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListCrateMembersLimited")
	if err := store.CheckCrateMemberLimit(limit); err != nil {
		return nil, false, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, false, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT id,title,state FROM tasks WHERE workspace_id=? AND crate_id=? AND id<>? ORDER BY created_at,id LIMIT ?`, ws, crateID, excludeTaskID, limit+1)
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
	ws, err := workspace(ctx)
	if err != nil {
		return store.CrateActivityPage{}, err
	}
	id = strings.TrimSpace(id)
	var count int
	if err := documentRow(ctx, s.db, `SELECT COUNT(*) FROM crates WHERE workspace_id=? AND id=?`, ws, id).Scan(&count); err != nil {
		return store.CrateActivityPage{}, err
	}
	if count == 0 {
		return store.CrateActivityPage{}, &store.CrateNotFoundError{ID: id}
	}
	args := append([]any{ws, id}, taskMapArgs(store.CrateActivityKinds)...)
	position := ""
	if cursor != nil {
		position = ` AND (e.at<? OR (e.at=? AND e.id<?))`
		args = append(args, cursor.At, cursor.At, cursor.ID)
	}
	args = append(args, limit+1)
	rows, err := documentRows(ctx, s.db, `SELECT e.id,COALESCE(e.task_id,''),COALESCE(e.job_id,''),e.kind,e.actor_id,e.actor_role,e.payload_json,e.at,t.title
FROM tasks t JOIN events_columnstore e ON e.workspace_id=t.workspace_id AND e.task_id=t.id
WHERE t.workspace_id=? AND t.crate_id=? AND e.kind IN (`+taskMapMarks(len(store.CrateActivityKinds))+`)`+position+`
ORDER BY e.at DESC, e.id DESC
LIMIT ?`, args...)
	if err != nil {
		return store.CrateActivityPage{}, err
	}
	defer rows.Close()
	var entries []store.CrateActivityEntry
	for rows.Next() {
		var entry store.CrateActivityEntry
		e := &entry.Event
		if err := rows.Scan(&e.ID, &e.TaskID, &e.JobID, &e.Kind, &e.ActorID, &e.ActorRole, &e.Payload, &e.At, &entry.TaskTitle); err != nil {
			return store.CrateActivityPage{}, err
		}
		e.At = e.At.UTC()
		entries = append(entries, entry)
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
	return s.moveTaskCrate(ctx, taskID, crateID)
}

// ClearTaskCrate implements store.CrateStore.
func (s *Store) ClearTaskCrate(ctx context.Context, taskID string) (core.Task, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ClearTaskCrate")
	return s.moveTaskCrate(ctx, taskID, "")
}

// moveTaskCrate changes only tasks.crate_id under the task's command lock and
// appends one task event with the actor, the crate joined, and the crate
// left. A move to the crate the task already holds records nothing
// (component-crates invariants 1, 2, and 8; req-crates AC-2.2, AC-2.3,
// AC-4.1).
func (s *Store) moveTaskCrate(ctx context.Context, taskID, crateID string) (core.Task, error) {
	if _, err := store.RequireActor(ctx); err != nil {
		return core.Task{}, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return core.Task{}, err
	}
	err = s.taskTx(ctx, taskID, func(tx *sql.Tx) error {
		var current sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT crate_id FROM tasks WHERE workspace_id=? AND id=? FOR UPDATE`, ws, taskID).Scan(&current); err != nil {
			return notFound(err, "task %s", taskID)
		}
		if crateID != "" {
			if err := lockJoinableCrateTx(ctx, tx, ws, crateID); err != nil {
				return err
			}
		}
		if current.String == crateID {
			return nil
		}
		if err := taskWrite(ctx, tx, taskID, map[string]any{"crate_id": nullString(crateID)}); err != nil {
			return err
		}
		return taskEvent(ctx, tx, store.TaskCrateMoveEvent(taskID, crateID, current.String, store.CrateTime(time.Now())))
	})
	if err != nil {
		return core.Task{}, err
	}
	return s.GetTask(ctx, taskID)
}

// lockJoinableCrateTx takes the crate's state claim and refuses a crate that
// is missing, in another workspace, or closed (component-crates invariants
// 3, 6, and 7). The claim's holder is the committed state. A claim row that
// no crate write has set yet falls back to the crate row.
func lockJoinableCrateTx(ctx context.Context, tx *sql.Tx, ws, crateID string) error {
	holder, err := claimKey(ctx, tx, crateStateKey(ws, crateID))
	if err != nil {
		return err
	}
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM crates WHERE workspace_id=? AND id=?`, ws, crateID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		if holder == "" {
			return &store.CrateNotFoundError{ID: crateID}
		}
		state = holder
	} else if err != nil {
		return err
	}
	if holder == string(store.CrateOpen) || holder == string(store.CrateClosed) {
		state = holder
	}
	if store.CrateState(state) != store.CrateOpen {
		return &store.CrateClosedError{ID: crateID}
	}
	return nil
}

func crateRequirementIDsTx(ctx context.Context, tx *sql.Tx, ws, crateID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT requirement_id FROM crate_requirements WHERE workspace_id=? AND crate_id=? ORDER BY requirement_id`, ws, crateID)
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

func crateMemberStatesTx(ctx context.Context, tx *sql.Tx, ws, crateID string) ([]core.TaskState, error) {
	rows, err := tx.QueryContext(ctx, `SELECT state FROM tasks WHERE workspace_id=? AND crate_id=?`, ws, crateID)
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

func replaceCrateRequirementsTx(ctx context.Context, tx *sql.Tx, ws, crateID string, ids []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM crate_requirements WHERE workspace_id=? AND crate_id=?`, ws, crateID); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO crate_requirements (workspace_id,crate_id,requirement_id) VALUES (?,?,?)`, ws, crateID, id); err != nil {
			return err
		}
	}
	return nil
}

func nullableDate(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

// crateInsightTaskColumns is the task scanner's column set for the insight
// read's member rows. The body is read as empty text: no part of the
// insight view or the next-action projection reads it.
var crateInsightTaskColumns = strings.Replace(taskColumns, ",body,", ",'' AS body,", 1)

// ReadCrateInsightInputs implements store.CrateStore in three set-based
// statements without correlated subqueries, whatever the member count: the
// crate row, the current member rows, and their events of the listed kinds.
// Each claim's stage comes from the order the claim records, or from the
// order row the claim names when the recorded payload carries no stage
// (component-crates invariant 22).
func (s *Store) ReadCrateInsightInputs(ctx context.Context, id string) (store.CrateInsightInputs, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadCrateInsightInputs")
	ws, err := workspace(ctx)
	if err != nil {
		return store.CrateInsightInputs{}, err
	}
	id = strings.TrimSpace(id)
	crate, err := scanCrate(documentRow(ctx, s.db, `SELECT `+crateColumns+` FROM crates WHERE workspace_id=? AND id=?`, ws, id), ws)
	if errors.Is(err, sql.ErrNoRows) {
		return store.CrateInsightInputs{}, &store.CrateNotFoundError{ID: id}
	}
	if err != nil {
		return store.CrateInsightInputs{}, err
	}
	tasks, err := s.scanTaskMapRows(ctx, `SELECT `+crateInsightTaskColumns+` FROM tasks WHERE workspace_id=? AND crate_id=? ORDER BY created_at,id`, ws, id)
	if err != nil {
		return store.CrateInsightInputs{}, err
	}
	if len(tasks) == 0 {
		return store.AssembleCrateInsightInputs(crate, nil, nil), nil
	}
	// Events match current members by workspace and task ID, whatever
	// crate an older membership event names.
	args := append([]any{ws, id}, taskMapArgs(store.CrateInsightEventKinds)...)
	rows, err := documentRows(ctx, s.db, `SELECT e.task_id,e.id,e.kind,e.at,
	CASE WHEN e.kind='work_order.claimed' THEN NULL ELSE e.payload_json END,
	CASE WHEN e.kind='work_order.claimed' THEN COALESCE(NULLIF(JSON_EXTRACT_STRING(e.payload_json,'stage'),''),w.stage,'') ELSE '' END
FROM tasks t
JOIN events_columnstore e ON e.workspace_id=t.workspace_id AND e.task_id=t.id
LEFT JOIN work_orders w ON e.kind='work_order.claimed' AND w.workspace_id=e.workspace_id AND w.id=JSON_EXTRACT_STRING(e.payload_json,'id')
WHERE t.workspace_id=? AND t.crate_id=? AND e.kind IN (`+taskMapMarks(len(store.CrateInsightEventKinds))+`)
ORDER BY e.task_id,e.at,e.id`, args...)
	if err != nil {
		return store.CrateInsightInputs{}, err
	}
	defer rows.Close()
	var events []store.CrateInsightEventRow
	for rows.Next() {
		var row store.CrateInsightEventRow
		var payload []byte
		var stage string
		if err := rows.Scan(&row.TaskID, &row.Event.ID, &row.Event.Kind, &row.Event.At, &payload, &stage); err != nil {
			return store.CrateInsightInputs{}, err
		}
		row.Event.Payload, row.Event.Stage = payload, core.Stage(stage)
		events = append(events, row)
	}
	if err := rows.Err(); err != nil {
		return store.CrateInsightInputs{}, err
	}
	return store.AssembleCrateInsightInputs(crate, tasks, events), nil
}
