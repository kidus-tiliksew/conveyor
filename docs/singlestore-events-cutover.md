# SingleStore events cutover

For an existing database with migrations 0001–0026 already applied and legacy rowstore events.
If v0.55.0 has already applied migration 0027, use the layout section below instead.
Run the SQL below in the SingleStore web SQL editor, against the application database, before deploying the corrected code.
Keep a restorable backup and stop every application instance, reader, and other writer BEFORE the copy and keep them stopped through verification, allocator synchronization, and corrected-code deployment.
The corrected code must never serve traffic against an empty or partially copied target.
Allow enough capacity for both tables; keep legacy `events` unchanged.
Rehearse the full copy and backup restore before scheduling the freeze.

## 1. Create the target

Run [01-create.sql](sql/events-columnstore/01-create.sql) once to create the empty `events_columnstore`.
If the table already exists or contains rows, inspect it before continuing; do not blindly repeat creation or copy.

## 2. Copy history

Run [02-copy.sql](sql/events-columnstore/02-copy.sql) once while writers remain stopped.
It preserves every event field and ID, including taskless events, and leaves `events` intact.
Do not deploy the corrected code before the copy is complete.

## 3. Verify the copy

Run [03-validate.sql](sql/events-columnstore/03-validate.sql).
Source and destination row counts must match, and `missing_rows`, `extra_rows`, `changed_rows`, and `duplicate_workspace_ids` must all be zero.
The per-workspace summaries must also match.
For the legacy rowstore copy, additionally require this derived shard check to return zero:

```sql
SELECT COUNT(*) AS changed_shard_keys
FROM events s JOIN events_columnstore d ON s.workspace_id=d.workspace_id AND s.id=d.id
WHERE NOT (BINARY d.event_shard_key <=>
 BINARY COALESCE(NULLIF(s.task_id, ''), CONCAT('event/', CAST(s.id AS CHAR))));
```

For the already-converted v0.55.0 layout use its source-shard check below instead.
Equal total counts alone are not sufficient.
Stop on any discrepancy; do not synchronize allocators or resume writes.

## 4. Synchronize allocators

Open [04-allocator.sql](sql/events-columnstore/04-allocator.sql), replace the backticked `your_database` with the actual database name, and execute it in the SQL editor.
Keep the database-qualified table name and `ALL`: copied explicit IDs do not advance every aggregator's allocator.
If the database name contains a backtick, double it inside the quoted identifier.
Do not resume writes unless synchronization succeeds across all aggregators.

## 5. Deploy corrected code

Start the corrected application only after successful copy verification and allocator synchronization.
For an existing database at 0026, only now start the corrected code: startup applies pending migrations 0027–0030 (which do not write application events); migration 0027's retired automatic conversion is not executed.
Check that a new event appears in `events_columnstore` with an ID above the copied history before reopening traffic.
Retain legacy `events`; do not rename, truncate, or drop it as part of this cutover.
After new writes, rolling back to code that reads legacy `events` would hide those writes; keep traffic stopped and reconcile history before any rollback.

## Fresh installations

There is no history to copy.
Create an empty database and run `01-create.sql`, then use normal `conveyor init` or application startup to apply the numbered schema migrations.
No schema-only helper, copy, or copied-ID allocator synchronization is needed for an empty installation.

## v0.55.0 layout: migration 0027 already converted events

With all application readers and writers stopped, inspect the database:

```sql
SHOW CREATE TABLE events;
SHOW TABLES LIKE 'events_columnstore';
SELECT version, name FROM conveyor_singlestore_migrations WHERE version >= 27;
```

If 0027 is recorded, `events` is COLUMNSTORE with `event_shard_key`, and `events_columnstore` is absent, the released automatic migration already ran.
Do not rename the source or rerun that migration.
Run `01-create.sql`, then copy once into the empty target, preserving the existing shard keys:

```sql
INSERT INTO events_columnstore
 (id, event_shard_key, task_id, job_id, kind, actor_id, actor_role, payload_json, at, workspace_id)
SELECT id, event_shard_key, task_id, job_id, kind, actor_id, actor_role, payload_json, at, workspace_id
FROM events;
```

This is one full source scan; no global ID batching or rowstore index assumption is needed.
Run `03-validate.sql` and also require this shard comparison to return zero:

```sql
SELECT COUNT(*) AS changed_shard_keys
FROM events s JOIN events_columnstore d ON s.workspace_id=d.workspace_id AND s.id=d.id
WHERE NOT (BINARY s.event_shard_key <=> BINARY d.event_shard_key);
```

Stop on any discrepancy. Then run `04-allocator.sql` with the actual database placeholder replaced, retaining `ALL`.
Only after successful validation AND allocator synchronization start corrected code and perform step 5's new-event check before reopening traffic.
Keep legacy `events` intact. For an unexpected or partially converted layout, stop and reconcile it before copying; this procedure does not promise automatic recovery.
