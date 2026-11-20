package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// markerTaskChunk is the number of tasks one marker statement reads: the
// latest-summary read and the fold read each bind fixed-width lists of
// exactly this many task IDs, so every chunk shares one statement text and
// the read stays proportional to the requested tasks
// (component-web-task-surfaces, Activity and list reads).
var markerTaskChunk = 100

func markerTaskBatches(taskIDs []string) [][]string {
	if len(taskIDs) == 0 {
		return nil
	}
	out := make([][]string, 0, (len(taskIDs)+markerTaskChunk-1)/markerTaskChunk)
	for start := 0; start < len(taskIDs); start += markerTaskChunk {
		out = append(out, taskIDs[start:min(start+markerTaskChunk, len(taskIDs))])
	}
	return out
}

type markerRow struct {
	taskID      string
	state       core.TaskState
	latestStage string
	lastEventAt time.Time
	lastEventID int64
	createdAt   time.Time
}

func markerTaskRowsQuery(ws string, taskIDs []string) (string, []any) {
	if taskIDs == nil {
		return `SELECT id,state,created_at FROM tasks WHERE workspace_id=? ORDER BY created_at,id`, []any{ws}
	}
	query := `SELECT id,state,created_at FROM tasks WHERE workspace_id=? AND id IN (` + taskMapMarks(len(taskIDs)) + `) ORDER BY created_at,id`
	return query, append([]any{ws}, taskMapArgs(taskIDs)...)
}

// markerTaskRows is the one state/ID selection: every listed task's state
// and creation order, or the requested tasks' states. The last-event and
// last-stage summaries are separate bounded reads, so a task whose history
// is large still costs one row (component-web-task-surfaces, Activity and
// list reads).
func (s *Store) markerTaskRows(ctx context.Context, taskIDs []string) ([]markerRow, error) {
	if taskIDs != nil {
		taskIDs = store.UniqueTaskIDs(taskIDs)
		if len(taskIDs) == 0 {
			return nil, nil
		}
	}
	query, args := markerTaskRowsQuery(documentWorkspace(ctx), taskIDs)
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []markerRow{}
	for rows.Next() {
		var row markerRow
		if err = rows.Scan(&row.taskID, &row.state, &row.createdAt); err != nil {
			return nil, err
		}
		row.lastEventAt = row.createdAt
		out = append(out, row)
	}
	return out, rows.Err()
}

// markerLatestSummariesQuery reads, for one chunk of tasks, one row per task:
// its newest (at,id) event tuple, the stage of its newest job by
// (started_at,id), and the stage of its newest claimed order by
// (execution_started_at,created_at,id). Each part is one grouped read of the
// chunk's rows inside the database, joined to the chunk's task rows. Every
// list binds exactly markerTaskChunk placeholders, so every chunk shares one
// statement text and SingleStore compiles one plan for the read. One arm per
// task with three LIMIT 1 subqueries, in place of the grouped reads, ran for
// minutes per chunk on SingleStore, longer than the response memo lifetime
// (component-web-task-surfaces, Activity and list reads; DEC-140(3)).
func markerLatestSummariesQuery(ws string, taskIDs []string) (string, []any) {
	b := &markerBinder{}
	events := func(prefix string) string {
		return prefix + `workspace_id=` + b.bind(ws) + ` AND ` + b.fixedIn(prefix+"event_shard_key", taskIDs) + ` AND ` + b.fixedIn(prefix+"task_id", taskIDs)
	}
	query := `SELECT t.id,newest_event.at,newest_event.id,newest_job.stage,newest_claim.stage FROM tasks t
			LEFT JOIN (
				SELECT e.task_id,e.at,MAX(e.id) AS id FROM events_columnstore e JOIN (
					SELECT task_id,MAX(at) AS at FROM events_columnstore WHERE ` + events("") + ` GROUP BY task_id
				) newest_at ON newest_at.task_id=e.task_id AND newest_at.at=e.at
				WHERE ` + events("e.") + ` GROUP BY e.task_id,e.at
			) newest_event ON newest_event.task_id=t.id
			LEFT JOIN (
				SELECT task_id,stage FROM (
					SELECT task_id,stage,ROW_NUMBER() OVER (PARTITION BY task_id ORDER BY started_at DESC,id DESC) AS position
					FROM jobs WHERE workspace_id=` + b.bind(ws) + ` AND ` + b.fixedIn("task_id", taskIDs) + `
				) ranked_jobs WHERE position=1
			) newest_job ON newest_job.task_id=t.id
			LEFT JOIN (
				SELECT task_id,stage FROM (
					SELECT task_id,stage,ROW_NUMBER() OVER (PARTITION BY task_id ORDER BY execution_started_at DESC,created_at DESC,id DESC) AS position
					FROM work_orders WHERE workspace_id=` + b.bind(ws) + ` AND state='claimed' AND ` + b.fixedIn("task_id", taskIDs) + `
				) ranked_claims WHERE position=1
			) newest_claim ON newest_claim.task_id=t.id
			WHERE t.workspace_id=` + b.bind(ws) + ` AND ` + b.fixedIn("t.id", taskIDs)
	return b.finish(query)
}

