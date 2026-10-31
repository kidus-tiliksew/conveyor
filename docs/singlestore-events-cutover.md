# SingleStore events: ordered release checklist

This procedure prepares the database **before releasing the corrected application from main**.
The correction is delivered for developer review as a draft PR; this document grants no rollout, merge, or database-access permission.
Use the exact reviewed correction commit and retain its SHA with the execution record.
Do not substitute a later main checkout without reviewing its migration inventory again.
The developer-review report records the candidate SHA and local evidence.

## Ownership and prerequisites

The application owns ordinary numbered schema migrations through `internal/store/singlestore/migrate.go`.
The operator owns creation, copy, validation, allocator preparation, traffic control, and release of `events_columnstore`.
No application startup copies or swaps event tables, drops the old table, or synchronizes its allocators.
The stable target must exist before application event access; there is no fallback to `events`.
Historical SQL files remain byte-for-byte unchanged, including the retired `0027_events_columnstore.sql`.
Do not source that historical file as a cutover procedure or edit recorded ledger checksums.

Before scheduling work, obtain developer approval for the exact code and SQL, a compatible backup, and a successful full restoration/cutover rehearsal on an isolated representative backup.
Inventory every API, worker, CLI, scheduled job, and direct database writer or reader, including every aggregator endpoint they use.
The startup migration lock excludes other migrators; it does not stop already-running processes.
The operator must stop them and prevent restart or reconnection throughout the freeze.
Comments in SQL do not enforce this.

The deployed ledger is unknown to this task.
The checklist below is conditional on the operator's read-only inspection, not a claim that any version has run.
Use a secure client option file, named below as `$S2_CLIENT_CNF`, and the approved database name `$S2_DATABASE`.
Keep passwords out of command arguments and retained output.
Run all commands from the reviewed repository root.

## 1. Read-only inventory, before any database mutation

The operator runs these queries; the implementing agent has not accessed the deployed database.

```sh
mysql --defaults-extra-file="$S2_CLIENT_CNF" --database="$S2_DATABASE" --batch <<'SQL'
SELECT DATABASE(), @@version, @@system_time_zone;
SELECT table_name,storage_type FROM information_schema.tables
 WHERE table_schema=DATABASE() AND table_name IN
 ('conveyor_singlestore_migrations','events','events_columnstore',
  'events_columnstore_0027','events_rowstore_0027') ORDER BY table_name;
SQL
```

If the ledger exists, read it without changing it:

```sh
mysql --defaults-extra-file="$S2_CLIENT_CNF" --database="$S2_DATABASE" --batch \
  --execute='SELECT version,name,checksum,applied_at FROM conveyor_singlestore_migrations ORDER BY version;'
```

Inspect `SHOW CREATE TABLE events` and, if present, `SHOW CREATE TABLE events_columnstore`.
A fresh database without a ledger follows every numbered prerequisite below.
An existing database follows only missing prerequisites, after the runner validates recorded filenames and checksums.
A version newer than this checkout, a mismatched checksum/name, missing legacy source, or any partially renamed old layout is a stop condition.
Do not reset the ledger or improvise table renames.
An `events` source already promoted to columnstore by the former release can still be copied if it is complete and validated; do not infer completeness from migration 27's ledger row alone.
Unexpected staging or backup tables require operator reconciliation against a restored backup before using this normal procedure.
This procedure neither drops nor renames them.

## 2. Freeze all traffic and create the operator-owned empty target

Stop all old and corrected readers/writers and prevent automatic restart.
Retain evidence of drained connections and stopped processes for every application/aggregator endpoint.
Keep the source immutable through the remaining steps.
If `events_columnstore` already exists, stop and inspect it; the create script deliberately refuses silent reuse.
Do not truncate it or drop a populated table to make a rerun succeed.

```sh
mysql --defaults-extra-file="$S2_CLIENT_CNF" --database="$S2_DATABASE" \
  < docs/sql/events-columnstore/01-create.sql
```

Creating the empty target before the migration-only command also supports fresh initialization without starting a serving process.
Do not run `conveyor init`, `conveyord`, or an older binary as a migration-only substitute: initialization and serving can emit current application events before the final legacy copy.

## 3. Apply every missing numbered prerequisite, with traffic still stopped

Run the corrected source's schema-only command, using the approved database connection injected into `CONVEYOR_DATABASE_URL` by the operator's existing secret mechanism:

