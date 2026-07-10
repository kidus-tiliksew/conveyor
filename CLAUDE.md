# Conveyor — agent notes

The authoritative design is [conveyor-spec.md](conveyor-spec.md) (v1.0, accepted).
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
- `TODO(phase1)` marks intentional Phase 1 gaps. Grep for them to find
  the current work surface.
- Build/test: `make build`, `make test`, `make vet`.

## Phase discipline

We are in Phase 1 (spec §19): Codex adapter, LocalDockerRunner, worktree
manager, secrets injection, base image, handoff snapshots, GitHub
issue → PR, logs only. Don't build ahead of the phase (no Postgres, no
review UI, no redaction engine) — but don't foreclose later phases
either: keep state behind `internal/store.Store` and all sandbox logs
flowing through the shim.