// markerLatestSummaries folds the latest claim stage, job stage, and event
// tuple into the rows. It reads one row per requested task: the database
// groups each task's history and returns only its newest tuple.
func (s *Store) markerLatestSummaries(ctx context.Context, rows []markerRow) error {
	byTask := make(map[string]*markerRow, len(rows))
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].taskID
		byTask[rows[i].taskID] = &rows[i]
	}
	for _, chunk := range markerTaskBatches(ids) {
		if err := s.markerLatestTuples(ctx, byTask, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) markerLatestTuples(ctx context.Context, byTask map[string]*markerRow, taskIDs []string) error {
	query, args := markerLatestSummariesQuery(documentWorkspace(ctx), taskIDs)
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID string
		var at sql.NullTime
		var id sql.NullInt64
		var jobStage, claimStage sql.NullString
		if err = rows.Scan(&taskID, &at, &id, &jobStage, &claimStage); err != nil {
			return err
		}
		row := byTask[taskID]
		if row == nil {
			continue
		}
		if at.Valid {
			row.lastEventAt = at.Time
		}
		if id.Valid {
			row.lastEventID = id.Int64
		}
		if jobStage.Valid {
			row.latestStage = jobStage.String
		}
		if claimStage.Valid {
			row.latestStage = claimStage.String
		}
	}
	return rows.Err()
}

// markerFoldRows reads the fold inputs for the page's tasks in chunks, each
// chunk with up to four statements:
//
//   - The narrow read (markerFoldQuery) selects, by kind and task alone,
//     the rows whose payloads are small: forge kinds, bounces, and the
//     review kinds of review tasks.
//   - The lifecycle read (markerLifecycleQuery) runs only for a chunk with
//     an open review order. It reads the claims and renewals of the tasks
//     that have one and reduces them. When a kept claim does not supersede
//     the claims the reduction left out (store.MarkerClaimBackfill), the
//     statement is read again without claim reduction and only that result
//     is used.
//   - The wide reads (markerWideRows) run only for the tasks the narrow rows
//     call for: submissions of a task with a merge failure, and claims of a
//     task from its last user change request on.
//
// Go then applies the exact row tests and the payload projection
// (markerFoldEvents). The page must list its tasks; an empty page issues no
// statement.
func (s *Store) markerFoldRows(ctx context.Context, in store.MarkerFoldInput) ([]core.Event, error) {
	out := []core.Event{}
	if in.Page == nil {
		return nil, errors.New("singlestore marker fold read needs an explicit task page")
	}
	for _, chunk := range markerTaskBatches(in.Page) {
		chunkSet := make(map[string]bool, len(chunk))
		for _, id := range chunk {
			chunkSet[id] = true
		}
		within := func(ids []string) []string {
			kept := make([]string, 0, len(ids))
			for _, id := range ids {
				if chunkSet[id] {
					kept = append(kept, id)
				}
			}
			return kept
		}
		part := store.MarkerFoldInput{Workspace: in.Workspace, Page: chunk, ReviewTasks: within(in.ReviewTasks), LiveReviewTasks: within(in.LiveReviewTasks), Now: in.Now, KeepAllClaims: in.KeepAllClaims}
		for _, order := range in.OpenReviewOrders {
			if chunkSet[order.TaskID] {
				part.OpenReviewOrders = append(part.OpenReviewOrders, order)
			}
		}
		query, args := markerFoldQuery(part)
		rows, err := s.markerFoldScan(ctx, query, args)
		if err != nil {
			return nil, err
		}
		if len(part.LiveReviewTasks) > 0 {
			query, args = markerLifecycleQuery(part)
			lifecycle, err := s.markerFoldScan(ctx, query, args)
			if err != nil {
				return nil, err
			}
			if partitions := store.MarkerClaimBackfill(lifecycle, part.OpenReviewOrders); len(partitions) > 0 {
				// Read every claim and renewal of the chunk again without
				// claim reduction, in one statement, so the review folds
				// read one snapshot.
				part.KeepAllClaims = true
				query, args = markerLifecycleQuery(part)
				if lifecycle, err = s.markerFoldScan(ctx, query, args); err != nil {
					return nil, err
				}
			}
			rows = append(rows, lifecycle...)
		}
		wide, err := s.markerWideRows(ctx, part, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, markerFoldEvents(append(rows, wide...))...)
	}
	// Statements and distributed execution return rows in physical order;
	// the lifecycle folds require the ledger's (at,id) order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].ID < out[j].ID
		}
		return out[i].At.Before(out[j].At)
	})
	return out, nil
}

