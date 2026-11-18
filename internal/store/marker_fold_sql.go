package store

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// MarkerSQLDialect holds the spellings of the activity-marker fold read that
// differ between the durable backends. MarkerFoldSQL writes the rest of the
// statement once, so both backends select, reduce, and project the same rows
// (component-web-task-surfaces invariant 19; component-task-lifecycle,
// Compact event reads).
type MarkerSQLDialect struct {
	Lease LeaseSQLDialect
	// Placeholder returns the bind marker of the n-th argument (1-based) in
	// the statement's text order.
	Placeholder func(n int) string
	// Scope returns the FROM and WHERE clause of the event rows: the events
	// of the workspace's page tasks, aliased e. A nil page means every task
	// of the workspace.
	Scope func(b *MarkerSQLBinder, workspace string, page []string) string
	// Compact is the compact payload column of the scoped events: snapshot
	// bodies are not marker inputs and never cross the connection.
	Compact string
	// InList returns a predicate that col is one of values (never empty).
	InList func(b *MarkerSQLBinder, col string, values []string) string
	// Int casts a bound integer parameter.
	Int func(placeholder string) string
	// Text renders a JSON value as its normalized text.
	Text func(json string) string
	// Keys renders the JSON array text of an object's top-level keys and
	// NULL for any other value.
	Keys func(json string) string
	// Field renders the JSON value of a top-level key, SQL NULL when absent.
	Field func(json, key string) string
	// FieldText renders the string value of a top-level key.
	FieldText func(json, key string) string
	// IsString reports whether a JSON value rendered by Field is a string.
	IsString func(value string) string
	// Object builds a JSON object from alternating key literals and values.
	Object func(pairs []string) string
	// Contains reports whether text holds a literal; it may match more
	// than a case-sensitive search, never less.
	Contains func(text, literal string) string
	// Lower lowers ASCII letters of text.
	Lower func(text string) string
	// Stripped returns the byte length of text with every case-sensitive
	// occurrence of literal removed.
	Stripped func(text, literal string) string
	// ASCII reports whether text holds only ASCII bytes.
	ASCII func(text string) string
}

// MarkerSQLBinder collects the bound arguments of one statement. Bind
// returns a token; finish numbers the tokens in the statement's text order,
// so a positional placeholder always meets its own argument.
type MarkerSQLBinder struct {
	dialect MarkerSQLDialect
	values  []any
}

const markerArgToken = "\x00"

// Bind records one argument and returns its token.
func (b *MarkerSQLBinder) Bind(value any) string {
	b.values = append(b.values, value)
	return markerArgToken + strconv.Itoa(len(b.values)-1) + markerArgToken
}

func (b *MarkerSQLBinder) in(col string, values []string) string {
	if len(values) == 0 {
		return "1=0"
	}
	return b.dialect.InList(b, col, values)
}

func (b *MarkerSQLBinder) finish(query string) (string, []any) {
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
		out.WriteString(b.dialect.Placeholder(len(args)))
	}
	return out.String(), args
}

// MarkerReviewOrder names an open review order of a page task: one whose
// claim and renewal events the review folds read.
type MarkerReviewOrder struct {
	TaskID string
	ID     string
	JobID  string
}

// MarkerFoldInput is the page an activity-marker fold read covers.
type MarkerFoldInput struct {
	Workspace string
	// Page lists the read's tasks; nil reads every task of the workspace.
	Page []string
	// ReviewTasks have at least one review order; their review lifecycle
	// kinds feed the review folds.
	ReviewTasks []string
	// LiveReviewTasks have an open (claimed or queued) review order; every
	// claim and renewal of theirs feeds the review folds.
	LiveReviewTasks []string
	// OpenReviewOrders are the open review orders of LiveReviewTasks.
	OpenReviewOrders []MarkerReviewOrder
	Now              time.Time
}

// markerForgeKeys are the keys store.LatestForgeFailure decodes from a forge
// failure kind.
var markerForgeKeys = []string{"review_work_order_id", "work_order_id", "forge_error_category", "last_error", "error", "automatic", "head_sha", "expected_head_sha"}