```sh
go run ./scripts/singlestore-migrate
```

This command calls the same backend migration runner as startup, then closes the store.
It starts no HTTP server, dispatcher, workers, or workspace bootstrap.
Do not put a connection string into shell history or this report.
The runner applies missing numbered migrations in ascending numeric order and records original embedded filenames/checksums after success.
Several files are markers whose Go hooks do the actual DDL or data repair; manually sourcing only those SQL files is **not** equivalent.

The actual task-base inventory and ordering are:

| Version | Local file under `internal/store/singlestore/migrations/` | Ownership and prerequisite |
|---|---|---|
| 0001 | `0001_schema.sql` | Automatic base schema, including legacy `events`; only if unrecorded. |
| 0002 | `0002_repository_install_task.sql` | Automatic repository-install column hook and table. |
| 0003 | `0003_workspace_github_apps.sql` | Automatic encrypted GitHub App schema. |
| 0004 | `0004_requirement_system_design_dismissal_notes.sql` | Automatic dismissal-note column hook; marker SQL alone is insufficient. |
| 0005 | `0005_retire_forge_tokens.sql` | Automatic historical forge-token retirement; unrelated to event cutover. |
| 0006 | `0006_task_start_over.sql` | Automatic start-over column hook and table. |
| 0007 | `0007_pull_request_close.sql` | Automatic pull-request-close table. |
| 0008 | `0008_open_task_branch_unique.sql` | Automatic old unique-index retirement hook. |
| 0009 | `0009_artifact_metadata_repairs.sql` | Automatic artifact-repair table. |
| 0010 | `0010_verify_stage.sql` | Automatic verify-stage history marker. |
| 0011 | `0011_verification_kit_persistence.sql` | Automatic verification persistence tables. |
| 0012 | `0012_verification_operations.sql` | Automatic verification-operation tables. |
| 0013 | `0013_verification_read.sql` | Automatic verification-read table. |
| 0014 | `0014_verification_permissions.sql` | Automatic verification-permission tables. |
| 0015 | `0015_verification_publication_delivery.sql` | Automatic publication-delivery table. |
| 0016 | `0016_requirement_system_design_dismissal_archival.sql` | Automatic archive-column hook plus transactional legacy dismissal archive events; **must finish before final copy**. |
| 0017 | `0017_verification_chunk_expiry_index.sql` | Automatic expiry-index hook. |
| 0018 | `0018_drop_features.sql` | Automatic feature-retirement/link-detachment hooks and historical table removal. |
| 0019 | `0019_task_gate_kind.sql` | Automatic gate-column hook and backfill reading legacy event history; before final copy. |
| 0020 | `0020_automatic_recovery_ledger.sql` | Automatic recovery-ledger column hook. |
| 0021 | `0021_submission_base_conflict_checked.sql` | Automatic nullable conflict-check column hook. |
| 0022 | `0022_lock_claims.sql` | Automatic lock-holder column hook. |
| 0023 | `0023_documentation_gate_evidence.sql` | Automatic exact-head documentation evidence table. |
| 0024 | `0024_review_round_seats.sql` | Automatic seat-column hook and review-seat tables. |
| 0025 | `0025_requirement_insight_headlines.sql` | Automatic insight-headline table. |
| 0026 | `0026_work_order_claim_failures.sql` | Automatic claim-failure table; unchanged by this correction. |
| 0027 | `0027_events_columnstore.sql` | Retired automatic event mechanism; retained immutable history, not operator SQL. The corrected runner does not execute its staging DDL or former Go cutover. |

The event-log schema is also ensured by the backend runner before the numbered files.
Re-run the read-only ledger query after the schema-only command and retain its output.
Do not proceed on command failure.
Do not run migration 0016 after copying: its helper-created archive events would otherwise remain only in legacy storage.
Any retry of a prerequisite must finish before the final copy and its new validation baseline.

## 4. Copy once from the frozen legacy source

Verify the target is empty immediately before copying:

```sh
mysql --defaults-extra-file="$S2_CLIENT_CNF" --database="$S2_DATABASE" --batch \
  --execute='SELECT COUNT(*) AS target_rows FROM events_columnstore;'
```

Require `target_rows = 0`.
Then run one server-side pass:

```sh
mysql --defaults-extra-file="$S2_CLIENT_CNF" --database="$S2_DATABASE" \
  < docs/sql/events-columnstore/02-copy.sql
```

The legacy primary key starts with `(workspace_id,id)`; repeated global `WHERE id > ? ORDER BY id` batches do not follow its leading key.
This script avoids those scans by doing one unfiltered `INSERT ... SELECT` over the entire frozen source.
It transfers no event bodies through a Go or shell batch client.
The distributed engine still reads, reshuffles, indexes, logs, and writes every event, and may consume substantial transaction, row-buffer, memory, disk, and network capacity.
Reserve room for both complete copies, index/segment/log overhead, existing rowstore memory, and operational headroom.
Measure the full copy and validation on a representative restored database with the actual engine topology and resource limits.
There is no fixed batch-memory cap, resumable copy cursor, or promised outage duration.

If the command fails or its commit acknowledgement is lost, keep traffic stopped and inspect target contents and engine transaction status.
Do not blindly rerun, use `IGNORE`/`REPLACE`, or reset the target; the procedure deliberately has no destructive retry script.
A developer/operator must reconcile or prepare an independently approved retry from an intact source or restored backup.

## 5. Validate exact history while still frozen

```sh
mysql --defaults-extra-file="$S2_CLIENT_CNF" --database="$S2_DATABASE" --batch \
  < docs/sql/events-columnstore/03-validate.sql
```

Require equal `source_rows` and `destination_rows`; require `missing_rows`, `extra_rows`, `changed_rows`, and `duplicate_workspace_ids` all zero.
The two per-workspace summaries must match, including taskless counts and ID bounds.
The comparison covers nullable task/job IDs, actors/roles, kind, JSON payload, microsecond timestamps, and derived shard keys, not merely totals.
Review representative task timelines, taskless governance events, and lineage reads in the isolated rehearsal.
Validation can scan/join all source and destination rows and needs its own resource budget.
Do not resume traffic on a partial result, query error, or mismatch.

## 6. Prepare every aggregator allocator

After validation, while no process can insert:

```sh
export S2_DATABASE
mkdir -p .orca/events-cutover
python3 scripts/render-events-allocator.py > .orca/events-cutover/allocator-reviewed.sql
# Inspect the rendered database-qualified statement before executing it.
mysql --defaults-extra-file="$S2_CLIENT_CNF" --database="$S2_DATABASE" \
  < .orca/events-cutover/allocator-reviewed.sql
```

The renderer fills the database identifier in `04-allocator.sql` with a safely quoted `$S2_DATABASE`.
Retain the rendered statement with the operator's execution evidence.
The database-qualified `AGGREGATOR SYNC AUTO_INCREMENT ON database.events_columnstore ALL` advances aggregator-local allocation after explicit copied IDs.
The connection must select the approved database and have the privileges needed across **all** aggregators.
Confirm topology coverage and command success; reconnecting a missed old writer is not safe.
SingleStore documents this operation and its implicit commit in [AGGREGATOR SYNC AUTO_INCREMENT](https://docs.singlestore.com/db/v8.9/reference/sql-reference/data-manipulation-language-dml/aggregator-sync-auto-increment/).
This does not create global commit-order semantics.

## 7. Controlled corrected startup, then release traffic

Only after prerequisites, copy, exact validation, and allocator preparation succeed may the operator start the reviewed corrected binary with external traffic and background writers still held.
Never restart an old binary against this split layout: it writes `events`, which corrected code does not read.
Probe task and document history, taskless events, cross-workspace isolation, and lineage using the intended aggregator endpoints.
Make one controlled current application write and verify its event appears in `events_columnstore` with an allocated ID beyond copied history and not in the legacy table.
Recheck legacy source counts to detect any late old writer.
If any check fails, stop corrected processes and keep the freeze; do not make the source and target diverge further.

After operator acceptance, release traffic and retain the reviewed SHA, backup/rehearsal identity, ledger before/after, validation output, allocator result, process freeze evidence, and resumed-write evidence.
Keep `events` intact without dropping or renaming it until a separately approved retention decision.
After corrected writes resume, legacy `events` is no longer a complete current history; switching old binaries back is not a rollback plan.
Rollback requires an approved compatible backup restoration and matching binary, with explicit handling of post-cutover writes.
No merge, release, deployed SQL, or rollback was performed as part of this task's developer-review delivery.