// markerWideNeeds says which wide rows the folds can act on, given the
// narrow rows of one chunk.
//
// Submissions: store.LatestForgeFailure reads a work_order.updated event only
// to learn which head a merge failure belongs to and whether a later
// submission replaced that head. Both need a merge.failed event, so a task
// without one reads no update.
//
// Claims: store.UserRequestChangesPending is true exactly when no implement
// claim follows the task's last user change request. A claim before that
// request cannot change the answer, so a task reads its claims from the
// instant of its last user change request on, and a task without one reads
// none. A claim at that same instant is read too; the fold orders it by
// (at, id). The request is tested with the fold's own decoder
// (store.MarkerEventFoldable), so a bounce that only resembles one never
// moves the instant later. A task with an open review order is left out: the
// lifecycle read already carries its claims for the review folds.
type markerWideNeeds struct {
	submissionTasks []string
	// claims lists each task that reads claims with the instant of its last
	// user change request.
	claims []markerClaimNeed
}

// markerClaimNeed is one task whose claims at or after since the request
// fold can act on.
type markerClaimNeed struct {
	taskID string
	since  time.Time
}

func markerWideNeedsOf(in store.MarkerFoldInput, rows []store.MarkerFoldRow) markerWideNeeds {
	live := make(map[string]bool, len(in.LiveReviewTasks))
	for _, id := range in.LiveReviewTasks {
		live[id] = true
	}
	failed := map[string]bool{}
	requested := map[string]time.Time{}
	for _, row := range rows {
		switch row.Event.Kind {
		case "merge.failed":
			failed[row.Event.TaskID] = true
		case "pipeline.bounced":
			if !live[row.Event.TaskID] && store.MarkerEventFoldable(row.Event) && row.Event.At.After(requested[row.Event.TaskID]) {
				requested[row.Event.TaskID] = row.Event.At
			}
		}
	}
	var needs markerWideNeeds
	// The page order keeps the bound lists, and so the statements, stable.
	for _, id := range in.Page {
		if failed[id] {
			needs.submissionTasks = append(needs.submissionTasks, id)
		}
		if at, ok := requested[id]; ok {
			needs.claims = append(needs.claims, markerClaimNeed{taskID: id, since: at})
		}
	}
	return needs
}

