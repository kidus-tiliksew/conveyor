# SingleStore migration rules

These numbers belong to this backend. Version one is a fresh schema and does
not replay PostgreSQL's migration history. Published files are immutable;
add a new numbered file for each correction. DDL commits implicitly, so every
file must be safe to retry after an interrupted startup.

The [backend README](../README.md#go-enforced-rules-for-aggregate-writers)
lists the mandatory Go write checks that replace unavailable triggers,
CHECKs, partial unique indexes and deferrable constraints. In particular,
`events`, `deployment_events` and `interventions` are append-only; task and
work-order states come from the core state sets; deployment credentials and
live reference names require transaction-locked uniqueness checks.

Keep workspace shard keys and all unique keys aligned. `events` is the
append-only columnstore exception: its non-null `event_shard_key` is the task
ID, or an independently distributed fallback for task-less events, and is
included in its unique hash key. Add workspace_id to parent-scoped child tables
rather than relying on a cross-shard parent join.
