# Conveyor — agent notes

The authoritative design is [conveyor-spec.md](conveyor-spec.md) (v1.1, accepted).
When code and spec disagree, the spec wins; spec changes go by amendment
with a version bump (§20), never silent edits.

## Conventions

- Backend is Go everywhere (spec §17.0): `net/http` + chi, cobra for the
  CLI. Phase 2 adds pgx + sqlc + River — do not introduce other
  persistence or queue dependencies.
- `cmd/conveyor-shim` must stay stdlib-only: it ships as a static binary
  inside every sandbox image.
- Comments citing the spec use `(spec §N)` — keep these accurate; they
  are the traceability layer between code and design.
- `TODO(phase1)` was the blocking-gap marker; none may remain on the closed
  Phase 1 baseline. `TODO(phase1-followup)` marks accepted deferred work.
- Build/test: `make build`, `make test`, `make vet`.

## Phase discipline

Phase 1 is complete (spec §19, amendment §21.1): Codex adapter,
LocalDockerRunner, isolated task checkout manager, secrets injection, base
image, handoff snapshots, GitHub issue → PR, logs only. Preserve that boundary
until Phase 2 starts explicitly (no Postgres, review UI, or redaction engine) —
but don't foreclose later phases either: keep state behind
`internal/store.Store` and all sandbox logs flowing through the shim.
