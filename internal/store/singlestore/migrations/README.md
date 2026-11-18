# SingleStore migration rules

These numbers belong to this backend; version one is a fresh schema, not a replay of PostgreSQL migrations.
Published SQL and recorded checksums are immutable, with one exception: migrations 0001, 0009, 0023, 0024, 0029, 0037 and 0040 were edited to create selected history tables as columnstore on new databases. `publishedChecksums` accepts their original checksums without rewriting ledger rows or existing tables. Any later correction to a published file is a new numbered migration (DEC-139(1)).
A new table is columnstore only when it has at most one unique key and no reader locks its rows with `FOR UPDATE`. `task_context_proposals` is the one exception, because each of its `FOR UPDATE` reads first locks the owning `tasks` or `planning_sessions` row (DEC-139(2)).
`task_event_positions` (migration 0043) is a rowstore support table: each workspace row is the task-event position the peer check reads.
DDL commits implicitly, so new migrations must tolerate an interrupted startup.
Normal startup applies pending numbered migrations under the backend migration lock.
Migration 0027 keeps its original ledger identity but its retired automatic event conversion is not executed.

Current audit events use operator-created `events_columnstore`; legacy `events` remains historical input.
`governance_snapshots` is columnstore: append-only, write-once, keyed by `(workspace_id, digest)`.
Fresh installations create the durable queue's `event_log` as columnstore, sorted by
workspace and position. Its stream/version hash key is non-unique; append transactions
serialize through the workspace position and stream head locks. The `event_log_positions`,
`event_log_streams`, and `event_log_job_index` support tables remain rowstore.
This is separate from the audit table `events_columnstore`. Startup does not convert
or rekey an existing event log; its existing layout remains operator-managed.
See the [events preparation guide](../../../../docs/singlestore-events-cutover.md) for existing databases and fresh installations.


Keep workspace shard keys and unique keys aligned.
Go aggregate writers enforce the constraints unavailable in SingleStore; see the [backend rules](../README.md#go-enforced-rules-for-aggregate-writers).
