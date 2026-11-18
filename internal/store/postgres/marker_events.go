package postgres

import "github.com/kidus-tiliksew/conveyor/internal/store"

// Snapshot bodies are not marker inputs and must not cross the connection.
const markerEventPayloadColumn = compactEventPayloadColumn

// Only simple renewal payloads can be reduced without changing Go's decoding
// or payload-identity matching. Other shapes retain their complete history.
func boundedMarkerFoldQuery(query string) string {
	const attemptField = `"attempt_id"[[:space:]]*:[[:space:]]*"[^"]*"`
	const leaseField = `"lease_expires_at"[[:space:]]*:[[:space:]]*"[0-9T:Z+.,-]+"`
	const comma = `[[:space:]]*,[[:space:]]*`
	const renewalShape = `^[[:space:]]*[{][[:space:]]*(` + attemptField + comma + leaseField + `|` + leaseField + `(` + comma + attemptField + `){0,1})[[:space:]]*[}][[:space:]]*$`
	const carry = "id,task_id,job_id,event_payload,event_at"
	instants := store.PostgresLeaseSQL.LeaseInstantSQL(`SELECT `+carry+`,
		event_payload->>'lease_expires_at' AS lease_text,
		CASE WHEN event_payload::text ~ '`+renewalShape+`'
			AND jsonb_typeof(event_payload->'lease_expires_at')='string'
			THEN 1 ELSE 0 END AS lease_string
		FROM marker_rows WHERE kind='work_order.lease_renewed'`, carry)
	const future = `(lease_sec>now_sec OR (lease_sec=now_sec AND lease_nanos>now_nanos))`
	return `WITH marker_rows AS (` + query + `),
		marker_clock AS (SELECT $6::bigint AS now_sec,$7::bigint AS now_nanos),
		renewal_instants AS (` + instants + `),
		ranked_renewals AS (
			SELECT ` + carry + `,lease_ok,lease_sec,lease_nanos,now_sec,now_nanos,
				ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY lease_ok DESC,event_at DESC,id DESC) AS latest_rank,
				ROW_NUMBER() OVER (PARTITION BY task_id,job_id ORDER BY
					CASE WHEN lease_ok=1 AND ` + future + ` THEN 0 ELSE 1 END,
					lease_sec,lease_nanos,event_at,id) AS horizon_rank
			FROM renewal_instants CROSS JOIN marker_clock
		)
		SELECT id,task_id,job_id,kind,event_payload,event_at FROM marker_rows WHERE kind<>'work_order.lease_renewed'
		UNION ALL
		SELECT id,task_id,job_id,'work_order.lease_renewed',event_payload,event_at FROM ranked_renewals
		WHERE lease_ok=0 OR latest_rank=1 OR (horizon_rank=1 AND ` + future + `)
		ORDER BY event_at,id`
}
