# SingleStore migration rules

These numbers belong to this backend. Version one is a fresh schema and does
not replay PostgreSQL's migration history. Published files are immutable;
add a new numbered file for each correction. DDL commits implicitly, so every
file must be safe to retry after an interrupted startup.

The [backend README](../README.md#go-enforced-rules-for-aggregate-writers)
lists the mandatory Go write checks that replace unavailable triggers,
CHECKs, partial unique indexes and deferrable constraints. In particular,
`events_columnstore`, `deployment_events` and `interventions` are append-only; task and
work-order states come from the core state sets; deployment credentials and
live reference names require transaction-locked uniqueness checks.

Keep workspace shard keys and all unique keys aligned.
The operator-created `events_columnstore` is the append-only columnstore exception: its non-null `event_shard_key` is the task ID, or an independently distributed fallback for taskless events, and is included in its unique hash key.
Legacy `events` remains historical input; migration 0027's published SQL stays immutable but its retired conversion does not run.
Follow the [operator checklist](../../../../docs/singlestore-events-cutover.md) instead.
Add workspace_id to parent-scoped child tables rather than relying on a cross-shard parent join.
