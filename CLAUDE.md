# Conveyor — agent notes

The authoritative design is [conveyor-spec.md](conveyor-spec.md) (v2.18,
accepted — the v2.0 consolidated restatement of v1.0–v1.40 plus §21.41–§21.58
through Phase 5 closure, the accepted Phase 6, the §21.47 dependency
semantics, §21.48 worktree containment, the §21.49 blueprint
presentation surface, §21.50–§21.52 planning repo exploration
across workspace repos with a configurable planning model and
configurable exploration caps, §21.53 attempt-aware recovery, §21.55
rejecting the memory store (Phase 7 → demand-triggered recall), the
accepted **§21.58 Phase 8 — the desired-state document model** (four
durable document tiers, the spec artifact retired with its functions
relocated, execution plans on the existing stage machinery,
task-centric delivery, Tasks view), and the §21.54 deployment &
multi-user scope resequenced to Phase 9; the body
§§1–20 is normative, §21 is the change record). When code and spec disagree,
the spec wins; spec changes go by amendment with a version bump (§21), never
silent edits.

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
- Build/test: `make build`, `make vet`, `make fmt-check`, `make test`, and
  `make test-integration`. `make test` is the ordinary local aggregate: Go
  tests plus web typecheck, Biome lint/format checking, and Playwright. CI
  enforces the complete gate on every pull request; its PostgreSQL service
  uses the CI-only `make test-integration-ci` entrypoint.

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

Beta was achieved July 15, 2026 (§19 exit criterion met). **Phase 5 is
complete** (5.1–5.6, closed by §21.43–§21.46; historical breakdown in
[docs/phase5-plan.md](docs/phase5-plan.md)): worker + gate toggles + one
queue with per-task hold (§21.31), adversarial review panel,
factory-coordinated GitHub, evidence-gated `submit_for_review`, worker
service packaging, monitor agent + `.conveyor/hints.yaml`. The worker is
a thin supervisor over the unchanged §17.4 MCP lifecycle — never a second
protocol, adapter interface, or credential pool.

**Active scope is Phase 6 — planning & the knowledge graph** (§21.46;
working breakdown in [docs/phase6-plan.md](docs/phase6-plan.md)), in
order: 6.1 blueprint materialization (approved specs with a
`decomposition` fan out into child tasks entering at `implement`) +
dependency-gated claiming (blocked is a derived predicate in the same
enforcement layer as hold — never a stored state, never a priority
field); 6.2 planning sessions producing requirement documents (living,
versioned intent docs with `REQ-n` statement IDs — confirmed, never
gated; the curated features tree retires and migrates) and blueprints
(through the unchanged §4.1 validation and §13.1 spec gate; optional
`serves` links to requirements); AI SDK stream protocol over SSE served
by `conveyord` — no Node sidecar; transcripts archived as artifacts;
6.3 lineage links (one polymorphic `links` table, edges written only by
pipeline machinery, projections of `events`) + graph-walk context
assembly. The chain is requirement → blueprint → code → evidence — no
epic entity, flat requirement corpus (no hierarchy curation). v1
dependencies are ordering gates — branch stacking stays deferred
(§8.3).

**Phase 8 — the desired-state document model is accepted (§21.58)**
and follows Phase 6 closure (working breakdown in
[docs/phase8-plan.md](docs/phase8-plan.md)), in order: 8.1
requirements v2 (REQ→AC nested structure, user-story framing) +
markdown-only product-overview uploads with promotion; 8.2 System
Design corpus (factory-resident versioned markdown, propose→confirm,
freshness-checked) + `DEC-n` decision records; 8.3 task-centric
delivery — §4.1 fences and decomposition materialization retire; the
spec stage survives re-contented as the plan stage (dispatched
stage-typed order, versioned markdown exec plans with done-criteria,
§13.1 gate/redirect semantics byte-compatible with today's spec
approval; `submit_plan` succeeds `submit_spec`); tasks carry attached
context (`serves` at task level), planning finalize proposes
document-revisions + task-set bundles; 8.4 Tasks view (list-first management — never priority,
assignee, or declared phases). Nothing retires before 8.3; do not
start 8.x while Phase 6 is the active scope.

**Phase 9 — deployment & multi-user (§21.54, resequenced §21.58)**
follows Phase 8: embedded worker (may land earlier), identity/grants,
delivery tiers making GitHub optional, packaging.

Do NOT build deferred surfaces beyond that: **there is no memory store —
`store_memory` and memory rows are rejected by §21.55, not deferred**;
Phase 7 is demand-triggered read-only recall over existing artifacts
(get-side MCP tools per §21.12, pgvector as secondary index), built
only on evidence of reachability misses from operating 6.3, and
knowledge promotes into requirements/design docs/decisions/hints/pack
instead of parking in rows; transcript mining /
self-improvement / eval rig (Phase 10); managed-execution
reintroduction — independent verification agent, K8sRunner, multi-repo
worktree sets, aggregate cost dashboard (Phase 11, demand-triggered);
enterprise SSO/SAML/SCIM, RBAC beyond two roles, HA (Phase 12,
demand-triggered). The
command-policy shim approval cards and environment inference/repair are
retired (§21.4), not deferred — do not build them at all.
