# SingleStore operations

Select `database.backend: singlestore` and supply the connection with `CONVEYOR_DATABASE_URL`.
The backend accepts SingleStore/MySQL URLs and MySQL DSNs.
`conveyor init`, `conveyor user issue-link`, and `conveyord` use the same backend factory.
Conveyor does not import PostgreSQL state.

Current audit events use the operator-created `events_columnstore`; mutable application state uses rowstore tables.
For existing databases and fresh installations, see the [events preparation guide](singlestore-events-cutover.md).
Startup applies pending numbered migrations and refuses a missing or incompatible event table.
It does not copy, rename, or drop event tables or synchronize allocators.

Workspace data is scoped by `workspace_id`.
The server must use UTC; outgoing timestamps are truncated to microseconds.
Go transactions enforce the constraints unavailable in SingleStore, and failed event writes roll back their associated entity change.
Statement arguments are interpolated by the driver to avoid unsupported long-data prepared statements.

Isolated SingleStore CI jobs run the store and event-log conformance packs plus CLI and daemon integration tests.
PostgreSQL remains the behavioral reference.
Local conformance does not measure a production cluster's capacity, query plans, or failover behavior.
