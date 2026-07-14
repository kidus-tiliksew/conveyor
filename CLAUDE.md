# Conveyor — agent notes

The authoritative design is [conveyor-spec.md](conveyor-spec.md) (v1.6, accepted).
When code and spec disagree, the spec wins; spec changes go by amendment
with a version bump (§21), never silent edits.

## Conventions

- Backend is Go everywhere (spec §17.0): `net/http` + chi, cobra for the
  CLI. Phase 2 adds pgx + sqlc + River — do not introduce other
  persistence or queue dependencies.
- `cmd/conveyor-shim` and the sandbox execution plane are retired and deleted
  by spec §21.4. Do not reintroduce them without an accepted amendment.
- Comments citing the spec use `(spec §N)` — keep these accurate; they
  are the traceability layer between code and design.
- `TODO(phase1)` was the blocking-gap marker; none may remain on the closed
  Phase 1 baseline. `TODO(phase1-followup)` marks accepted deferred work.
- Build/test: `make build`, `make test`, `make vet`.

## Phase discipline

Phases 1–2 are complete and validated. The roadmap was re-phased for the
Beta milestone (spec §19 v1.5, rationale in §21.2–§21.5; working breakdown
in [docs/beta-plan.md](docs/beta-plan.md)). Note §21.4 retires the sandbox
execution plane — Phase 1–3 execution contracts (runner, adapters,
credential pool, shim, images) are superseded, not preserved. Pre-Beta is
exactly four phases:

- **Phase 3** *(complete)* — full pipeline: multi-stage orchestration,
  triage/spec/code-review agents, §4.1 spec format, proto-pack role prompts,
  per-repo images, PR-comment redirects, and timeouts. The historical budget
  breaker was removed by spec §21.6.
- **Phase 4** *(complete)* — UI rewrite (shadcn/ui, full §13.3): app shell,
  stage-grouped feed, task detail panel, task intake, read-only workspace
  snapshot.
- **Phase 4.5** *(complete)* — dynamic workspace configuration (spec §21.3):
  Postgres-backed workspace config, validated config write API with
  `config.updated` audit events, hot reload, editable Workspace UI.
- **Phase 4.7** *(implementation complete; live exit pending)* — MCP execution pivot (spec §21.4): retire the
  sandbox execution plane (runner, adapters, credential pool, shim, images,
  snapshots); triage/spec become in-process API calls on
  `CONVEYOR_API_KEY`; implementation *and code review* delegate to
  operator-owned agents via the MCP work-order server (stage-typed work
  orders, self-review forbidden at claim time, in-session review loop via
  `await_review`, in-process review as fallback); requirements tree UI for
  the spec corpus; artifacts; idempotent MCP `create_task` intake that
  enqueues the existing triage pipeline (§21.5) →
  **Beta: Conveyor develops Conveyor**.

Do NOT build post-Beta or deferred surfaces:
monitor agent, `.conveyor/` repo hints (Phase 5); memory store / pgvector
(Phase 6); transcript mining / self-improvement / eval rig (Phase 7);
managed-execution reintroduction — verification agent, K8sRunner, multi-repo
worktree sets, aggregate cost dashboard (Phase 8, demand-triggered);
enterprise SSO/SCIM/RBAC/HA (Phase 9, demand-triggered). The command-policy
shim approval cards and environment inference/repair are retired (§21.4),
not deferred — do not build them at all.
