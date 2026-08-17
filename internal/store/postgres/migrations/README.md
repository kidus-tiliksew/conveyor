# PostgreSQL migrations

Migration numbers are append-only delivery identifiers. Never fill a gap or
renumber a migration after its number has appeared on a branch: parallel work
may already depend on the ordering observed from `main`.

`096` is permanently burned. It was assigned to work that landed out of order
after `097`; reusing it would make migration order depend on which branch a
deployment observed first. The sequence therefore advances from `095` to
`097`, and all new migrations must use a number greater than the highest one
already present.