// markerProjections lists, per group of event kinds, the keys every fold that
// reads the kinds decodes. A payload whose keys the database can prove Go
// matches exactly is sent as these keys only (component-web-task-surfaces
// invariant 19). The decoders are store.UserRequestChangesPending (bounce),
// store.LatestForgeFailure (forge kinds and the implement submission), and
// the review folds' reviewLifecycleEventMatches, ReviewVerdictDiagnostics,
// ActivityReviewLeaseHorizon, completedReviewWorkOrderIDs, and
// SupersededReviewWorkOrders (review lifecycle kinds). A claim decodes as a
// whole work order: the database would have to prove every typed field of
// the work order decodes before leaving one out, and that proof made the
// SingleStore statement too costly to compile, so a claim keeps its compact
// payload.
var markerProjections = func() []struct {
	kinds []string
	keys  []string
} {
	forge := make([]string, 0, len(ForgeFailureEventKinds))
	for _, kind := range ForgeFailureEventKinds {
		if kind != "work_order.updated" {
			forge = append(forge, kind)
		}
	}
	return []struct {
		kinds []string
		keys  []string
	}{
		{[]string{"pipeline.bounced"}, []string{"source"}},
		{[]string{"work_order.updated"}, []string{"id", "stage", "state", "head_sha"}},
		{forge, markerForgeKeys},
		{[]string{"work_order.lease_renewed"}, []string{"id", "review_work_order_id", "lease_expires_at"}},
		{[]string{"work_order.released", "review.accepted"}, []string{"id", "review_work_order_id"}},
		{[]string{"review.completed"}, []string{"id", "review_work_order_id", "review_round"}},
		{[]string{"task.setup.changed"}, []string{"id", "review_work_order_id", "review_transition"}},
	}
}()

// markerSafeOrderID is the order-ID alphabet whose JSON string text equals
// the ID between quotes on both backends, so a text search for the quoted
// ID finds every payload that names it.
var markerSafeOrderID = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

func quotedSQL(kinds []string) string {
	quoted := make([]string, len(kinds))
	for i, kind := range kinds {
		quoted[i] = "'" + kind + "'"
	}
	return strings.Join(quoted, ",")
}

// keyUnambiguous is true when the row's payload is an object whose top-level
// keys (k_text, and k_lower lowered) are ASCII and none of them matches key
// case-insensitively without being key itself. Go's json decoder matches a
// struct field by any key that folds to its name, so only then is the value
// at the exact key the value Go decodes. Both backends store JSON
// normalized: escapes decoded and a duplicated key collapsed to its last
// value, as Go keeps the last value.
func (d MarkerSQLDialect) keyUnambiguous(key string) string {
	quoted := `'"` + key + `"'`
	return fmt.Sprintf("(k_text IS NOT NULL AND %s AND %s=%s)", d.ASCII("k_text"), d.Stripped("k_text", quoted), d.Stripped("k_lower", quoted))
}

// mayEqual is false only when Go cannot decode the string value at key as
// value: the payload holds no JSON string equal to value, or the key is
// unambiguous and its value differs. It never excludes a row Go would read
// as value.
func (d MarkerSQLDialect) mayEqual(key, value string) string {
	return fmt.Sprintf("(%s AND (NOT %s OR COALESCE(%s,'')='%s'))",
		d.Contains(d.Text("event_payload"), `'"`+value+`"'`), d.keyUnambiguous(key), d.FieldText("event_payload", key), value)
}

// projectedPayload returns the payload the folds read: the kind's keys when
// every one of them is unambiguous, and the compact payload otherwise.
func (d MarkerSQLDialect) projectedPayload() string {
	branches := make([]string, 0, len(markerProjections))
	for _, projection := range markerProjections {
		checks := make([]string, len(projection.keys))
		pairs := make([]string, 0, 2*len(projection.keys))
		for i, key := range projection.keys {
			checks[i] = d.keyUnambiguous(key)
			pairs = append(pairs, "'"+key+"'", d.Field("event_payload", key))
		}
		branches = append(branches, fmt.Sprintf("WHEN kind IN (%s) AND %s THEN %s", quotedSQL(projection.kinds), strings.Join(checks, " AND "), d.Object(pairs)))
	}
	return "CASE " + strings.Join(branches, " ") + " ELSE event_payload END"
}

