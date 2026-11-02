# SingleStore backend

DEC-39 admits this backend under the DEC-38 store and event-log contract.
`Store` owns a bounded `database/sql` pool; the MySQL driver stays inside this package and `internal/eventlog/s2log`.
Connections use `parseTime`, UTC, `STRICT_ALL_TABLES`, and microsecond timestamp precision.
Driver-interpolated statement arguments avoid unsupported `COM_STMT_SEND_LONG_DATA` writes for large JSON and string parameters.
`Open` refuses a server whose actual system time zone is not UTC.

## Schema and startup

Mutable state uses rowstore tables.
Current audit reads and writes use the operator-created `events_columnstore`, sharded by task ID or a fallback for taskless events.
Legacy `events` is retained history, not a fallback read/write target.
Follow the [events preparation guide](../../../docs/singlestore-events-cutover.md) for existing databases or fresh installation.
Normal `Open` applies numbered migrations and checks the current event-table prerequisite.
It performs no automatic event copy, rename, drop, or allocator synchronization.
Published migration SQL and ledger checksums are immutable; pending migration 0027 records its original identity without running the retired conversion.

## Go-enforced rules for aggregate writers

SingleStore does not provide PostgreSQL foreign keys, CHECK constraints, triggers, partial indexes, or deferrable constraints.
Aggregate writers validate parents, workspace ownership, core lifecycle states, and uniqueness within their transactions.
Unique keys must include the shard key; deployment-wide uniqueness requires Go-side locking and checks.
Event ledgers are append-only.
`withTx` rolls back on error, cancellation, or panic, and translates supported backend conflicts at the transaction boundary.

## Tests

The store and event-log conformance packs exercise the backend contract, with PostgreSQL as the reference.
Integration fixtures create their own `_test` databases and explicitly create the event target before normal startup.
The regression tests cover exact operator copy validation, allocator synchronization, event-table prerequisites, retained history, and compact work-order events and released claim-holder compatibility.