// markerWideRows reads the wide rows the narrow rows of one chunk call for
// (markerWideNeeds), with at most one statement per kind. A chunk whose
// tasks have no merge failure and no user change request reads none.
func (s *Store) markerWideRows(ctx context.Context, in store.MarkerFoldInput, rows []store.MarkerFoldRow) ([]store.MarkerFoldRow, error) {
	needs := markerWideNeedsOf(in, rows)
	var out []store.MarkerFoldRow
	if len(needs.submissionTasks) > 0 {
		query, args := markerSubmissionQuery(in.Workspace, needs.submissionTasks)
		wide, err := s.markerFoldScan(ctx, query, args)
		if err != nil {
			return nil, err
		}
		out = append(out, wide...)
	}
	if len(needs.claims) > 0 {
		query, args := markerRequestClaimQuery(in.Workspace, needs.claims)
		wide, err := s.markerFoldScan(ctx, query, args)
		if err != nil {
			return nil, err
		}
		out = append(out, wide...)
	}
	return out, nil
}

func (s *Store) markerFoldScan(ctx context.Context, query string, args []any) ([]store.MarkerFoldRow, error) {
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.MarkerFoldRow{}
	for rows.Next() {
		var row store.MarkerFoldRow
		if err = rows.Scan(&row.Event.ID, &row.Event.TaskID, &row.Event.JobID, &row.Event.Kind, &row.Event.Payload, &row.Event.At, &row.Dropped, &row.DroppedImplement, &row.FutureDropped); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// The rest of this file is the SQL of the SingleStore activity-marker fold
// read. The backend owns all of its SQL (component-persistence invariant 2);
// the shared store package holds only the pure Go facts the folds read, the
// claim read-back decision, and the folds' own row tests
// (store.MarkerPayloadProjections, store.MarkerClaimBackfill,
// store.MarkerEventFoldable).
//
// The read has five statement texts: the narrow read, the lifecycle read
// with reduced claims or with every claim, and the two wide reads. Each list
// a statement binds has a fixed width, markerTaskChunk, and the open review
// orders travel as one JSON array, so neither the page size nor the number
// of open review orders changes a text. SingleStore compiles one plan per
// statement text, so the read compiles at most five plans
// (component-web-task-surfaces invariant 19).
//
// What a statement may do with a payload follows from how SingleStore
// stores one. It keeps the keys of a JSON column apart, so selecting the
// compact payload is cheap even for a work_order.updated or
// work_order.claimed event, whose payload holds a whole work order with its
// governance snapshot, 100 KB and more. A text test on the stored payload,
// such as INSTR, is not cheap: the engine builds the whole document for
// every row that reaches the test, and which rows reach it is the engine's
// choice, not the statement's. On a production workspace with a long history
// those text tests were the whole cost of the read: 2 to 8 seconds per
// 100-task chunk, where the same rows without a test read in under half a
// second, and where a test on bounces alone still added a second. So:
//
//   - The narrow read and the wide reads test no payload. They select by
//     kind, bound task lists, and instants, and Go applies the exact tests
//     (store.MarkerEventFoldable) and the folds.
//   - What bounds their rows is which tasks they read: a submission only for
//     a task with a merge failure, a claim only from a task's last user
//     change request on.
//   - Only the lifecycle read tests payloads, to reduce the claims and
//     renewals of the tasks with an open review order. Those are few tasks,
//     and the statement selects their rows by a bound task list first.

func markerPlaceholder(int) string { return "?" }

// markerCompact is the compact payload column: snapshot bodies are not
// marker inputs and never cross the connection.
const markerCompact = compactEventPayloadColumn

// markerBinder collects the bound arguments of one statement. Bind
// returns a token; finish numbers the tokens in the statement's text order,
// so a placeholder always meets its own argument.
type markerBinder struct {
	values []any
}

const markerArgToken = "\x00"

// Bind records one argument and returns its token.
func (b *markerBinder) bind(value any) string {
	b.values = append(b.values, value)
	return markerArgToken + strconv.Itoa(len(b.values)-1) + markerArgToken
}

// fixedIn is an IN list of exactly markerTaskChunk placeholders. A nonempty
// list is padded with its own first member, so the padding matches only rows
// the list already matches. An empty list is padded with the empty string,
// which no task ID equals; the caller pairs it with markerBinder.flag.
func (b *markerBinder) fixedIn(col string, values []string) string {
	marks := make([]string, markerTaskChunk)
	for i := range marks {
		value := ""
		switch {
		case i < len(values):
			value = values[i]
		case len(values) > 0:
			value = values[0]
		}
		marks[i] = b.bind(value)
	}
	return col + " IN (" + strings.Join(marks, ",") + ")"
}

// flag binds 1 for a nonempty optional list and 0 for an empty one.
func (b *markerBinder) flag(values []string) string {
	if len(values) == 0 {
		return b.bind(int64(0))
	}
	return b.bind(int64(1))
}

func (b *markerBinder) finish(query string) (string, []any) {
	parts := strings.Split(query, markerArgToken)
	var out strings.Builder
	args := make([]any, 0, len(parts)/2)
	for i, part := range parts {
		if i%2 == 0 {
			out.WriteString(part)
			continue
		}
		index, err := strconv.Atoi(part)
		if err != nil || index < 0 || index >= len(b.values) {
			panic("marker SQL holds a malformed argument token")
		}
		args = append(args, b.values[index])
		out.WriteString(markerPlaceholder(len(args)))
	}
	return out.String(), args
}

func markerQuotedSQL(kinds []string) string {
	quoted := make([]string, len(kinds))
	for i, kind := range kinds {
		quoted[i] = "'" + kind + "'"
	}
	return strings.Join(quoted, ",")
}

// markerMayName is a necessary condition for Go to decode a JSON string equal
// to value anywhere in the payload: both backends store JSON normalized, so a
// value made of characters JSON writes plainly appears in the text between
// quotes. It never excludes a row Go would read as value.
func markerMayName(column, value string) string {
	return `(INSTR(` + column + `,'"` + value + `"')>0)`
}

// markerLeaseKey is the exact top-level lease key as the claim text spells
// it.
const markerLeaseKey = `'"lease_expires_at"'`

// markerUTCLeaseShape is the only lease text the claim fast path reads: the
// UTC form Go's time.Time.MarshalJSON writes, with an optional fraction. The
// pattern holds no `?`, which the driver reads as a bind placeholder.
const markerUTCLeaseShape = `^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]([.][0-9]+){0,1}Z$`

// markerPastLease is true only for a claim whose top-level lease Go decodes
// as an instant at least one second before the read's clock second. It
// accepts only plain ASCII text without a backslash, so no key or value is
// escaped and no non-ASCII key can fold to the lease key; it requires the
// lowercased text to hold the quoted lease key exactly as often as the text
// itself, so no case variant of the key is present for Go to match; and it
// reads only the exact UTC shape, whose first 19 bytes order like the
// instant. Any other claim, including a missing lease, an offset or
// same-second lease, and a malformed one, is kept.
func markerPastLease(b *markerBinder, now string) string {
	lease := "JSON_EXTRACT_STRING(event_payload,'lease_expires_at')"
	return `(LENGTH(event_payload)=CHAR_LENGTH(event_payload) AND INSTR(event_payload,CHAR(92))=0
				AND LENGTH(REPLACE(CAST(LOWER(event_payload) AS BINARY),` + markerLeaseKey + `,''))=LENGTH(REPLACE(CAST(event_payload AS BINARY),` + markerLeaseKey + `,''))
				AND ` + lease + ` ` + markerRegexOp + ` '` + markerUTCLeaseShape + `'
				AND ASCII(SUBSTRING(` + lease + `,11,1))=84 AND ASCII(RIGHT(` + lease + `,1))=90
				AND CAST(LEFT(` + lease + `,19) AS BINARY)<CAST(` + now + ` AS BINARY))`
}

// markerFoldColumns are the result columns of the narrow and wide reads: the
// compact event and no claim left out.
const markerFoldColumns = `e.id,e.task_id,COALESCE(e.job_id,'') AS job_id,e.kind,` + markerCompact + ` AS event_payload,e.at AS event_at,0 AS dropped,0 AS dropped_implement,0 AS future_dropped`

// markerFoldQuery is the narrow activity-marker fold read of one chunk of
// tasks: every forge kind and every bounce of the page's tasks, and the
// review lifecycle and review-round kinds of its review tasks. It selects by
// kind and bound task list alone and tests no payload. Go keeps of the
// bounces only a user change request (store.MarkerEventFoldable). It selects
// no work_order.updated event, no claim, and no renewal: the lifecycle read
// and the wide reads own those. Open review orders and KeepAllClaims do not
// change it (component-web-task-surfaces invariant 19).
//
// The result columns of every fold statement are id, task_id, job_id, kind,
// event_payload (the compact payload), event_at, dropped,
// dropped_implement, and future_dropped, in no particular order.
func markerFoldQuery(in store.MarkerFoldInput) (string, []any) {
	b := &markerBinder{}
	forge := store.MarkerForgePageKinds()
	review := append([]string{"work_order.released", "review.completed", "review.accepted", "task.setup.changed"}, store.MarkerReviewRoundKinds...)
	candidates := append(append([]string{"pipeline.bounced"}, review...), forge...)
	return b.finish(`SELECT ` + markerFoldColumns + `
			FROM events_columnstore e
			WHERE e.workspace_id=` + b.bind(in.Workspace) + ` AND e.task_id IS NOT NULL AND ` + b.fixedIn("e.task_id", in.Page) + `
			AND e.kind IN (` + markerQuotedSQL(candidates) + `)
			AND (e.kind='pipeline.bounced'
			OR e.kind IN (` + markerQuotedSQL(forge) + `)
			OR (e.kind IN (` + markerQuotedSQL(review) + `) AND ` + b.flag(in.ReviewTasks) + `=1 AND ` + b.fixedIn("e.task_id", in.ReviewTasks) + `))`)
}

// markerSubmissionQuery reads every work_order.updated event of the listed
// tasks, which have a merge failure (markerWideNeeds). It tests no payload;
// Go keeps only an implement order submitted with a head
// (store.MarkerEventFoldable). tasks must not be empty.
func markerSubmissionQuery(ws string, tasks []string) (string, []any) {
	b := &markerBinder{}
	return b.finish(`SELECT ` + markerFoldColumns + `
			FROM events_columnstore e
			WHERE e.workspace_id=` + b.bind(ws) + ` AND e.task_id IS NOT NULL AND ` + b.fixedIn("e.task_id", tasks) + `
			AND e.kind='work_order.updated'`)
}

// markerRequestClaimQuery reads, for each listed task, its claims at or
// after the task's instant (markerWideNeeds). It tests no payload: the fold
// itself tells an implement claim from any other. It leaves no claim of that
// span out, so the claim read-back never applies to these rows. The task
// list selects the rows, and the per-task instants, a fixed number of pairs
// padded with the first, narrow them. needs must not be empty.
func markerRequestClaimQuery(ws string, needs []markerClaimNeed) (string, []any) {
	b := &markerBinder{}
	tasks := make([]string, len(needs))
	for i, need := range needs {
		tasks[i] = need.taskID
	}
	list := b.fixedIn("e.task_id", tasks)
	pairs := make([]string, markerTaskChunk)
	for i := range pairs {
		need := needs[0]
		if i < len(needs) {
			need = needs[i]
		}
		pairs[i] = `(e.task_id=` + b.bind(need.taskID) + ` AND e.at>=` + b.bind(need.since.UTC()) + `)`
	}
	return b.finish(`SELECT ` + markerFoldColumns + `
			FROM events_columnstore e
			WHERE e.workspace_id=` + b.bind(ws) + ` AND e.task_id IS NOT NULL AND ` + list + `
			AND e.kind='work_order.claimed'
			AND (` + strings.Join(pairs, " OR ") + `)`)
}

// markerOpenOrders is the JSON array the reduced lifecycle statement binds:
// one object per open review order with its task, job, and order ID, and
// safe=0 for an ID outside the store.MarkerSafeOrderID alphabet.
func markerOpenOrders(orders []store.MarkerReviewOrder) string {
	type openOrder struct {
		TaskID string `json:"task_id"`
		JobID  string `json:"job_id"`
		ID     string `json:"id"`
		Safe   int    `json:"safe"`
	}
	out := make([]openOrder, 0, len(orders))
	for _, order := range orders {
		safe := 0
		if store.MarkerSafeOrderID(order.ID) {
			safe = 1
		}
		out = append(out, openOrder{TaskID: order.TaskID, JobID: order.JobID, ID: order.ID, Safe: safe})
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		panic("marker open review orders do not encode: " + err.Error())
	}
	return string(encoded)
}

// markerLifecycleQuery reads the claims and renewals of the chunk's tasks
// that have an open review order, in.LiveReviewTasks, which must not be
// empty. It keeps one claim per job where the folds read only the newest
// and the renewals the review folds read (component-web-task-surfaces
// invariant 19).
//
// Renewals: the last valid renewal for diagnostics and the earliest future
// renewal for the horizon; only the simple renewal payload the store writes
// is eligible, and other shapes keep their complete history. The lease
// decoder reads only renewals.
//
// Claims: an older claim is left out only when its job ID is not empty, its
// payload cannot name an open review order of another job of its task, no
// open review order of the task has an ID outside the safe alphabet, and its
// lease is provably past (markerPastLease); a future, same-second,
// uncertain, or missing lease keeps the claim. The newest claim carries
// dropped, the number left out, and dropped_implement, how many of those may
// name the implement stage. No claim with a lease that can be future is left
// out, so future_dropped is always 0. The caller decodes the newest claims
// and reads the statement again with KeepAllClaims when they do not
// supersede the claims left out (store.MarkerClaimBackfill).
//
// The statement's payload tests meet only the claims and renewals of the
// bound tasks: marker_rows and the open review order match both select
// their rows by that list.
func markerLifecycleQuery(in store.MarkerFoldInput) (string, []any) {
	b := &markerBinder{}
	const carry = "id,task_id,job_id,kind,event_payload,event_at"
	rows := `SELECT e.id,e.task_id,COALESCE(e.job_id,'') AS job_id,e.kind,` + markerCompact + ` AS event_payload,e.at AS event_at
				FROM events_columnstore e
				WHERE e.workspace_id=` + b.bind(in.Workspace) + ` AND e.task_id IS NOT NULL AND ` + b.fixedIn("e.task_id", in.LiveReviewTasks) + `
				AND e.kind IN ('work_order.claimed','work_order.lease_renewed')`
	// Only the simple renewal payload the store writes is eligible for the
	// renewal reduction; other renewal shapes keep their complete history.
	const attemptField = `"attempt_id"[[:space:]]*:[[:space:]]*"[^"]*"`
	const leaseField = `"lease_expires_at"[[:space:]]*:[[:space:]]*"[0-9T:Z+.,-]+"`
	const comma = `[[:space:]]*,[[:space:]]*`
	const renewalShape = `^[[:space:]]*[{][[:space:]]*(` + attemptField + comma + leaseField + `|` + leaseField + `(` + comma + attemptField + `){0,1})[[:space:]]*[}][[:space:]]*$`
	instants := markerLeaseInstantSQL(`SELECT `+carry+`,
				JSON_EXTRACT_STRING(event_payload,'lease_expires_at') AS lease_text,
				CASE WHEN event_payload `+markerRegexOp+` '`+renewalShape+`'
					AND COALESCE(JSON_GET_TYPE(JSON_EXTRACT_JSON(event_payload,'lease_expires_at')),'')='string'
					THEN 1 ELSE 0 END AS lease_string
				FROM marker_rows WHERE kind='work_order.lease_renewed'`, carry)
	const future = `(lease_sec>now_sec OR (lease_sec=now_sec AND lease_nanos>now_nanos))`
	query := `WITH marker_rows AS (` + rows + `),
			marker_clock AS (SELECT CAST(` + b.bind(in.Now.Unix()) + ` AS SIGNED) AS now_sec,CAST(` + b.bind(int64(in.Now.Nanosecond())) + ` AS SIGNED) AS now_nanos),
			renewal_instants AS (` + instants + `),
			ranked_renewals AS (
				SELECT ` + carry + `,lease_ok,lease_sec,lease_nanos,now_sec,now_nanos,
					ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY lease_ok DESC,event_at DESC,id DESC) AS latest_rank,
					ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY
						CASE WHEN lease_ok=1 AND ` + future + ` THEN 0 ELSE 1 END,
						lease_sec,lease_nanos,event_at,id) AS horizon_rank
				FROM renewal_instants CROSS JOIN marker_clock
			)`
	const renewals = `
			SELECT ` + carry + `,0 AS dropped,0 AS dropped_implement,0 AS future_dropped FROM ranked_renewals
			WHERE lease_ok=0 OR latest_rank=1 OR (horizon_rank=1 AND ` + future + `)
			UNION ALL`
	if in.KeepAllClaims {
		return b.finish(query + renewals + `
			SELECT ` + carry + `,0,0,0 FROM marker_rows WHERE kind='work_order.claimed'`)
	}
	return b.finish(query + `,
			named_claims AS (
				SELECT DISTINCT e.id AS named_id
				FROM events_columnstore e JOIN (
					SELECT JSON_EXTRACT_STRING(table_col,'task_id') AS order_task,JSON_EXTRACT_STRING(table_col,'job_id') AS order_job,
						CONCAT('"',JSON_EXTRACT_STRING(table_col,'id'),'"') AS order_needle,JSON_EXTRACT_BIGINT(table_col,'safe') AS order_safe
					FROM TABLE(JSON_TO_ARRAY(` + b.bind(markerOpenOrders(in.OpenReviewOrders)) + `))) o ON e.task_id=o.order_task
				WHERE e.workspace_id=` + b.bind(in.Workspace) + ` AND ` + b.fixedIn("e.task_id", in.LiveReviewTasks) + ` AND e.kind='work_order.claimed'
				AND (o.order_safe<>1 OR (COALESCE(e.job_id,'')<>o.order_job AND INSTR(e.payload_json,o.order_needle)>0))
			),
			claim_facts AS (
				SELECT ` + carry + `,
					CASE WHEN job_id<>'' AND named_id IS NULL AND ` + markerPastLease(b, b.bind(in.Now.UTC().Format("2006-01-02T15:04:05"))) + ` THEN 1 ELSE 0 END AS reducible,
					CASE WHEN ` + markerMayName("event_payload", string(core.StageImplement)) + ` THEN 1 ELSE 0 END AS maybe_implement
				FROM marker_rows LEFT JOIN named_claims ON named_id=id WHERE kind='work_order.claimed'
			),
			claim_counts AS (
				SELECT ` + carry + `,reducible,maybe_implement,
					ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY event_at DESC,id DESC) AS claim_rank,
					SUM(reducible) OVER (PARTITION BY task_id,job_id) AS reducible_total,
					SUM(reducible*maybe_implement) OVER (PARTITION BY task_id,job_id) AS implement_total
				FROM claim_facts
			)` + renewals + `
			SELECT ` + carry + `,CASE WHEN claim_rank=1 THEN reducible_total-reducible ELSE 0 END,
				CASE WHEN claim_rank=1 THEN implement_total-reducible*maybe_implement ELSE 0 END,0
			FROM claim_counts WHERE claim_rank=1 OR reducible=0`)
}

// markerFoldEvents turns the rows of one chunk into fold inputs. The claim
// read-back has already run on the compact claims. Go then leaves out each
// bounce and update the folds cannot act on, using the folds' own decoders
// (store.MarkerEventFoldable), and sends each other payload as its fold keys
// where a top-level key scan proves Go decodes them the same
// (markerProjectPayload). Claims keep their whole compact payload.
func markerFoldEvents(rows []store.MarkerFoldRow) []core.Event {
	out := make([]core.Event, 0, len(rows))
	for _, row := range rows {
		event := row.Event
		if !store.MarkerEventFoldable(event) {
			continue
		}
		event.Payload = markerProjectPayload(event.Kind, event.Payload)
		out = append(out, event)
	}
	return out
}
