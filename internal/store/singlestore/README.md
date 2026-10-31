# SingleStore backend

DEC-39 admits this backend under the complete DEC-38 store and event-log
contract. The factory opens it without an experimental option. The conformance
factory has all capabilities and no skipped suites.

`Store` owns a bounded `database/sql` pool. Every connection uses `parseTime`,
UTC, microsecond timestamp truncation and `STRICT_ALL_TABLES`. The driver
truncates every outgoing `time.Time` to the schema's `DATETIME(6)` precision,
including clocks that supply nanosecond fractions. Every connection also
interpolates statement arguments into one text query. SingleStore refuses the
driver's `COM_STMT_SEND_LONG_DATA` command with error 1706, which a prepared
statement uses for any argument longer than the packet size divided by its
parameter count plus one, such as a pinned claim snapshot of a few MiB. SingleStore accepts but ignores the MySQL
`time_zone` session variable. `Open` verifies that `system_time_zone` is UTC
and refuses another server time zone rather than silently misreading timestamps. The MySQL driver stays inside this package and
`internal/eventlog/s2log`. No SQL layer is shared with PostgreSQL.

`connectionConfig` forces `interpolateParams=true`, including when a supplied
DSN disables it. SingleStore rejects `COM_STMT_SEND_LONG_DATA` with errno 1706;
the MySQL driver's prepared-statement path selects that command for a parameter
at least `maxAllowedPacket/(parameter count+1)` bytes long, even when the full
statement fits the packet budget. Driver-escaped text queries preserve complete
large string and JSON parameters without that unsupported protocol command.

## Implemented behavior

The backend implements the complete `store.Backend` interface, including
identity, membership, tokens, tasks, work orders, document governance, lineage,
activity, monitor projections and startup reconciliation. Lifecycle projections,
audit events and event-log queue intents commit in one transaction.

The full shared conformance pack exercises each aggregate. `PopulatedProjections`
covers caller attention and checkpoint candidates with a real task and work
order, pending and published GitHub lifecycles, terminal blueprint children,
and missing durable dispatch repair. Empty startup coverage alone cannot prove
these methods. PostgreSQL defines the expected behavior; volatile queues have
no durable dispatch or publication jobs to reconcile.

## Schema and migrations

`migrations/0001_schema.sql` defines all 56 current relational tables from
scratch. It uses the schema domains in component-persistence as its
checklist. No PostgreSQL migration runs against SingleStore. JSON stores JSON
objects and array-valued projections, timestamps use `DATETIME(6)`, and binary
UTF-8 collation preserves case-sensitive identifiers. The initial tables are
rowstores; migration 0027 converts the append-only `events` table to columnstore.
Key columns use `VARCHAR(255)`; other text is `LONGTEXT`.

Workspace tables shard on `workspace_id`, except `events`, which shards on
the non-null `event_shard_key`: the task ID when present, otherwise
`event/<id>` for historical rows or `event/<random id>` for new rows. Each
task's timeline stays together, while task-less events spread across partitions
instead of concentrating a workspace's NULL task IDs on one shard.
Jobs, transcripts, specs and interventions carry workspace scope explicitly
rather than inheriting it from a parent join. Deployment tables shard on their
primary key. Every unique key
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

### Events columnstore upgrade (0027)

Stop **all** old API/worker writers and readers before starting the upgraded binary.
Keep all traffic stopped until migration, allocator synchronization, and post-upgrade checks complete.
The startup lock excludes other migrators, not old processes.
SingleStore supports only separate `ALTER TABLE ... RENAME TO` operations, so `events` is briefly absent during the swap.
An interrupted start resumes from its staging table or rename state.
Restart the same upgraded binary against the same database with traffic still frozen; do not reset the migration ledger or manually drop either migration table.

The copy preserves IDs, timestamps, nullable task/job IDs and JSON payloads.
Each server-side `INSERT SELECT` copies at most 256 candidate IDs and roughly
8 MiB of payload (an oversized row or equal IDs across workspaces can exceed
that budget). Go fetches only ID/length metadata, never document bodies.
The staging columnstore is flushed every 32 batches and before promotion, so
its row buffer does not retain a second full event log in RAM (roughly
256 MiB of payload between flushes, plus indexes and row overhead).
The source stays intact until counts match and promotion succeeds. All
aggregators' AUTO_INCREMENT allocators are synchronized before the old table
is dropped, preserving existing cursor IDs. AUTO_INCREMENT retains
SingleStore's existing per-aggregator ordering semantics, not a new global
commit-order guarantee.

