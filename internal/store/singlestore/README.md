# Experimental SingleStore backend

The factory selects this backend only with `backend.AllowExperimental`.
`conveyord` does not supply that option. This is the foundation of the second
backend under DEC-38. It is not production-capable.

`Store` owns a bounded `database/sql` pool. Every connection uses `parseTime`,
UTC and `STRICT_ALL_TABLES`. SingleStore accepts but ignores the MySQL
`time_zone` session variable. `Open` verifies that `system_time_zone` is UTC
and refuses another server time zone rather than silently misreading timestamps. The MySQL driver stays inside this package and
`internal/eventlog/s2log`. No SQL layer is shared with PostgreSQL.

## Implemented behavior

Workspace creation and versioned configuration writes persist repos and audit
rows in the same transaction. Bootstrap is idempotent. The `WorkspaceControl`
and `EmptyProjections` conformance suites run. The remaining suites are named
explicitly in `Factory.Skip`, which is forbidden on production factories.

`unimplemented.go` supplies explicit `store.ErrNotImplemented` methods for the
remaining contract. `ConfigureForgeTokenEncryptionKey` has no error result and
is inert until the identity aggregate replaces it. `empty_projections.go`
checks persisted rows before returning an empty result; it refuses populated
domains with `ErrNotImplemented`. Aggregate tasks must replace those methods
as well as their stubs when adding populated projections. No reconciliation
method claims success on a populated task domain.

## Schema and migrations

`migrations/0001_schema.sql` defines all 56 current relational tables from
scratch. It uses the schema domains in component-persistence v4 as its
checklist. No PostgreSQL migration runs against SingleStore. JSON stores JSON
objects and array-valued projections, timestamps use `DATETIME(6)`, and all
tables are rowstores with binary UTF-8 collation for case-sensitive identifiers. Key columns use `VARCHAR(255)`; other text is `LONGTEXT`.

Every workspace table shards on `workspace_id`. Jobs, transcripts, specs and
interventions carry that column explicitly rather than inheriting scope from
a parent join. Deployment tables shard on their primary key. Every unique key
includes the shard key. Consequently, a unique key alone does not establish a
deployment-wide identity when that identity is not the shard key. Identity
writers must serialize and check global email, token hash and singleton rules.
Foreign keys do not exist on SingleStore; aggregate writers must check parent
existence and workspace consistency within their command transaction.

The runner creates `conveyor_locks`, acquires `conveyor:startup-migrations`,
creates `conveyor_singlestore_migrations`, validates existing ledger rows,
then calls `s2log.EnsureSchema` before version one. Ledger rows hold version,
filename, SHA-256 of embedded bytes, and applied time. Newer versions, changed
names and changed checksums refuse startup.

SingleStore DDL commits implicitly. The startup lock lives on a dedicated
connection while DDL executes on another connection. Each migration must be
restart-safe. Version one uses `CREATE ... IF NOT EXISTS`; the runner records
a version only after its complete file succeeds. A failed file can leave DDL
in place and is retried on startup. This differs from PostgreSQL's atomic DDL
transaction. Published migration files are immutable; corrections get a new
number. Checksums are never rewritten to conceal a failure.

## Lock and transaction contract

`withTx` rolls back on errors, cancellation and panic. It translates MySQL
1062 on `jobs_pkey`/`jobs_dispatch_unique` and
`reference_documents_live_name_idx` into the store conflict sentinels; 1205
and 1213 become `store.ErrRetryable`. Callers retry the whole command, never
an individual statement inside a failed transaction. Other MySQL errors lose
their driver type before crossing the backend boundary.

`lockKey` hashes a logical key to a fixed-width row, inserts it on first use,
and selects it `FOR UPDATE`. A transaction holds that row until commit or
rollback. `sessionLock` reserves a connection and holds its transaction until
release or context cancellation. Callbacks must obey cancellation. Lock rows
remain as reusable coordination records. The caller takes locks in a stable
order; callbacks may perform writes on separate connections.

The event log uses `s2log.WithTx(ctx, tx)` to bind its reads and appends to the
same `*sql.Tx` as an aggregate mutation. The caller owns commit and rollback.
Position locks precede stream locks, preserving committed workspace order.
The store's `Log` handle also supports independent appends.

