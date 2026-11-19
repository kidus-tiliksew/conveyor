package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// markerFoldRows reads the fold inputs of one page. When a kept claim does
// not supersede the claims the reduction left out (store.MarkerClaimBackfill),
// it reads the whole page again without claim reduction and uses only that
// result.
func (s *Store) markerFoldRows(ctx context.Context, in store.MarkerFoldInput) ([]core.Event, error) {
	query, args := markerFoldQuery(in)
	rows, err := s.markerFoldScan(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if partitions := store.MarkerClaimBackfill(rows, in.OpenReviewOrders); len(partitions) > 0 {
		// Read the whole page again without claim reduction, in one statement,
		// so every fold input comes from one snapshot.
		full := in
		full.KeepAllClaims = true
		query, args = markerFoldQuery(full)
		if rows, err = s.markerFoldScan(ctx, query, args); err != nil {
			return nil, err
		}
	}
	out := make([]core.Event, len(rows))
	for i := range rows {
		out[i] = rows[i].Event
	}
	return out, nil
}

func (s *Store) markerFoldScan(ctx context.Context, query string, args []any) ([]store.MarkerFoldRow, error) {
	rows, err := s.boundary.Query(ctx, query, args...)
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

// The rest of this file is the PostgreSQL activity-marker fold read. The backend owns all of
// its SQL (component-persistence invariant 2); the shared store package holds
// only the pure Go facts the folds read and the claim read-back decision
// (store.MarkerPayloadProjections, store.MarkerClaimBackfill).

func markerPlaceholder(n int) string { return fmt.Sprintf("$%d", n) }

// markerScope is the FROM and WHERE clause of the read: the events of the
// workspace's page tasks, or of every task when the page is empty.
func markerScope(b *markerBinder, ws string, page []string) string {
	workspace, tasks := b.bind(ws), b.bind(emptyIfNil(page))
	return `FROM events e JOIN tasks t ON t.id=e.task_id
		WHERE t.workspace_id=` + workspace + `
		AND (cardinality(` + tasks + `::text[])=0 OR t.id=ANY(` + tasks + `::text[]))`
}

// markerCompact is the compact payload column: snapshot bodies are not
// marker inputs and never cross the connection.
const markerCompact = compactEventPayloadColumn

func markerInList(b *markerBinder, col string, values []string) string {
	return col + "=ANY(" + b.bind(values) + "::text[])"
}
func markerInt(placeholder string) string { return placeholder + "::bigint" }
func markerText(json string) string       { return "(" + json + ")::text" }

// markerKeys is the JSON array text of an object's top-level keys, NULL for
// any other value.
func markerKeys(json string) string {
	return "(CASE WHEN jsonb_typeof(" + json + ")='object' THEN (SELECT COALESCE(jsonb_agg(k),'[]'::jsonb) FROM jsonb_object_keys(" + json + ") AS k)::text END)"
}
func markerField(json, key string) string     { return "(" + json + ")->'" + key + "'" }
func markerFieldText(json, key string) string { return "(" + json + ")->>'" + key + "'" }
func markerIsString(value string) string {
	return "(COALESCE(jsonb_typeof(" + value + "),'')='string')"
}
func markerObject(pairs []string) string {
	return "jsonb_build_object(" + strings.Join(pairs, ",") + ")"
}
func markerContains(text, literal string) string {
	return "(strpos(" + text + "," + literal + ")>0)"
}
func markerLower(text string) string { return "lower(" + text + ")" }

// markerStripped is the byte length of text with every case-sensitive
// occurrence of literal removed.
func markerStripped(text, literal string) string {
	return "octet_length(replace(" + text + "," + literal + ",''))"
}

// markerASCII reports whether text holds only ASCII characters, tested by
// character range so a single-byte server encoding, where octet and
// character lengths always agree, cannot pass a non-ASCII character.
func markerASCII(text string) string { return "(" + text + ` ~ '^[\x01-\x7f]*$')` }

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

func (b *markerBinder) in(col string, values []string) string {
	if len(values) == 0 {
		return "1=0"
	}
	return markerInList(b, col, values)
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

// keyUnambiguous is true when the row's payload is an object whose top-level
// keys (k_text, and k_lower lowered) are ASCII and none of them matches key
// case-insensitively without being key itself. Go's json decoder matches a
// struct field by any key that folds to its name, so only then is the value
// at the exact key the value Go decodes. Both backends store JSON
// normalized: escapes decoded and a duplicated key collapsed to its last
// value, as Go keeps the last value.
func markerKeyUnambiguous(key string) string {
	quoted := `'"` + key + `"'`
	// Both the key list and its lowered form must be ASCII: lower() under a
	// single-byte encoding and locale can map an ASCII letter outside ASCII.
	return fmt.Sprintf("(k_text IS NOT NULL AND %s AND %s AND %s=%s)", markerASCII("k_text"), markerASCII("k_lower"), markerStripped("k_text", quoted), markerStripped("k_lower", quoted))
}

// mayEqual is false only when Go cannot decode the string value at key as
// value: the payload holds no JSON string equal to value, or the key is
// unambiguous and its value differs. It never excludes a row Go would read
// as value.
func markerMayEqual(key, value string) string {
	return fmt.Sprintf("(%s AND (NOT %s OR COALESCE(%s,'')='%s'))",
		markerContains(markerText("event_payload"), `'"`+value+`"'`), markerKeyUnambiguous(key), markerFieldText("event_payload", key), value)
}

// projectedPayload returns the payload the folds read: the kind's keys when
// every one of them is unambiguous, and the compact payload otherwise.
func markerProjectedPayload() string {
	branches := make([]string, 0, len(store.MarkerPayloadProjections))
	for _, projection := range store.MarkerPayloadProjections {
		checks := make([]string, len(projection.Keys))
		pairs := make([]string, 0, 2*len(projection.Keys))
		for i, key := range projection.Keys {
			checks[i] = markerKeyUnambiguous(key)
			pairs = append(pairs, "'"+key+"'", markerField("event_payload", key))
		}
		branches = append(branches, fmt.Sprintf("WHEN kind IN (%s) AND %s THEN %s", markerQuotedSQL(projection.Kinds), strings.Join(checks, " AND "), markerObject(pairs)))
	}
	return "CASE " + strings.Join(branches, " ") + " ELSE event_payload END"
}

// markerRowsSQL is the marker_rows common table expression: the compact
// rows each fold can act on, with their top-level key lists. A bounce only
// from user-request-changes, an update only for an implement order
// submitted, a claim only of an implement order or of a task with an open
// review order, every forge kind, the review lifecycle and review-round
// kinds of review tasks, and renewals of tasks with an open review order. The tests read the
// payload text, so a row Go might decode as selected is kept.
func markerRowsSQL(b *markerBinder, in store.MarkerFoldInput) string {
	forge := store.MarkerForgePageKinds()
	review := append([]string{"work_order.released", "review.completed", "review.accepted", "task.setup.changed"}, store.MarkerReviewRoundKinds...)
	candidates := append(append([]string{"pipeline.bounced", "work_order.updated", "work_order.claimed", "work_order.lease_renewed"}, review...), forge...)
	implement := markerMayEqual("stage", string(core.StageImplement))
	return `scoped_rows AS (
			SELECT e.id,e.task_id,COALESCE(e.job_id,'') AS job_id,e.kind,` + markerCompact + ` AS event_payload,e.at AS event_at
			` + markerScope(b, in.Workspace, in.Page) + `
			AND e.kind IN (` + markerQuotedSQL(candidates) + `)
		),
		keyed_rows AS (SELECT id,task_id,job_id,kind,event_payload,event_at,` + markerKeys("event_payload") + ` AS k_text FROM scoped_rows),
		folded_rows AS (SELECT id,task_id,job_id,kind,event_payload,event_at,k_text,` + markerLower("k_text") + ` AS k_lower FROM keyed_rows),
		marker_rows AS (
			SELECT id,task_id,job_id,kind,event_payload,event_at,k_text,k_lower FROM folded_rows
			WHERE (kind='pipeline.bounced' AND ` + markerMayEqual("source", store.UserRequestChangesSource) + `)
			OR (kind='work_order.updated' AND ` + implement + ` AND ` + markerMayEqual("state", string(core.WorkOrderSubmitted)) + `)
			OR (kind='work_order.claimed' AND (` + implement + ` OR ` + b.in("task_id", in.LiveReviewTasks) + `))
			OR kind IN (` + markerQuotedSQL(forge) + `)
			OR (kind IN (` + markerQuotedSQL(review) + `) AND ` + b.in("task_id", in.ReviewTasks) + `)
			OR (kind='work_order.lease_renewed' AND ` + b.in("task_id", in.LiveReviewTasks) + `)
		)`
}

// markerFoldQuery is the activity-marker fold read of one page of tasks. It
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
// reads the page again with KeepAllClaims when they do not supersede them
// (store.MarkerClaimBackfill).
//
// The result columns are id, task_id, job_id, kind, event_payload, event_at,
// dropped, dropped_implement, and future_dropped, ordered by event_at and id.
func markerFoldQuery(in store.MarkerFoldInput) (string, []any) {
	b := &markerBinder{}
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
		query = `WITH ` + markerRowsSQL(b, in) + `,
		ranked_claims AS (
			SELECT ` + carry + `,
				ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY event_at DESC,id DESC) AS claim_rank,
				` + markerReducibleOrNone(in, "CASE WHEN job_id<>'' THEN 1 ELSE 0 END") + ` AS reducible,
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
		leaseString := markerIsString(markerField("event_payload", "lease_expires_at"))
		instants := markerLeaseInstantSQL(`SELECT `+carry+`,
			`+markerFieldText("event_payload", "lease_expires_at")+` AS lease_text,
			CASE WHEN kind='work_order.claimed' AND `+leaseString+` THEN 1
				WHEN kind='work_order.lease_renewed' AND `+markerText("event_payload")+` `+markerRegexOp+` '`+renewalShape+`' AND `+leaseString+` THEN 1
				ELSE 0 END AS lease_string
			FROM marker_rows WHERE kind IN ('work_order.lease_renewed','work_order.claimed')`, carry)
		const future = `(lease_sec>now_sec OR (lease_sec=now_sec AND lease_nanos>now_nanos))`
		query = `WITH ` + markerRowsSQL(b, in) + `,
		marker_clock AS (SELECT ` + markerInt(b.bind(in.Now.Unix())) + ` AS now_sec,` + markerInt(b.bind(int64(in.Now.Nanosecond()))) + ` AS now_nanos),
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
				CASE WHEN COALESCE(` + markerKeyUnambiguous("lease_expires_at") + `,1=0) THEN 1 ELSE 0 END AS lease_key
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
				` + markerLiveReducible(b, in) + ` AS reducible,
				CASE WHEN ` + markerMayEqual("stage", string(core.StageImplement)) + ` THEN 1 ELSE 0 END AS maybe_implement,
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
	query += `		SELECT id,task_id,job_id,kind,` + markerProjectedPayload() + ` AS event_payload,event_at,dropped,dropped_implement,future_dropped
		FROM kept ORDER BY event_at,id`
	return b.finish(query)
}

// markerReducibleOrNone is the claim reduction rule, or no reduction when the
// read must keep every claim: a read-back after store.MarkerClaimBackfill
// reruns the whole page in one statement, so every fold input comes from one
// snapshot.
func markerReducibleOrNone(in store.MarkerFoldInput, rule string) string {
	if in.KeepAllClaims {
		return "0"
	}
	return rule
}

// markerLiveReducible is the claim reduction rule of a page with an open
// review order: an older claim of a job is left out only when it names no
// other open review order, its lease key is unambiguous, and it holds no
// lease after now or is not the job's earliest future claim.
func markerLiveReducible(b *markerBinder, in store.MarkerFoldInput) string {
	if in.KeepAllClaims {
		return "0"
	}
	return `CASE WHEN job_id<>'' AND (NOT (` + b.in("task_id", in.LiveReviewTasks) + `) OR (` + markerIdentitySafe(b, in) + ` AND lease_key=1
					AND (future_lease=0 OR claim_horizon_rank>1)))
					THEN 1 ELSE 0 END`
}

// markerIdentitySafe is true for a claim whose payload names no open review
// order of its task other than the one its job runs: Go matches a lifecycle
// event to an order by job ID or by a payload id or review_work_order_id
// string equal to the order ID, and a JSON string equal to an ID made of
// markerSafeOrderID characters appears in the text as the quoted ID. A task
// with an order outside that alphabet keeps every claim.
func markerIdentitySafe(b *markerBinder, in store.MarkerFoldInput) string {
	var unsafeTasks, tasks, jobs, quotedIDs []string
	for _, order := range in.OpenReviewOrders {
		if !store.MarkerSafeOrderID(order.ID) {
			unsafeTasks = append(unsafeTasks, order.TaskID)
			continue
		}
		quoted, _ := json.Marshal(order.ID)
		tasks, jobs, quotedIDs = append(tasks, order.TaskID), append(jobs, order.JobID), append(quotedIDs, string(quoted))
	}
	// The orders are three bound arrays, so the statement's parameter count
	// does not grow with the number of open review orders.
	safe := "1=1"
	if len(tasks) > 0 {
		safe = "NOT EXISTS (SELECT 1 FROM unnest(" + b.bind(tasks) + "::text[]," + b.bind(jobs) + "::text[]," + b.bind(quotedIDs) + "::text[]) AS open_order(order_task,order_job,order_quoted)" +
			" WHERE open_order.order_task=task_id AND open_order.order_job<>job_id AND " + markerContains(markerText("event_payload"), "open_order.order_quoted") + ")"
	}
	if len(unsafeTasks) > 0 {
		safe += " AND NOT (" + b.in("task_id", unsafeTasks) + ")"
	}
	return "(" + safe + ")"
}
