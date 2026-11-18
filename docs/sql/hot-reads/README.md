# Prepare hot-read indexes on an existing database

Use these scripts when an existing SingleStore or PostgreSQL database must gain the approved hot-read layout by hand. Conveyor does not apply this DDL on startup for an existing installation. A migration 0044 or 171 ledger row is not readiness.

This procedure is operator work under your authority. Keep a restorable backup before any ALTER or CREATE INDEX. Do not run the application as the migrator. Do not put credentials in these files or in pasted EXPLAIN output.

The scripts do not DROP, TRUNCATE, rename, or create tables or globals. They do not change column storage, types, or index names beyond the approved manifest in `internal/store/singlestore/hot_read_indexes.go` and `internal/store/postgres/migrations/171_hot_read_indexes.sql`.

## Treat prepared, legacy, partial, and error as different

Readiness is per lookup, captured once when a Store opens. Preparation of the live schema does not change a process that is already running.

- **Legacy.** The approved generated column or key is absent. Reads keep the original-value filter only and omit the generated name. Results stay correct. EXPLAIN may show only existing workspace keys plus a residual original filter. That is not proof of the new index.
- **Partial.** Some lookups match the approved definition and key. Others are absent. Ready lookups may name the generated column. Missing lookups stay on the original filter. Independent keys do not enable each other.
- **Prepared.** Every approved column and key for that lookup matches, and the process opened after that layout existed. Prepared SingleStore predicates name the generated column and still compare the original value. The original comparison is the exact guard. A digest alone is never authoritative.
- **Error.** A metadata query fails, or a present name has the wrong definition, type, or key columns. Treat that as an explicit error. Stop. Do not guess absence. Do not mark the Store ready. Do not DROP or rewrite the object to force a match.

A running Store keeps the snapshot from Open until you close and reopen it, or restart it. There is no schema polling, no automatic ALTER, and no catch-and-retry on an unknown column.

## Inspect names and definitions first

1. Select the application database.
2. Run the preflight queries in [prepare-singlestore.sql](prepare-singlestore.sql) or [prepare-postgres.sql](prepare-postgres.sql).
3. Compare each present name to the approved definition in that file.

On SingleStore, `SHOW CREATE TABLE` is the definition check for each `PERSISTED VARCHAR(64)` column. `information_schema.statistics` is the key check. `COLUMNSTORE HASH` is a valid actual `index_type` for `USING HASH` keys. `HASH` is also valid. Rowstore keys report `BTREE`. `SKIPLIST` is also valid for those keys. `non_unique` must be 1. Key columns must match the approved order.

On PostgreSQL, compare `pg_get_indexdef` to the `CREATE INDEX` text. PostgreSQL does not use the SingleStore generated names.

If a metadata statement fails, stop. That failure is not evidence that the object is missing.

If a name exists with a different expression, type, uniqueness, or column order, stop. Do not claim the layout is repaired.

## Apply only missing matching statements

1. Apply a statement only when its column or index name is absent.
2. Skip a statement whose present definition already matches.
3. Do not repeat ADD on SingleStore for a name that already exists. Those `ADD COLUMN` and `ADD KEY` statements do not take `IF NOT EXISTS`.
4. On PostgreSQL, `CREATE INDEX IF NOT EXISTS` is rerunnable when the name is absent or already present. It does not compare definitions. Do not use it to overwrite a mismatch.
5. After a partial run, inspect again. SingleStore commits each ALTER. Do not undo with DROP.

## Confirm generated names, original equality, and EXPLAIN

1. Re-run the postflight `SHOW CREATE` or `pg_get_indexdef` queries.
2. Confirm every intended object now matches.
3. Replace the quoted `WORKSPACE_ID` and entity literals in the EXPLAIN statements with values from your fixture. Do not paste credentials.
4. On SingleStore, EXPLAIN must name the generated column on a prepared lookup. The statement must still include the original-value equality.
5. On PostgreSQL, EXPLAIN must be able to use the 171 index for that original-value predicate.
6. Record prepared and legacy plans separately. A correct legacy result is not the prepared index result.

## Close and reopen, or restart, after preparation

1. Finish postflight while traffic may still use the old snapshot.
2. Close and reopen each Store, or restart each process, after the schema matches.
3. Restart one process at a time if you need a rolling reopen. A process that has not reopened keeps legacy or partial behavior, which stays correct.
4. After reopen, Open discovers the new layout once. Lookups that now match become prepared. Lookups that remain absent stay legacy. A metadata or definition error fails Open instead of serving a guessed path.

Uncoordinated removal of an indexed column from a running Store is not a supported transition.