## Go-enforced rules for aggregate writers

Use `writeRow` for ordinary inserts, updates and deletes. It validates state
and ledger rules before issuing SQL and locks the uniqueness checks in the
same transaction. For expression-based SQL, invoke the corresponding checks
and hold the same locks before issuing the statement. Never bypass these
checks because the schema lacks a PostgreSQL constraint.

| PostgreSQL guarantee | SingleStore write rule | Evidence |
| --- | --- | --- |
| `events` append-only trigger | `checkWrite` permits inserts only; workspace audit uses `writeRow` | `TestEventsAppendOnly` |
| `deployment_events` append-only trigger | `checkWrite` permits inserts only | `TestDeploymentEventsAppendOnly` |
| `interventions` append-only trigger | `checkWrite` permits inserts only | `TestInterventionsAppendOnly` |
| `tasks.state` CHECK | `checkWrite` validates `core.TaskStates()` | `TestTaskStates` |
| `work_orders.state` CHECK | `checkWrite` validates `core.WorkOrderStates()` | `TestWorkOrderStates` |
| One deployment credential | `writeRow` locks `one-deployment-credential`, counts other deployment credentials, then calls `checkDeploymentCredential` | `TestOneDeploymentCredential` |
| Live reference-document name | `writeRow` requires name, workspace, and explicit deleted_at, locks workspace reference names, counts other live case-insensitive names, then calls `checkReferenceName` | `TestLiveReferenceName` |

Reference updates must pass the complete name/deleted-at projection, including
restore. Credential writes must pass a boolean `deployment_credential`.
Task and work-order inserts must pass their canonical state. The foundation
does not implement those aggregates; their tests must call these write paths
when the stubs are replaced.

The remaining partial indexes, expressions and trigger behavior belong to the
sibling aggregates. Their replacement checks are prerequisites for admission:

- Artifact-link uniqueness for task, feature, requirement, planning session,
  and workspace-level ownership, plus ownership exclusivity.
- One confirmed successor per superseded decision.
- One dependency-unsatisfiable event per edge outcome.
- Positive GitHub issue-number uniqueness within a repository and workspace.
- Non-null task intake-key uniqueness and live blueprint-origin uniqueness.
- Unsuperseded review-seat uniqueness for positive review rounds.
- Dependency-cycle rejection, immutable version rows, supported event and
  intervention vocabularies, singleton organization protection, and parent
  consistency formerly supplied by triggers, CHECKs and foreign keys.
- Case-insensitive trimmed workspace-name uniqueness, implemented here under
  the workspace-registry lock.

The schema cannot synthesize `work_orders.queue_deadline`; its writer supplies
the frozen queue deadline. Deferred uniqueness must be checked at the command
boundary under locks after all intermediate writes, before committing.

## Verification and CI

`make test-singlestore-unit` runs the affected unit packages without a database.
`make test-integration-singlestore-ci` requires `CONVEYOR_TEST_SINGLESTORE_URL`
as a MySQL DSN and runs both `s2log` and this package. The test helper rejects a
database whose name does not end in `_test`. Each store fixture creates and
drops only its own timestamp-named `conveyor_*_test` database. Event-log tests
use unique workspace IDs. The test account needs CREATE/DROP DATABASE rights
only on the disposable integration server.

The CI job uses `ghcr.io/singlestore-labs/singlestoredb-dev:latest`, limits the
container to four CPUs and 4 GiB, and creates `conveyor_test`. It requires the
repository secret `SINGLESTORE_ROOT_PASSWORD`; it never retrieves credentials
from another environment. The dev image's free edition needs no license at
those limits. The job proves that a missing URL and an injected regression
fail before running the real suites. PostgreSQL's job and migrations remain
unchanged.

Upstream references: [dev image setup](https://github.com/singlestore-labs/singlestoredb-dev-image)
and [SingleStore CREATE TABLE](https://docs.singlestore.com/db/v8.5/reference/sql-reference/data-definition-language-ddl/create-table/).

[SingleStore time-zone variables](https://docs.singlestore.com/db/v8.5/reference/configuration-reference/engine-variables/list-of-engine-variables/) document the session-variable compatibility behavior.