// markerRowsSQL is the marker_rows common table expression: the compact
// rows each fold can act on, with their top-level key lists. A bounce only
// from user-request-changes, an update only for an implement order
// submitted, a claim only of an implement order or of a task with an open
// review order, every forge kind, the review lifecycle kinds of review
// tasks, and renewals of tasks with an open review order. The tests read the
// payload text, so a row Go might decode as selected is kept.
func markerRowsSQL(d MarkerSQLDialect, b *MarkerSQLBinder, in MarkerFoldInput) string {
	forge := make([]string, 0, len(ForgeFailureEventKinds))
	for _, kind := range ForgeFailureEventKinds {
		if kind != "work_order.updated" {
			forge = append(forge, kind)
		}
	}
	candidates := append([]string{"pipeline.bounced", "work_order.updated", "work_order.claimed", "work_order.lease_renewed", "work_order.released", "review.completed", "review.accepted", "task.setup.changed"}, forge...)
	implement := d.mayEqual("stage", string(core.StageImplement))
	return `scoped_rows AS (
			SELECT e.id,e.task_id,COALESCE(e.job_id,'') AS job_id,e.kind,` + d.Compact + ` AS event_payload,e.at AS event_at
			` + d.Scope(b, in.Workspace, in.Page) + `
			AND e.kind IN (` + quotedSQL(candidates) + `)
		),
		keyed_rows AS (SELECT id,task_id,job_id,kind,event_payload,event_at,` + d.Keys("event_payload") + ` AS k_text FROM scoped_rows),
		folded_rows AS (SELECT id,task_id,job_id,kind,event_payload,event_at,k_text,` + d.Lower("k_text") + ` AS k_lower FROM keyed_rows),
		marker_rows AS (
			SELECT id,task_id,job_id,kind,event_payload,event_at,k_text,k_lower FROM folded_rows
			WHERE (kind='pipeline.bounced' AND ` + d.mayEqual("source", UserRequestChangesSource) + `)
			OR (kind='work_order.updated' AND ` + implement + ` AND ` + d.mayEqual("state", string(core.WorkOrderSubmitted)) + `)
			OR (kind='work_order.claimed' AND (` + implement + ` OR ` + b.in("task_id", in.LiveReviewTasks) + `))
			OR kind IN (` + quotedSQL(forge) + `)
			OR (kind IN ('work_order.released','review.completed','review.accepted','task.setup.changed') AND ` + b.in("task_id", in.ReviewTasks) + `)
			OR (kind='work_order.lease_renewed' AND ` + b.in("task_id", in.LiveReviewTasks) + `)
		)`
}