The sort key `(workspace_id, task_id, at, id)` supports task timelines.
Hash indexes cover task equality and workspace/event-ID lookups; workspace
time/kind scans aggregate across task shards. The unique hash key includes
`event_shard_key` as required by SingleStore. Normal event writers always
allocate IDs; no new explicit-ID write path is introduced.
Reserve disk for both copies and keep the existing rowstore plus the staging
buffer/index RAM available during copying. Compression and host I/O determine
actual upgrade time and peak memory; measure a restored production backup
before scheduling the outage.

Before scheduling the upgrade, take a compatible backup and rehearse restoration and the complete upgrade against an isolated restored database.
Record backup identity, restoration success, pre-upgrade event counts, migration completion, post-upgrade counts, taskless-event reads, workspace isolation, and a successful resumed write.
Measure elapsed time and peak resources in that rehearsal; local synthetic results do not establish a production outage duration or memory bound.
If recovery cannot converge, leave traffic frozen and restore the compatible backup with the matching old binary.
Rollback is a backup restoration, not an in-place reverse migration or manual table rename.
Only the operator performs production backup, deployment, traffic changes, or rollback.
After local validation, tear down only the invocation-owned fixture databases and processes through the repository validation tooling.
Do not stop the coordinator's shared SingleStore container or remove its unrelated databases.

## Lock and transaction contract

`withTx` rolls back on errors, cancellation and panic. It translates MySQL
1062 on `jobs_pkey`/`jobs_dispatch_unique` and
`reference_documents_live_name_idx` into the store conflict sentinels; 1205
and 1213 become `store.ErrRetryable`. Callers retry the whole command, never
an individual statement inside a failed transaction. Other MySQL errors log
their errno, SQLSTATE, message, and translating operation on the server, then
lose their driver type and detail before crossing the backend boundary.

`lockKey` hashes a logical key to a fixed-width row, inserts it on first use,
and selects it `FOR UPDATE`. A transaction holds that row until commit or
rollback. `sessionLock` reserves a connection and holds its transaction until
release or context cancellation. Callbacks must obey cancellation. Lock rows
remain as reusable coordination records. The caller takes locks in a stable
order; callbacks may perform writes on separate connections.

A commit that spans partitions can release a lock row before the rows it wrote
on other partitions become visible. The next holder may then acquire the lock
and miss those rows (task 261012-832d9c measured this in CI). A rule that must
see its predecessor's rows uses `claimKey` instead: it locks the same row and
returns the `holder` column, which `setClaim` writes on the locked row itself
and therefore commits atomically with the release. Migration 0022 adds the
column. Rows written before claims existed are covered by the rules' counts.

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
| One deployment credential | `writeRow` claims `one-deployment-credential`, counts other deployment credentials plus a claim held by another token, calls `checkDeploymentCredential`, then records the token as holder; setting the holder's `deployment_credential` false releases the claim | `TestOneDeploymentCredential`, `TestUniqueClaimsOutliveUnseenRowsIntegration` |
| Live reference-document name | `writeRow` requires name, workspace, and explicit deleted_at, claims the workspace's `LOWER(name)`, counts other live names whose `BINARY LOWER(name)` equals those claimed bytes (no PAD SPACE, so trailing spaces stay distinct as in PostgreSQL) plus a claim held by another document, calls `checkReferenceName`, then records the document as holder; a soft delete releases the claim | `TestLiveReferenceName`, `TestUniqueClaimsOutliveUnseenRowsIntegration`, `ReferenceDocuments` conformance |
| `tasks_open_repo_branch_idx` partial unique index | `writeRow` claims `taskBranchClaimKey(workspace, repo, branch)` when a task row starts occupying an open branch (non-terminal insert, reopen, or branch/repo change), refuses a claim held by another task or another committed open row with `store.TaskBranchInUseError`, then records the task as holder; a terminal state or a branch change releases the old claim when the task holds it. The occupant checks in `claimTaskBranch` and `AttachTaskBranch` compare `BINARY repo_name` and `BINARY branch` against the claimed bytes (no PAD SPACE, so trailing spaces stay distinct as in PostgreSQL). Every `tasks` write that sets `state`, `branch` or `repo_name` must go through `writeRow` | `TestUniqueRuleInputsFailBeforeSQL`, `TestUniqueClaimsOutliveUnseenRowsIntegration`, `TaskBranchConcurrentClaims` and `TaskBranchUniqueness` conformance |
| `tasks_workspace_intake_key_idx` (non-null intake key) | `writeRow` claims `taskIntakeClaimKey(workspace, intake_key)`, counts other committed rows, and records the task as holder; non-empty intake keys are immutable and task hard deletes are refused, so claims are never released | `TestUniqueClaimsOutliveUnseenRowsIntegration`, `TaskCreationProvenance` conformance |
| `workspaces_name_lower_unique` | `writeRow` folds `LOWER(TRIM(name))`, claims `workspace-name:<folded>`, counts other committed rows and records the workspace as holder; workspace name updates and hard deletes are refused, so claims are never released | `TestUniqueClaimsOutliveUnseenRowsIntegration`, `WorkspaceNameUniqueness` conformance |

