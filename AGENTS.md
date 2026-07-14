# Conveyor — agent notes

The authoritative design is [conveyor-spec.md](conveyor-spec.md) (v1.3, accepted).
When code and spec disagree, the spec wins; spec changes go by amendment
with a version bump (§21), never silent edits.

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

Phases 1–4.5 are complete. Preserve their contracts and the full multi-stage
pipeline described by spec §19 and [docs/beta-plan.md](docs/beta-plan.md). Do
not build post-Beta work without explicit activation. K8sRunner,
multi-repo worktree sets, verification, and the aggregate cost dashboard
remain demand-triggered Phase 8 scope.
