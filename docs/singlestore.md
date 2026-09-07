# SingleStore operations

Set `database.backend: singlestore` and supply the connection through
`CONVEYOR_DATABASE_URL`. Use `singlestore://user:password@host:3306/conveyor`,
`mysql://` with the same URL shape, or a MySQL DSN such as
`user:password@tcp(host:3306)/conveyor`. Add `?tls=true` for TLS. Percent-encode
reserved characters in URL passwords. Create an empty database before running
`conveyor init`; Conveyor creates its own tables and never imports PostgreSQL
state.

`conveyor init`, `conveyor user issue-link`, and `conveyord` use the same
backend factory. Startup applies migrations, ensures the event-log schema,
and bootstraps deployment identity and the configured workspace. It logs
`using durable SingleStore store with the event log`. The tested local engine
version and admission measurements are in
[the admission report](measurements/singlestore-admission-260905-d3c03a.md).

## Shard keys and write rules

Workspace tables shard on `workspace_id`; deployment tables shard on their
primary key. Each SQL read and write includes its workspace scope. Unique keys
must contain the shard key, so Go enforces deployment-wide identity, branch,
credential and other uniqueness rules that a workspace key cannot express.
The backend uses rowstore tables, binary UTF-8 identifier collation, JSON,
LONGTEXT and UTC `DATETIME(6)`. Startup refuses a server whose actual system
time zone is not UTC. Every outgoing timestamp is truncated to microseconds.

SingleStore has no PostgreSQL foreign keys, CHECK constraints, triggers or
partial indexes. Aggregate transactions validate parents, lifecycle states,
workspace ownership and uniqueness before committing. Ledger writes are
append-only. A failed event or queue append rolls back the associated entity
change. SQL and driver errors stay inside the backend and become the shared
store errors; retryable conflicts require retrying the whole command.

## Locks and migrations

`conveyor_locks` stores hashed coordination keys in rowstore rows. A command
inserts its key if absent and selects it `FOR UPDATE`; transaction completion
releases ownership. Callback locks hold a dedicated connection until the
callback returns. Lock rows remain for reuse. Same-task commands share the
task-operation key; unrelated tasks have different keys.

`conveyor:startup-migrations` serializes daemon starts. SingleStore DDL commits
implicitly, so that lock lives on one connection while DDL runs on another.
Migration files must be restart-safe. The runner records a version only after
its entire file succeeds and retries a partially applied file on the next
start. It never rewrites checksums to conceal partial work.

`conveyor_singlestore_migrations` records each version, filename, SHA-256 and
applied time. Startup rejects a newer-than-binary version, a changed filename,
or a changed checksum. Upgrade the binary before reopening a newer schema.
New migrations get new numbers; published files stay immutable. The backend
starts from its own version-one schema and does not replay PostgreSQL history.

The event-log driver binds to the aggregate's `*sql.Tx`. Entity state, audit
history and queue intent therefore commit together. The queue needs no second
database or external broker.

## Verification

`make test-integration-singlestore-ci` requires a MySQL DSN in
`CONVEYOR_TEST_SINGLESTORE_URL` naming an isolated `_test` database. Fixtures
create and drop only their own fresh `_test` databases. Each uses two partitions and
`interpreter_mode=interpret` on its connections to avoid compiling plans for
short-lived schemas. These are fixture choices; the daemon smoke uses its
default query mode. SingleStore documents these modes in
[Code generation](https://docs.singlestore.com/db/v9.1/query-data/advanced-query-topics/code-generation/).

The target runs the complete store and event-log conformance packs plus CLI
initialization and daemon startup tests. It prints suite timings. CI requires
`SINGLESTORE_ROOT_PASSWORD` and proves that an unset URL and a deliberately
failing test cannot produce a green integration job. PostgreSQL stays the
behavioral reference. Passing the dev-container pack does not measure a
production cluster's query plans, capacity or failover behavior.
