package singlestore

// crateMigrationVersion is the migration that adds the crates and
// crate_requirements tables. Its Go step adds tasks.crate_id after the file
// runs (feature-crates CR-1; component-crates).
const crateMigrationVersion = 45

// crateTaskColumn is the nullable membership column. SingleStore DDL commits
// implicitly and has no ADD COLUMN IF NOT EXISTS, so migrate adds it through
// addNullableColumns, which checks information_schema under the startup
// migration lock, and a rerun after an interrupted startup adds nothing.
// There is no backfill: every existing task belongs to no crate.
var crateTaskColumn = nullableColumn{"tasks", "crate_id", "VARCHAR(64) NULL"}
