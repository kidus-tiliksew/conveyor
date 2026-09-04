# The log core

Conveyor is moving its persistence from a hand-maintained PostgreSQL schema
plus River to an append-only event log that any database can host. This page
describes what exists today, how to run it against a deployment, and what it
does not yet do. The rollout plan has six phases; phase 1 and the phase 2
checker are implemented.

## What the log is

Every entity is one stream inside a workspace: `task/<id>`,
`work_order/<id>`, `requirement/<id>`, `design/<id>`, `decision/<id>`,
`reference_document/<id>`, `planning_session/<id>`, `planning_bundle/<id>`,
`workspace/<id>`, `worker/<id>`, and `user/<id>` under the reserved
`_deployment` workspace. A write is an append that names the stream version it
expects; two writers racing on the same stream have exactly one lose. Read
models fold streams and can be rebuilt by any replica.

The contract is `internal/eventlog`. Two drivers implement it: `memlog`
(in-process, the test double) and `pglog` (PostgreSQL). `logtest` is the
conformance suite; passing it is what "implements the contract" means. A
`Router` binds workspaces to drivers, so one workspace can move to another
engine while the rest stay put.

The PostgreSQL driver uses four tables (`event_log`, `event_log_streams`,
`event_log_positions`, `event_snapshots`) that it creates itself at startup,
the way River's migrator does. They carry no foreign keys, no CHECK
constraints, and no triggers. Serialization is two row locks in a fixed
order: the workspace's position row, then the stream's head row. Holding the
workspace row until commit means positions become visible in order, so a
tailer that has seen position N knows every lower position is committed.

## What runs today

**Dual-append.** Every legacy `events` insert also appends to the log in the
same transaction. The stream is chosen by event family and payload id
(`requirement.*` by `requirement_id`, `system_design.*` by `document_id`,
`work_order.*` by `work_order_id`, and so on), falling back to the task the
row was bound to and then to the workspace stream. Nothing reads the log to
serve traffic.

**Genesis import.** `conveyor migrate-log` builds the log for a deployment
whose data predates it. Per workspace it appends legacy events not yet in the
log, in id order, then writes one `log.snapshot_imported` event per entity
carrying its projection rows verbatim (row plus child rows, minus credential
columns) with a content hash. Re-running with unchanged rows writes nothing.
Each snapshot is written while holding the stream's head lock, so a live
write cannot slip between reading the rows and snapshotting them. The run
holds the startup-migrations lock and never overlaps a daemon applying
migrations.

The legacy events table cannot rebuild state by replay: it was task-scoped at
birth, has no per-stream versions, and document bodies live in version tables.
That is why the import snapshots current state instead of replaying history,
and why history is imported for timelines only.

**Parity.** `conveyor log-parity` replays a workspace's log into an
in-process catalog and compares every entity's last snapshot hash with its
live rows. `drift` names the entities whose rows changed and the event kinds
that arrived since; those kinds are the fold rules the next projector has to
learn. It exits 1 on any drift or missing entity, so it works as a soak gate.

**Projection framework.** `internal/projection` runs projectors over a
workspace's log from the last snapshot position, snapshots at a fixed cadence,
and rebuilds from zero when a projector's version changes. `catalog` is the
first projector.

## Operating it

1. Deploy a binary that includes the log core. On startup it creates the log
   tables and begins mirroring writes. Nothing else changes.
2. Take a dump. Run `conveyor migrate-log` with `CONVEYOR_DATABASE_URL` set.
   It is safe with the daemon running and safe to repeat.
3. Run `conveyor log-parity --workspace-id <ws>` and keep running it during
   the soak. Every drifted entity lists the kinds that moved it.

Rollback of everything above is dropping the four tables. No legacy table
changed shape.

## Measured on the production data

Taken 2026-09-04 against a restore of the devbox database on a laptop.

| Measure | Value |
|---|---|
| Legacy events in the demo workspace | 325,615 |
| `migrate-log`, first run, all workspaces | 36 s |
| `migrate-log`, second run (writes nothing) | 11 s |
| Snapshots written for demo | 2,117 (408 tasks, 1,616 work orders, 93 documents and decisions) |
| Catalog replay of the demo log | 4.7 s for 327,733 events |
| `log-parity`, all workspaces | clean |
| `event_log` table size | 375 MB, against 361 MB for `events` |
| Snapshot payloads | 83 MB total, largest 2 MB |

Two kinds account for 84% of the demo log: `worker.heartbeat` (196,062) and
`work_order.lease_renewed` (77,349). Both are liveness telemetry rather than
facts about an entity. Whether the mirror and the import should carry them is
an open decision; excluding them would shrink the log by that share.

## What it does not do yet

- Serve any read from the log. Phase 2 continues with projectors per entity
  family, driven by the parity report's unfolded kinds.
- Replace River. Phase 3 puts the queue behind a store port, then runs a
  log-backed queue in shadow.
- Cut a workspace over. Phase 4 adds a per-workspace flag.
- Run on SingleStore or SQLite. Phase 6 adds drivers against the conformance
  suite; the contract needs only a unique key and a transaction.
