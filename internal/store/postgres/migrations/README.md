# PostgreSQL migrations

Migration numbers are append-only delivery identifiers. Never fill a gap or
renumber a migration after its number has appeared on a branch: parallel work
may already depend on the ordering observed from `main`.

`096` is permanently burned. It was assigned to work that landed out of order
after `097`; reusing it would make migration order depend on which branch a
deployment observed first. The sequence therefore advances from `095` to
`097`, and all new migrations must use a number greater than the highest one
already present.

Two migrations were briefly released with version `118`:
`118_dependency_additions.sql` and
`118_requirement_system_design_archival.sql`. The dependency migration is the
canonical version-118 ledger identity and its bytes remain unchanged. Migration
119 idempotently establishes both schemas, while the migrator recognizes the
known archival ledger identity so databases that applied it can advance without
rewriting their existing version-118 row.