// MarkerFoldSQL is the activity-marker fold read of one page of tasks. It
// selects the rows each fold can act on (markerRowsSQL), keeps one claim per
// order where the folds read only the newest, and sends each payload's fold
// keys where the database proves Go decodes them the same
// (component-web-task-surfaces invariant 19).
//
// Renewals: the last valid renewal for diagnostics and the earliest future
// renewal for the horizon; only the simple renewal payload the store writes
// is eligible, and other shapes keep their complete history.
//
// Claims: an older claim of the same task and job is left out when no fold
// can read it. Without an open review order, the newest claim of the job
// supersedes it when that claim decodes. With one, the claim must also name
// no other open review order of the task, have an unambiguous lease key, and
// either hold no lease after now or not be the job's earliest claim with a
// lease after now. The newest claim carries dropped, the number left out,
// and dropped_implement, how many of those may name the implement stage; the
// earliest future claim carries future_dropped, how many claims with a
// later future lease were left out. The caller decodes those claims and
// reads the left-out claims back with MarkerClaimHistorySQL when they do not
// supersede them (MarkerClaimBackfill).
//
// The result columns are id, task_id, job_id, kind, event_payload, event_at,
// dropped, dropped_implement, and future_dropped, ordered by event_at and id.
func MarkerFoldSQL(d MarkerSQLDialect, in MarkerFoldInput) (string, []any) {
	b := &MarkerSQLBinder{dialect: d}
	const carry = "id,task_id,job_id,kind,event_payload,event_at,k_text,k_lower"
	const counted = `counted_claims AS (
			SELECT ` + carry + `,claim_rank,reducible,horizon_claim,
				SUM(CASE WHEN claim_rank>1 AND reducible=1 THEN 1 ELSE 0 END) OVER (PARTITION BY task_id,job_id) AS dropped,
				SUM(CASE WHEN claim_rank>1 AND reducible=1 AND maybe_implement=1 THEN 1 ELSE 0 END) OVER (PARTITION BY task_id,job_id) AS dropped_implement,
				SUM(CASE WHEN claim_rank>1 AND reducible=1 AND future_lease=1 THEN 1 ELSE 0 END) OVER (PARTITION BY task_id,job_id) AS future_dropped
			FROM ranked_claims
		)`
	const keptClaims = `SELECT ` + carry + `,
				CASE WHEN claim_rank=1 THEN dropped ELSE 0 END,CASE WHEN claim_rank=1 THEN dropped_implement ELSE 0 END,
				CASE WHEN horizon_claim=1 THEN future_dropped ELSE 0 END
			FROM counted_claims WHERE claim_rank=1 OR reducible=0`
	var query string
	if len(in.LiveReviewTasks) == 0 {
		// Without an open review order no fold reads a lease: no renewal is
		// selected, and every selected claim may name the implement stage,
		// so only the newest claim of each job can matter while it decodes.
		query = `WITH ` + markerRowsSQL(d, b, in) + `,
		ranked_claims AS (
			SELECT ` + carry + `,
				ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY event_at DESC,id DESC) AS claim_rank,
				CASE WHEN job_id<>'' THEN 1 ELSE 0 END AS reducible,
				1 AS maybe_implement,0 AS future_lease,0 AS horizon_claim
			FROM marker_rows WHERE kind='work_order.claimed'
		),
		` + counted + `,
		kept AS (
			SELECT ` + carry + `,0 AS dropped,0 AS dropped_implement,0 AS future_dropped FROM marker_rows
			WHERE kind<>'work_order.claimed'
			UNION ALL
			` + keptClaims + `
		)
`
	} else {
		// One lease decoder serves renewals and claims. Only the simple
		// renewal payload the store writes is eligible for the renewal
		// reduction; other renewal shapes keep their complete history. Go
		// decodes a claim's lease only when the whole work order decodes, so
		// an invalid lease, a missing one, or the zero time never lowers the
		// horizon, and lease_ok=1 is exactly Go's non-zero decode.
		const attemptField = `"attempt_id"[[:space:]]*:[[:space:]]*"[^"]*"`
		const leaseField = `"lease_expires_at"[[:space:]]*:[[:space:]]*"[0-9T:Z+.,-]+"`
		const comma = `[[:space:]]*,[[:space:]]*`
		const renewalShape = `^[[:space:]]*[{][[:space:]]*(` + attemptField + comma + leaseField + `|` + leaseField + `(` + comma + attemptField + `){0,1})[[:space:]]*[}][[:space:]]*$`
		leaseString := d.IsString(d.Field("event_payload", "lease_expires_at"))
		instants := d.Lease.LeaseInstantSQL(`SELECT `+carry+`,
			`+d.FieldText("event_payload", "lease_expires_at")+` AS lease_text,
			CASE WHEN kind='work_order.claimed' AND `+leaseString+` THEN 1
				WHEN kind='work_order.lease_renewed' AND `+d.Text("event_payload")+` `+d.Lease.RegexOp+` '`+renewalShape+`' AND `+leaseString+` THEN 1
				ELSE 0 END AS lease_string
			FROM marker_rows WHERE kind IN ('work_order.lease_renewed','work_order.claimed')`, carry)
		const future = `(lease_sec>now_sec OR (lease_sec=now_sec AND lease_nanos>now_nanos))`
		query = `WITH ` + markerRowsSQL(d, b, in) + `,
		marker_clock AS (SELECT ` + d.Int(b.Bind(in.Now.Unix())) + ` AS now_sec,` + d.Int(b.Bind(int64(in.Now.Nanosecond()))) + ` AS now_nanos),
		lease_instants AS (SELECT ` + carry + `,lease_ok,lease_sec,lease_nanos,now_sec,now_nanos FROM (` + instants + `) decoded CROSS JOIN marker_clock),
		ranked_renewals AS (
			SELECT ` + carry + `,lease_ok,lease_sec,lease_nanos,now_sec,now_nanos,
				ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY lease_ok DESC,event_at DESC,id DESC) AS latest_rank,
				ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY
					CASE WHEN lease_ok=1 AND ` + future + ` THEN 0 ELSE 1 END,
					lease_sec,lease_nanos,event_at,id) AS horizon_rank
			FROM lease_instants WHERE kind='work_order.lease_renewed'
		),
		claim_facts AS (
			SELECT ` + carry + `,lease_ok,lease_sec,lease_nanos,
				CASE WHEN lease_ok=1 AND ` + future + ` THEN 1 ELSE 0 END AS future_lease,
				CASE WHEN COALESCE(` + d.keyUnambiguous("lease_expires_at") + `,1=0) THEN 1 ELSE 0 END AS lease_key
			FROM lease_instants WHERE kind='work_order.claimed'
		),
		ordered_claims AS (
			SELECT ` + carry + `,lease_ok,future_lease,lease_key,
				ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY event_at DESC,id DESC) AS claim_rank,
				ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY
					CASE WHEN future_lease=1 AND lease_key=1 THEN 0 ELSE 1 END,
					lease_sec,lease_nanos,event_at,id) AS claim_horizon_rank
			FROM claim_facts
		),
		ranked_claims AS (
			SELECT ` + carry + `,claim_rank,future_lease,
				CASE WHEN job_id<>'' AND (NOT (` + b.in("task_id", in.LiveReviewTasks) + `) OR (` + markerIdentitySafe(d, b, in) + ` AND lease_key=1
					AND (future_lease=0 OR claim_horizon_rank>1)))
					THEN 1 ELSE 0 END AS reducible,
				CASE WHEN ` + d.mayEqual("stage", string(core.StageImplement)) + ` THEN 1 ELSE 0 END AS maybe_implement,
				CASE WHEN claim_horizon_rank=1 AND future_lease=1 AND lease_key=1 THEN 1 ELSE 0 END AS horizon_claim
			FROM ordered_claims
		),
		` + counted + `,
		kept AS (
			SELECT ` + carry + `,0 AS dropped,0 AS dropped_implement,0 AS future_dropped FROM marker_rows
			WHERE kind NOT IN ('work_order.lease_renewed','work_order.claimed')
			UNION ALL
			SELECT ` + carry + `,0,0,0 FROM ranked_renewals
			WHERE lease_ok=0 OR latest_rank=1 OR (horizon_rank=1 AND ` + future + `)
			UNION ALL
			` + keptClaims + `
		)
`
	}
	query += `		SELECT id,task_id,job_id,kind,` + d.projectedPayload() + ` AS event_payload,event_at,dropped,dropped_implement,future_dropped
		FROM kept ORDER BY event_at,id`
	return b.finish(query)
}

