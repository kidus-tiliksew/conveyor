# SingleStore events cutover

For an existing database with migrations 0001–0026 already applied.
Run the SQL below in the SingleStore web SQL editor, against the application database, before deploying the corrected code.
Keep a restorable backup and stop every application instance and other writer for the entire copy, verification, and allocator synchronization.
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
Equal total counts alone are not sufficient.
Stop on any discrepancy; do not synchronize allocators or resume writes.

## 4. Synchronize allocators

Open [04-allocator.sql](sql/events-columnstore/04-allocator.sql), replace the backticked `your_database` with the actual database name, and execute it in the SQL editor.
Keep the database-qualified table name and `ALL`: copied explicit IDs do not advance every aggregator's allocator.
If the database name contains a backtick, double it inside the quoted identifier.
Do not resume writes unless synchronization succeeds across all aggregators.

## 5. Deploy corrected code

Start the corrected application only after successful copy verification and allocator synchronization.
Normal startup applies any pending numbered migrations; migration 0027's retired automatic conversion is not executed.
Check that a new event appears in `events_columnstore` with an ID above the copied history before reopening traffic.
Retain legacy `events`; do not rename, truncate, or drop it as part of this cutover.
After new writes, rolling back to code that reads legacy `events` would hide those writes; keep traffic stopped and reconcile history before any rollback.

## Fresh installations

There is no history to copy.
Create an empty database and run `01-create.sql`, then use normal `conveyor init` or application startup to apply the numbered schema migrations.
No schema-only helper, copy, or copied-ID allocator synchronization is needed for an empty installation.
