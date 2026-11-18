# SingleStore operations

Select `database.backend: singlestore` and supply the connection with `CONVEYOR_DATABASE_URL`.
The backend accepts SingleStore/MySQL URLs and MySQL DSNs.
`conveyor init`, `conveyor user issue-link`, and `conveyord` use the same backend factory.
Conveyor does not import PostgreSQL state.

Current audit events use the operator-created `events_columnstore`.
Most application state uses rowstore tables.
On a new database, startup creates eligible tables as columnstore: history tables and some mutable tables that have at most one unique key and no reader that locks their rows with `FOR UPDATE`.
`task_context_proposals` is the one exception: it is columnstore although `FOR UPDATE` reads it, because each such read first locks the owning `tasks` or `planning_sessions` row in the same transaction (DEC-139(2)).
Startup never converts or rekeys an existing table, so a database created before this layout keeps its rowstore tables until an operator converts them (DEC-139(3)).
`task_event_positions` is a rowstore support table that holds each workspace's task-event position; every transaction that commits a task event advances it, and the peer check reads it.
For existing databases and fresh installations, see the [events preparation guide](singlestore-events-cutover.md).
Startup applies pending numbered migrations and refuses a missing or incompatible event table.
It does not copy, rename, or drop event tables or synchronize allocators.
Published migration files are append-only: a correction is a new numbered file.
The one exception is the edit of migrations 0001, 0009, 0023, 0024, 0029, 0037, and 0040 that made eligible history tables columnstore on new databases; startup accepts each of those files' last published checksum beside its current one, so an existing database keeps its ledger rows and tables (DEC-139(1)).

A connection requests protocol compression by default.
For `conveyord`, `database.singlestore_compress` in the deployment file wins, then `CONVEYOR_SINGLESTORE_COMPRESS`, then the connection string's `compress` parameter, then `true`.
`conveyor init` and `conveyor user` read only the `compress` parameter, then `true`.
A server that does not negotiate compression leaves the connection uncompressed with no error, and `conveyord` logs whether compression was requested and negotiated once at startup.
See [Database selection](configuration.md#database-selection) for the full rules.

Workspace data is scoped by `workspace_id`.
The server must use UTC; outgoing timestamps are truncated to microseconds.
Go transactions enforce the constraints unavailable in SingleStore, and failed event writes roll back their associated entity change.
Statement arguments are interpolated by the driver to avoid unsupported long-data prepared statements.

Isolated SingleStore CI jobs run the store and event-log conformance packs plus CLI and daemon integration tests.
PostgreSQL remains the behavioral reference.
Local conformance does not measure a production cluster's capacity, query plans, or failover behavior.