// markerIdentitySafe is true for a claim whose payload names no open review
// order of its task other than the one its job runs: Go matches a lifecycle
// event to an order by job ID or by a payload id or review_work_order_id
// string equal to the order ID, and a JSON string equal to an ID made of
// markerSafeOrderID characters appears in the text as the quoted ID. A task
// with an order outside that alphabet keeps every claim.
func markerIdentitySafe(d MarkerSQLDialect, b *MarkerSQLBinder, in MarkerFoldInput) string {
	var unsafeTasks []string
	var terms []string
	for _, order := range in.OpenReviewOrders {
		if !markerSafeOrderID.MatchString(order.ID) {
			unsafeTasks = append(unsafeTasks, order.TaskID)
			continue
		}
		quoted, _ := json.Marshal(order.ID)
		terms = append(terms, fmt.Sprintf("(task_id=%s AND job_id<>%s AND %s)",
			b.Bind(order.TaskID), b.Bind(order.JobID), d.Contains(d.Text("event_payload"), b.Bind(string(quoted)))))
	}
	safe := "1=1"
	if len(terms) > 0 {
		safe = "NOT (" + strings.Join(terms, " OR ") + ")"
	}
	if len(unsafeTasks) > 0 {
		safe += " AND NOT (" + b.in("task_id", unsafeTasks) + ")"
	}
	return "(" + safe + ")"
}

