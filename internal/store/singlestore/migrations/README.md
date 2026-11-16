# SingleStore migration rules

These numbers belong to this backend; version one is a fresh schema, not a replay of PostgreSQL migrations.
Published SQL and recorded checksums are immutable, with one exception: migrations 0001, 0009, 0023, 0024, 0029, 0037 and 0040 were edited to create selected history tables as columnstore on new databases. `publishedChecksums` accepts their original checksums without rewriting ledger rows or existing tables.
DDL commits implicitly, so new migrations must tolerate an interrupted startup.
Normal startup applies pending numbered migrations under the backend migration lock.
Migration 0027 keeps its original ledger identity but its retired automatic event conversion is not executed.

Current audit events use operator-created `events_columnstore`; legacy `events` remains historical input.
`governance_snapshots` is columnstore: append-only, write-once, keyed by `(workspace_id, digest)`.
See the [events preparation guide](../../../../docs/singlestore-events-cutover.md) for existing databases and fresh installations.


Keep workspace shard keys and unique keys aligned.
Go aggregate writers enforce the constraints unavailable in SingleStore; see the [backend rules](../README.md#go-enforced-rules-for-aggregate-writers).