Reference documents are inserted live and updated only to soft-delete, with
the complete name/deleted-at projection. Credential writes must pass a boolean
`deployment_credential` and one token id. `writeRow` refuses hard deletes of
`user_tokens`, `reference_documents` and `tasks`, which would orphan a claim.
Task and work-order inserts must pass their canonical state. Aggregate implementations call these write paths or enforce the matching
checks inside their command transaction.

Aggregate commands enforce the following rules where SingleStore cannot
express the PostgreSQL constraints:

- Artifact-link uniqueness for task, requirement, planning session, and
  workspace-level ownership, plus ownership exclusivity. Migration 0018 and its
  version-18 hook retired the feature owner and the `features` table.
- One confirmed successor per superseded decision.
- One dependency-unsatisfiable event per edge outcome.
- Positive GitHub issue-number uniqueness within a repository and workspace.
- Non-null task intake-key uniqueness and live blueprint-origin uniqueness;
  the intake key is an immutable lock-row claim.
- Unsuperseded review-seat uniqueness for positive review rounds.
- Dependency-cycle rejection, immutable version rows, supported event and
  intervention vocabularies, singleton organization protection, and parent
  consistency formerly supplied by triggers, CHECKs and foreign keys.
- Case-insensitive trimmed workspace-name uniqueness, implemented by a
  lock-row claim in `writeRow` after the `workspace-registry` lock.

The schema cannot synthesize `work_orders.queue_deadline`; its writer supplies
the frozen queue deadline. Deferred uniqueness must be checked at the command
boundary under locks after all intermediate writes, before committing.

Work-order lifecycle events retain the order ID and attempt/lifecycle metadata,
not governance or served-requirement snapshot bodies. The `work_orders` row is
the stored authority pin; all backends share the same event payload projection.

`task.created` likewise references the task instead of copying its body or live
display fields, including blueprint-child and planning-bundle creation. It
retains only the initial state, policy switches, assignee ID, ancestry, intake
context IDs and verified trigger provenance used by event consumers. These
historical values cannot be recovered from the task's current mutable row.

## Verification and CI

`make test-singlestore-unit` runs the affected unit packages without a database.
`make test-integration-singlestore-ci` requires `CONVEYOR_TEST_SINGLESTORE_URL`
as a MySQL DSN and runs `s2log`, this package and the host CLI/daemon tests. The test helper rejects a
database whose name does not end in `_test`. The store package creates one
timestamp-named `conveyor_*_test` database with two partitions per test binary,
migrates it once, and drops it from `TestMain`; each fixture empties every
table but the migration ledger with `DELETE` and reuses the one warm pool, so
compiled query plans survive across fixtures. Creating a database or truncating
a table is distributed DDL that costs seconds or hundreds of milliseconds on
SingleStore, which is why fixtures never do either. The complete pack has a
twenty-minute timeout and logs suite and reset timings. Event-log tests
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