// MarkerClaimPartition names the claims of one task and job.
type MarkerClaimPartition struct {
	TaskID string
	JobID  string
}

// MarkerFoldRow is one row of MarkerFoldSQL.
type MarkerFoldRow struct {
	Event            core.Event
	Dropped          int64
	DroppedImplement int64
	FutureDropped    int64
}

// MarkerClaimBackfill returns the task and job claim histories whose left-out
// claims the folds may still read. The newest claim that MarkerFoldSQL kept
// must decode as a work order, name the implement stage when a left-out
// claim may, and carry a non-zero lease when an open review order runs on
// its job; otherwise an earlier claim can still clear the user's change
// request, or give an open review order its lease. The earliest future
// claim must decode; otherwise a left-out claim's lease can be the review
// lease horizon. The caller reads such a history back whole
// (MarkerClaimHistorySQL).
func MarkerClaimBackfill(rows []MarkerFoldRow, openReviewOrders []MarkerReviewOrder) []MarkerClaimPartition {
	openJobs := map[MarkerClaimPartition]bool{}
	for _, order := range openReviewOrders {
		openJobs[MarkerClaimPartition{TaskID: order.TaskID, JobID: order.JobID}] = true
	}
	var out []MarkerClaimPartition
	seen := map[MarkerClaimPartition]bool{}
	for _, row := range rows {
		if row.Event.Kind != "work_order.claimed" || (row.Dropped == 0 && row.FutureDropped == 0) {
			continue
		}
		key := MarkerClaimPartition{TaskID: row.Event.TaskID, JobID: row.Event.JobID}
		var claimed core.WorkOrder
		decoded := json.Unmarshal(row.Event.Payload, &claimed) == nil
		// The earliest future claim stands for the later future claims left
		// out: it must decode for its lease to count.
		supersedes := decoded
		if row.Dropped == 0 {
			if !supersedes && !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
			continue
		}
		if row.DroppedImplement > 0 && claimed.Stage != core.StageImplement {
			supersedes = false
		}
		if openJobs[key] && claimed.LeaseExpiresAt.IsZero() {
			supersedes = false
		}
		if !supersedes && !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// MarkerClaimHistorySQL reads every claim of the listed task and job
// histories with MarkerFoldSQL's selection and compact payload. The result
// columns match MarkerFoldSQL's, with zero counts.
func MarkerClaimHistorySQL(d MarkerSQLDialect, in MarkerFoldInput, partitions []MarkerClaimPartition) (string, []any) {
	b := &MarkerSQLBinder{dialect: d}
	rows := markerRowsSQL(d, b, in)
	terms := make([]string, len(partitions))
	for i, partition := range partitions {
		terms[i] = fmt.Sprintf("(task_id=%s AND job_id=%s)", b.Bind(partition.TaskID), b.Bind(partition.JobID))
	}
	return b.finish(`WITH ` + rows + `
		SELECT id,task_id,job_id,kind,event_payload,event_at,0 AS dropped,0 AS dropped_implement,0 AS future_dropped FROM marker_rows
		WHERE kind='work_order.claimed' AND (` + strings.Join(terms, " OR ") + `)
		ORDER BY event_at,id`)
}

// MergeMarkerFoldRows adds the backfilled claims to the fold rows, once each,
// in the ledger's (at, id) order.
func MergeMarkerFoldRows(rows, backfill []MarkerFoldRow) []MarkerFoldRow {
	if len(backfill) == 0 {
		return rows
	}
	seen := make(map[int64]bool, len(rows))
	for _, row := range rows {
		seen[row.Event.ID] = true
	}
	for _, row := range backfill {
		if !seen[row.Event.ID] {
			seen[row.Event.ID] = true
			rows = append(rows, row)
		}
	}
	slices.SortStableFunc(rows, func(a, b MarkerFoldRow) int {
		if c := a.Event.At.Compare(b.Event.At); c != 0 {
			return c
		}
		switch {
		case a.Event.ID < b.Event.ID:
			return -1
		case a.Event.ID > b.Event.ID:
			return 1
		}
		return 0
	})
	return rows
}
