# Conveyor — agent notes

The authoritative design is [conveyor-spec.md](conveyor-spec.md) (v1.26, accepted).
When code and spec disagree, the spec wins; spec changes go by amendment
with a version bump (§21), never silent edits.

## Conventions

- Backend is Go everywhere (spec §17.0): `net/http` + chi, cobra for the
  CLI. Phase 2 adds pgx + sqlc + River — do not introduce other
  persistence or queue dependencies.
- The sandbox execution plane, including `cmd/conveyor-shim`, is retired by
  spec §21.4. Do not reintroduce runner, adapter, image, credential-pool,
  snapshot, secret-ref, or tool-policy code without a new accepted amendment.
- Comments citing the spec use `(spec §N)` — keep these accurate; they
  are the traceability layer between code and design.
- `TODO(phase1)` was the blocking-gap marker; none may remain on the closed
  Phase 1 baseline. `TODO(phase1-followup)` marks accepted deferred work.
- Build/test: `make build`, `make test`, `make vet`.

## Phase discipline

Phases 1–4.7 are complete and Beta was achieved July 15, 2026. Phase 5.1 is
active under spec §21.12–§21.14 and §§21.20–21.21 and [docs/phase5-plan.md](docs/phase5-plan.md).
Preserve the full multi-stage pipeline described by spec §19 and
[docs/beta-plan.md](docs/beta-plan.md). Do not build later post-Beta work
without explicit activation. K8sRunner, multi-repo worktree sets,
verification, and the aggregate cost dashboard remain demand-triggered
Phase 8 scope.
MCP task intake is part of the accepted Phase 4.7 surface (§21.5): it must
create the normal durable task and enqueue existing triage, never bypass the
pipeline with a second ad hoc triage implementation.
Task branch names are assignments, not pre-created refs (§21.7). Immediately
after reading a work order, the implementing agent uses `conveyor checkout
<task-id>` to resolve a dedicated sibling worktree and performs every edit,
test, commit, and push there (§21.8). Conveyor must not mutate the agent's
primary checkout or reset task history.
Workspace-scoped REST, CLI, MCP, dispatch, and reconciliation operations carry
an explicit immutable workspace ID (§21.10); omitted context is compatible only
when exactly one workspace exists and ambiguity must fail closed.
