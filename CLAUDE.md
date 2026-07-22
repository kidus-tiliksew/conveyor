# Conveyor — agent notes

The authoritative design is [conveyor-spec.md](conveyor-spec.md) (v1.39, accepted).
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
Beta milestone (spec §19, rationale in §21.2–§21.10; working breakdown
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
- **Phase 4.7** *(complete — Beta achieved July 15, 2026)* — MCP execution pivot (spec §21.4): retire the
  sandbox execution plane (runner, adapters, credential pool, shim, images,
  snapshots); triage/spec become in-process API calls on
  `CONVEYOR_API_KEY`; implementation *and code review* delegate to
  operator-owned agents via the MCP work-order server (stage-typed work
  orders, self-review forbidden at claim time, in-session review loop via
  `await_review`, in-process review as fallback); requirements tree UI for
  the spec corpus; artifacts; idempotent MCP `create_task` intake that
  enqueues the existing triage pipeline (§21.5) →
  **Beta: Conveyor develops Conveyor**.

Task branch names are assignments, not pre-created refs (§21.7). Immediately
after reading a work order, the implementing agent uses `conveyor checkout
<task-id>` to resolve a dedicated sibling worktree and performs every edit,
test, commit, and push there (§21.8). Conveyor does not mutate the primary
checkout or reset task history.
Workspace context is explicit across REST, CLI, MCP, dispatch, and
reconciliation (§21.10); omission is compatible only for a singleton workspace.

Beta was achieved July 15, 2026 (§19 exit criterion met). Post-Beta scope is
accepted by spec §21.12–§21.21 and now active (working breakdown in
[docs/phase5-plan.md](docs/phase5-plan.md)): Phase 5.1 worker (`conveyor worker run`) + independent
spec/merge gate toggles replacing L0–L3 + harness registry (execution modes
were subsequently removed by §21.31 — one queue, workers claim what they can
serve, with a per-task hold as the reservation primitive); Phase 5.2 adversarial
review panel; Phase 5.3 factory-coordinated GitHub (issue on spec approval,
verdict mirroring; PR stays at submit, §21.15); Phase 5.4 evidence-gated
`submit_for_review`; Phase 5.5 worker service packaging (`conveyor worker
install`, §21.16). Sequence 5.1 → {5.2 ∥ 5.3} → 5.4 → 5.5 → 5.6. The worker is
a thin supervisor over the unchanged §17.4 MCP lifecycle — never a second
protocol, adapter interface, or credential pool.

Do NOT build post-Beta or deferred surfaces beyond that:
monitor agent, `.conveyor/` repo hints (Phase 5.6, after the worker); memory
store / pgvector (Phase 6 — transport decided as MCP tools by §21.12, scope
not pulled forward); transcript mining / self-improvement / eval rig
(Phase 7); managed-execution reintroduction — independent verification
agent, K8sRunner, multi-repo worktree sets, aggregate cost dashboard
(Phase 8, demand-triggered); enterprise SSO/SCIM/RBAC/HA (Phase 9,
demand-triggered). The command-policy shim approval cards and environment
inference/repair are retired (§21.4), not deferred — do not build them at
all.
