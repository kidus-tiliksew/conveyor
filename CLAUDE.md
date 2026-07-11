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

Phase 2 is complete (spec §19): Claude Code + Codex adapters, credential
routing, Postgres/River state, the standalone local runner, append-only events,
review UI, and shim-boundary redaction are implemented and validated. Preserve
the accepted Phase 1 core loop and Phase 2 contracts. Phase 3 has not started;
do not build K8sRunner, multi-repo worktree sets, aggregate cost dashboard, or
budget breakers until it is explicitly activated.
