# SingleStore backend

DEC-39 admits this backend under the DEC-38 store and event-log contract.
`Store` owns a bounded `database/sql` pool; the MySQL driver stays inside this package and `internal/eventlog/s2log`.
Connections use `parseTime`, UTC, `STRICT_ALL_TABLES`, and microsecond timestamp precision.
Driver-interpolated statement arguments avoid unsupported `COM_STMT_SEND_LONG_DATA` writes for large JSON and string parameters.
`Open` refuses a server whose actual system time zone is not UTC.

## Schema and startup

Most mutable state uses rowstore tables.
On a new database, eligible tables are created columnstore: history tables and some mutable tables with at most one unique key and no `FOR UPDATE` reader.
`task_context_proposals` is columnstore although `FOR UPDATE` reads it, because each such read first locks its owning `tasks` or `planning_sessions` row (DEC-139(2)).
Startup never converts or rekeys an existing table, so an older database keeps its rowstore tables (DEC-139(3)).
`task_event_positions` is a rowstore support table: every transaction that commits a task event advances its workspace row, and the peer check reads it.
Current audit reads and writes use the operator-created `events_columnstore`, sharded by task ID or a fallback for taskless events.
Legacy `events` is retained history, not a fallback read/write target.
Follow the [events preparation guide](../../../docs/singlestore-events-cutover.md) for existing databases or fresh installation.
Normal `Open` applies numbered migrations and checks the current event-table prerequisite.
It performs no automatic event copy, rename, drop, or allocator synchronization.
Published migration SQL and ledger checksums are immutable, and a correction is a new numbered file.
The one exception is the edit of migrations 0001, 0009, 0023, 0024, 0029, 0037, and 0040 that made eligible history tables columnstore on new databases; `publishedChecksums` accepts each file's last published checksum beside its current one (DEC-139(1)).
Pending migration 0027 records its original identity without running the retired conversion.

## Compression

`Open` requests protocol compression unless the caller or the connection string's `compress` parameter turns it off.
`conveyord` passes `database.singlestore_compress`, or else `CONVEYOR_SINGLESTORE_COMPRESS`, ahead of the `compress` parameter; other callers pass nothing, so the parameter or the default `true` applies.
An invalid `compress` parameter refuses the open without echoing the value.
A server that does not negotiate compression leaves the connection uncompressed with no error.
`Store.Compression` reports whether compression was requested and whether the first observed handshake negotiated it (`negotiated`, `not_negotiated`, or `unknown`); it sends no statement.
Read accounting counts bytes on the network connection, so on a compressed connection it counts the bytes after compression.

## Go-enforced rules for aggregate writers

SingleStore does not provide PostgreSQL foreign keys, CHECK constraints, triggers, partial indexes, or deferrable constraints.
Aggregate writers validate parents, workspace ownership, core lifecycle states, and uniqueness within their transactions.
Unique keys must include the shard key; deployment-wide uniqueness requires Go-side locking and checks.
Event ledgers are append-only.
`withTx` rolls back on error, cancellation, or panic, and translates supported backend conflicts at the transaction boundary.
A statement outside `withTx` goes through `backendRow`, `backendRows`, `backendExec`, or their workspace-checked `document*` forms, which translate its errors, including row iteration and `RowsAffected`; `TestPackageQueriesUseTranslatingPaths` enforces this.

## Tests

The store and event-log conformance packs exercise the backend contract, with PostgreSQL as the reference.
Integration fixtures create their own `_test` databases and explicitly create the event target before normal startup.
The regression tests cover exact operator copy validation, allocator synchronization, event-table prerequisites, retained history, and compact work-order events and released claim-holder compatibility.
