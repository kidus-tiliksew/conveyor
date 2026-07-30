# Conveyor: A Software Factory Platform

**Specification — v2.9**
**Date:** July 30, 2026
**Status:** Accepted — **Beta achieved July 15, 2026** (§19 exit criterion met). The v2.0 text is the **consolidated restatement** of v1.0–v1.40: the body (§§1–20) states the current design directly, with every accepted amendment folded in. The amendment log (§21) is the change record and review rationale; §21.40 records the consolidation itself. v2.1 (§21.41) adds supervision hygiene adopted from an external comparative review — worker stall detection, deterministic claim ordering, worktree path safety, pinned defaults, forge error categories, observational rate-limit telemetry — and corrects the W14 restatement defect. v2.2 (§21.42) adds worker-side first-activity liveness. v2.3 (§21.43) completes the Phase 5.3 GitHub review projection and corrects its publication invariant. v2.4 (§21.44) completes Phase 5.4 evidence-gated review submission. v2.5 (§21.45) completes the Phase 5.6 monitor, reverse synchronization, and advisory repository hints. v2.6 (§21.46) closes Phase 5 (5.5 worker service packaging complete) and accepts **Phase 6 — planning & the knowledge graph**: blueprint materialization with dependency-gated claiming, in-product planning sessions producing requirement documents and blueprints, requirements reformed as living intent documents (the curated features tree retires), and first-class lineage links along the chain requirement → blueprint → code → evidence, renumbering the deferred phases (memory → 7, flywheel → 8, managed execution → 9, enterprise → 10). v2.7 (§21.47) clarifies dependency semantics from the Phase 6.1 implementation review: unsatisfiable edges surfaced with an audited operator unlink, cross-repo edges legal, the claim gate scoped to implementation orders at claim time only, and queue-clock suspension while blocked. v2.8 (§21.48) contains implicit task worktrees, verifies checkout repository identity, and reconciles terminal cleanup plus primary-checkout pruning without deleting branches or dirty work. v2.9 (§21.49) moves blueprint anchors onto a dedicated presentation surface beside requirements and out of the stage-grouped feed — presentation only, the epic-entity bar stands. Subsequent changes proceed by amendment with version bumps.
**Naming note:** "Conveyor" is a working title pending trademark clearance (known adjacent uses include Hydraulic's Conveyor packaging tool and the Konveyor modernization project). The CLI command, branch prefix (`conveyor/task-<id>`), paths, and issue labels are branded `conveyor`; a final-name change would require renaming these user-facing conventions, so clearance should happen before external users script against them.

---

## 1. Overview

Conveyor is an orchestration platform for automated software development —
a **software factory**. It runs a multi-stage pipeline — triage, spec,
implementation, adversarial code review, human gates, merge — against Git
repositories, and it delegates the expensive stages to coding agents the
operator already owns.

The division of labor is the design's thesis, established by the MCP
execution pivot (§21.4): **Conveyor keeps the brain and delegates the
hands.** The control plane owns orchestration, lifecycle state, gates,
specs, review aggregation, audit, and branch metadata. It never executes
implementation itself: implementation, specification, and review run in
operator-owned agent sessions — interactive (Claude Code, Codex CLI, and
similar, connected over MCP) or unattended (the same CLIs spawned headless
by the operator's own **worker** daemon). Conveyor does not implement a
coding agent, does not sandbox one, and does not pool anyone's
credentials; agents run under the operator's own logins on the operator's
own machines, and the **pushed branch is the trust boundary** every gate
judges.

The design philosophy follows the software-factory model: engineers do not
primarily write code; they operate and improve a system that writes code.
Every human intervention is treated as a signal to be recorded, analyzed,
and engineered away over time. Beta — **Conveyor developing Conveyor** —
was achieved July 15, 2026 (§19).

### 1.1 Goals

1. Run multi-stage agent pipelines end to end: from an issue or trigger to
   a merged pull request, with human gates only where the operator keeps
   them.
2. Delegate execution to any coding agent over one MCP work-order
   lifecycle (§17.4) — no adapter interface, no vendor coupling; a
   declarative harness registry (§5) is all a new CLI needs for unattended
   dispatch.
3. Support unattended operation on operator hardware: a worker daemon that
   claims work orders and drives the operator's harness CLIs headless
   (§6), in the tradition of a CI runner.
4. Make review adversarial and independent by construction: self-review is
   forbidden at the protocol boundary, panels aggregate
   unanimous-approve, and independence is labeled honestly where it
   cannot be enforced (§4, §21.4, §21.12).
5. Keep every lifecycle a formal state machine with one enforcement
   point: normative transition tables, a serialized per-task command
   plane, and an append-only event log that is the source of truth
   (§3.3–§3.4).
6. Provide first-class Git worktree conventions so agents and humans work
   the same repository in parallel without interfering — and without the
   factory ever mutating an operator's checkout (§8).
7. Make the factory legible: a costed event timeline per task, a
   living requirement corpus with blueprint and code lineage, and a
   task trail mirrored to GitHub (§11, §13).
8. Close the loop over time: accumulated transcripts, reason codes, and
   the spec corpus feed a memory store and self-improvement engine
   (Phases 7–8, §15).

### 1.2 Non-goals

- Building a novel coding agent or model. Conveyor is an orchestrator.
- Owning execution environments. The sandbox execution plane — runners,
  containers, harness adapters, credential pooling — was retired by
  §21.4; managed execution returns, if ever, as demand-triggered Phase 9
  scope.
- Environment promotion machinery (dev/staging/prod tiers). Deployment to
  production remains the responsibility of the repository's existing
  CI/CD; Conveyor never deploys.
- A hosted multi-tenant SaaS in v1. The initial target is a single team
  self-hosting the control plane.
- IDE features. Humans interact through the review UI, the CLI, and their
  existing editors via worktrees.

### 1.3 Guiding metrics

Two numbers define success and are instrumented from day one:

- **Automation rate:** the percentage of completed tasks shipped with zero
  human turns, and the distribution of human touches across gate
  decisions, check-ins, and recoveries.
- **Cost per shipped change:** inference cost plus attributed human review
  time, per merged PR, per workspace, per harness. Usage is telemetry,
  never an execution gate (§14).

---

## 2. Concepts and terminology

| Term | Definition |
|---|---|
| **Workspace** | A named set of repositories plus configuration (setups, harness registry, gates). The top-level unit of scoping; the control plane serves many (§7). |
| **Task** | A unit of intended change. Tasks move through the factory pipeline under the canonical lifecycle state machine (§3.3). |
| **Stage** | A pipeline step for a task: triage, spec, implement, review. Triage is in-process; spec, implement, and review execute as work orders. |
| **Job** | One execution of one stage for a task. In-process jobs run inside `conveyord`; external jobs are performed by whoever claims the work order. |
| **Work order** | The stage-typed, leased unit of delegated work on the MCP surface (§17.4): queued → claimed → submitted → completed, with recoverable `stale`/`timed_out` and terminal `cancelled` (§3.3). |
| **Worker** | The operator's daemon (`conveyor worker run`): enrolls against a workspace, long-polls the queue, and spawns registered harness CLIs headless to serve claims (§6.4). A thin supervisor over the unchanged §17.4 lifecycle — never a second protocol. |
| **Harness** | An external coding-agent CLI described by a declarative registry entry — command template, model/effort argv, MCP transport, health probe (§5). Data, not an adapter interface. |
| **Setup** | A named, per-workspace execution contract: which harnesses/models/efforts implement, spec, and review, plus the review panel. Selected per task and frozen by value at intake (§6.1–§6.2). |
| **Hold** | A per-task boolean reservation: while held, workers never claim the task's orders; operator-attached agents still may. The only dispatch-routing knob a task carries (§6.3). |
| **Gates** | Two independent human-approval toggles — **spec approval** and **merge approval** — workspace defaults, per-task override, resolved and frozen at intake (§13.1). |
| **Review round / seat** | One dispatch of the setup's review panel: one work order per seat, each seat a pinned model (optionally harness and effort); rounds aggregate unanimously and round-locally (§4). |
| **Triage brief** | Triage's structured framing output — questions the spec must answer, suspected areas, risks — delivered advisory into the spec or implement order (§4). |
| **Requirement** | A living, versioned intent document: what the system is supposed to do in an area and why — generated from operator intent, maintained conversationally, amended by drift reconciliation. Flat corpus, stable `REQ-n` statement IDs, optionally served by blueprints (§4.2, §13.3). |
| **Blueprint** | A spec (§4.1) with a non-empty `decomposition`; approval materializes child tasks with dependency edges. Optionally linked to the requirement(s) it serves — linkable at drafting or retroactively (§4.1, §4.2). |
| **Artifact** | A workspace-scoped context file (document, image, audio) attachable to tasks and requirements, injected into agent context and fetchable over MCP (§9). |
| **Prompt/policy pack** | The versioned bundle of role prompts shipped with the platform (proto-pack since Phase 3). Full pack versioning with shadow-run gating is Phase 8 scope (§2.2). |

### 2.1 Configuration scopes

Configuration is layered across three scopes, with inheritance flowing
downward. A new deployment is functional before any workspace exists.

**Deployment scope (file).** Boot-time substrate the control plane cannot
reconfigure from a table it hasn't connected to yet: database URL, listen
address, pack/role-prompt directory, cache directories, and the
deployment-owned `CONVEYOR_API_KEY` used by in-process AI (triage, title
generation, in-process review fallback). `conveyor.yaml`'s
workspace-scope sections act only as a first-boot bootstrap seed (§21.3).

**Workspace scope (Postgres).** The running truth for everything an
operator tunes while the factory runs, mutated through the authenticated
API/UI with one shared validator, optimistic concurrency
(`config_version` + `If-Match`), `config.updated` audit events, and hot
reload — a routing or registry change takes effect from the next
dispatch, never on in-flight work (§21.3). The document contains: repos
(`{name, url, github, base}`), the harness registry (§5), **setups** and
`default_setup` (§6.1), `max_bounces` (default 10, §4),
`work_order_queue_timeout` (default 24h, §3.3), and
`first_activity_timeout` (default 2m, positive and shorter than every
MCP stage execution timeout, §6.4). `conveyor config export` / `import`
round-trip the database copy for git-versioned backup. Secret values
never appear in configuration in any form.

**Task scope (frozen at intake).** A task resolves and persists **by
value** at creation: its effective setup contract (§6.2), gate toggles
(§13.1), base branch, and assigned branch name. Later workspace edits
never change an in-flight task; the audited exceptions are the explicit
setup-reassignment and recovery re-freeze operations (§6.2). Hold is the
deliberate exception to freezing — a live, mutable, audited toggle
(§6.3).

**Named defaults.** Every operational default the specification names,
gathered in one place; each value's owning section governs (§21.41):

| Default | Value | Owner |
|---|---|---|
| `max_bounces` | 10 | §4 |
| `work_order_queue_timeout` | 24h | §3.2 |
| Work-order claim lease | 5m, renewable | §3.2 |
| Worker liveness lease | 15s | §6.4 |
| Child retry delays | 1s / 2s / 4s, then retry-suppressed | §6.4 |
| Worker `first_activity_timeout` | 2m | §6.4 |
| Harness `stall_timeout` | 10m (`0` disables) | §5.1, §6.4 |
| Dispatch retry (River, T12/T13) | 5 attempts, 10s doubling, 5m cap | §3.3 |
| Control-plane checkout staleness TTL | 14 days | §8.1 |
| Stage timeouts | per-setup, frozen at intake | §6.1, §6.2 |

### 2.2 Role prompts and the pack

Every pipeline stage's behavior is defined by a versioned role-prompt
file (triage, spec, implement, review) — the proto-pack shipped in
Phase 3. The full pack lifecycle designed in v1.0 — workspace-pinned pack
versions, self-improvement diffs against pack content, and **shadow-run
gating** (candidate packs replayed against completed tasks and compared
on routing decisions, bounce counts, and cost before adoption) — is
Phase 8 scope, deferred until the transcript corpus exists to replay
(§15.2, §19). One invariant survives from the original design unchanged:
behaviors live in prompts and configuration; invariants (lifecycle
legality, self-review prohibition, redaction, credential handling) live
in the orchestrator and are not overridable from any scope.

---

## 3. Architecture

Conveyor is a **control plane without an execution plane of its own**.

```
 Triggers (UI · CLI · REST · MCP create_task · GitHub ready-issues · PR comments)
      │
      ▼
 ┌───────────────────────── Control plane (conveyord) ─────────────────────────┐
 │  Command plane + lifecycle state machines (§3.3–§3.4) · River queue          │
 │  In-process AI: triage, titles (CONVEYOR_API_KEY) · Review UI · CLI API      │
 │  MCP work-order server (§17.4) · Append-only events · Postgres               │
 └───────────────┬──────────────────────────────────────────────────────────────┘
                 │ stage-typed work orders (spec · implement · review)
                 ▼
 ┌───────────── Operator-owned execution (BYOA) ────────────────────────────────┐
 │  Interactive agent sessions over MCP · Worker daemon spawning harness CLIs   │
 │  Each claim: conveyor checkout → dedicated worktree → work → push → submit   │
 └───────────────┬──────────────────────────────────────────────────────────────┘
                 │ pushed branches, PRs, verdicts, self-reported usage
                 ▼
        GitHub: PR at submit · commit status + review trail · CI checks · merge
```

The control plane is the durable brain: pipeline state, the work-order
queue, gates, spec versions, review aggregation, the requirement
corpus, the review UI, and the append-only audit/event store. It holds no
plaintext operator credentials and executes no implementation. The
execution side is whatever the operator connects: their own agent
sessions, or their own worker daemon driving their own harness CLIs on
their own hardware.

### 3.1 Control plane components

**Orchestrator / command plane.** A state machine per task, enforced as
one: every lifecycle transition flows through the serialized command
plane (§3.4), which consults the normative transition tables (§3.3),
persists the projection update and its event in one transaction, and
performs post-effects (queue insertion, notifications). Event-sourced:
`events` is append-only and is the source of truth; task and work-order
rows are projections for querying (§16).

**Queue.** River — Postgres-backed jobs, transactionally consistent with
event commits — is the only production dispatch queue (§21.38 change 5).
There is **one work queue** per workspace: any authenticated agent may
claim any order; workers claim what their registered harnesses can serve,
except held orders (§6.3).

**In-process AI.** Triage, task-title generation, and the in-process
review fallback run as direct vendor-API calls inside `conveyord` on the
deployment-owned `CONVEYOR_API_KEY` — cheap, bounded, always-on stages
that intake must never depend on worker availability for. In-process
jobs record provider-reported token counts and no USD cost (§14).

**MCP work-order server.** The agent-facing surface (§17.4): idempotent
task intake plus the leased, stage-typed work-order lifecycle for spec,
implementation, and review.

**Review UI.** The human gate and the factory's primary surface (§13):
stage-grouped activity feed, per-task event timeline, verdict-first gate
cards, the needs-operator tray, the Requirements view, and workspace
administration (setups, harness registry, worker health).

**Audit and transcript store.** Append-only record of every job, claim,
verdict, gate decision, configuration change, and recovery — with
transcript redaction applied to everything the control plane stores
(§10). In-process transcripts are captured first-hand; MCP-side usage
and transcripts are accepted as self-reported and labeled so.

### 3.2 Execution model: delegation over MCP

Spec, implementation, and review execute as **stage-typed work orders**
claimed over MCP (§17.4). Triage is the fixed in-process front door.

- An **implement** order delivers the approved spec, assigned branch and
  base, bounce history, prior feedback, and artifact references. The
  claiming agent resolves a dedicated worktree (`conveyor checkout`,
  §8), adopts the assigned branch safely (§8.2), implements, pushes, and
  calls `submit_for_review`.
- A **spec** order delivers the spec role prompt, task context, triage
  brief, artifacts, and repository/base — but **no branch**: a spec run
  makes no edits. The agent investigates the repository before writing
  (grounded specs, §21.33) and completes with `submit_spec`.
- A **review** order delivers the PR/diff reference, the approved spec,
  bounce history, and the review role prompt, answered via
  `submit_review_verdict`.

**Self-review is forbidden at the protocol boundary**: a review order for
task T cannot be claimed by the session that claimed T's implement order,
and panel seats must be distinct sessions from one another. Spec→implement
continuity is deliberately legitimate — the session that authored a spec
may claim its implement order; provenance stays legible from recorded
identities (§21.33 change 4). Deeper independence — different model
family, different human — is the operator's responsibility; the
platform's obligation is **independence labels**: every verdict records
`reviewer_session: distinct` (guard-enforced), the self-reported reviewer
agent and model, `same_model_as_implementer`, and `model_enforcement:
worker-pinned | self-reported`, rendered honestly on the review card.

**The pushed branch is the trust boundary.** Every gate judges the
artifact, not the environment: spec approval and merge approval are
factory-side; review is an independent-session judgment against the
pushed branch; mechanical verification delegates to the repository's own
CI. What is given up is recorded plainly: no confinement of the
implementing or reviewing process and no observed transcripts of either —
jobs run this way are recorded `harness: external-mcp` (or the worker's
CLI name), `confinement: none`, `auth: byoa`, with usage marked
self-reported.

**Clocks.** Queue residence and execution are separate clocks (§21.9): a
new order records queue entry and queue deadline; the per-stage execution
timeout starts at first successful claim and is fixed to that attempt —
release, cancellation, or lease expiry ends the attempt and clears its
clocks, and a later claim starts a fresh window (§21.21). The claim
lease defaults to **5 minutes**, renewed via `renew_work_order` inside
its safety margin; expiry is safe by design — the order simply returns
to `queued` (W5) for a fresh claim (§21.41). A never-claimed
order past `work_order_queue_timeout` goes `stale`; an elapsed execution
deadline goes `timed_out`; both are non-claimable and recoverable by
audited operator action (§3.3, §13.2).

### 3.3 Canonical lifecycle state machines

Every lifecycle state space carries a **normative transition table**: a
lifecycle is `(state, command) → state'`, where a command is a named
cause. A transition absent from the table is illegal by construction. The
tables govern *reachability*; the sections describing each command's
semantics govern meaning, actors, and side effects; guards (hold at claim
time, self-review, gate toggles, reassignment rules) are preconditions on
*issuing* a command and live with the command's handler. `JobState` and
the GitHub/review publication state spaces adopt the same formalism with
their smaller tables recorded in code under the same module. (Origin:
§21.37; restated here as the current authority per §21.40.)

**Task lifecycle.** States: `claiming`, `queued`, `running`,
`awaiting_human`, `approved`, `merged`, `closed`, `parked`. Terminal:
`merged`, `closed`; `parked` is quiescent but recoverable.

| # | From | Command | To | Semantics |
|---|------|---------|----|-----------|
| T1 | `claiming` | `intake.finalize` | `queued` | intake finalization (§9) |
| T2 | `queued` | `dispatch.start` | `running` | in-process stage begins |
| T3 | `queued` | `order.claim` | `running` | agent/worker claims the stage's work order (§17.4; hold guard §6.3) |
| T4 | `running` | `stage.advance` | `queued` | stage output accepted, next stage set (§4) |
| T5 | `running` | `stage.bounce` | `queued` | redirect/bounce below cap, same stage re-queued (§4) |
| T6 | `running` | `stage.bounce_limit` | `awaiting_human` | bounce cap reached — check-in (§4) |
| T7 | `running` | `job.fail` | `awaiting_human` | in-process job failure, recovery stage recorded |
| T8 | `running` | `triage.route_human` | `awaiting_human` | triage routes to human (§4) |
| T9 | `running` | `triage.park` | `parked` | triage parks the task |
| T10 | `running` | `gate.spec` | `awaiting_human` | spec gate on; spec version awaits approval (§13.1) |
| T11 | `running` | `gate.merge` | `awaiting_human` | review verdict lands, merge gate on (§13.1) |
| T12 | `queued` | `dispatch.fail_retry` | `queued` | queue attempt failed, retry remains; recovery stage recorded |
| T13 | `queued` | `dispatch.fail_final` | `parked` | final queue attempt failed |
| T14 | `awaiting_human` | `intervention.reject` | `closed` | human rejects (§13.2) |
| T15 | `awaiting_human` | `intervention.approve_spec` | `queued` | spec approved → implement |
| T16 | `awaiting_human` | `intervention.approve_review` | `approved` | review approved; merge readiness begins (§11.2) |
| T17 | `awaiting_human` | `intervention.redirect` | `queued` | human redirects to recovery stage (§13.2) |
| T18 | `approved` | `merge.confirm` | `merged` | merge confirmed on the forge (§11.2) |
| T19 | `approved` | `refresh.review` | `queued` | base head moved; refresh review dispatched (§11.2) |
| T20 | `approved` | `conflict.dispatch` | `queued` | merge conflict; fix order dispatched (§11.2) |
| T21 | `parked` | `task.recover` | `queued` | operator recovery re-queues |
| T22 | any non-terminal | `task.cancel` | `closed` | task cancellation (§13.2) |

Dispatch retries (T12/T13) are bounded: a failed queue attempt retries
up to five times with exponential backoff from 10s (doubling, capped at
5m); exhaustion is `dispatch.fail_final` and parks the task, visible in
the needs-operator tray (§13.2, §21.41).

**Work-order lifecycle.** States: `queued`, `claimed`, `submitted`,
`completed`, `cancelled`, `stale`, `timed_out`. Terminal: `completed`,
`cancelled`; `stale` and `timed_out` are recoverable.

| # | From | Command | To | Semantics |
|---|------|---------|----|-----------|
| W1 | — | `order.create` | `queued` | dispatch creates the order (§17.4) |
| W2 | `queued` | `order.claim` | `claimed` | claim; hold, self-review, serviceability guards |
| W3 | `claimed` | `claim.renew` | `claimed` | lease extension (never extends the execution deadline) |
| W4 | `claimed` | `claim.release` | `queued` | worker or agent releases |
| W5 | `claimed` | `claim.expire` | `queued` | lease expiry returns the order to queue |
| W6 | `claimed` | `submit_for_review` | `submitted` | implementation delivered; when configured, eligible task-owned verification evidence is required (§12, §21.44) |
| W7 | `claimed` | `submit_spec` | `completed` | spec order completes |
| W8 | `claimed` | `submit_review_verdict` | `completed` | review order completes |
| W9 | `submitted` | `review.terminal` | `completed` | review round reaches a terminal verdict |
| W10 | `submitted` | `review.revise` | `claimed` | in-session revision loop via `await_review` |
| W11 | `queued`, `claimed` | `order.timeout` | `timed_out` | execution clock elapses |
| W12 | `queued`, `claimed`, `submitted` | `order.stale` | `stale` | superseded — head moved or task advanced |
| W13 | `timed_out`, `stale` | `order.recover` | `queued` | audited recovery (§13.2) |
| W14 | `stale` | `order.redispatch` | `queued` | redispatch of a never-claimed, queue-timed-out order; rejects active claims and execution-timed-out orders (§21.9, §21.41) |
| W15 | any non-terminal | `order.cancel` | `cancelled` | task cancellation cascades |

**Enforcement.** A `core`-level machine module holds each table as data
(a plain Go map — no FSM library) and exposes one guarded
`Transition(from, command) (to, error)` per space; an illegal pair
returns a typed `ErrInvalidTransition` carrying the allowed alternatives,
translated at handlers into the existing wire vocabulary
(`ErrWorkOrderStale` and friends) so MCP and REST surfaces are unchanged.
SQL `AND state=...` predicates and transactional re-checks remain as
defense in depth — they restate the table's edge for the concurrent-writer
case, never extend it. **Transitions and events are one act**: every
legal transition pairs its projection UPDATE with `insertEvent` in the
same transaction, and each command maps to exactly one event kind, so the
event log records the machine's exact edge sequence and fold-over-events
readers are sound. Before enforcement switched on, a migration-time
**event-corpus audit** folds every historical task and order and reports
any observed edge absent from the tables; discrepancies are resolved by
table amendment or annotated as historical corruption, never rewritten.
`tasks.state` and work-order state carry CHECK constraints generated from
the machine module's state sets so schema and code cannot drift. The
machine module is annotated `(spec §21.37)` per row group; a generated
state diagram is published into the Requirements view — the diagram
illustrates, the table governs.

### 3.4 The serialized task command plane

The concurrency substrate is shared Postgres — advisory locks, row locks,
transactions, leases — which is correct for a factory whose actors are
external, crash-prone, operator-owned processes. At the system boundary
the design is already actor-shaped: work orders are messages, workers are
actors, claim/renew/release is the mailbox protocol, reconciliation is
supervision. What the substrate needs is *ergonomics*: guarantees as
structures, not conventions. The remedy is the **virtual-actor pattern**
on the existing substrate — the actor for a task is not a goroutine but
an identity (the advisory-lock key) activated on demand, processing
exactly one command at a time, persisting every step. (Origin: §21.38;
restated here as the current authority per §21.40.)

1. **One entry point for all task mutation.** `internal/taskops` exposes
   `Perform(ctx, taskID, cmd) (Outcome, error)`, where `Command` is the
   closed vocabulary of the §3.3 tables, each with a typed payload.
   Internally: acquire the per-task serialization → load the snapshot →
   check command preconditions (gates, hold, identity) → consult the
   machine for legality → persist projection and event in one
   transaction → run post-effects (queue insertion keyed off the
   machine's outcome). Work orders receive the same consolidation under
   their existing claim-scoped serialization.
2. **Lock possession is a type.** `taskops` mints an
   unexported-constructor `TaskLease` token; every store mutator that
   writes lifecycle state requires one. Code outside the plane cannot
   construct the token, so a state write outside the serialized section
   does not compile. Read paths take no token and acquire no lock.
3. **Transaction-scoped locks by default.** `Perform` serializes with
   `pg_advisory_xact_lock` inside the write transaction. A session-scoped
   variant (`WithTaskSideEffectLock`) survives only for spans that must
   cover an external call across transactions — forge merge confirmation
   — and commands declare whether they carry such a span; callers never
   choose the lock scope. Lock keys are workspace-scoped.
4. **Reads are pure; the clock is a sender.** Expiry transitions never
   run inside `GetWorkOrder`/`ListWorkOrders`. A periodic River job — the
   **order clock** — scans elapsed deadlines and issues `order.timeout`,
   `order.stale`, and `claim.expire` commands through the plane.
   Submission-time enforcement still compares deadlines directly, so a
   stale order is rejected even if the clock has not ticked.
5. **One durable mailbox.** River is the only production dispatch queue;
   the in-memory channel survives solely as the memory-store test double
   behind the same interface.

Explicitly rejected: goroutine-per-task runtimes, in-memory mailboxes as
a durability layer, supervision trees, or any second protocol beside the
§17.4 MCP lifecycle. Postgres is the supervisor: leases, clocks, and
reconciliation are the restart semantics. Migration is staged per §21.38
change 7 and is in flight as factory-executed tasks at v2.0.

---

## 4. The factory pipeline

The pipeline is triage → (spec) → implement → review → human gates →
merge, with each stage's execution contract as follows. Any stage can
bounce back a bounded number of times before a human check-in, and every
bounce is recorded with a reason code.

1. **Triage — in-process, always-on, route-and-frame.** The fixed front
   door; intake never depends on worker availability. Output contract:
   `class` (bug/feature/chore); `route` ∈ {implement, spec, human,
   parked}, with `human` honored for every task (transition to awaiting
   input); a proposed requirement link (§4.2); and a structured
   **brief** — the questions the spec must answer, suspected affected
   areas, and risks or ambiguities in the task body — recorded on
   `triage.completed` and delivered advisory into the task's spec order
   (or implement order on a direct route). Triage runs on a strong model
   tier: a misroute wastes an entire downstream run.
2. **Spec — an MCP work order; grounded, not blind.** The specifying
   agent reads the repository before writing the contract (§21.33).
   There is deliberately **no in-process fallback**: a blind fallback
   would reintroduce the failure mode the delegation removed. When no
   agent can serve a spec order it queues openly with the reason
   visible; `work_order_queue_timeout` is the stall backstop.
   `submit_spec` accepts `markdown`, `acceptance`, and `decomposition`;
   the §4.1 validators are unchanged, and a validation failure returns
   as the tool result so the agent corrects in the same warm session.
   An accepted submission creates the next spec version: human gate
   when the spec gate is on, auto-approval when off. A gate redirect
   enqueues a regeneration order carrying the declined revision and
   every gate comment. Spec provenance (self-reported agent and model)
   is recorded on the version.
3. **Implement — an MCP work order.** The claiming agent resolves its
   dedicated worktree, adopts the assigned branch safely (§8), works,
   pushes with upstream tracking, and calls `submit_for_review`.
   Conveyor then opens or reuses the PR (§11.1) and dispatches review.
4. **Review — an adversarial panel of independent seats.** The task's
   frozen setup defines the panel: one review work order per seat, each
   seat a pinned model with optional harness override and optional
   reasoning effort (`low`/`medium`/`high`). The self-review guard
   applies to every seat; seats are distinct sessions from one another.
   Aggregation is **unanimous-approve and round-local**: `await_review`
   returns when all verdicts arrive; any `changes_requested` bounces the
   task with all reviewers' feedback merged into one structured round —
   one bounce against the cap regardless of panel size. With a reviewer
   available promptly, the loop lives in the implementer's warm session
   (claim → block on `await_review` → revise → resubmit); with none, it
   degrades gracefully to async delivery on the next claim. A workspace
   with review `execution: in_process` gets a synchronous single-seat
   verdict as the tool result instead.
5. **Human gates.** Per the task's frozen gate toggles (§13.1).
6. **Merge.** Merge readiness, approved-head binding, conflict-fix
   dispatch, refresh review, and auto-merge per §11.2. Merging triggers
   the repository's existing CI/CD unchanged; Conveyor never deploys.
7. **Monitor.** A workspace-scoped GitHub observer converts failed
   post-merge checks and out-of-pipeline direct pushes, external PR
   merges, and reverts into ordinary idempotent task intake. It records
   occurrence identity, provenance, deduplication, health/backoff, and
   drift lineage; it owns no claim, implementation, review, merge, or
   deployment capability (§4.2, §21.45).

**The bounce cap is a check-in, not a kill switch.** `max_bounces`
(default 10) bounds how long an implementer↔reviewer loop runs
*unsupervised*: reaching it parks the task at the human gate as a
check-in on a still-live loop. The comparison window is bounces **since
the last human intervention** — a human who reviews and sends the task
back grants a fresh full window. Every bounce is still recorded as
training signal. The surface names it "review rounds before human
check-in" (wire name `max_bounces` unchanged).

### 4.1 Spec format

A spec is a markdown document — prose for intent, context, approach, and
rationale — containing a small number of schema-validated fenced blocks
that machines own. Prose serves the human approver and the implementing
agent; the fenced blocks serve the orchestrator and verifier, which need
mechanically enumerable content with stable IDs.

````markdown
# Fix default-state launch latency

## Intent
Users report 3–4s cold launches when no session is restored. We believe …
(free-form prose: context, constraints, approach, rationale)

## Non-goals
Session-restore latency; startup telemetry redesign.

```conveyor:acceptance
- id: AC-1
  criterion: Cold launch to interactive < 800ms on the reference container
  verify: test            # test | playwright | computer-use | human
  ref: bench/launch_test.rs
- id: AC-2
  criterion: Default state renders visually identical to current release
  verify: computer-use
```

```conveyor:decomposition
- id: SUB-1
  repo: api
  summary: Add lazy session hydration behind existing interface
  depends_on: []
- id: SUB-2
  repo: web
  summary: Adopt hydration signal
  depends_on: [SUB-1]
```
````

Rules: (1) blocks are validated at submission time; a malformed block
returns to the submitting agent for correction. (2) Each acceptance
criterion carries a **verification method**; `test` criteria are
exercised by the repository's CI, `human`-tagged criteria surface on the
review card as explicit checkboxes, and `playwright`/`computer-use`
criteria await the Phase 9 verification agent (§12). (3) Verdicts attach
to `AC-n` IDs. (4) `Non-goals` is what review enforces when emitting
`scope-creep` codes. (5) The *approved* spec version is the contract: a
redirect that changes criteria produces a new version requiring
re-approval. (6) The block schema is deliberately minimal — `acceptance`
and `decomposition` only. (7) Non-`conveyor:*` fences are ordinary
prose; **Mermaid is the sanctioned notation** for optional architecture
and flow diagrams, and diagrams are **non-normative** — approval,
review, and verification enforce prose, criteria, and Non-goals, never a
picture. The dashboard renders Mermaid best-effort with fenced-text
fallback; GitHub issue projection inherits diagrams natively. Role-prompt
guidance: roughly fifteen nodes or fewer, and only where a diagram
clarifies more than prose.

**Blueprint materialization (Phase 6.1, §21.46).** A spec whose
`decomposition` is non-empty is a **blueprint**: on approval, the
control plane materializes each `SUB-n` into a child task in the same
transaction. A child's body carries the SUB summary plus the blueprint
reference; it inherits the parent's frozen gates and setup contract and
targets the SUB's declared repo; `depends_on` entries become task
dependency edges (§6.3, §8.3); and each child records its originating
`(spec version, SUB-n)` as lineage (§4.2). Children enter the pipeline
at `implement` — their scope was defined by the approved blueprint, so
triage and a second spec stage are skipped; a child that proves
mis-scoped is corrected through redirect or a revised blueprint
version, never ad-hoc re-triage. Approval validates the decomposition
as a DAG — a cycle fails approval closed — and materialization is
idempotent per spec version: re-approval of a revised blueprint
materializes only SUB IDs without a live child. The parent task takes
no implement order of its own; it is the batch anchor — it holds the
blueprint, rolls up child progress, is presented through the Blueprints
surface rather than as ordinary work (§13.3, §21.49), and moves to
`closed` by an audited
control-plane transition when its last child reaches a terminal state.
A decomposition-free spec keeps today's single-task flow unchanged.

### 4.2 Coherence: requirements, the corpus, and drift governance

1. **Requirements are living intent documents (Phase 6.2, §21.46).** A
   requirement states what the system is supposed to do in an area and
   why, in the operator's own language. It is created from intent — the
   operator states what they want, the planning agent (§9) generates the
   structured document — and maintained the same way: revisions happen by
   chatting, never by filing. The format is deliberately §4.1-shaped:
   prose for intent and rationale, plus one fenced
   `conveyor:requirements` block of enumerable statements with stable
   IDs (`REQ-n`) that citations (item 4), acceptance criteria, and
   verdicts can reference. Requirements are **versioned and confirmed,
   not gated**: every revision — chat edit or drift amendment — creates
   a version the operator confirms, audited like any event; the
   approval gate stays on blueprints (§13.1). The corpus is **flat**:
   `relates_to` links between requirements exist, but there is no
   hierarchy to curate — the curated feature tree is retired (§21.46)
   because filed taxonomy rots while conversation plus pipeline
   write-back does not. "What is this system supposed to do here" keeps
   one queryable answer: the confirmed requirement corpus plus its
   lineage.

   **Blueprints link to requirements optionally and loosely.** A
   blueprint drafted in a requirement's planning context auto-proposes a
   `serves` link; any blueprint may be linked retroactively; a blueprint
   with no requirement is legal forever (chores, refactors, exploration).
   One requirement accumulates many blueprints over its life — that
   accumulation is its delivery history. Links are machinery-suggested
   (planning agent at drafting; triage proposes a requirement link for
   stray tasks) and human-confirmed. **Staleness is surfaced, not
   discovered**: each requirement shows its last-confirmed version
   against subsequent merges and drift touching its linked code — the
   alignment layer rendered per requirement.
2. **Reverse sync is live.** The monitor watches configured repositories
   for changes landing outside recorded Conveyor task/PR lineage (direct
   pushes, external PRs, reverts) and files reconciliation tasks that
   propose a corpus amendment or flag genuine conflicts for human
   decision. Reconciliation's `requirements_amended` outcome targets a
   **requirement document**: it proposes a new requirement version,
   which the operator confirms like any other revision (item 1). It
   never silently edits confirmed requirements or approved specs.
3. **Drift as a metric:** count and age of
   unreconciled out-of-pipeline changes per workspace; the healthy value
   is zero. Only an audited reconciliation outcome reduces the count.
4. **Lineage links are first-class (Phase 6.3, §21.46).** One
   polymorphic `links` table (§16) records typed edges deposited by
   pipeline machinery at stage transitions — planning session →
   requirement or blueprint, requirement → blueprint (`serves`),
   requirement → requirement (`relates_to`), blueprint version →
   materialized child (`SUB-n`), task →
   dependency, task → commit range and PR, evidence → verdict, spec or
   requirement version → superseded predecessor. Links are projections of `events`,
   rebuildable, and carry provenance (`created_by_event`); no edge is
   volunteered as free-standing data by an agent or human, because
   volunteered edges rot. Two layers with opposite properties:
   the **lineage graph** (intent → requirement → blueprint → work order
   → code → evidence) is append-only and immutable — the audit chain;
   the **alignment layer** (current code ↔ current spec) is mutable and
   decays, so it is maintained only where the pipeline already stops —
   review verdicts, planning-session context reads, and monitor drift
   records — with in-repo `REQ-n` citations (the `(spec §N)`
   convention, generalized to requirement-statement IDs and checked at
   review) as the one anchor class that survives refactoring: a `REQ-n`
   outlives every blueprint that serves it. Derived
   file/symbol maps are cache, recomputed from commit history, never
   asserted. Context assembly for work orders and planning sessions is
   a link traversal under a depth/size budget; vector retrieval (§15.1)
   is a secondary index over the same nodes, never the primary
   structure.

---

## 5. Harness layer

There is no adapter interface. A harness is **data**: a declarative
registry entry in workspace configuration describing how to invoke a
coding-agent CLI headless, validated at write time under the standard
configuration mechanics (§2.1). The registry serves worker dispatch;
interactive agents need none of it.

### 5.1 Registry schema and expansion contract

An entry is `{name, command, model_args, effort_args?, probe_command,
probe_timeout, stall_timeout?, mcp_transport, mcp_attachment?}`. All
command fields are
argv arrays, **never shell-evaluated**; placeholders substitute as whole
argv elements. Expansion is field-local and deterministic (§21.14):

- `command` — the base invocation; contains exactly one `{prompt}` and
  (for file/override transports) exactly one `{mcp_config}`; `{model}`
  is invalid here.
- `model_args` — appended in declared order; may contain only `{model}`;
  omitted when the route resolves no model.
- `effort_args` — a map from `low`/`medium`/`high` to literal,
  placeholder-free argv, appended only when the seat or implementation
  route requests that exact value. This is the vendor-neutral effort
  boundary (§21.19): the workspace contract names semantic efforts; the
  registry maps them to each vendor's flags.
- `probe_command` — standalone argv, no placeholders; used with
  `probe_timeout` for health probes (binary present, authenticated,
  trivial invocation succeeds).
- `stall_timeout` — optional per-harness child-inactivity bound for
  worker supervision (§6.4); default 10 minutes. The literal `0`
  disables stall detection for harnesses with legitimately silent runs
  — the one sanctioned exception to the non-positive-timeout rule
  (§21.41).

Unknown placeholders, placeholders in the wrong field, missing required
placeholders, unknown effort values, and non-positive timeouts are
validation errors at write time, not runtime surprises.

### 5.2 MCP transports

`mcp_transport` declares how the spawned child discovers the Conveyor
MCP server (§21.20, §21.29):

- **`json_file`** (default) — `{mcp_config}` expands to a mode-0600
  temporary JSON config path, removed after the child exits (Claude
  Code style, e.g. `--mcp-config {mcp_config}`).
- **`toml_override`** — `{mcp_config}` expands to one TOML `key=value`
  string defining the server (Codex CLI style, `--config
  {mcp_config}`). The TOML names `CONVEYOR_API_TOKEN` as the bearer
  environment variable; the token value never enters argv.
- **`environment`** — the harness discovers its registration from its
  own operator-authored configuration, resolving the runtime URL and
  authorization header from the child environment (`${CONVEYOR_ADDR}`,
  `${CONVEYOR_API_TOKEN}`); `{mcp_config}` must not appear. The entry's
  non-secret `mcp_attachment` names the intended registration, and
  readiness fails closed before every model turn unless that exact
  registration positively identifies and handshakes (Grok Build's real
  no-model-turn doctor path, §21.29). Conveyor never creates, rewrites,
  or repairs operator-owned harness configuration, and literal
  credentials in persistent registration are forbidden.

Headless permission grants are **launch data**: a non-interactive
command must pre-authorize exactly the Conveyor MCP lifecycle tools it
needs (e.g. allowing the scoped `mcp__conveyor__*` server, not a blanket
permission bypass) and never depends on a human answering a child
prompt (§21.22).

### 5.3 Snapshots, fingerprints, and queue re-entry

Dispatch snapshots the normalized harness definition, effective model
(including an intentionally omitted harness-default model), effort argv,
timeouts (execution and stall), and transport into the work order;
workers execute the snapshot
without reinterpretation, and hot reload cannot alter an in-flight
launch. Immutability is scoped to the **active attempt** (§21.32):
whenever an order re-enters the queue — release, retry, recovery, or
redispatch — the server re-resolves the pinned snapshot from the current
registry (same-name harness, effort still declared), auditing a material
refresh as `work_order.harness_refreshed`; the pinned harness name,
model, effort, and seat assignments are preserved. Snapshots and
fingerprints include transport and attachment identity and exclude
addresses, tokens, and other launch-time values.

Model policy for implementation and spec routes is explicit or
`harness_default` — symbolic labels are never forwarded as provider
model IDs, and an unresolvable model is an actionable configuration
error, not a silent fallback (§21.18).

---

## 6. Execution: setups, hold, and the worker

### 6.1 Execution setups

A **setup** is a named contextual execution contract — the workspace
staged for one class of task (§21.27). The workspace document carries
`setups` (non-empty) and `default_setup`. Each setup defines:

- **Control plane:** the triage model and timeout.
- **Spec:** work-order harness/model-policy resolution, optional effort,
  timeout.
- **Implementation:** harness (registry reference), model policy,
  optional effort, timeout.
- **Review:** execution (`mcp` with a panel, or `in_process` fallback),
  timeout, and the **panel**: seats of `{model, harness?, effort?}`,
  with route-level fallback model/harness used only by seats that omit
  their own. `refresh_review: delta | full | none` (default `delta`,
  §11.2).

Setups normalize and validate independently; referential integrity runs
both ways (routes and seats cannot name unregistered harnesses; a
referenced registry entry cannot be deleted). Registry entries stay
workspace-level and shared.

### 6.2 Selection, freezing, and audited reassignment

A task selects a setup by name at intake (unset → workspace default;
unknown names fail, never silently fall back) and the resolved contract
is **frozen by value** with the task — later setup edits, renames, or
deletions never change in-flight work, which also makes deleting a setup
always safe. There is no per-task free-form override: a task selects
among operator-defined setups and never names a harness, model, or
effort directly.

Two audited human operations move a frozen contract, both requiring a
reason and idempotency key (§21.34–§21.36):

- **Setup reassignment** (**Change execution setup** / **Apply latest
  setup**): re-freezes the task to a currently defined named setup for
  **future work only** — rejected while any order is claimed or a review
  verdict submission is in flight (a *submitted*, delivered attempt does
  not block; hold never relaxes the exclusion). Unclaimed queued work
  rebuilds from the new contract; completed attempts keep their
  snapshots. Compatible completed review verdicts are retained by the
  positional compatible-result rule; a panel-shape change supersedes the
  round entirely.
- **Recovery re-freeze:** operator recovery of a released, expired,
  retry-suppressed, or timed-out order re-freezes the setup from the
  current same-named definition and re-pins the recovered order from it
  (`task.setup.refrozen`); if no same-named setup exists the prior
  contract is retained — re-freeze never fails a recovery. Automatic
  queue re-entry keeps §5.3 semantics exactly (definition-only refresh,
  pinned model preserved).

The line all three mechanisms hold: frozen under automation, mutable only
under explicit, audited human action.

### 6.3 One queue, per-task hold, advisory serviceability

There is **one queue**: any authenticated agent may claim any order; an
enrolled worker claims any order whose task's frozen setup it can serve
— except held orders. **Hold** is the reservation primitive (§21.31): an
optional per-task boolean, settable at intake and toggleable live
(`task.hold.set` / `task.hold.cleared`), enforced server-side at claim
time in the same layer as the self-review guard. Hold governs worker
claiming only — it does not pause in-flight sessions, operator-attached
agents may still claim explicitly, control-plane work (auto-merge,
conflict-fix enqueue) proceeds regardless, and system-dispatched orders
inherit the hold. There is no execution mode: modes were removed by
§21.31, and existing records keep theirs as history.

**Dependencies gate implementation claiming the same way** (Phase 6.1,
§21.46; scoped by §21.47). A task with unmerged dependencies (§8.3)
cannot have its **implementation work orders** claimed; spec orders
stay claimable — dependencies order code landing, not design. Blocked
is a **derived predicate** — an unmerged dependency exists — never a
stored state: no new lifecycle state, nothing to drift out of sync. It
is enforced **at claim time** in the same server-side layer as hold and
the self-review guard; an already-claimed order is never rejected
mid-flight for blocking (edges added after claim cannot destroy
in-progress work, §21.47). Every surface that renders claimability
names the blocking task IDs — worker-facing MCP listings included.
Dispatch still creates the order, which queues openly with the reason
visible and keeps its original queue entry; while a task is blocked,
its orders' queue-timeout clock is **suspended**, so the stall backstop
cannot stale a legitimately waiting order and FIFO position survives
long dependency chains (§21.47). When a dependency
reaches `merged`, the control plane re-nudges each dependent's dispatch
so a waiting worker sees the order within one poll rather than at the
queue-timeout backstop.

**A dependency that reaches a terminal state other than `merged` makes
the dependent's edge *unsatisfiable*** (§21.47): waiting will never end,
so it is surfaced distinctly from ordinary blocking — an event on the
dependent (`task.dependency_unsatisfiable`) and the needs-operator tray
(§13.2) — and resolved only by an audited operator action: remove the
dependency edge (`task.dependency_removed`; UI/CLI/REST, deliberately
not MCP, like cancel) or cancel the dependent. Nothing removes an edge
automatically, and an unsatisfiable dependent never counts as
delivered.

**Claim order is deterministic** (§21.41). Among the orders a given
claimant is eligible for, `claim_work_order` serves the oldest queue
entry first — workspace-scoped FIFO by the §3.2 queue clock, so service
order is specified and starvation under backlog is testable rather than
accidental. Stage preference sits above age: the worker's reserved
review slot prefers review orders (§6.4). Queue re-entry (release,
recovery, redispatch) records a fresh queue entry, so recovered orders
rejoin behind the existing backlog. There is no task priority field;
introducing one requires an amendment.

Serviceability is **advisory**: the liveness lease, per-harness probes,
and per-setup serviceability are computed and surfaced; intake warns when
no enrolled worker can serve the selected setup, but nothing is rejected
— orders queue openly with the reason visible, and
`work_order_queue_timeout` is the stall backstop.

### 6.4 The worker

`conveyor worker run` — a subcommand of the existing CLI — is a **thin
supervisor over the unchanged §17.4 lifecycle**: no second protocol, no
adapter interface, no credential pool, no containers. It enrolls against
one workspace, heartbeats, long-polls `list_work_orders` /
`claim_work_order`, and on claim spawns the snapshot's harness CLI
headless with the Conveyor MCP configuration attached — the spawned
session performs the standard flow itself (checkout, implement/spec/
review, push, submit).

- **Enrollment** is a token exchange: a short-lived, single-use pairing
  token (issued via UI/CLI) becomes a revocable, workspace-scoped worker
  credential stored server-side only as a hash. Liveness is a
  server-issued lease (default 15s) refreshed by heartbeat; heartbeats
  carry per-harness probe results, rendered on the Workspace page.
- **Capacity is stage-aware:** configurable implement concurrency plus
  at least one reserved, prioritized review slot — implementers blocking
  in `await_review` must never occupy every slot. Each order executes
  under a fresh identity/token pair so the self-review guard and
  independence labels hold.
- **Supervision:** lease renewal while the child lives (renewal never
  extends the execution deadline), exit-status capture, and claim
  release on failure. Child retry is durable and bounded: 1s/2s/4s
  automatic retries recorded on the order; failure after that leaves the
  queued order **retry-suppressed** — visible in the needs-operator tray
  (§13.2) and recoverable by audited operator action.
- **Stall detection** (§21.41): the worker bounds child inactivity — no
  child output for the snapshot's `stall_timeout` (§5.1; default 10m,
  `0` disables) stops the child, records the distinct outcome
  `stalled`, and releases the claim. A stall consumes the same bounded
  child-retry allowance and lands in the same retry-suppressed path as
  a crash. This is supervision, not budget enforcement (§14): it
  watches silence, never spend. Server-side, the timeline renders the
  order's last agent activity (`report_progress`, lifecycle calls) so a
  quiet-but-alive session stays distinguishable from a dead one.
- **First-activity liveness is separate from the lease** (§21.42): the
  effective worker configuration explicitly carries
  `first_activity_timeout` (default 2m, positive, and shorter than every
  configured MCP stage execution timeout). For spec, implementation, and
  review children, the clock starts only after successful launch; the
  first non-empty stdout or stderr write through the redacted output path
  permanently disarms it. If a still-authoritative, still-running child
  remains silent through the deadline, the worker terminates and reaps it,
  then releases the exact claim once as `child_failure` with reason
  `harness produced no output before first_activity_timeout`, entering
  the ordinary bounded retry, suppression, and audit path. There is no
  later-silence heuristic in the first-activity watchdog itself; ongoing
  output inactivity is governed separately by `stall_timeout` (§21.41),
  and the fixed execution deadline remains the total-duration backstop.
- **Reconnection is safe, authority is not assumed** (§21.26): idle
  transport failures retry with bounded jittered backoff on the saved
  enrollment; active renewal retries only inside the lease's safety
  margin, and a terminal response or unresolved renewal stops the child
  and permanently abandons local authority; the worker then reconciles
  against durable server claim state. Expired sessions stay rejected for
  renewal and submission. Sleep prevention is optional and correctness
  never depends on a platform sleep API.
- Jobs it runs are recorded `harness: <cli>, confinement: none, auth:
  byoa, dispatch: worker`. What is given up is recorded plainly:
  unconfined execution on a real operator machine, mitigated by explicit
  enrollment, hold, dedicated worktrees, gates on by default, and the
  trust labels.

### 6.5 Service packaging *(Phase 5.5, complete — §21.46)*

`conveyor worker install` / `uninstall` / `status` wrap `worker run` as
an OS-managed service — launchd agent (macOS), systemd user unit (Linux)
— with restart-on-failure, start-on-boot, and a documented log location.
Supervision only: no new protocol or behavior; interactive `worker run`
remains fully supported (§21.16).

---

## 7. Workspaces

### 7.1 Multi-workspace control plane

One control plane serves many workspaces (§21.10). A workspace has an
immutable lowercase slug `id` and an immutable display name (unique;
names case-insensitively). Context is **explicit and fail-closed** across
every surface: canonical routes are `/v1/workspaces[/<id>[/config]]`;
scoped routes accept `workspace_id` or `X-Workspace-ID`; CLI commands
take `--workspace`; MCP workspace-scoped tools take `workspace_id`.
Conflicting context is invalid; omitted context resolves only when
exactly one workspace exists — ambiguity never selects one arbitrarily.
Isolation is end to end: stores, intake, idempotency, activity,
requirements, artifacts, work orders, dispatch, review publication, and
reconciliation all constrain by immutable workspace context; River
payloads and per-workspace queues carry the ID. Rename and deletion
remain out of scope, as do RBAC/SSO (§18.1).

Workspace configuration storage, mutation, audit, and hot-reload
mechanics are as §2.1. Repos are `{name, url, github, base}`; per-repo
images, tool policies, and secret references left the document with the
execution pivot.

### 7.2 Multi-repo coordination *(deferred design, Phase 9)*

Multi-repo worktree sets and linked-PR gating are deferred with managed
execution. The design is preserved: expand/contract **decomposition
first** (the spec agent sequences subtasks so no PR is ever unsafe to
merge alone, materialized as stacked tasks), with **linked-PR gating** as
the fallback for changes that genuinely cannot decompose — declared merge
order, no merge until all are green and approved, freshness-checked
back-to-back merges, and automatic fix-forward on partial landing. A
cross-repo merge train remains explicitly deferred until monitored
incident rates justify it.

---

## 8. Worktree and branch management

Branch-per-task is the core Git model, under one boundary rule
established by the pivot: **task branches are assignments, not
pre-created refs, and Conveyor never mutates an agent's or operator's
checkout** (§21.7).

### 8.1 Intake assigns metadata, not a ref

Task creation selects the base and reserves the canonical
`conveyor/task-<id>` branch name in the durable task record. Conveyor
does not create, check out, push, reset, delete, or otherwise mutate a
corresponding local or remote Git ref; reads and UI expose the assigned
name as metadata without implying the ref exists. Branch availability
has three states: **assigned** (name and base only), **local**
(agent-owned, unpushed), **pushed** (available to review coordination
and the human checkout flow).

The control plane keeps a fetch-only bare-clone cache per repo for
branch management, diffing, and checkout support; fetches into it are
serialized per repo. Control-plane-managed task checkouts are removed on
merge, on task close, or after a staleness TTL (default 14 days).
Background maintenance prunes stale Git worktree registrations both in
that cache and in every available configured repository primary
checkout. Repository failures are isolated and best-effort; pruning
never removes a live worktree, branch, or primary checkout (§21.48).

### 8.2 Dedicated task worktrees

A dedicated checkout is mandatory: immediately after claiming and
reading an implementation work order, the agent resolves a clean
checkout dedicated to the assigned branch — by default a registered Git
worktree at the deterministic contained sibling
`../conveyor-worktrees/<repo>-task-<task-id>` — and performs **every**
edit, build, test, commit, and push there. The container name is fixed;
there is no workspace-level worktree-root setting. A shared primary
checkout is never an implementation directory (§21.8, §21.48).

`conveyor checkout <task-id>` is the shared safe resolver for agents and
humans alike, before or after the first push. Before any fetch, ref
inspection, worktree reuse, or creation, it compares the current
checkout's unambiguous normalized `origin` identity with the assigned
workspace repository's configured identity. Standard equivalent GitHub
HTTPS/SSH forms compare equal; missing, unreadable, ambiguous, or
non-matching identity blocks with both assigned and current identity
context. This precondition is identical whether assignment came from
the worker environment or an authenticated task lookup, and `--path`
does not bypass it. The resolver then inspects repository safety, reuses
a clean worktree already owning the branch (including one at the former
sibling location), and otherwise creates the deterministic path —
fetching and verifying `origin/<base>` first, adopting an existing local
branch without reset, tracking an existing remote branch, or creating
the branch from the fresh base.
History is preserved absolutely: no `-B`/`-C`, reset, rebase, automatic
stash, or forced ref updates, ever; divergence between local and remote
task history blocks and is reported, never resolved by rewriting. Dirty
unrelated work, in-progress Git operations, and ambiguous state block.

**Path safety** (§21.41). The deterministic contained name
`<repo>-task-<task-id>` is exactly one path component beneath the fixed
`conveyor-worktrees` directory: repo `name` is
validated at configuration write time to `[A-Za-z0-9._-]` (and not `.`
or `..`), and task IDs are server-generated from the same alphabet by
construction. `conveyor checkout` verifies the canonical container is
directly beneath the primary checkout's parent and the resolved worktree
path remains inside it — any symlink, separator, or traversal outcome
outside that boundary blocks, like every other ambiguous state.
`--path` remains the explicit operator placement override and bypasses
only this implicit placement rule.

The worktree stays authoritative across the review loop: a successor
session (redirect, `changes_requested`) claims first, returns to the
same path, commits, pushes, and resubmits the existing PR. Independent
review uses the pushed PR diff or a separate detached checkout — never
the implementation worktree. `conveyor done <task-id>` removes the
worktree only when the task is terminal and the worktree clean; it
retains the branch and never touches the primary checkout. In addition,
every reconciliation pass schedules equivalent best-effort cleanup for
each merged or closed task until it succeeds or is a no-op. Cleanup
removes only a registered, clean, non-primary task worktree; never
deletes a branch, resets, rebases, stashes, or rewrites history; leaves
dirty or locked worktrees untouched; treats missing registrations as a
successful no-op; and prunes a registration whose directory is already
missing. Failures carry workspace, task, repository, and branch context
in logs and remain retryable without changing terminal task state or
blocking other repository maintenance (§21.48).

### 8.3 Task dependencies and stacking

A task may depend on other tasks. Dependencies are first-class edges
(`task_dependencies`, §16) — materialized from a blueprint's
`depends_on` (§4.1) or declared at intake — validated acyclic at write
time, **workspace-scoped, and free to span repositories** (§21.47;
§4.1's canonical example is cross-repo). Removal is an audited
operator action only (§6.3). **v1 dependencies are ordering gates, not
stacked branches**
(§21.46): every task branches from its recorded base; a dependent
simply cannot have its implementation claimed until its dependencies
merge (§6.3), so its
branch is cut from a base that already contains them. Full stacking —
cutting the branch from the dependency's branch with a rebase task on
parent landing — is explicitly deferred, not designed here; the model
does not block it, and reintroducing it requires an amendment.

---

## 9. Intake and triggers

Tasks enter through four surfaces sharing one validation and one
pipeline: the dashboard, `conveyor task new`, REST, and MCP
`create_task`. Common contract:

- **Body, not title.** `body` — GitHub-flavored Markdown, stored as the
  original string and delivered unchanged downstream (§21.39) — is
  required on every surface and is the source of task intent. **Title is
  not an intake field anywhere**: before persistence, `conveyord`
  generates the title in-process (configured triage model; one non-empty
  line, ≤200 characters) and fails creation closed on provider failure —
  no untitled tasks, no invented fallbacks (§21.24–§21.25).
- **Idempotent MCP intake.** `create_task` requires a workspace-scoped
  `idempotency_key`; exact retries return the original task (and its
  generated title, without re-invoking generation); key reuse with
  different input fails closed. The call returns when creation and
  triage enqueue commit — MCP intake is not a second triage
  implementation (§21.5).
- **Per-task options at intake:** setup selection (§6.2), hold (§6.3),
  gate overrides (§13.1), base branch, attachments/artifacts.
- **Provenance** is recorded per trigger for audit and the
  automation-rate metric; MCP intake records the authenticated agent
  actor and grants no capability beyond HTTP intake.

**Planning sessions (Phase 6.2, §21.46)** are the fifth intake surface:
an in-product chat with the planning agent producing **two artifact
types** — requirement documents and blueprints. The operator states
intent and the agent generates the structured requirement (§4.2 item
1); revisions to either artifact happen in the same conversation. The
agent runs in-process on the factory credential
— planning is Conveyor-owned, like triage; implementation stays
delegated (§3.2, §21.4 boundary unchanged) — with read tools over the
requirement corpus, approved specs, artifacts, and lineage links, and
draft/revise/finalize tools. **Finalizing a requirement** creates or
revises the versioned document for operator confirmation (§4.2 item 1
— no gate). **Finalizing a blueprint** creates the
parent task and its spec version through the same §4.1 validation and
§13.1 spec gate as every other spec — and, when drafted in a
requirement's context, proposes the `serves` link; a planning session
grants no approval authority. Sessions are durable rows; on finalize
the transcript is archived as an artifact linked to the produced
requirement or parent task, so the rationale — alternatives rejected,
constraints surfaced — is part of its lineage (§4.2). The streaming transport is the AI SDK
UI-message protocol over SSE served by `conveyord` (§17.0/§17.3 — no
new tier). MCP remains the headless twin: an operator agent reaches the
identical outcome through `create_task` + `submit_spec`; both doors
converge on one blueprint contract.

**Artifacts** are workspace-scoped context files (documents, images,
audio) uploaded through the UI, attachable to requirements and tasks,
injected into pipeline-agent context, listed in work orders, and
fetchable over MCP (`read_artifact`). Artifacts are context, never
secrets; storage is size-bounded and content-addressed.

**GitHub triggers:** ready-labeled issues are polled into tasks
(provenance `github:<repo>#<n>`), and PR review comments on Conveyor PRs
convert to redirect feedback. Monitor-agent triggers are live (§4.2,
§21.45); the chat trigger is the Phase 6.2 planning surface; cron
triggers remain future scope.

---

## 10. Credentials, secrets, and redaction

Conveyor's credential story after the pivot is deliberately small:

1. **The deployment key.** In-process AI (triage, titles, in-process
   review) runs on the deployment-owned `CONVEYOR_API_KEY`, configured
   at boot, never stored in workspace documents.
2. **Operator agents bring their own auth.** Implementation, spec, and
   review run under the operator's own harness logins. Conveyor never
   pools, stores, or routes anyone's model credentials — every
   credential-class concern of the retired execution plane is moot, and
   the no-circumvention posture stands: Conveyor never disguises
   automated traffic or evades vendor limits.
3. **Worker and session tokens.** The worker's workspace-scoped
   enrollment credential is stored server-side only as a hash and is
   operator-revocable; per-work-order freshness and identity are the
   claim's session ID and client token. None of these values may appear
   in argv, workspace documents, snapshots, fingerprints, events, logs,
   diagnostics, transcripts, or persistent harness configuration — the
   worker supplies `CONVEYOR_API_TOKEN` only in the child-only
   environment, and `json_file` transport uses a mode-0600 temporary
   file with post-exit cleanup (§5.2).
4. **Transcript redaction is non-negotiable** and applies to everything
   the control plane stores: pattern/entropy detection (JWTs, `sk-…`,
   `ghp_…`, PEM blocks, high-entropy assignments) plus exact-match
   scrubbing of every control-plane-known credential. Redaction events
   are logged by count and pattern class, never value.
5. **GitHub access** uses least privilege: a fine-grained token with
   commit-status and PR-comment permissions is a valid default for
   self-hosted deployments; no GitHub App is required (§11.1).

---

## 11. GitHub coordination

The factory coordinates GitHub; the agent only commits and pushes.

### 11.1 PR lifecycle and portable review publication

The PR is opened (or reused) by the control plane at
`submit_for_review`, and not before — no draft PRs, no push-event
machinery (§21.15). Review results publish portably by default: one
aggregate **`Conveyor / Code review` commit status** for the reviewed
SHA (`pending` until the round aggregate exists, then `success` /
`failure`) plus a deterministic factory PR comment. Internal state is
authoritative: a publication failure never rolls back a verdict, and
retries rebuild both projections from the event stream. Issue creation
or source-issue update on spec approval and the complete aggregate
verdict/resolution trail are delivered Phase 5.3 behavior (§21.43).
A required review publication is not `published` until the deterministic
comment upsert returns a nonzero comment ID.

Forge-call failures record a stable error category on the failure
event — `forge_request` (transport), `forge_status` (non-success
response), `forge_response` (malformed payload), `forge_rate_limited`,
`forge_permission` — so needs-operator evidence and retry decisions are
uniform rather than stringly-typed; Phase 5.3's issue creation and
verdict mirroring adopt the same taxonomy (§21.41).

### 11.2 Merge readiness, conflict-fix dispatch, refresh review

- **Readiness is read before merge is offered.** At the merge gate the
  control plane resolves PR head and mergeability first: `MERGEABLE`
  renders the ready card; `UNKNOWN` renders pending and re-reads with
  bounded backoff (never an error); `CONFLICTING` renders a blocked
  card. The read is advisory — the authoritative pre-merge check inside
  the merge operation still fails closed on races.
- **Approval binds to the reviewed head.** Approving persists the PR
  head SHA; merge requires the current head to equal it. Any mismatch
  fails closed, marks the approval stale, and routes through the fix /
  refresh paths rather than merging content no round approved.
- **Conflict fixes are dispatched, not performed by operators.** The
  blocked card's **Fix merge conflict** action (or the control plane
  itself, when the merge gate is off) records a system redirect with
  reason `merge-conflict` and creates one implement order instructing
  the agent to merge the base into the task branch in the task worktree
  — history rewrites and force-pushes remain forbidden — resolve, pass
  validation, push, and resubmit. Idempotent while a fix order is
  active.
- **Refresh review scope is a setup decision** (`refresh_review`,
  frozen at intake): `delta` reviews changes since the approved
  baseline SHA; `full` reviews the entire diff; `none` lets a clean
  update with **no manually resolved hunks** re-arm the gate directly —
  but authored conflict resolutions always get at least a `delta`
  round. Refresh rounds are ordinary next rounds: frozen-setup panel,
  round-local aggregation.
- **Auto-merge keys on the gate alone:** with the merge gate off, the
  control plane merges an approved review with green checks
  automatically — merges are control-plane work, and hold governs
  worker claiming only.

---

## 12. Verification

The explicit goal is unchanged from v1.0: compress human review time by
letting the reviewer **confirm evidence rather than reproduce
behavior**. Current mechanics, in order of arrival:

1. **Repository CI is the mechanical verifier** (now). PR checks gate
   merge; automatic merge requires green checks.
2. **Evidence-gated submission** *(Phase 5.4, delivered)*: a workspace
   toggle refuses `submit_for_review` until at least one
   verification-evidence artifact (screenshots or a short recording of
   the exercised change) is attached; evidence lists in review work
   orders, renders on the review card, and mirrors to the PR. Eligibility is
   explicit: role `verification_evidence`; PNG, JPEG, or WebP screenshots up
   to 10 MiB; MP4 or WebM recordings up to 25 MiB; direct ownership by the
   submitting task in the active workspace. Filenames do not establish type.
3. **The verification agent** *(Phase 9, demand-triggered)*: scripted
   Playwright flows and computer-use judgment against the spec's
   acceptance criteria, with evidence attached per `AC-n`. With
   K8sRunner, this is explicitly the reintroduction of managed
   execution, built only on demonstrated demand.

---

## 13. Human gates and the review surface

### 13.1 Two gates, not a ladder

Human oversight is two independent toggles — workspace defaults with
per-task override, resolved and **frozen at intake** (hold excepted, by
design):

- **Spec gate on:** the spec stage is forced for every task and each
  spec version waits for human approval. **Off:** triage may route
  straight to implement, and any generated spec auto-approves.
- **Merge gate on:** an approved review waits for human merge approval.
  **Off:** the control plane auto-merges approved reviews with green
  checks (§11.2).

The shipped default is both gates on. The historical L0–L3 escalation
ladder and the Auto/Manual mode axis are both retired (§21.12, §21.31);
existing records keep recorded levels/modes as history. Phase 8
graduation, when it arrives, operates on gate toggles and hold usage.

### 13.2 The review inbox

One queue of pending human decisions plus a **needs-operator tray** for
stalled work. Gate cards are **verdict-first** (§21.11): a headline and
tone matched to the actual state ("Ready to merge"; amber only for
genuinely stuck states), one context-matched primary action, secondary
decisions demoted. Each card shows the diff, the governing spec,
independence labels, cost telemetry, the agent's summary, and bounce
history.

- Actions: **Approve**, **Request changes** (wire action `redirect`;
  written feedback, task re-dispatches into its existing worktree), and
  **Reject** (close). Reason codes are **derived** from the action
  (approve → `approved`, redirect → `changes-requested`, reject →
  `rejected`); the operator's free text carries nuance, while agents and
  integrations may still record curated codes, which render as badges.
- **Cancel** is lifecycle, not judgment: valid from any non-terminal
  state (UI, CLI `conveyor task close`, REST — deliberately not MCP),
  requires a reason, moves every non-terminal order to `cancelled`, and
  refuses in-flight sessions at their next lifecycle call. Cancel
  reuses task state `closed`, distinguished by the recorded
  intervention; it never touches agent-owned branches or worktrees.
- **Stalled tasks surface, not vanish:** retry-suppressed orders,
  queue-timed-out orders, repeating dispatch failures, and
  unsatisfiable dependencies (§6.3) badge the
  task as stalled (a derived presentation state) and appear in the tray
  with recovery and cancel actions plus last-failure evidence — for an
  unsatisfiable dependency, the actions are dependency removal or
  cancel (§21.47).
  Recovery operations are explicit, scoped, idempotent, and audited:
  order recovery with setup re-freeze (§6.2), stale-order redispatch,
  **Retry review round** for a terminal timed-out panel (a whole new
  round snapshotting current configuration against the verified PR
  head, §21.23), and **Recover interrupted review round** for expired
  unclaimed seats (in-place requeue retaining completed verdicts,
  §21.26).
- Local work starts from `conveyor checkout <task-id>`, surfaced with a
  copy affordance in every task header — not a gate action.

### 13.3 Activity view and requirements

The primary UI: an app shell with a **stage-grouped feed** (Triage /
Spec / Implementing / Reviewing / Awaiting human, with counts, hold
chips, stalled badges, provenance chips, and "Needs attention" as the
only alarm color); a **costed event timeline** per task narrating each
stage with its agent summary, duration, token telemetry, harness, and
independence labels — the audit log rendered as a story; **review
actions in place** so a reviewer never leaves the timeline to act; task
intake; workspace administration (setups, harness registry, worker and
per-harness health, serviceability); the **Requirements view** —
the flat corpus of living requirement documents, each showing its
confirmed version, staleness against subsequent merges and drift, the
blueprints serving it, and full task/PR/evidence lineage (§4.2); the
**planning surface**
(Phase 6.2) — session list and chat with streamed replies,
tool-activity markers, and attachments, ending in a blueprint hand-off
to the ordinary spec gate; and **dependency affordances** (Phase 6.1) —
blocked chips naming the blocking tasks in feed and detail — an
unsatisfiable dependency (§6.3) rendered distinctly from ordinary
waiting, with the audited unlink action on the detail surface — and the
**Blueprints surface** (§21.49): blueprint anchors are intent
artifacts, not work, so they are excluded from the stage-grouped feed
and presented on the planning side — a list of blueprints with child
rollup and delivery state, and a detail view leading with the approved
spec, the materialized children in dependency order with per-child
state, the batch timeline, and lineage. Anchor detail suppresses the
task affordances that are inert on an anchor (checkout, assigned
branch, hold); cancel remains, as lifecycle. Children remain ordinary
tasks in the feed.

---

## 14. Usage telemetry (no budget enforcement)

Conveyor records what execution cost and never lets cost gate execution
(§21.6, §21.28):

1. **No allocation, no breakers.** There are no per-stage or per-job
   budgets, limits, or policies; no job or work order is rejected,
   paused, or rerouted because of token or USD values. The runaway-loop
   backstops are structural: stage timeouts, work-order clocks, the
   bounce-cap check-in (§4), and `work_order_queue_timeout`.
2. **Telemetry is observational.** In-process stages record
   provider-reported `tokens_in`/`tokens_out` and **no USD cost**
   (`cost_usd` is NULL — never rendered as `$0.00`); there is no
   in-process price catalog, and pricing can never fail a control-plane
   action. Worker/MCP jobs report cumulative token and USD telemetry
   through `report_usage`, persisted as self-reported. `report_usage`
   MAY additionally carry the provider's latest rate-limit status,
   persisted self-reported and rendered on worker/harness health
   surfaces — observational like every other usage value, never a gate
   (§21.41).
3. **Attribution stays visible** in the per-task timeline. The
   aggregate cost dashboard is Phase 9, observational only; nothing
   revives spending enforcement without a new accepted amendment.

---

## 15. Memory and self-improvement *(Phases 7–8)*

### 15.1 Memory store *(Phase 7)*

Workspace-scoped knowledge — architecture
decisions, conventions, lessons — with hybrid keyword/vector retrieval
(Postgres + pgvector) and per-role context budgets. Its transport is
already decided: control-plane MCP tools (`get_memories`,
`store_memory`) on the §17.4 server, available to any connected session;
the worker is deliberately uninvolved. The spec corpus half of the
original design ships as the requirement corpus (§4.2), and
Phase 6.3's lineage links are the primary knowledge structure — the
memory store is **recall over that graph**: retrieval ranks and returns
linked nodes with their provenance, and vector similarity is the
secondary index, never the authority (§4.2 item 4, §21.46).

### 15.2 Self-improvement engine *(Phase 8)*

Consumes transcripts, reason
codes, bounce histories, and telemetry; produces human-reviewable
proposals (never auto-applied): role-prompt and policy edits, memory
entries, gate-default and hold-usage graduation, and pack versioning
with the eval rig and shadow runs (§2.2). Deferred until Beta-era
operation accumulates the corpus it mines. The design intent is a
measurable flywheel: every human intervention makes the next similar
task less likely to need one.

---

## 16. Data model (core tables, abridged)

One Postgres database is the source of truth. **`events` is append-only
and authoritative**; task, job, and work-order rows are projections for
querying, and every lifecycle transition commits its projection update
and event in one transaction (§3.3). The core tables:

- `workspaces` — slug ID, display name, versioned config document
  (§2.1).
- `repos` — `{name, url, github, base}` per workspace.
- `tasks` — state (CHECK-constrained from the machine module), current
  stage, assigned branch/base metadata, frozen setup contract, frozen
  gates, hold, provenance, generated title, GFM body,
  parent (blueprint) task link with originating `(spec version, SUB-n)`.
- `requirements` / `requirement_versions` — living intent documents
  (§4.2): slug, current confirmed version; versions carry prose, the
  `REQ-n` statement block, confirmation, and origin (chat revision or
  drift amendment). The deprecated `features` tree is retired:
  content-bearing nodes seed requirement documents, empty nodes drop,
  and `tasks.feature_id` assignments convert to history links (§21.46).
- `task_dependencies` — acyclic, workspace-scoped, repo-spanning
  `(task, depends_on)` edges enforcing
  §6.3 claim gating; written by blueprint materialization or intake;
  removed only by the audited operator unlink action (§6.3, §21.47).
- `planning_sessions` — durable planning chats (§9): status, message
  log, finalized requirement/parent-task link; transcript archived as
  an artifact on finalize.
- `links` — polymorphic typed lineage edges
  `(src_type, src_id, dst_type, dst_id, kind, created_by_event)`;
  projections of `events`, rebuildable (§4.2 item 4).
- `work_orders` — stage-typed; state, queue/execution/lease clocks,
  retry/suppression state, harness snapshot and fingerprint, claim
  identity (worker, session, client token), review round/seat linkage.
- `jobs` — one execution of one stage: stage, runner (`in-process` or
  worker/BYOA labels), token telemetry, optional self-reported
  `cost_usd`, timing.
- `events` — append-only, workspace/task/job-scoped, actor and role on
  every row; the command vocabulary of §3.3 is the event vocabulary.
- `interventions` — every human decision with action, derived or
  curated reason code, comment, and (for approvals) the approved head
  SHA.
- Spec versions, review rounds and verdicts, artifacts
  (content-addressed), intake idempotency keys, and
  worker enrollments (credential hashes, probe results) round out the
  schema.

---

## 17. Interfaces

### 17.0 Implementation stack

**Backend — Go**, everywhere: control plane, CLI, worker. `net/http` +
chi (API), **pgx + sqlc** (typed Postgres access), **River**
(Postgres-backed job queue, transactionally consistent with event
commits — queue and database are one stateful dependency), cobra (CLI).
The CLI and worker distribute as a single static binary.

**Frontend — Vite + React + TypeScript**, TanStack Router (typesafe
deep links, `/tasks/$taskId`) and TanStack Query for all server state,
Tailwind + shadcn/ui. Live surfaces stream over SSE into the Query
cache. The dashboard is a pure SPA embedded into `conveyord` via
`go:embed`: the entire control plane deploys as one binary plus
Postgres. (TanStack Start was considered and rejected — SSR and server
functions would add a Node tier an authenticated internal dashboard
doesn't use; cheaply reversible since Start builds on Router.)

**Permitted exception:** the Phase 8 analysis side of the
self-improvement engine may run as a small Python sidecar if transcript
mining outgrows Go; everything else stays in the two stacks above.

### 17.1 CLI

```
conveyor task new "…body…" --repo api [--base main] [--setup ui]
                            [--hold] [--workspace acme]
conveyor task list | show <id> | redirect <id> -m "…" | approve <id>
conveyor task close <id> --reason "…"       # cancel (lifecycle, not judgment)
conveyor checkout <task-id>                 # safe worktree resolver (§8.2)
conveyor done <task-id>                     # terminal-state worktree cleanup
conveyor worker run [--workspace acme]      # enroll/poll/supervise (§6.4)
conveyor worker install | uninstall | status   # Phase 5.5
conveyor config export | import             # git-versioned config backup
```

### 17.2 First-run sequence

1. Deploy `conveyord` (one binary + Postgres); set the deployment file's
   substrate settings and `CONVEYOR_API_KEY`.
2. Create a workspace and point it at repos; the seed file imports on
   first boot, then the database is the truth.
3. Register harnesses in the workspace registry (or rely on interactive
   agents only); define setups or accept the default.
4. For unattended operation: issue a pairing token, run `conveyor worker
   run` on the operator machine, confirm health on the Workspace page.
5. Create the first task from the dashboard, CLI, or an agent session
   over MCP — and review your first spec at the gate.

### 17.3 HTTP API

REST + SSE mirroring the CLI and UI: task CRUD and intake,
planning-session lifecycle and chat streaming (AI SDK UI-message
protocol over SSE, §9), job/timeline
streaming, review actions, hold toggle, setup reassignment, recovery
operations (order recovery, redispatch, review-round retry/recover),
workspace CRUD and config read/write (`If-Match` optimistic
concurrency), worker pairing-token issuance and revocation, webhook/poll
ingestion. All mutating endpoints require auth and are recorded in
`events`.

### 17.4 MCP task intake and work orders

`conveyord` exposes one authenticated MCP server for agent-facing intake
and execution; the bearer is authenticated as an agent actor, and every
resulting task, event, claim, report, and verdict follows the same
durable audit path as the corresponding HTTP action.

- **Intake:** `create_task` (idempotent, §9).
- **Discovery and claim:** `list_work_orders` (diagnostic: includes
  active, stale, and timed-out orders with claimability and all three
  clock groups), `claim_work_order` (leased; guards: hold, self-review,
  serviceability), `get_work_order`, `renew_work_order`,
  `release_work_order`.
- **Progress and telemetry:** `report_progress`, `report_usage`
  (optionally carrying provider rate-limit status, §14),
  `upload_transcript` (self-reported, labeled).
- **Stage completion:** `submit_spec` (spec orders), `submit_for_review`
  (implementation; opens/reuses the PR and dispatches the review
  panel), `await_review` (long-poll; the in-session revision loop),
  `submit_review_verdict` (review orders).
- **Recovery:** `redispatch_work_order` (stale, never-claimed orders).
- **Context:** `read_artifact`.

Claims and submissions are refused once a task's clocks are spent or its
orders are cancelled — enforcement lives at the protocol boundary.

---

## 18. Security summary

- **The pushed branch is the trust boundary** (§3.2): every gate judges
  the pushed artifact — independent review, CI checks, human approval
  bound to the approved head SHA (§11.2) — never the execution
  environment.
- **Execution is operator-owned and honestly labeled.** No confinement
  of implementing or reviewing processes; jobs record `confinement:
  none, auth: byoa` with self-reported usage, and independence labels
  state what is guard-enforced versus self-reported. Mitigations for
  unattended execution: explicit worker enrollment with revocable
  hashed credentials, per-order identity, hold, dedicated worktrees,
  and gates on by default.
- **History is never rewritten**: append-only events; no force-pushes or
  resets of task branches anywhere in the system (§8.2).
- **Credential hygiene** per §10: deployment key at boot, worker tokens
  hashed server-side and passed child-environment-only, no credential
  in argv/config/snapshots/logs, transcript redaction on everything
  stored.
- **Prompt-injection posture:** task bodies, issue content, and PR
  comments are untrusted data. Headless harness permission grants are
  explicit launch data scoped to the Conveyor MCP tools (§5.2), and
  agents file tasks but cannot cancel them (§13.2).

### 18.1 Enterprise path

The enterprise strategy is bottom-up: a platform team self-hosts
Conveyor, proves the automation-rate and cost metrics, and champions
adoption. Self-hosting is the wedge — the control plane runs in the
customer's infrastructure and code never leaves their network; the audit
chain, spec→PR→review→merge provenance, and credential hygiene are core
architecture rather than enterprise add-ons. v1 is explicitly not
enterprise-ready (no SSO/SAML, SCIM, RBAC enforcement, or HA story) but
avoids foreclosing it: authentication flows through a pluggable identity
interface, every actor and event carries a role for later RBAC
enforcement, and operations stay boring — one Postgres, documented
backup/restore, versioned upgrades. SSO/OIDC, SCIM, and RBAC are Phase
10, demand-triggered.

---

## 19. Roadmap

| Phase | Deliverable | Status |
|---|---|---|
| **1–2** | Core loop and durable control plane: task pipeline on Postgres + River, event sourcing, activity view, review queue, redaction | Complete |
| **3** | Full pipeline: multi-stage orchestration, triage/spec/review agents, §4.1 spec format, proto-pack role prompts, PR-comment redirects, timeouts | Complete |
| **4** | UI rewrite (shadcn/ui, §13.3): app shell, stage-grouped feed, task detail timeline, intake | Complete |
| **4.5** | Dynamic workspace configuration (§2.1): Postgres-backed config, validated write API, audit events, hot reload, editable Workspace UI | Complete |
| **4.7** | MCP execution pivot (§21.4–§21.5): sandbox plane retired; work-order lifecycle; requirements tree; artifacts; idempotent MCP intake | Complete — **Beta achieved July 15, 2026** |
| **5.1** | Worker: enrollment, harness registry, health-gated dispatch, stage-aware capacity, gate toggles (modes since removed by §21.31) | Complete |
| **5.2** | Adversarial review panel: per-seat pinned models/efforts, unanimous round-local aggregation, independence labels | Complete (operational; extensions continue by amendment) |
| **5.3** | Factory-coordinated GitHub: issue create/update on spec approval, portable aggregate review status, deterministic PR verdict/resolution comment (§21.22, §21.43) | Complete |
| **5.4** | Evidence-gated `submit_for_review` (§12, §21.44) | Complete |
| **5.5** | Worker service packaging: `conveyor worker install`/`uninstall`/`status` (§6.5) | Complete (§21.46) |
| **5.6** | Platform agents & policy: monitor agent (CI/post-merge signals → tasks, reverse sync §4.2), repo-resident `.conveyor/` hints (§21.45) | Complete |
| **—** | Lifecycle state machines & command plane implementation (§3.3–§3.4, accepted §21.37–§21.38): machine module + event-corpus audit, then staged `taskops` migration — lands ahead of further 5.2+ lifecycle growth | In flight (factory-executed) |
| **6** | Planning & the knowledge graph: **6.1** blueprint materialization + dependency-gated claiming (§4.1, §6.3, §8.3), **6.2** planning sessions — intent → requirement documents, chat → blueprints, features-tree migration (§4.2, §9, §13.3, §17.3), **6.3** lineage links + graph context assembly (§4.2 item 4, §16) | Accepted (§21.46) — next up |
| **7** | Memory store: recall over lineage links; hybrid keyword/pgvector as secondary index; MCP memory tools (§15.1) | Post-Phase-6 |
| **8** | Flywheel: transcript mining, self-improvement proposals, gate/hold graduation, pack versioning with eval rig and shadow runs (§15.2) | Consumes the accumulated corpus |
| **9** | Managed-execution reintroduction, demand-triggered: verification agent (§12), K8sRunner, multi-repo worktree sets and linked-PR gating (§7.2), aggregate cost dashboard | Demand-triggered |
| **10** | SSO/OIDC, SCIM, RBAC enforcement, HA/backup hardening (§18.1) | Demand-triggered |

Sequence: **Phase 5 is complete** (5.1 → 5.6, closed by §21.43–§21.46;
working breakdown in docs/phase5-plan.md). Phase 6 runs 6.1 → 6.2 →
6.3, with the first lineage links written by 6.1's materialization; the
working breakdown lives in docs/phase6-plan.md. The state-machine /
command-plane implementation lands ahead of further lifecycle-surface
growth. This section is authoritative for scope and ordering.

**Beta exit criterion (met July 15, 2026):** five consecutive real tasks
on the Conveyor repository shipped through the full pipeline — issue →
triage → approved spec → implement work order claimed over MCP by the
operator's own agent → `submit_for_review` → review claimed by a
*different* agent session → PR → merge — with at least one task
completing a `changes_requested` round inside the implementing agent's
session, zero manual git operations outside the agents' own workflows,
and all human actions taken through the UI or CLI. The operator ran the
live exit flow; the merged `conveyor/task-*` pull requests (#3–#7, #9,
#10) are the recorded trail.

---

## 20. Decision log

Standing resolutions, with retired ones marked:

1. **Execution ownership.** *Resolved by the MCP pivot (§21.4):* the
   control plane keeps the brain; execution is delegated to
   operator-owned agents over one MCP lifecycle; the pushed branch is
   the trust boundary. The v1.0 sandbox/runner/adapter/credential-pool
   answers (original decisions on runners, session resume, subscription
   terms, and local confinement) are retired with that plane and
   preserved in §21 as the historical record; managed execution
   returns, if ever, as Phase 8.
2. **Spec format.** Markdown prose plus two schema-validated fenced
   blocks (`conveyor:acceptance`, `conveyor:decomposition`) with stable
   IDs and per-criterion verification methods; the approved version is
   the contract; Mermaid diagrams are sanctioned and non-normative
   (§4.1).
3. **Grounded specs.** The spec stage is delegated to a repository-
   reading agent with no blind in-process fallback; triage is
   route-and-frame with an advisory brief (§21.33).
4. **Human oversight.** Two independent gates (spec, merge) frozen at
   intake replaced the L0–L3 ladder; the Auto/Manual mode axis was
   subsequently removed in favor of one queue plus a per-task hold
   (§21.12, §21.31).
5. **Cost.** Usage is telemetry, never enforcement — budget allocation
   and breakers removed (§21.6), in-process USD accounting removed
   (§21.28); structural backstops are timeouts, clocks, and the
   bounce-cap check-in.
6. **Lifecycles.** Normative transition tables with one guarded
   transition primitive per state space (§21.37), enforced through a
   serialized virtual-actor command plane on the Postgres substrate —
   explicitly not an in-process actor runtime (§21.38).
7. **Frozen contracts.** Setup and gates freeze by value at intake;
   mutation is only ever explicit, audited human action (reassignment,
   recovery re-freeze, hold) — never a silent side effect of workspace
   edits (§21.27, §21.34–§21.36).
8. **Titles and bodies.** Titles are generated at the trusted creation
   boundary and are not intake input; bodies are GitHub-flavored
   Markdown stored verbatim (§21.24–§21.25, §21.39).
9. **Cross-repo coordination** *(design preserved, Phase 9):*
   expand/contract decomposition first, linked-PR gating as the
   non-overridable safety floor, merge train deferred until incident
   data justifies it (§7.2).
10. **Supervision hygiene (§21.41).** External orchestrator patterns
    are adopted only where they fit the durability posture: worker-side
    stall detection (watches silence, never spend), deterministic FIFO
    claim ordering, worktree path sanitization, pinned defaults, and
    forge error categories were taken; in-memory scheduler state,
    repo-owned workflow policy files, unbounded exponential retry, and
    server-side concurrency caps were reviewed and rejected.

---

## 21. Amendments

The change record: every accepted amendment from v1.1 through v1.40,
retained verbatim as the design rationale and review history. Since the
v2.0 consolidation (§21.40), the body (§§1–20) is the normative
authority; section references inside §§21.1–21.39 refer to the body as
it stood at that amendment's version.

### 21.1 v1.1 — Phase 1 closure boundaries (July 10, 2026)

Three implementation boundaries are amended; all other v1.0 decisions remain
unchanged:

1. **The Phase 1 local runner is co-process with the volatile control
   plane.** `conveyor runner start --local` starts the combined `conveyord`
   control plane and LocalDockerRunner. A separately registered runner that
   claims jobs over the network begins in Phase 2, where Postgres + River can
   provide durable claims, leases, and recovery. Building a temporary HTTP
   claim protocol over Phase 1's in-memory store would add a second volatile
   queue and be discarded immediately. This amends the Phase 1 interpretation
   of §3.2 and §17.1; the long-term runner protocol and standalone static
   binary requirement remain unchanged.

2. **Phase 1 task checkouts are isolated clones seeded from the shared bare
   cache, not linked `git worktree` entries.** Each task clone has its own
   writable objects and refs and is mounted at
   `/conveyor/jobs/task-<id>/<repo>`. The shared cache is never mounted into a
   sandbox. On eviction, the task branch and task-only objects are copied back
   into the trusted bare cache so committed work survives re-dispatch. This
   amends the Phase 1 storage mechanism in §8.1 while preserving the branch,
   persistence, deterministic-path, and human-worktree contracts in §8.2–§8.4.
   Later phases may replace the full copy with a safe writable object overlay;
   the isolation contract does not change.

3. **Phase 1 includes the minimal human checkout escape hatch.**
   `conveyor checkout <id>` creates a human `git worktree` from the pushed task
   branch, and `conveyor done <id> [--redispatch]` safely returns committed
   human work by pushing and re-queueing the task. This moves the single-repo
   CLI lifecycle from the Phase 3 row of §19 into Phase 1 because it is needed
   to exercise the handoff/redirect boundary. Phase 3 retains the product
   expansion: review-UI deep links, multi-repo checkout sets, and scaled remote
   runners.

### 21.2 v1.2 — Beta re-phasing (July 11, 2026)

Phases 3–9 of §19 are restructured around an explicit operating milestone:
**Beta, defined as Conveyor developing Conveyor** — the platform running its
own repository's development loop, with the operator's involvement limited to
gate decisions and merges. The pre-Beta scope is deliberately minimal — the
full pipeline plus the UI to operate it — so the feedback loop starts turning
as early as possible; everything else is sequenced *behind* Beta, where it is
built with (and increasingly by) the factory itself. Five changes; all other
v1.1 decisions remain unchanged:

1. **The pipeline completes before anything scales.** The former Phase 4
   agents (triage, spec, code review) plus multi-stage orchestration move
   ahead of every infrastructure expansion, as the new Phase 3. Dogfooding
   requires the factory workflow (§4), not more runners.

2. **A dedicated UI phase gates Beta entry** (new Phase 4): a ground-up
   rewrite of the dashboard to the §13.3 design on the §17.0 stack
   (Tailwind + shadcn/ui), designed against the full pipeline's data model
   — triage classes, spec versions, bounce histories, per-stage costs.
   Post-Beta phases extend it (approval cards with Phase 5, memory surfaces
   with Phase 6) rather than reshape it.

3. **Platform agents & policy (new Phase 5) and the memory store (new
   Phase 6) move post-Beta.** For a single-operator Beta on one repository,
   IAM scoping and per-repo tool policy already bound what jobs can do
   (§11.2 layer 1 and the Phase 1 execpolicy work), the operator is the
   monitor agent, and workspace knowledge lives in the repo the agents
   already read. These phases land first *after* Beta, prioritized by
   observed operational load, and are themselves built through the factory.

4. **The flywheel (new Phase 7) consumes what Beta produces.** Transcript
   mining, self-improvement proposals, escalation graduation, and the eval
   rig follow the memory store post-Beta. This strengthens the v1.0
   rationale ("useless until a few hundred transcripts exist") rather than
   contradicting it: Beta generates exactly the corpus Phase 7 mines. The
   monitor agent, which v1.0 described (§4 step 8, §4.2) but never placed
   in the roadmap, is explicitly phased at 5.

5. **Verification, K8sRunner, multi-repo sets, and the aggregate cost
   dashboard become demand-triggered** (new Phase 8), joining enterprise
   readiness in deferral. Rationale mirrors §18.1's: GitHub review
   substitutes for the verification agent, one local runner is sufficient
   capacity, and per-task cost is visible in the activity timeline. These
   activate on evidence of need, not on schedule. The §12 verification
   design and §3.2/§7.1 architecture are unchanged — only their scheduling
   moves.

The Beta exit criterion is recorded in §19. Operational note: because merged
PRs change the running factory, deployment of Conveyor itself remains a
manual operator action (build + restart) during Beta — consistent with the
§1.2 non-goal of Conveyor never deploying to production.

### 21.3 v1.3 — Dynamic workspace configuration (July 12, 2026)

Phase 4's review surfaced an operating-friction gap: every routing, budget,
or repo change requires editing `conveyor.yaml` on the control-plane host and
restarting `conveyord`. That is tolerable for deployment plumbing and wrong
for the knobs an operator turns while running the factory — the §13.3 design
goal ("operators absorb per-stage cost passively") implies they can also *act*
on what they see. A new **Phase 4.5** is inserted pre-Beta and gates Beta
entry. Six changes; all other v1.2 decisions remain unchanged:

1. **Workspace configuration is Postgres-backed.** The `workspaces` table
   (§16) — whose `config_yaml` column has been the anticipated home since
   v1.0 — becomes the source of truth for workspace-scope configuration
   (§2.1): stage routing, repos and their environments, budgets, bounce
   limits, and the workspace base image. A `config_version` column supports
   optimistic concurrency and rollback-by-reference.

2. **`conveyor.yaml` becomes the bootstrap seed, not the running truth.**
   On first boot against an empty workspace row, the file's workspace-scope
   sections import into Postgres (this generalizes the existing
   `BootstrapConfig` path). Thereafter the database wins; the file's
   workspace sections are ignored with a startup notice. `conveyor config
   export` / `import` round-trip the database copy for git-versioned
   backups and disaster recovery. **Boot-time deployment settings stay
   file-only**: database backend/URL, listen address, pack directory,
   secrets backend, cache/jobs directories — the control plane cannot
   reconfigure its own substrate from a table it hasn't connected to yet.

3. **Configuration mutates through the authenticated API.** New §17.3
   endpoints: `GET /v1/workspace/config` (full workspace-scope document +
   version) and `PUT /v1/workspace/config` (full-document write,
   `If-Match` on version). Writes pass the same validation as file load —
   one validator, two entry points — and rejected writes return structured
   field errors the UI renders inline. Every accepted write appends a
   `config.updated` event carrying actor identity, the version pair, and a
   section-level diff summary: configuration changes enter the same
   append-only audit stream as every other state transition (§3.1, §16).

4. **Hot reload, bounded.** The dispatcher, router, and trigger poller read
   from a config snapshot that refreshes on change notification (or
   per-dispatch fetch — implementation's choice); a routing or repo change
   takes effect from the next dispatched job. In-flight jobs keep the
   snapshot they started with — budgets, timeouts, and tool policy are
   immutable per job once dispatched, preserving §14.1's audit semantics.

5. **Editable scope, first cut.** UI-editable: workspace basics
   (`max_bounces`, base image), per-stage routing (harness order, model
   tier, budget, timeout), and repos & environments (URL, GitHub slug,
   base branch, per-repo image, tool-policy allow/deny lists, secret-set
   *references*). **Excluded and still file-based:** the credential pool
   and vendor policies — credential refs name host paths and secret
   entries, and §5.2's consent model makes them the wrong first surface
   for HTTP mutation; they migrate no earlier than Phase 5 alongside the
   approval-card machinery. Secret *values* never appear in config in any
   form (§10.1, unchanged).

6. **Beta entry re-gates on Phase 4.5.** The §21.2 rationale ("minimal
   pre-Beta") bends here deliberately: during Beta the operator tunes
   routing and budgets continuously, and doing that through SSH-and-restart
   would put the factory's primary control surface outside its own audit
   log. **Phase 4.5 exit criterion:** a stage-routing change and a repo
   addition made through the UI take effect on the next dispatched job
   without a control-plane restart, each recorded as a `config.updated`
   event with actor identity; a rejected invalid write surfaces its
   validation error in the UI and leaves state untouched. The Beta exit
   criterion itself (§19) is unchanged.

### 21.4 v1.4 — MCP execution pivot, requirements tree, artifacts (July 14, 2026)

The largest amendment to date, and a deliberate change of thesis. v1.0–v1.3
Conveyor owned execution: sandboxes, harness adapters, and a pooled credential
layer existed so the factory could run implementation itself. Operating
experience and the arrival of capable operator-owned coding agents (Claude
Code, Cursor) invert the economics: the hardest parts of the execution plane —
subscription pooling, headless-use terms, sandbox provisioning — exist to
solve a problem the ecosystem now solves for us. **Conveyor keeps the brain
and delegates the hands**: the control plane owns orchestration, gates,
specs, audit, and branch management; implementation happens in agents the
operator brings, connected over MCP. A new **Phase 4.7** is inserted pre-Beta
and gates Beta entry. Nine changes; all other v1.3 decisions remain unchanged:

1. **The sandbox execution plane is retired.** LocalDockerRunner, the
   harness adapters, the credential pool and router, the job shim, sandbox
   images, handoff snapshots and session resume, confinement tiers, and
   sandbox CLI provisioning are removed — code deleted in Phase 4.7, not
   mothballed. This supersedes §3.2, §5.1–§5.3, §6, §8.3 (snapshots; the
   worktree-persistence *contract* survives trivially since the implementer
   owns its own checkout), §8.5, and §11. Repo-level tool policies,
   per-repo sandbox images, and sandbox secret injection retire with it;
   transcript redaction (§10.3) survives and applies to everything the
   control plane stores. The bare-clone cache (§8.1) survives for branch
   management, diffing, and the human/CLI checkout flow.

2. **Pipeline agents move in-process; code review is MCP-first.** Triage
   and spec run as direct vendor-API calls inside `conveyord` on a single
   deployment-owned key (`CONVEYOR_API_KEY`) — no harness CLI, no
   container, no credential routing; they are cheap, bounded, and must be
   always-on. Code review is the expensive stage (it reads the diff, the
   spec, and surrounding code), so it **executes as an MCP work order by
   default**, with the in-process agent as per-stage fallback: routing
   becomes a per-stage `{model, budget_usd, timeout, execution:
   in_process | mcp}` table, with triage and spec fixed `in_process` and
   review defaulting to `mcp`. The §4/§4.1 output validators are
   unchanged regardless of where a stage executes. Two structural wins
   are recorded as design intent: exact token metering makes the §14.1
   budget breaker natively enforceable for in-process stages, and the
   control plane captures complete in-process transcripts first-hand — a
   better Phase 7 corpus than sandbox log scraping ever produced.

3. **Implementation and review are delegated over MCP (new §17.4).**
   `conveyord` exposes an MCP server with the work-order lifecycle:
   `list_work_orders`, `claim_work_order` (leased; abandoned claims
   return to queue), `get_work_order`, `report_progress`,
   `report_usage`, `upload_transcript`, `submit_for_review`, and
   `submit_review_verdict`. Work orders are **stage-typed**: an
   *implement* work order delivers the approved spec, task branch, base,
   bounce history, prior feedback, and artifact references; a *review*
   work order delivers the diff/PR reference, the approved spec, the
   bounce history, and the review role prompt, and is answered with a
   §4.1-validated verdict via `submit_review_verdict`. **Self-review is
   forbidden at the protocol boundary**: a review work order for task T
   cannot be claimed by the token or session that claimed T's implement
   work order — §5.3's different-from-implementer rule restated for the
   BYOA world; reviewer identity is recorded on the intervention.
   Deeper independence — a different model family, a different human —
   is deliberately the operator's responsibility, not platform
   enforcement; the platform's obligation is **independence labels**
   (the §8.5 audit-labeling pattern applied to review provenance):
   `submit_review_verdict` carries the reviewer's self-reported agent
   and model, and each review is recorded and surfaced with
   `reviewer_session: distinct` (guard-enforced),
   `reviewer_model: <self-reported>`, and
   `same_model_as_implementer: true | false | unknown`, shown on the
   review card and timeline entry so an operator reads at a glance how
   independent a verdict actually was. Claims
   and submissions are refused once a task's budget or wall clock is
   spent — enforcement moves from the process boundary to the protocol
   boundary. Jobs run this way are recorded `harness: external-mcp,
   confinement: none, auth: byoa`, with usage and transcripts marked
   self-reported. Every credential-class concern of §5.2 becomes moot:
   the operator's agents run under the operator's own login,
   interactively or headless, on the operator's machines.

4. **The review loop lives in the implementer's session when a reviewer
   is available.** `submit_for_review` pushes the factory forward: the
   control plane opens the PR if none exists (retaining the §9 GitHub
   machinery) and dispatches review per the stage's execution mode. With
   `in_process` review the verdict returns synchronously *as the tool
   result*. With `mcp` review (the default), submit enqueues a review
   work order and the implementing session may block on an
   `await_review` long-poll tool — so when a reviewer agent claims
   promptly (the single-operator pattern: a fresh session of the
   operator's agent), a bounce is still a conversation turn in a warm
   session; when no reviewer is available, the loop degrades gracefully
   to async and feedback is delivered on the next claim. Either way this
   is strictly better than what §8.3's snapshot machinery existed to
   approximate. Bounce counting, `pipeline.bounced` events, and the
   §21.2 bounce cap apply unchanged.

5. **The pushed branch is the trust boundary.** Every merge gate judges
   the artifact, not the environment: spec approval and human gates are
   factory-side; code review is an independent-session judgment against
   the pushed branch (change 3's no-self-review rule) wherever it
   executes; mechanical verification delegates to the repository's own
   CI (PR checks) until the Phase 8 verification agent — which, with
   K8sRunner, is now explicitly the demand-triggered *reintroduction* of
   managed execution. What is given up is recorded plainly: no
   confinement of the implementing or reviewing process, no observed
   transcripts of either, and unattended automation becomes "a headless
   agent the operator points at the MCP server" rather than a capability
   the factory provides.

6. **Requirements tree (amends §13.3, pulls the corpus UI forward from
   Phase 6).** Approved specs accumulate into a browsable, hierarchical
   feature tree — the spec corpus (§15.1) as a first-class UI module
   rather than a retrieval store. Feature nodes are operator-managed;
   triage suggests a node for each task and a human can reassign; a node
   renders its accumulated approved requirement text and links every
   task, PR, and event that touched it. This is the durable
   requirement → work → code lineage, built on the existing event graph.
   Embedding retrieval and per-role context budgets remain Phase 6.

7. **Artifacts.** Workspace-scoped context files (documents, images,
   audio), uploaded through the UI, attachable to feature nodes and
   tasks. Attached artifacts are injected into pipeline-agent context and
   listed in `get_work_order` for MCP clients to fetch. Artifacts are
   context, never secrets (§10.1 unchanged); storage is size-bounded and
   content-addressed.

8. **The §21.3 config document slims.** Credentials, vendor policies,
   tool policies, per-repo images, and repo secret references leave
   workspace configuration; repos keep `{name, url, github, base}`;
   routing becomes the per-stage `{model, budget_usd, timeout}` of
   change 2. The Phase 4.5 storage, API, hot-reload, and audit mechanics
   are unchanged — only the document shrinks.

9. **Beta is redefined around the pivot.** Phase 4.7 gates Beta; the exit
   criterion is restated in §19: five consecutive real tasks where the
   operator's own agent claims each work order over MCP, at least one
   completes a `changes_requested` round in-session, zero manual git
   operations outside the implementing agent's workflow, and all human
   actions go through the UI or CLI. Phase 5 sheds the command-policy
   shim approval cards and environment inference (retired with the
   sandboxes it policed and provisioned); the monitor agent is
   unaffected. Phase 7's corpus improves (change 2); Phase 8 absorbs
   managed execution as demand-triggered scope.

---

### 21.5 v1.5 — MCP task intake (July 14, 2026)

v1.4 exposed MCP only after a task had passed triage and its spec gate. That
left agent-discovered issues dependent on a separate UI, CLI, or REST client
before the agent could hand work to the factory. The MCP surface now accepts
intake while preserving one orchestration path. Four changes; all other v1.4
decisions remain unchanged:

1. **`create_task` is the MCP intake operation.** It accepts `title`, `body`,
   `repo`, `base_branch`, `source`, and `level` using the same defaults and
   validation as normal task creation. It creates a standard queued task with
   the normal generated branch and initial triage stage; it does not return an
   ad hoc triage answer. Section 21.25 later removes `title` from every intake
   surface while leaving the rest of this operation unchanged.
2. **Idempotency is durable and workspace-scoped.** Every call requires an
   `idempotency_key`, persisted under a uniqueness constraint. Retrying the
   same input with the same key returns the original task and does not enqueue
   triage again. Reusing a key for different task input fails closed.
3. **The existing pipeline remains authoritative.** The call returns as soon
   as task creation and enqueue commit. River then dispatches the configured
   in-process triage stage, including its schema validation, exact usage,
   timeout, budget, transcript-redaction, bounce, and audit behavior.
4. **MCP intake uses agent identity and the existing bearer boundary.** The
   `task.created` event records the authenticated MCP actor. Repository and
   escalation validation are identical to HTTP intake; this amendment grants
   no additional execution or repository capability.

---

### 21.6 v1.6 — Remove budget allocation and enforcement (July 14, 2026)

The v1.5 design carried spending allocation through routing, persistence,
public contracts, operator views, and execution gates. At Conveyor's current
stage that surface creates configuration and operational complexity without a
useful user outcome. v1.6 removes it as one coherent capability. This
amendment supersedes every earlier monetary or token allocation, remaining
balance, circuit-breaker, anomaly-breaker, budget pause, and budget-policy
claim in §§1.1, 2–5, 10, 13–14, 16, 19, and §21.2–§21.5. The earlier text is
retained as the historical record of v1.0–v1.5 rather than silently rewritten.
Six changes; all other v1.5 decisions remain unchanged:

1. **Stage routing has no budget dimension.** Current deployment and
   workspace documents define each route as `{model, timeout, execution}`.
   There are no per-stage or per-job defaults, overrides, limits, or policies,
   and the workspace API and UI neither accept nor expose them. Existing
   Postgres workspace documents are canonicalized once on startup to remove
   the retired v1.5 field; current configuration inputs reject that field.
2. **Usage never gates execution.** Jobs and work orders are not rejected,
   paused, stopped, escalated, or otherwise routed because of token or USD
   values. `job.budget_exhausted`, the budget-exhausted error path, and the
   budget-specific paused state have no current producer or operator surface.
   Timeout enforcement, work-order leases, retry behavior, bounce caps,
   escalation gates, and the normal triage → spec → implement → review
   pipeline are unchanged.
3. **Usage telemetry remains observational.** `report_usage` and persisted
   `cost_usd`, `tokens_in`, and `tokens_out` remain audit facts describing what
   occurred. In-process usage is provider-reported and MCP usage is marked
   `self_reported`; neither is an allocation, balance, or enforcement input.
   Per-stage cost may remain in the event timeline as audit context, but no
   current UI or API labels it as a budget or computes consumption/remaining.
4. **Persistence migrates forward.** Migration 011 removes the obsolete
   `jobs.budget_usd` projection without modifying any applied migration and
   canonicalizes the budget-only paused projection to failed. Append-only
   historical events and all cost/token telemetry are preserved. Generated
   sqlc models and queries describe the post-migration schema.
5. **The operator surface follows the contract.** The task summary no longer
   renders allocation, consumption, or remaining balance; the workspace view
   no longer edits stage allocations; and budget-specific activity messages
   are retired. Active examples and operating documentation use the v1.6
   route shape and describe usage solely as audit telemetry.
6. **No replacement control is introduced.** v1.6 adds no quota, rate limit,
   billing system, managed-execution facility, or aggregate cost dashboard.
   Phase 8's demand-triggered aggregate dashboard remains observational scope
   only if activated later; it does not revive spending enforcement without a
   new accepted amendment.

---

### 21.7 v1.7 — Operator-owned task branches and repository Codex plugin (July 14, 2026)

Phase 4.7 moved implementation out of Conveyor's retired sandbox plane and into
operator-owned agents, but the historical §8.2 branch-creation language and
parts of §21.4 still implied that Conveyor had already created a task ref or
owned the agent's checkout. That implication is unsafe and contradicts the
BYOA responsibility boundary. This amendment supersedes §8.2, the factory-
worktree interpretation of §8.3, §8.4's pre-push checkout implication, and
§21.4 changes 1, 3, 4, and 5 wherever they assign Git mutation to Conveyor.
The older text remains as the v1.0–v1.6 historical record. Six changes; all
other v1.6 decisions remain unchanged:

1. **Intake assigns metadata, not a ref.** Task creation selects the base and
   reserves the canonical `conveyor/task-<id>` branch name in the durable task
   record. Conveyor does not create, check out, push, reset, delete, or
   otherwise mutate a corresponding local or remote Git ref. Ordinary task
   reads and UI rendering expose that assigned name as metadata and never imply
   that the ref exists.
2. **The implementation agent owns branch setup in its checkout.** Immediately
   after `get_work_order` and before edits, the agent inspects repository
   cleanliness and its current branch, fetches the assigned base from `origin`,
   and then safely adopts an existing local task branch, tracks an existing
   remote task branch, or creates the exact assigned branch from the freshly
   fetched `origin/<base>`. Dirty or unsafe Git state, ambiguous ownership, or
   divergent local and remote task histories block the run. The agent never
   cleans or stashes unrelated work and never uses reset-style `-B`/`-C`,
   forced ref updates, or equivalent commit-overwriting behavior.
3. **Branch availability has three explicit states.** (a) Assigned: the task
   stores a canonical branch name and base but no local or remote task ref need
   exist. (b) Local: the implementation agent has created or adopted the branch
   in its checkout but has not pushed it; this is agent-owned state and is not
   represented as a factory branch. (c) Pushed: the agent has pushed the exact
   assigned branch, making it available to Conveyor's review coordination and
   the human checkout flow.
4. **Redispatch and review bounces preserve the branch.** A successor
   implementation session resumes the existing assigned branch when present.
   It may fast-forward an ancestry-safe local branch from its remote counterpart
   but must not recreate the branch from base, rebase or force-reset it, or
   discard task commits. Divergence is reported as a blocker instead of being
   resolved by history rewriting.
5. **The pushed branch remains the review trust boundary.** The implementation
   agent commits and pushes the assigned branch with upstream tracking before
   `submit_for_review`. Conveyor then opens or reuses the PR and dispatches
   independent review; it does not push on the agent's behalf. Review,
   self-review prevention, CI, and human gates judge the pushed artifact.
   `conveyor checkout <task-id>` and pull-to-local UI guidance remain unavailable
   until Conveyor records a pushed-branch PR; the CLI independently fails
   closed when `origin` lacks the task ref.
6. **The Codex integration is repository-owned.** This repository contains the
   installable Conveyor plugin manifest, token-free local MCP configuration,
   operator skill, and local marketplace metadata. Authentication is supplied
   only through the operator environment's `CONVEYOR_API_TOKEN`. The operator
   skill is the reusable procedure for the safe branch setup above and for the
   push-before-review handoff; it does not restore factory-side checkout or the
   retired sandbox execution plane.

---

### 21.8 v1.8 — Dedicated local task worktrees (July 14, 2026)

The v1.7 operator-owned branch boundary correctly removed factory mutation of
the agent's checkout, but it still permitted an implementation agent to switch
or edit a shared primary checkout and kept `conveyor checkout` behind the first
push. That leaves operator work exposed and duplicates safe Git setup between
agents and humans. This amendment supersedes §8.4 and §21.7 changes 2–5 where
they describe a shared-checkout or post-push-only local flow. The historical
text remains the v1.0–v1.7 record. Seven changes; all other v1.7 decisions
remain unchanged:

1. **A dedicated checkout is mandatory.** Immediately after claiming and
   reading an implementation work order, the agent resolves a clean checkout
   dedicated to the assigned task branch. A registered Git worktree at the
   deterministic sibling `../<repo>-task-<task-id>` is the default; an existing
   clean clone or worktree already dedicated to the branch is acceptable. A
   shared primary checkout is not an implementation directory. Every edit,
   build, test, commit, and push occurs in the resolved task checkout, and the
   primary checkout's branch and files remain untouched.
2. **`conveyor checkout` is the shared safe resolver.** The command is usable by
   coding agents and humans before or after the first push, retains `--path`,
   and emits the resolved path as stable success output. It first inspects the
   repository root, primary and current checkout safety, current branch,
   registered worktrees, and assigned branch/base. A clean registered worktree
   already owning the branch is reused exactly, including across repeated calls
   and redirects; otherwise the deterministic path is created. Dirty unrelated
   work, an in-progress Git operation, detached or ambiguous state, a conflicting
   path or worktree, and a dirty task checkout block the operation.
3. **Branch creation preserves history.** Before creating a worktree, the helper
   fetches and verifies `origin/<base>`. An existing unclaimed local task branch
   is added without reset; an existing remote task branch is fetched and tracked;
   a missing task branch is created from the freshly fetched base as part of
   `git worktree add -b`. Ancestry-safe remote-ahead state may fast-forward
   normally and local-ahead commits are preserved. Divergence blocks. The helper
   never uses `worktree add -B`, `switch -C`, `checkout -B`, reset, rebase,
   automatic stash, forced ref updates, or an equivalent history-rewriting path.
4. **Worktree continuity spans the review loop.** The resolved worktree remains
   authoritative through `changes_requested` rounds and human redirects. A warm
   implementation session claims the successor work order before editing,
   returns to the same path, commits and pushes feedback there, and resubmits the
   existing PR. Independent review uses the pushed PR diff or a separate
   read-only/detached checkout; it never shares or mutates the implementation
   worktree.
5. **Cleanup follows terminal task state.** `conveyor done <task-id>` removes a
   task worktree only after the task is merged or closed and only when the
   worktree is clean. It does not redispatch, mutate the primary checkout, or
   automatically delete an unmerged branch. Cleanup retains the task branch,
   reports worktree/branch disposition, and is idempotent when the directory or
   registration is already gone. Missing on-disk directories with stale Git
   registrations are removed through Git's normal worktree cleanup.
6. **Operator guidance and UI use the same contract.** The repository-owned
   Conveyor skill prefers `conveyor checkout` immediately after
   `get_work_order`, uses equivalent non-resetting Git worktree operations only
   when the CLI is unavailable, persists the returned path across review rounds,
   and documents review isolation and terminal cleanup. The task UI exposes the
   command without treating a pushed PR as a prerequisite.
7. **Scope remains single-repository and governance remains deferred.** This
   amendment does not restore the retired runner/adapter plane or implement
   Phase 8 multi-repository worktree sets. An audited `update_task`,
   `add_task_context`, or post-creation spec-amendment operation is a future
   intake/governance consideration and is not introduced here.

---

### 21.9 v1.9 — Independent work-order clocks and stale recovery (July 15, 2026)

Phase 4.7 created an external job before an operator agent claimed it and used
that creation timestamp as the stage execution start. Queue residence therefore
consumed the execution allowance and could leave an order advertised as queued
after it was no longer claimable. This amendment supersedes §21.4 change 3 and
the Phase 4.7 timeout language in §19 wherever they conflate queue age,
execution wall clock, or claim lease expiry. The historical text remains the
v1.0–v1.8 record. Six changes; all other v1.8 decisions remain unchanged:

1. **Queue and execution use separate clocks.** A newly created external work
   order records queue entry and queue deadline but leaves its job execution
   start and deadline unset. Its configured per-stage timeout begins only when
   the first claim succeeds; queue residence never consumes execution time.
2. **Execution deadlines are fixed.** The first successful claim atomically
   records the execution start and deadline and marks the external job running.
   Lease expiry may return ownership to the queue, but reclaiming or renewing a
   lease preserves the original execution deadline.
3. **Queue retention is finite and configurable.** The versioned workspace
   document exposes `work_order_queue_timeout`, default `24h`. A never-claimed
   order that passes that deadline transitions to explicit `stale` state and is
   non-claimable. This timeout is independent of every stage route timeout.
4. **Expired execution is explicit.** A queued-for-reclaim or claimed order
   whose fixed execution deadline passes transitions to `timed_out`, marks its
   job failed, and is non-claimable. Listing and claiming both materialize due
   transitions transactionally so callers never depend on a prior list call.
5. **Listing is diagnostic.** `list_work_orders` includes active, `stale`, and
   `timed_out` orders with `claimable`, queue timing, execution timing, and lease
   timing fields. A queued order never reports execution as started.
6. **Stale recovery is audited.** `redispatch_work_order` is the supported
   recovery for a stale, never-claimed order. It resets queue timing and stale
   claim metadata, increments the redispatch count, retains the same task/job/
   work-order linkage and append-only history, and leaves execution unset until
   a later claim. It rejects active claims and execution-timed-out orders; those
   continue through existing retry or operator policy.

---

### 21.10 v1.10 — Multi-workspace control plane (July 15, 2026)

1. **Durable identity and lifecycle.** A workspace has an immutable lowercase
   slug `id` (`[a-z0-9][a-z0-9-]{0,62}`) and an immutable, trimmed display
   `name`; IDs are unique and names are unique case-insensitively. Authenticated
   operators may list, retrieve, and create workspaces. Creation validates the
   initial Phase 4.5 document and atomically commits the workspace, config v1,
   repositories, and `workspace.created`. Rename and deletion remain out of
   scope.
2. **Explicit, fail-closed context.** Canonical routes are
   `/v1/workspaces[/<id>[/config]]`. Existing singular and workspace-scoped
   routes accept `workspace_id` or `X-Workspace-ID`. Conflicting context is
   invalid; omitted context resolves only with exactly one workspace. Zero or
   multiple candidates fail with `workspace_unavailable` or
   `workspace_required`; an explicit unknown ID is not found.
3. **CLI, MCP, and UI use the same identity.** CLI commands accept
   `--workspace`; MCP workspace-scoped tools accept `workspace_id`. The UI
   lists, creates, persists, revalidates, and switches one shared workspace
   selection, cancelling/invalidating prior-workspace requests before refetch.
4. **Isolation is end to end.** Store calls receive immutable workspace context;
   HTTP, task intake, idempotency, activity, requirements, artifacts, work
   orders, dispatch, review publication, and reconciliation constrain reads and
   writes by it. River payloads and dynamically registered per-workspace queues
   carry the same ID, and runtime configuration is loaded for that ID.
5. **Compatibility and scope.** File `workspace: demo` continues to seed that
   workspace idempotently without rewriting existing data. Singleton clients
   may omit context; ambiguity never selects an arbitrary workspace. This does
   not add RBAC/SSO, rename/deletion, aggregate reporting, multi-repo worktree
   sets, verification, Phase 8 execution, or a parallel task pipeline.

---

### 21.11 v1.11 — Verdict-first human gate: derived reason codes, pull-to-local retired from the review UI (July 15, 2026)

The Phase 4 dashboard rework rebuilt the human gate as a verdict-first card:
a headline and tone matched to the actual gate state ("Ready to merge" on an
approved task; amber reserved for parked, bounce-limit, and timeout states),
one context-matched primary action, and the remaining decisions demoted to
quiet secondaries. Three operator decisions (July 15, 2026) simplify what the
gate asks of a human. The wire contract is untouched — `POST
/v1/tasks/{id}/review` still requires an action and a non-empty reason code
(≤64 characters, free-form), and the `Intervention` record keeps all four
§13.2 wire actions — the changes are to the operator surface only. All other
v1.10 decisions remain unchanged:

1. **Reason codes are derived, not picked.** The review UI no longer offers a
   reason-code selector. The dashboard derives the code from the action —
   approve → `approved`, redirect → `changes-requested`, reject → `rejected`
   — and the operator's free-text comment carries the nuance. API validation
   is unchanged, so agents, the CLI, and PR review-comment conversion (§9)
   may still record curated §13.2 codes, and history renders them: the
   timeline shows a curated code as a badge and suppresses the badge only
   when the code merely repeats the action-derived default.
2. **The §15 training signal narrows, knowingly.** Dashboard gate decisions no
   longer carry operator-picked curated classification; the self-improvement
   engine (§15.2) must classify those decisions from the free-text comment,
   transcripts, and bounce history instead of reading a picked code. This
   trades training-signal fidelity for gate throughput. If Phase 7 needs
   structured tags back, reintroducing a picker (or post-hoc tagging) requires
   a new accepted amendment rather than quiet UI creep.
3. **Pull-to-local is retired from the review UI.** Under the MCP pull model
   (§21.4) agents pull work orders; a human who wants the work locally runs
   `conveyor checkout <task-id>` (§8.4, §21.8), which remains surfaced with a
   copy affordance in the task header of every task, not only gated ones.
   Pull-to-local is no longer a gate decision. The `pull_to_local` wire
   action, its recorded interventions, and their timeline rendering remain
   for the historical record; §8.4 semantics are unchanged for the checkout
   path.
4. **"Redirect" surfaces as "Request changes."** Label only: the wire action,
   the `conveyor task redirect` CLI verb (§17.1), and GitHub
   review-comment conversion (§9) are unchanged. The gate renders the action
   as "Request changes" with required written feedback; the timeline renders
   recorded redirects as "Requested changes."

---

### 21.12 v1.12 — Worker execution, Auto/Manual modes, adversarial review, factory-coordinated GitHub (July 15, 2026)

Beta testing validated the §21.4 pull model: operator-owned agents claiming
work orders over MCP. What §21.4 change 5 left as an exercise — unattended
automation as "a headless agent the operator points at the MCP server" —
this amendment ships as a product: a worker the operator installs on their
own machine that polls the work-order queue and drives their own harness
CLIs, in the tradition of a CI runner. Nothing in the pivot's thesis
reverses: the control plane keeps the brain, and the hands stay
operator-owned — the worker is the operator's hands on a timer. No sandbox
plane, no adapter interface, and no credential pooling returns; the worker
invokes `claude -p` / `codex exec` under the operator's own login on the
operator's own hardware. This amendment supersedes §13.1 (the escalation
ladder), amends §19 (the former Phase 5 row becomes phases 5.1–5.5), and
reaffirms the §21.7 branch boundary. The Beta gate and its §19 exit
criterion are untouched: everything here is post-Beta scope. Eight changes;
all other v1.11 decisions remain unchanged:

1. **The worker is a thin supervisor over the existing lifecycle.**
   `conveyor worker run` — a subcommand of the existing CLI, not a second
   binary — enrolls against one workspace with an operator-issued pairing
   token, heartbeats, and long-polls `list_work_orders` /
   `claim_work_order`: the §17.4 lifecycle unchanged, no parallel protocol.
   On claim it resolves the configured harness (change 3) and spawns it
   headless with the Conveyor MCP configuration attached, so the spawned
   session performs the standard flow itself — `conveyor checkout` into the
   dedicated worktree (§21.8), safe branch adoption (§21.7), implement,
   push, `submit_for_review`, `await_review`. The worker supervises:
   liveness reporting, exit-status capture, and claim release on failure;
   §21.9's queue/execution/lease clocks and stale recovery already govern
   abandonment. Jobs it runs are recorded `harness: <cli>, confinement:
   none, auth: byoa, dispatch: worker`. What is given up is recorded
   plainly, in the §21.4 tradition: the worker executes unconfined on a
   real operator machine, on tasks that may originate from GitHub issues or
   chat intake. The mitigations are explicit enrollment, the worker
   claiming only Auto-mode orders (change 2), dedicated worktrees (§21.8),
   human gates on by default, and the trust labels above.

2. **Auto/Manual execution modes replace the escalation ladder (supersedes
   §13.1).** A task carries an execution mode: **Auto** — the worker may
   claim its work orders — or **Manual** — they wait for an
   operator-attached agent. Human gating, which the L0–L3 ladder conflated
   with dispatch, becomes two independent workspace toggles — **spec
   approval** and **merge approval** — each overridable per task; Auto with
   both gates on is the shipped default. There is one queue: any
   authenticated agent may claim any order regardless of mode; the worker
   claims only Auto orders. Legacy mapping for the historical record:
   L3 ≈ Manual; L1/L2 ≈ Auto with gates; L0 ≈ Auto with gates off. Existing
   task records keep their recorded levels; the UI replaces the
   escalation-level badge with a mode chip; `conveyor task new` replaces
   `--level` with `--mode`, and MCP `create_task`'s optional escalation
   level becomes an optional mode (§21.5 otherwise unchanged). Phase 7
   graduation, when it arrives, operates on gate toggles and mode defaults
   instead of ladder levels.

3. **Harness registry and health-gated Auto.** Workspace configuration
   gains a declarative harness registry — `{name, command template, model
   flag syntax}` per entry — under the standard §21.3 mechanics (validated
   writes, `config.updated` events, hot reload). It is data, not an adapter
   interface; §5.1 stays retired. The worker probes each configured harness
   (binary present, authenticated, a trivial invocation succeeds) and
   reports results with its heartbeat; the workspace surfaces worker and
   per-harness health. Auto mode is offered only while a worker is
   enrolled, live, and at least one harness probes healthy, and the
   "default new tasks to Auto" workspace toggle greys out when that fails —
   new tasks fall back to Manual explicitly rather than queueing silently
   against a dead worker.

4. **Adversarial review panel.** The workspace review setting becomes a
   panel: an operator-chosen reviewer count with a model pinned per seat.
   `submit_for_review` dispatches one review work order per seat; the
   self-review guard applies to every seat, and seats must also be distinct
   sessions from one another. Aggregation is unanimous-approve:
   `await_review` returns once all verdicts arrive, and any
   `changes_requested` bounces the task with all reviewers' feedback merged
   into a single structured round — one bounce against the §21.2 cap
   regardless of panel size. Independence labels (§21.4 change 3) gain
   `model_enforcement: worker-pinned | self-reported`: a seat executed by
   the worker is invoked with its pinned model and labeled enforced; a seat
   claimed by an arbitrary MCP agent remains self-reported, and the review
   card renders the difference honestly rather than implying enforcement
   the platform cannot deliver.

5. **The factory coordinates GitHub; the agent only commits and pushes.**
   Three additions to the existing §9/§21.4/§21.7 machinery, two reaffirmed
   boundaries. Added: (a) on spec approval the factory creates a GitHub
   issue carrying the approved spec (intent and acceptance criteria) and
   links it to the task — unless the task originated from an issue (§9), in
   which case that issue is updated rather than duplicated; the eventual PR
   closes it. (b) On the first push of the assigned branch the factory
   opens the PR as a draft and marks it ready at `submit_for_review` —
   earlier visibility, same trust boundary. (c) Review verdicts and their
   resolutions are mirrored onto the PR, extending the existing Check Run
   and factory comment into a complete review trail. Reaffirmed: intake
   assigns branch metadata and never creates refs (§21.7 change 1), and no
   PR exists before the first push — GitHub cannot represent an empty one,
   and a stub commit would violate the §21.8 history rules.

6. **Verification evidence at the submit boundary.** A workspace toggle
   requires evidence before review: with it on, `submit_for_review` is
   refused until at least one verification-evidence artifact — screenshots
   or a short recording of the exercised change — is attached via the
   §21.4 artifacts machinery. Evidence artifacts are listed in review work
   orders, rendered on the review card, and mirrored to the PR (change 5c).
   This delivers §12's stated goal — reviewers confirm evidence rather than
   reproduce behavior — without a new pipeline stage; the independent
   verification agent remains Phase 8 scope.

7. **Memory stays Phase 6; its transport is decided.** The memory store
   remains deferred, but its delivery mechanism is fixed as control-plane
   MCP tools (`get_memories`, `store_memory`) on the §17.4 server,
   available to any connected session. The worker is deliberately
   uninvolved: memory is control-plane state, not worker state.

8. **Roadmap re-phase (amends §19).** The new work lands as Phase 5.1
   (worker & execution modes), 5.2 (adversarial review), 5.3 (GitHub
   coordination), and 5.4 (verification evidence). 5.2 depends on 5.1; 5.3
   touches neither the worker nor review dispatch and may run in parallel
   with 5.2; 5.4 follows 5.3 for the PR-mirroring path. The former Phase 5
   scope — monitor agent and `.conveyor/` repo hints — moves unchanged to
   Phase 5.5, deliberately after the worker: monitor-filed tasks plus Auto
   dispatch is the original autonomous loop completed. Phases 6–9 keep
   their numbers and scope. None of 5.1–5.5 gates Beta; the §19 exit
   criterion runs first, on the Manual pull flow already validated.

---

### 21.13 v1.13 — Worker execution contract (July 16, 2026)

Pre-implementation review of the Phase 5.1 working breakdown resolved a set
of contract details that are normative protocol and pipeline behavior, not
plan minutiae: they extend the §17.4 surface, define gate semantics
including auto-merge, and fix task-lifetime invariants. Per §21 they are
recorded by amendment rather than left in the working plan;
docs/phase5-plan.md remains the breakdown and this section is authoritative.
This amendment refines §21.12 changes 2 and 3 (the legacy-level mapping and
the health-gating rule) where the newer text below is more precise. Seven
changes; all other v1.12 decisions remain unchanged:

1. **Stage routes select harnesses — implement and review both.** The
   per-stage routing table (§21.4 change 2, §21.12 change 3) gains a
   `harness` field on the implement and review routes, referencing a
   registry entry by name; a review route with `execution: in_process`
   takes none. Validation enforces referential integrity both ways: a
   route cannot name an unregistered harness, and a registry entry
   referenced by a route or panel seat cannot be deleted. With exactly one
   registered harness the field may be omitted and inherits it; with more
   than one it is required. The field binds worker dispatch only — a
   Manual claim cannot be forced through a harness — and is surfaced
   enforced vs. advisory exactly as `model_enforcement` (§21.12 change 4).
   Phase 5.2 panel seats override the review route per seat. There is no
   per-task harness override.

2. **Registry schema and placeholder vocabulary.** A registry entry is
   `{name, command, model_args, probe_command, probe_timeout}`. `command`,
   `model_args`, and `probe_command` are argv arrays, never
   shell-evaluated. Placeholders (`{model}`, `{prompt}`, `{mcp_config}`)
   are substituted as whole argv elements at invocation; an unknown
   placeholder is a validation error at write time, not a runtime
   surprise.

3. **Health gating is route-scoped.** Auto mode is offered only while (a)
   an enrolled worker holds a live liveness lease — a server-issued lease,
   default **15 seconds**, refreshed by heartbeat — and (b) every harness
   referenced by the applicable implement and review routes probes healthy
   (an `in_process` review route is exempt). "Any healthy harness" is not
   sufficient: an unrelated healthy harness must not enable Auto while the
   routed harness is down. While unhealthy, an explicitly requested
   `mode: auto` is rejected (HTTP 409 / MCP error) and a workspace-default
   Auto resolves to Manual with a recorded fallback event; nothing queues
   silently against a dead worker.

4. **Worker-control lease endpoints.** The server gains additive
   worker-authenticated operations for **lease renewal** and **active
   claim release**. The agent-facing §17.4 lifecycle is unchanged — no
   second protocol. Renewal never extends the §21.9 execution deadline;
   release returns the order to the queue immediately instead of waiting
   out the lease; §21.9 expiry and stale recovery remain the backstop for
   a worker that dies outright.

5. **Enrollment is a token exchange.** A short-lived, single-use pairing
   token (issued by the operator via UI or CLI) is exchanged at enrollment
   for a revocable, workspace-scoped worker credential stored server-side
   only as a hash. Revocation is an operator action; heartbeats carry the
   per-harness probe results of change 3.

6. **Worker capacity is stage-aware.** Session-count minimums do not
   prevent deadlock: implement sessions blocking in `await_review` could
   occupy every slot while the review orders that would unblock them sit
   unclaimed. The worker therefore runs configurable implement concurrency
   plus **at least one reserved, prioritized review slot**, with each
   order executed under a fresh identity/token pair so the self-review
   guard and independence labels hold.

7. **Gate truth table and intake-time resolution (refines §21.12
   change 2).** Complete gate behavior: spec gate **on** — the spec stage
   is forced for every task and waits for human approval; spec gate
   **off** — triage may skip spec, and any generated spec is
   auto-approved. Merge gate **on** — an approved review waits for human
   merge approval; merge gate **off** — the control plane invokes the
   existing merge machinery automatically on an approved review with green
   checks. The effective mode and gates are resolved and **persisted at
   intake**; later workspace edits never change an in-flight task (the
   §21.3 dispatch-time-snapshot rule applied to gating). The faithful
   legacy mapping, correcting §21.12's coarser one: L0 ≈ Auto with both
   gates off; L1 ≈ Auto with spec gate off and merge gate on; L2 ≈ Auto
   with both gates on; L3 ≈ Manual. In-flight legacy tasks finish under
   their recorded level.

---

### 21.14 v1.14 — Harness-template expansion contract (July 16, 2026)

Implementation preflight found that §21.13 change 2 named a global
placeholder vocabulary without defining which registry field could consume
which runtime value. That allowed configurations which passed validation but
could not expand during a health probe. This amendment refines §21.13 change
2; all other v1.13 decisions remain unchanged:

1. **Expansion is field-local and deterministic.** `command` is the base
   invocation argv and must contain exactly one `{prompt}` element and one
   `{mcp_config}` element; `{model}` is invalid there. `model_args` is
   appended to `command` in declared order, may contain only the `{model}`
   placeholder, and is omitted only when the selected route has no model.
   `probe_command` is a standalone argv with no placeholders because it runs
   outside task context. Placeholders always occupy a whole argv element;
   unknown placeholders, placeholders in the wrong field, missing required
   command placeholders, and a non-positive `probe_timeout` are workspace
   configuration validation errors.

---

### 21.15 v1.15 — Drop draft-PR-on-first-push (July 16, 2026)

§21.12 change 5(b) directed the factory to open a draft PR on the first
push of the assigned branch and mark it ready at `submit_for_review`.
Reviewed against the documented flow before any implementation, the feature
serves a window that does not exist: under the §21.8 operator skill the
implementing agent pushes immediately before `submit_for_review`, so the
draft would live for seconds and flip unobserved. Its theoretical benefits
do not land — early CI feedback pays off only under incremental pushing,
which the flow does not do; mid-flight visibility is already provided by
`report_progress` in the activity view; and the "trail legible on GitHub
alone" requirement is satisfied by issue → PR → mirrored verdicts → merge
regardless of when the PR appeared. The machinery, meanwhile, is real:
push-event branch matching, idempotent draft creation, a draft→ready flip
with retry semantics, and orphan cleanup for abandoned orders. This
amendment supersedes §21.12 change 5(b) and amends the §19 Phase 5.3 row.
One change; all other v1.14 decisions remain unchanged:

1. **The PR is opened at `submit_for_review`, and not before.** This
   retains the behavior already specified and implemented (§21.4 change 4,
   §21.7 change 5): the factory opens or reuses the PR when the agent
   submits the pushed branch for review. No draft PRs are created and no
   push-event machinery is added. Phase 5.3 accordingly slims to issue
   creation on spec approval and verdict/resolution mirroring (§21.12
   changes 5a and 5c, unchanged). If agents later adopt incremental
   pushing and early CI proves valuable, reintroduction is a new amendment
   made against demonstrated need.

---

### 21.16 v1.16 — Worker service packaging phase (July 17, 2026)

Phase 5.1 shipped `conveyor worker run` as a foreground process: enrollment
persists across restarts (§21.13 change 5), but the process itself dies with
the terminal or the machine, and Auto capacity stays down until an operator
relaunches by hand. Health gating makes that failure visible — Auto greys
out within one liveness lease — but unattended operation, the point of Auto
mode, should not depend on an operator remembering to restart a process.
This amendment amends the §19 roadmap. Two changes; all other v1.15
decisions remain unchanged:

1. **New Phase 5.5 — worker service packaging.** `conveyor worker install`
   registers the worker as an OS-managed service wrapping the existing
   `worker run` — a launchd agent on macOS, a systemd user unit on Linux —
   with restart-on-failure and start-on-boot/login; it requires existing
   enrollment and refuses with guidance when the credential is absent.
   `conveyor worker uninstall` stops and removes the unit idempotently.
   `conveyor worker status` reports service state, enrollment identity,
   last heartbeat, and per-harness probe results. Service stdout/stderr go
   to a documented log location surfaced by `status`. The service is
   supervision only: no new protocol, endpoints, or behavior beyond the
   foreground command it wraps; pairing and enrollment are unchanged, and
   interactive `worker run` remains fully supported. Placement after
   Phase 5.4 is deliberate operator prioritization — factory-coordinated
   GitHub and evidence gating land before convenience packaging; the phase
   has no technical dependency on 5.2–5.4.

2. **Platform agents renumber to 5.6.** The former Phase 5.5 — monitor
   agent and `.conveyor/` repo hints (§21.12 change 8) — becomes Phase
   5.6, scope unchanged. The post-Beta sequence is
   5.1 → {5.2 ∥ 5.3} → 5.4 → 5.5 → 5.6.

---

### 21.17 v1.17 — Bounce cap retuned: check-in semantics (July 17, 2026)

The §4/§21.2 bounce cap parks a task at the human gate rather than
terminating it — it bounds how long an implementer↔reviewer loop runs
*unsupervised*, and it matters more since §21.6 left bounces and timeouts
as the only circuit breakers against runaway loops (§14), a risk the
§21.12 unanimous-approve panels raise further. Operating experience found
three defects in its tuning, not its existence: the default of 2 fires
inside healthy review iteration; the counter is cumulative for the task's
life, so after a first park every later bounce re-parks immediately even
when a human has said "keep going"; and the operator surface names it a
"maximum," implying termination rather than escalation. A per-mode cap
(none for Manual, cap for Auto) was considered and not adopted — one
uniform rule is simpler and Manual operators benefit from the check-in
too. Three changes; all other v1.16 decisions remain unchanged:

1. **The default rises from 2 to 10.** `max_bounces` remains
   workspace-configurable under the §21.3 mechanics; the shipped default
   becomes 10. The tripwire exists to catch structural disagreement
   between agents, not to pace healthy iteration — it should fire on
   stuck loops only.

2. **Human intervention resets the window.** The cap compares bounces
   *since the last human intervention on the task* (redirect, or resume
   from the parked state), not the lifetime count. A human who reviews a
   parked task and sends it back grants a fresh unsupervised allowance of
   the full configured window. Every bounce is still recorded
   (`pipeline.bounced` events and bounce histories are unchanged as §15
   training signal); only the parking comparison changes.

3. **The surface says what it does.** The workspace field renders as
   "review rounds before human check-in" (the `max_bounces` wire and
   config name is unchanged for compatibility); the parked card presents
   the state as a check-in on a still-live loop — escalation, not
   failure — with resume-with-a-fresh-window as its context-matched
   primary action per the §21.11 verdict-first pattern.

---

### 21.18 v1.18 — Contextual execution settings (July 17, 2026)

The generic per-stage routing table from §21.4/§21.13 no longer matches the
execution boundary established by the worker and adversarial review panel.
Triage and spec are fixed control-plane stages, implementation is the only
worker route with a workspace-owned model policy, and review seats own their
models and optional harnesses. This amendment changes the configuration
surface and normalization rules without breaking stored documents or durable
dispatch snapshots. Six changes; all other v1.17 decisions remain unchanged:

1. **Execution settings are contextual.** The canonical workspace document
   exposes `execution_settings.control_plane.{triage,spec}` model/timeout
   settings, `execution_settings.implementation` harness/model-policy/timeout
   settings, and `execution_settings.review` execution/timeout plus optional
   fallback model/harness. The Workspace UI presents those contexts rather
   than a generic Stage Routing table. Triage and spec expose no harness or
   execution selector; implementation is fixed to MCP and requires a harness;
   review execution and timeout are co-located with the adversarial panel.

2. **Legacy routing is compatibility-only.** `routing.stages` remains readable
   and is still emitted during the v1.18 deprecation period for older REST/CLI
   consumers and stored snapshots. A single normalization step maps a legacy
   document into contextual settings. When both shapes are present, contextual
   settings are authoritative and legacy fields cannot override them. Existing
   values are preserved when the mapping is unambiguous; old triage/spec
   harness values are ignored because those stages are fixed in-process.

3. **Implementation owns a model policy, not an accidental model string.** Its
   policy is either `explicit`, which requires and forwards the configured
   provider model, or `harness_default`, which omits model arguments unless the
   selected harness declares a supported default sentinel. Symbolic policy
   labels such as `subscription` are never forwarded as provider model IDs
   merely because they occupied the historical route `model` field. An
   unresolved explicit model or undeclared sentinel is an actionable config or
   dispatch error.

4. **Review seats own review routing.** Each enabled seat owns its pinned model,
   optional harness override, and first-class effort (§21.19 coordinates the
   effort addition with the same panel contract). The route-level review model
   and harness are fallback data only. When every seat names a harness, neither
   fallback is required, validated, health-gated, nor dispatched. If any seat
   omits its harness, only the fallback needed by that seat is validated.
   Review execution mode and timeout remain route-level and always apply.

5. **Snapshots contain the effective decision once.** New implementation and
   review work orders snapshot the normalized harness definition, effective
   model (including an intentionally omitted harness-default model), timeout,
   execution mode, and per-seat effort. Compatibility fields may remain on the
   wire, but workers and health checks consume the normalized snapshot and do
   not make a second routing decision from legacy fields. Existing snapshots
   without the additive fields remain readable and use their historical path.

6. **Health follows actual requirements.** Control-plane stages never require
   a worker harness. Implementation requires its normalized harness and model
   policy. MCP review requires each effective seat harness, using the route
   fallback only for seats without an override; a fully explicit panel does
   not require a review-route harness. In-process review remains exempt. The
   disabled “Inherit single harness” UI and equivalent misleading controls are
   removed; validation explains when review fallback is unnecessary.

---

### 21.19 v1.19, amended by v1.20 — Provider-neutral reasoning effort (July 17, 2026)

The Phase 5.2 review-seat contract pins a model and optional harness, but
cannot independently express the reasoning effort expected from each seat.
Smuggling provider flags through `model_args` would violate §21.14 and tie
the workspace review contract to one vendor. This amendment refines §21.12
change 4, §21.13 change 2, and §21.14; all other v1.17 decisions remain
unchanged from v1.18:

1. **The review-seat contract is additive and vendor-neutral.** A seat is
   `{model, harness?, effort?}`. `model` remains required; `harness` and
   `effort` are optional. `effort`, when present, is exactly `low`,
   `medium`, or `high`. Harness selection remains explicit and is never
   inferred from a model name. Omitting `effort` preserves the selected
   harness's existing default and serialization omits the field.

2. **Harness adapters declare exact effort argv.** A harness registry entry
   gains optional `effort_args`, a map from the supported semantic values to
   literal argv arrays. These arrays contain no placeholders and are appended
   only when a seat requests that exact value. This is the adapter boundary:
   a Codex harness may map `high` to its `model_reasoning_effort` config argv,
   while a Claude harness may map it to `--effort high`; the review-seat
   schema remains provider-neutral. `command`, `model_args`, `effort_args`,
   and `probe_command` are always executed as argv arrays without a shell.

3. **Validation fails closed.** Unknown effort values, empty effort argv,
   placeholders inside `effort_args`, and a seat effort unsupported by its
   explicitly selected or inherited harness are workspace configuration
   errors. A configured value is never ignored, defaulted, negotiated, or
   translated by inspecting the model name.

4. **Effort is snapshotted and auditable.** Review dispatch snapshots the
   requested effort and the selected harness's effort argv together with the
   existing model/harness execution contract. Work-order and review audit
   payloads include `required_effort` when configured. Worker launches append
   the snapshotted adapter argv; hot reload cannot change an in-flight seat.

5. **The operator surface is first-class.** The Workspace UI exposes an
   optional per-seat effort selector with unset, low, medium, and high values,
   independently of model and harness. Operator documentation includes a
   two-seat `high` example for explicit Codex and Claude harnesses and states
   that unset effort preserves the harness default.

---

#### v1.20 implementation extension

Implementation execution gains the same provider-neutral reasoning-effort
control as review seats without weakening the contextual settings or immutable
dispatch contracts introduced by §§21.18–21.19:

1. **Implementation effort is optional and semantic.**
   `execution_settings.implementation.effort` is omitted for the selected
   harness default or is exactly `low`, `medium`, or `high`. The deprecated
   compatibility route may mirror this value, but contextual implementation
   settings remain authoritative. Effort is never inferred from a model name,
   model policy, provider, or symbolic model value.

2. **The selected harness validates the value.** An explicit implementation
   effort is accepted only when the selected harness declares a non-empty
   literal argv array for that value in `effort_args`. Unknown or unsupported
   values fail as implementation-field validation errors. Workspace settings
   expose semantic effort only; raw argv remains an adapter concern.

3. **Dispatch snapshots the exact launch contract.** An implementation work
   order durably captures both the requested semantic effort and a copy of the
   exact adapter argv resolved at dispatch. The work-order and associated audit
   event omit both fields when effort is unset. Harness or workspace hot reload
   may affect later dispatches but cannot alter an in-flight snapshot.

4. **Workers execute the snapshot without reinterpretation.** Implementation
   launch appends only the captured effort argv to the existing shell-free
   command vector. Workers do not recompute implementation effort from live
   configuration or model data. Unset effort appends nothing and preserves the
   historical command and serialization behavior.

5. **The operator surface is first-class.** The Workspace Implementation
   section offers Harness default, Low, Medium, and High beside the existing
   harness/model controls. Documentation explains harness-declared support,
   provider-neutral semantics, and the omission behavior of Harness default.
Review-seat effort behavior remains unchanged.

---

### 21.20 v1.21 — Explicit harness MCP transport formats (July 17, 2026)

Worker dogfooding found that §21.14's `{mcp_config}` placeholder did not define
the representation of its runtime value. Claude Code accepts a JSON file path,
while Codex CLI 0.142.0 treats `--config` as a TOML `key=value` override and
rejects that path before startup. This amendment refines §21.13 change 2 and
§21.14; all other v1.20 decisions remain unchanged:

1. **Transport format is explicit and vendor-neutral.** A harness registry
   entry gains `mcp_transport`, exactly `json_file` or `toml_override`.
   Omission on existing documents and snapshots normalizes to `json_file` for
   backward compatibility. Transport is snapshotted and included in the
   harness fingerprint so hot reload cannot change an in-flight launch.

2. **`{mcp_config}` remains one whole argv element.** For `json_file`, it
   expands to the existing mode-0600 temporary JSON file and the worker removes
   its containing directory after the child exits. For `toml_override`, it
   expands to one TOML `key=value` string defining the Conveyor MCP server.
   Command, model, effort, and probe argv retain the §21.14 and §21.19 rules;
   there is no shell evaluation or repository-owned harness wrapper.

3. **Credentials do not enter TOML argv.** The TOML value names
   `CONVEYOR_API_TOKEN` as the bearer-token environment variable; the worker
   already supplies the scoped credential in that child-only environment.
   The token value is never serialized into argv, config documents, snapshots,
   events, logs, or transcripts. JSON-file transport retains its existing
   credential-bearing temporary file lifecycle.

4. **Validation and operator guidance fail closed.** Unknown transports are
   rejected at workspace-config write time. The Workspace UI creates a Codex
   template with `toml_override` and `--config {mcp_config}`. Examples explain
   that Claude-style `--mcp-config` commands use `json_file`. Regression
   coverage validates both formats and, when Codex is installed, invokes its
   real config parser without starting an agent turn.

---

### 21.21 v1.22 — Worker attempt recovery and bounded child retry (July 17, 2026)

Operational review of supervised children found that an immediate harness
exit could repeatedly claim and release one order, while lease-expiry cleanup
could retain stale worker ownership, a running job projection, and an execution
window that no longer belonged to a live attempt. This amendment supersedes
§21.9 change 2 and §21.13 change 4 only for released or expired execution
attempts; all other v1.21 decisions remain unchanged:

1. **Execution clocks belong to one attempt.** A claim starts one fixed
   execution window. Renewal cannot extend it. Release, cancellation, or lease
   expiry ends that attempt, clears every active ownership and execution-clock
   field, and returns the job projection to pending. The append-only claim,
   release, failure, and expiry events retain attempt history. A later eligible
   claim starts a fresh execution window; an old deadline is never revived.

2. **Child retry is durable and bounded.** A child failure records its message,
   available exit status, time, automatic-retry count, next eligibility, and
   suppression state on the work order. The first three automatic retries wait
   1, 2, and 4 seconds (subject to a configured maximum no lower than the
   initial delay). Failure after those retries leaves the queued order
   suppressed from automatic claim. Cancellation records its distinct outcome
   and requires recovery without consuming the child-failure retry allowance.

3. **Queue eligibility is independent.** A queued order is claimable only when
   it is not suppressed and its durable retry time has arrived. The original
   queue-retention clock, the fixed execution clock of a live attempt, and its
   claim lease remain separate. Listing and worker dispatch expose the retry
   state rather than relying on a process-local timer.

4. **Cleanup is atomic and stale-safe.** Release and lease expiry clear worker,
   session, token, claimant, agent/model, lease, and active execution fields in
   the same store transaction that resets the job projection. Conditional
   ownership checks prevent a stale worker, refresh, or cancellation from
   overwriting a newer claim. Memory and Postgres stores expose the same
   lifecycle.

5. **Recovery is explicit, scoped, and idempotent.** An authenticated operator
   may recover a queued, unclaimed released/expired/suppressed order through the
   workspace-scoped work-order recovery API and UI. A request identity is
   required and durably deduplicated. Recovery clears backoff/suppression,
   refreshes queue eligibility, records actor/workspace/target/prior outcome
   and request identity in the audit stream, and attaches no worker. Active,
   completed, cross-workspace, and otherwise incompatible targets fail closed.

---

### 21.22 v1.23 — Portable review publication and headless-worker reliability (July 18, 2026)

Dogfooding exposed three avoidable self-hosting failures: GitHub Check Run
writes require every deployment to provision a GitHub App; a headless Claude
child can discover the Conveyor MCP server but stop for interactive tool
approval; and a timeout observed after its deadline can inflate the displayed
job duration. This amendment refines §21.12 change 5, §21.13, and §21.21 while
leaving their durable internal review and retry decisions unchanged:

1. **Portable GitHub publication is the default.** The factory publishes one
   aggregate `Conveyor / Code review` commit status for the reviewed SHA plus
   the existing deterministic PR comment. The status is `pending` until the
   durable review-round aggregate exists, then `success` for unanimous approval
   or `failure` for requested changes. A retry first compares the latest status
   for that context and does not duplicate an identical projection. Historical
   Check Run identifiers remain readable, but new default publications require
   no GitHub App. A future optional Check Run projection may be added without
   making it a prerequisite for self-hosted operation.

2. **Internal state remains authoritative.** Commit-status or comment failure
   cannot roll back a recorded verdict, bounce, or round aggregate. Publication
   retries rebuild both projections from the durable event stream. A
   fine-grained user token with commit-status and PR-comment permissions is a
   valid default credential for an open-source deployment.

3. **Headless harness permissions are launch data.** A non-interactive harness
   command must pre-authorize the exact Conveyor MCP lifecycle tools it needs;
   it must never depend on an operator answering a child-process permission
   prompt. Provider-specific permission argv stays in the shell-free harness
   command and is snapshotted with that command. The shipped Claude example
   allows only the scoped `mcp__conveyor__*` server instead of bypassing all
   permission checks.

4. **Deadline time and observation time remain distinct.** When an execution
   timeout is observed after its fixed deadline, the append-only transition is
   still recorded at observation time, while the failed job's `ended_at` and
   the timeout timeline marker use `execution_deadline`. UI duration therefore
   reports the contracted execution window rather than control-plane polling or
   downtime latency.

---

### 21.23 v1.24 — Audited recovery for terminal review rounds (July 18, 2026)

A review panel can become permanently non-aggregatable when one immutable seat
work order reaches terminal `timed_out`. Individual work-order redispatch and
worker-attempt recovery do not apply to that state, while replacing a seat in
the same round would violate the round/seat uniqueness and history contract.
This amendment refines §§21.12, 21.13, 21.21, and 21.22 while leaving their
normal dispatch and aggregation behavior unchanged:

1. **Recovery retries the full panel as a new round.** An authenticated
   operator may retry only when the latest review round contains a terminal
   timed-out seat and no review round is active. The prior round, work orders,
   verdicts, configuration snapshots, timeouts, and failures remain immutable.
   Seat-only replacement and same-round attempt generations are not introduced.

2. **The current implementation handoff is reverified.** Before mutation,
   Conveyor resolves the current pull-request head and requires it to match the
   last verified implementation handoff. A missing or changed head, a task
   routed back to implementation, an active round, or any other incompatible
   state fails closed without creating jobs or work orders.

3. **Current configuration is snapshotted.** The next monotonically increasing
   review round contains one queued work order for every seat in the current
   workspace review panel. Each order snapshots the current model, harness,
   effort, MCP transport, argv, and probe contract. No execution field is
   copied from the timed-out seat.

4. **The transition is scoped, atomic, and idempotent.** The operator supplies
   a non-empty reason and a request ID bound to that workspace. Repeating the
   exact request returns its original round; reusing the ID for another
   workspace or any other different inputs is rejected. Task and request
   serialization prevent concurrent requests from
   creating duplicate active rounds or seat work orders. Memory and PostgreSQL
   stores implement equivalent validation and transaction outcomes.

5. **Recovery is visible and auditable.** Activity marks the stalled task as
   needing operator attention and exposes the prior round, timed-out seats,
   deadlines, and failure context. The dashboard labels the action **Retry
   review round**, requires the operator reason, shows the new active round on
   success, and retains historical results. The audit event records workspace,
   task, actor, request ID, reason, verified PR head, prior round, new round,
   and affected timed-out work orders.

6. **Aggregation remains round-local.** Verdict and quorum evaluation uses
   only work orders and completed-review events from the new round. Historical
   approvals or requested changes remain visible but never satisfy a new seat
   or change the new round's aggregate.

---

### 21.24 v1.25 — AI-generated task titles (July 18, 2026)

Requiring operators to invent a title before submitting the richer task
description duplicates work the control-plane model can perform consistently.
This amendment refines §§17.3–17.4 and §21.5 only for title intake; all other
task metadata, validation, idempotency, attachment, and pipeline behavior
remains unchanged:

1. **The dashboard no longer asks for a title.** The New task flow removes the
   title input and submits the description as the source of task intent. A
   non-empty description is required when title is omitted, while repository,
   base branch, attachments, execution mode, and approval-gate behavior remain
   unchanged.

2. **Generation happens at the trusted creation boundary.** Before a missing
   or whitespace-only title can be persisted, `conveyord` invokes the existing
   in-process AI integration with the configured triage model and timeout. The
   model receives the submitted description plus repository/source context and
   must return one non-empty plain-text line no longer than 200 characters.
   Provider failure, timeout, missing configuration, or invalid output fails
   task creation; Conveyor never reports success for a persisted untitled task
   and does not invent a fallback title.

3. **Explicit-title compatibility remains narrow and deterministic.** REST,
   CLI, and MCP callers may still supply a non-empty title; existing trimming,
   length validation, and persistence behavior applies and AI does not replace
   it. CLI `task new` and MCP `create_task` therefore make title optional rather
   than removing compatibility. Exact idempotent retries of title-less MCP
   intake return the already persisted generated title without invoking
   generation again.

4. **The normal durable pipeline remains authoritative.** Generated titles
   enter the same task record, branch assignment, attachment finalization,
   triage enqueue, task lists, and detail views as explicit titles. Title
   generation is an intake operation, not a parallel triage implementation;
   the existing triage and spec stages still run unchanged after creation.

---

### 21.25 v1.26 — Generated-only title intake (July 18, 2026)

The first review of §21.24 found that retaining a manual title input duplicated
the same decision across less-visible REST, CLI, and MCP surfaces after the
dashboard removed it. This amendment supersedes §21.24 change 3 and narrows
title intake consistently; the persisted task, pipeline, and downstream title
contract remain unchanged:

1. **Title is not task-creation input.** The dashboard has no title control,
   CLI `task new` accepts no title argument, and REST and MCP reject a supplied
   `title` field. `body` is required on every creation surface and remains the
   source of task intent. Repository, base branch, source, attachments,
   execution mode, approval gates, and workspace-scoped idempotency retain
   their existing behavior.

2. **Every new title is generated before persistence.** The trusted creation
   boundary always invokes the §21.24 generator and validates its output before
   creating a task. Provider or validation failure fails closed; the durable
   `tasks.title` field remains required and downstream consumers never observe
   an empty title.

3. **Retries do not regenerate.** Exact idempotent retries compare the
   caller-controlled creation fields, return the already persisted task and
   generated title, and do not invoke AI again. A changed body or any other
   caller-controlled input with the same key remains a conflict.

---

### 21.26 v1.27 — Reconnect-safe workers and interrupted review-seat recovery (July 18, 2026)

Operational evidence showed that a short control-plane restart terminated the
foreground worker, while a host scheduling gap could make child authority
ambiguous. Review seats whose claims expired were left queued but suppressed,
which is different from the terminal timed-out panel handled by §21.23. This
amendment refines §§21.13, 21.21, and 21.23 without weakening the fail-closed
lease boundary:

1. **Idle workers reconnect.** Configuration, heartbeat, and polling transport
   failures plus retryable 5xx responses use interruptible bounded exponential
   backoff with jitter. Saved enrollment credentials are reused. Revoked or
   invalid credentials, non-retryable responses, and structurally invalid
   configuration remain terminal with actionable errors.
2. **Active renewal uses the known lease.** A transient renewal failure is
   retried only before the current lease's safety margin. A successful renewal
   retains the same child and attempt. A terminal response, scheduler gap, or
   unresolved request at the safety boundary stops the child and permanently
   abandons local authority; neither reconnect nor best-effort release grants
   it back.
3. **Reconciliation is server-authoritative.** After uncertain authority the
   worker stops the child, then reads the durable claim state through the
   worker-authenticated reconciliation operation. Expired sessions remain
   rejected for renewal, release, implementation submission, and review
   verdict submission.
4. **Interrupted seats recover in place.** A workspace-authorized operation
   identifies the latest review round and atomically requeues only incomplete,
   unclaimed, retry-suppressed seats. Completed work orders and their verdicts
   are retained. This does not replace §21.23's full new-round retry for a
   terminal `timed_out` panel.
5. **Round recovery is idempotent and audited.** A workspace-wide request ID
   binds the task and round. Exact retries return the original result;
   divergent or concurrent conflicting requests fail closed. Round and
   seat-level events record actor, workspace, request, prior/resulting state,
   recovered seats, and retained completed seats in memory and PostgreSQL.
6. **Operators see serviceability.** Task and workspace views report required
   harness health, latest heartbeat/disconnection context, and whether queued
   work never started or was interrupted. Exactly one **Recover interrupted
   review round** action appears when eligible seats exist and no conflicting
   active attempt does.
7. **Sleep prevention remains optional.** Durable service-manager operation is
   documented. macOS `caffeinate` may be used intentionally, but Conveyor does
   not force wakefulness and correctness never depends on a platform sleep API.

---

### 21.27 v1.28 — Execution setups (July 19, 2026)

§21.18 made execution settings contextual but singleton: one implementation
contract and one adversarial panel per workspace. Operating the factory
across dissimilar task classes (UI work, backend migrations, documentation)
produces exactly the recurring override clusters §2.2 reserved as the
trigger for productizing pre-bundled override sets. This amendment pulls
that trigger for **execution settings only**: a **setup** is a named,
selectable configuration of the §21.18 contextual surface plus its
adversarial panel — which harnesses and models implement, review, and run
the control plane for a task. The term is the manufacturing one: a setup
is the line configured for a particular job family — tooling and settings
staged before the run begins — which is exactly this, the workspace staged
for one class of task. It also avoids colliding with *pack* (§2.2), which
already owns prompt/policy content; pack-content variants remain a single
default pack, and the §2.2 deferral stands for everything that is not an
execution setting. This amendment refines §21.13 changes 1, 3, and 7 and
§21.18; all other v1.27 decisions remain unchanged. Seven changes:

1. **A setup is a named contextual execution contract.** The workspace
   document gains `setups`, a non-empty list of entries
   `{name, execution_settings, review}`, and `default_setup`, which must
   name one of them. `name` is unique and path-safe. Each setup carries
   the full §21.18 contextual surface — control-plane model/timeout
   settings, implementation harness/model-policy/effort/timeout, review
   execution/timeout and fallbacks — plus its own §21.12/§21.19 panel
   seats. The harness registry stays workspace-level and shared; each
   setup normalizes and validates independently under the unchanged
   §21.14 and §21.18–§21.20 rules, and referential integrity extends
   across setups: a registry entry referenced by any setup's
   implementation route or panel seat cannot be deleted.

2. **A single-setup document is the v1.27 document.** A stored document
   with top-level `execution_settings`/`review` and no setup list
   normalizes into one setup named `default`, which becomes the default
   setup; behavior is identical to v1.27. During the deprecation period
   the top-level fields remain readable and are emitted as a projection
   of the default setup for older REST/CLI consumers (the §21.18
   change 2 pattern). When both shapes are present, the setup list is
   authoritative and legacy fields cannot override it.

3. **Setup selection is per-task, at setup granularity only.** REST and
   MCP `create_task` gain an optional `setup` field referencing a setup
   by name; unset resolves to the workspace default. This refines §21.13
   change 1: "no per-task harness override" becomes "no per-task
   free-form override" — a task selects among operator-defined setups
   and never names a harness, model, or effort directly. An unknown
   setup name is an intake error (HTTP 400 / MCP error), never a silent
   fallback to the default.

4. **Setup content freezes at intake.** The effective setup is resolved,
   normalized, and persisted **by value** with the task, extending
   §21.13 change 7's intake-time-resolution rule from mode and gates to
   the full execution contract. Later setup edits, renames, or deletions
   never change an in-flight task. Because tasks carry the frozen value,
   deleting a setup is always safe for history and in-flight work; the
   workspace must simply retain at least one setup and a valid default
   at all times.

5. **Dispatch and snapshots are unchanged in mechanism.** Implementation
   and review work orders continue to snapshot the normalized harness
   definition, effective model, timeout, execution mode, and per-seat
   effort (§21.18 change 5) — sourced from the task's frozen setup
   instead of the workspace singleton. The §17.4 agent-facing lifecycle,
   claim-time validation, self-review guard, and the
   `model_enforcement`/harness enforcement labels (§21.12 change 4) are
   untouched — no second protocol.

6. **Health gating and Auto availability are setup-scoped.** Refines
   §21.13 change 3: Auto is offered for a task only while an enrolled
   worker holds a live lease and every harness required by **that task's
   frozen setup** — the implementation harness plus each effective seat
   harness, with `in_process` review exempt — probes healthy. An
   unrelated setup's broken harness must not disable Auto for tasks that
   do not require it. Serviceability reporting (§21.26 change 6) becomes
   per-setup, and an intake-time auto→manual fallback event records
   which setup's requirements failed.

7. **The operator surface is first-class.** The Workspace UI manages
   setups — create, duplicate, edit, set default, delete — rendering the
   §21.18 contextual layout per setup. Task intake (UI, CLI, REST, MCP)
   exposes a setup selector defaulting to the workspace default, with
   the setup's composition available as secondary detail rather than
   inline jargon. Task detail surfaces the frozen setup name and
   composition. The working breakdown slots into docs/phase5-plan.md;
   this section is authoritative.

---

### 21.28 v1.29 — Remove in-process USD cost accounting (July 19, 2026)

The in-process OpenAI path previously converted provider-reported token usage
into USD through a hardcoded model-price catalog. A newly configured model
could complete a paid Responses generation and then fail because the local
catalog did not recognize it, discarding the output and disabling other
control-plane actions such as task-title generation. The USD budget breaker
that motivated fail-closed pricing was removed by §21.6, so this amendment
removes the obsolete accounting boundary without changing worker reporting:

1. **In-process execution has no USD cost accounting.** Conveyor does not
   maintain an in-process token-price catalog, estimate an in-process USD
   value, or check a model against local pricing before or after generation.
   Any model accepted by workspace configuration can run in-process without a
   separate pricing allowlist. Pricing availability can never fail triage,
   specification, in-process review fallback, task-title generation, job
   persistence, or any other in-process control-plane action.

2. **Provider token usage remains telemetry.** Successful OpenAI Responses
   generations continue recording provider-reported `tokens_in` and
   `tokens_out`. In-process result propagation carries those counts and model
   output, but carries no `CostUSD` value.

3. **In-process job cost is absent.** Jobs whose execution runner is
   `in-process` persist `cost_usd` as `NULL`; REST and CLI representations omit
   it or expose it as null, and UI surfaces suppress USD cost while continuing
   to display token counts. They must not substitute `$0.00`, another numeric
   zero, or an unknown-estimate label that implies a measured charge.

4. **Worker-plane reporting is unchanged.** External worker jobs continue to
   accept cumulative token and USD telemetry through `report_usage`, persist
   an explicit worker-reported `cost_usd`, and display it where they do today.
   Shared job APIs distinguish the optional cost by the existing runner field;
   a missing amount does not infer the execution plane.

5. **There is no replacement breaker.** Conveyor has no in-process USD budget
   breaker, spend ceiling, admission check, network price lookup, or historical
   cost backfill. Section 21.6 remains authoritative: cost telemetry does not
   control whether a job may start or complete.

---

### 21.29 v1.30 — Grok Build CLI harness and environment-backed MCP attachment (July 19, 2026)

§21.14 introduced pluggable harness definitions and §21.20 made generated
per-run MCP transport explicit. Both accepted transports materialize a
per-launch configuration representation and therefore require exactly one
`{mcp_config}` argv placeholder. Grok Build's direct headless CLI instead
discovers MCP registrations from its normal user or project configuration and
has no per-run MCP-config argv option. This amendment refines §§21.14 and
21.20 by reference; their historical text and all other v1.29 decisions remain
unchanged. Nine changes:

1. **Environment attachment is a third, vendor-neutral transport.**
   `mcp_transport: environment` identifies a harness whose operator-owned,
   non-secret MCP registration resolves the runtime Conveyor URL and
   authorization header from the launched child's environment. The harness
   definition also carries a normalized, non-secret `mcp_attachment` identity
   naming the intended registration. It is not a general environment-template
   or arbitrary vendor-configuration field: it cannot contain credentials,
   URLs, command fragments, opaque configuration, or arbitrary variable
   references. Command, model, effort, transport, and attachment identity
   remain separate settings.

2. **Placeholder validation is transport-aware.** Every command contains
   exactly one whole-element `{prompt}`. `json_file` and `toml_override`
   continue to require exactly one whole-element `{mcp_config}`;
   `environment` requires zero and rejects any occurrence. Environment
   launches create no dummy MCP file and pass no meaningless path. Legacy
   documents omitting `mcp_transport` retain `json_file` behavior. The same
   rules apply during load, update, hot reload, snapshotting, fingerprinting,
   REST/CLI serialization, UI editing, and launch preparation.

3. **The initial first-class environment harness is Grok Build.** Its
   shell-free command is `grok --single {prompt} --permission-mode
   bypassPermissions --no-plan`; model argv is `--model {model}`; and low,
   medium, and high effort map to `--reasoning-effort <level>`. Verified Grok
   0.2.103 behavior interpolates `${CONVEYOR_ADDR}` in an HTTP registration URL
   and `${CONVEYOR_API_TOKEN}` in its authorization header at connection time.
   `grok mcp doctor` performs a real handshake without starting a model turn.

4. **Persistent registration remains operator-owned and non-secret.** The
   operator authors the intended registration directly in
   `~/.grok/config.toml` or the applicable project scope. Conveyor never
   creates, rewrites, deletes, or repairs Grok, Claude-compatible, user, or
   project MCP configuration. `grok mcp add` is not the setup mechanism because
   it rejects variable-based URLs at write time. Literal Conveyor credentials
   and token-bearing endpoints are forbidden in persistent registration.

5. **Readiness fails closed before every implementation or review model
   turn.** Using the same effective configuration context and isolated child
   environment as the eventual launch, Conveyor must establish non-empty
   runtime `CONVEYOR_ADDR`, `CONVEYOR_API_TOKEN`, session ID, and client token;
   positively identify the normalized intended registration; verify that its
   URL and authorization header use exactly the fixed environment references;
   and run Grok's real no-model-turn doctor/inspection path to prove the
   intended registration handshakes. Binary presence, `grok --version`, process
   startup, a model turn, or an unrelated successful MCP connection is never
   sufficient. Missing, stale, malformed, ambiguous, unrelated, literal-token,
   or token-bearing registrations are unusable. This includes a successful
   Claude-compatible registration discovered through `~/.claude.json`.

6. **The existing credential model is unchanged.** The workspace-scoped
   worker enrollment credential remains `CONVEYOR_API_TOKEN` in the child
   environment; per-work-order freshness and identity remain the claim's
   `sessionID` and `clientToken`. None may appear in argv, workspace documents,
   generated files for `environment`, harness definitions, snapshots,
   fingerprints, REST/CLI records, events, logs, diagnostics, transcripts, or
   persistent Grok configuration. Diagnostics expose only bounded, safe status.
   Existing output redaction remains mandatory.

7. **Snapshots preserve behavior, not runtime state.** Immutable harness
   snapshots and fingerprints include `mcp_transport` and normalized
   `mcp_attachment`, so queued work retains its attachment contract across hot
   reload. They exclude addresses, tokens, sessions, client tokens, and other
   launch-time values. Concurrent launches use independent child environments
   and claim state. Completion, launch failure, cancellation, and process-start
   failure discard Conveyor-owned child state without altering operator-owned
   configuration.

8. **Every operator surface carries the same durable contract.** Workspace
   REST and CLI types, configuration files, examples, and UI expose transport
   and non-secret attachment identity separately from argv, model, and effort.
   They explain the transport-specific placeholder rules, reject durable
   credential material, and surface readiness failures without echoing runtime
   environment values or inspected configuration. Operator documentation shows
   direct Grok configuration using `${CONVEYOR_ADDR}` and
   `${CONVEYOR_API_TOKEN}`, explains why `grok mcp add` is unsuitable, and says
   missing, stale, or token-bearing registration blocks launch until the
   operator repairs it without copying a credential into configuration.

9. **Verification is deterministic and includes the real parser when
   available.** Unit and integration coverage exercises validation, immutable
   snapshots/fingerprints, hot reload, implementation and independent-review
   launches, concurrency, missing variables, registration failures, cleanup,
   legacy documents, and UI/API round trips without requiring Grok. When Grok
   is installed, focused coverage runs its real no-model-turn doctor/inspection
   path and clearly skips when unavailable; ordinary build, test, and vet gates
   never require Grok.

---

### 21.30 v1.31 — Merge readiness, conflict-fix dispatch, and refresh review (July 20, 2026)

Operational evidence (kidus-tiliksew/conveyor#81, July 19–20, 2026): the
human gate offered **Merge pull request** on an approved task while GitHub
reported the pull request `UNKNOWN` and later `CONFLICTING`, so the
operator's click converted an unverified promise into a `merge.failed`
warning — twice — and recovering required branch surgery outside the
pipeline. GitHub computes mergeability asynchronously, so a first read
after any push is routinely `UNKNOWN`; treating that as failure is noise,
and treating `CONFLICTING` as an operator problem contradicts the factory
premise that mechanical work is pipeline work. This amendment refines the
§13.2/§21.11 gate card, §21.13 change 7 (automatic merge), and §21.27
setups; the §17.4 agent-facing lifecycle, claim-time validation, and
self-review guard are untouched — no second protocol. Six changes:

1. **Merge readiness is read before merge is offered.** For a task at the
   merge gate, the control plane resolves the pull request head and
   mergeability before rendering the primary action. `MERGEABLE` renders
   the existing ready card; `UNKNOWN` renders a pending state and re-reads
   with bounded backoff — it is never surfaced as an error or recorded as
   a merge failure; `CONFLICTING` renders a blocked card whose primary
   action is change 3's fix. The readiness read is advisory: the
   authoritative pre-merge read inside the merge operation (§21.11 era
   behavior) is unchanged, and a click that races a fresh conflict still
   fails closed there.

2. **Approval binds to the reviewed head.** Recording an approve
   intervention persists the pull-request head SHA it approved. The merge
   operation requires the current head to equal the approved head; any
   mismatch — conflict fix, stray push, anything — fails closed, marks
   the approval stale, and routes the task through changes 3–5 rather
   than merging content no round approved. Memory and PostgreSQL stores
   implement equivalent validation.

3. **Conflict fixes are dispatched, not performed by operators.** The
   blocked card's **Fix merge conflict** action records a
   system-generated redirect to the implement stage with reason code
   `merge-conflict` and creates one implement work order instructing the
   agent to: check out the task worktree (§21.8), **merge the base branch
   into the task branch** — history rewrites and force-pushes of the
   assigned branch are forbidden (§21.7) — resolve conflicts, pass the
   task's validation, push, and `submit_for_review`. The order carries
   the task's frozen setup (§21.27) and dispatches under the task's
   recorded mode. In Auto with the merge gate off, the control plane
   enqueues the same fix itself when readiness reports `CONFLICTING`.
   The action is idempotent: while a conflict-fix order is active, repeat
   requests return it rather than creating a duplicate.

4. **Refresh review scope is a setup decision.** Each setup gains
   `refresh_review: delta | full | none` (default `delta`), frozen at
   intake with the rest of the setup (§21.27 change 4). When a stale
   approval's task returns through `submit_for_review`, the next round's
   scope is: `delta` — review the changes since the last approved head
   (each seat order carries the approved baseline SHA and the new head);
   `full` — review the entire diff against base; `none` — a clean update
   whose merge introduced **no manually resolved hunks** skips the
   refresh round and re-arms the gate directly. A fix that resolved
   conflict hunks always receives at least a `delta` round regardless of
   setting — authored resolution content is never merged unreviewed.

5. **Round mechanics are unchanged.** A refresh round is the next
   monotonically increasing round, snapshots the frozen setup's panel
   exactly as §21.18/§21.27 dispatch does, labels its orders as a refresh
   with the baseline SHA, and aggregates round-locally (§21.23 change 6).
   Seat uniqueness, quorum rules, and verdict publication are untouched.

6. **The gate re-arms visibly.** After a refresh round approves, the
   merge gate returns to the ready card via a fresh readiness read (gate
   on) or invokes the existing automatic merge machinery (gate off,
   §21.13 change 7). The timeline renders the blocked state, the fix
   dispatch, and the refresh round as ordinary events with their reason
   codes; audit events for staleness, fix dispatch, and refresh rounds
   record workspace, task, actor or `system`, approved head, and new
   head. The working breakdown slots into docs/phase5-plan.md; this
   section is authoritative.

---

### 21.31 v1.32 — Remove execution modes; per-task hold (July 20, 2026)

Operating experience with the §21.12 two-axis model shows the mode axis
carries no decision weight: human oversight is fully expressed by the gate
toggles, the queue already admits any authenticated agent regardless of
mode, and Manual's only concrete effect is preventing the worker daemon
from claiming — a reservation, not a mode. For that one effect the
workspace carries a default-mode setting, an intake selector across
UI/CLI/MCP, per-setup Auto gating with fallback events, and a task chip.
This amendment removes the execution mode and keeps the reservation as a
per-task hold. Gate semantics, the §17.4 lifecycle, claim-time validation,
and the self-review guard are untouched. Six changes:

1. **Execution mode is removed (supersedes §21.12 change 2's mode axis;
   the gate toggles it introduced stand).** Tasks no longer carry an
   execution mode. There is one queue: any authenticated agent may claim
   any order; an enrolled worker may claim any order whose task's frozen
   setup it can serve (§21.27), except held orders (change 2). The
   workspace default-mode setting, the intake mode selector (UI, CLI
   `--mode`, MCP `create_task` mode), and the task mode chip are removed.

2. **A per-task hold replaces Manual's reservation.** A task carries an
   optional boolean **hold** (default off), settable at intake (UI, CLI
   `--hold`, MCP `create_task`) and toggleable on a live task through the
   authenticated API with `task.hold.set` / `task.hold.cleared` audit
   events. While held, workers never claim the task's work orders —
   enforced server-side at claim time, in the same layer as the
   self-review guard — while operator-attached agents may claim them
   explicitly. Hold governs claiming only: it does not pause in-flight
   sessions, and system-dispatched orders for a held task (e.g. §21.30
   change 3 conflict fixes) inherit the hold.

3. **Serviceability becomes advisory (supersedes §21.13 change 3's
   gating; its reporting stands).** The liveness lease, per-harness
   probes, and per-setup serviceability (§21.26 change 6, §21.27
   change 6) continue to be computed and surfaced. Intake warns — and
   the task view explains — when no enrolled worker can serve the
   selected setup, but nothing is rejected or resolved to another mode
   at intake; auto→manual fallback events are removed. Orders queue
   openly with the reason visible, and `work_order_queue_timeout`
   remains the stall backstop, so nothing queues silently against a
   dead worker.

4. **Automatic merge keys on the gate alone (refines §21.13 change 7,
   §21.30 changes 3 and 6).** The gate truth table stands. Every
   "Auto with the merge gate off" condition reduces to "merge gate
   off": the control plane auto-merges an approved review with green
   checks, and enqueues conflict fixes itself, regardless of hold —
   merges are control-plane work, and hold governs worker claiming
   only. §21.30 change 3's "dispatches under the task's recorded mode"
   becomes ordinary dispatch with hold inheritance per change 2.

5. **Hold is exempt from intake freezing (refines §21.13 change 7).**
   Gates continue to resolve and persist at intake, immutable
   thereafter. Hold is deliberately a live, mutable task property —
   its purpose is handing a task to or reclaiming it from the worker
   mid-flight — and every toggle is audited per change 2.

6. **Legacy and compatibility.** Existing task records keep their
   recorded modes as history, exactly as they keep pre-§21.12
   escalation levels. At migration, non-terminal Manual tasks receive
   `hold=true` with a migration audit event. REST/CLI/MCP accept `mode`
   through a v1.33 deprecation window, mapping `manual` to `hold=true`
   and `auto` to a no-op, recording deprecated usage. The UI shows a
   **Held** chip only when hold is set. Phase 7 graduation, when it
   arrives, operates on gate toggles and hold usage instead of mode
   defaults. The working breakdown slots into docs/phase5-plan.md; this
   section is authoritative.

---

### 21.32 v1.33 — Queue re-entry re-resolves harness snapshots (July 20, 2026)

Dogfooding found a recovery dead end in the §21.19 snapshot contract. A work
order pins its harness snapshot at dispatch; the worker executes it verbatim
(§21.19 changes 3–4). When that pinned command itself is the failure — on
July 20, 2026 three implement orders relaunched a `claude -p` argv that
lacked a permission-bypass flag, failing identically on every automatic
retry — no supported recovery exists: release-requeue, operator recovery
(§21.21), and stale redispatch (§21.9 change 6) all preserve the broken
snapshot, so a corrected harness registry can never reach an already-queued
order and the repair was a manual database edit. This amendment scopes
snapshot immutability to the active attempt and refines §21.9 change 6,
§21.19 changes 3–4, and §21.21; all other v1.32 decisions remain unchanged.
Four changes:

1. **Immutability is scoped to the active attempt.** A claimed or executing
   order's snapshot never changes: hot reload cannot alter an in-flight
   launch, exactly as §21.19 change 3 states. "In-flight" ends when the
   attempt ends; a queued order awaiting its next claim is not in flight.

2. **Queue re-entry re-resolves the snapshot.** Whenever an order re-enters
   the queue — worker release including automatic retry, operator recovery
   of a released/expired/retry-suppressed order, or redispatch of a stale
   order — the server re-resolves the pinned snapshot from the current
   harness registry, provided a same-name harness exists and, when the
   order pins an effort, that definition still declares argv for it.
   Otherwise the prior snapshot is retained unchanged — refresh never
   fails an order. Only the harness definition refreshes: the pinned
   harness name, model, effort, and review-seat assignments are preserved.

3. **Refresh is audited.** A material refresh appends a
   `work_order.harness_refreshed` event carrying the prior and new command;
   an identical resolution appends nothing. Refresh is server-side and
   durable before any subsequent claim.

4. **Workers are unaffected.** Workers continue to execute snapshots without
   reinterpretation (§21.19 change 4); no worker, protocol, or claim-path
   change is involved.

---

### 21.33 v1.34 — Grounded specs: spec delegation over MCP, triage briefs, non-normative diagrams (July 21, 2026)

Operating experience with §21.4 change 2's in-process spec stage shows the
blind-spec bet failing exactly where the pipeline leans on it. The spec stage
is a single tool-less vendor-API call: everything it will ever know about the
task arrives in its prompt, so its role prompt must coach it to avoid file
paths, invent nothing, and state codebase assumptions for the human gate to
catch. With the §21.12 spec gate off, nothing catches them — the blind spec
auto-approves and becomes the binding contract whose Non-goals code review
enforces verbatim, so a wrong assumption is not corrected but *enforced*.
Acceptance criteria are capped the same way: an agent that cannot see the
repository cannot anchor a `verify: test` criterion to a real file, so
criteria are only as concrete as the task body happened to be. Meanwhile
triage's contract has decayed underneath it: `automatability` has had no
consumer since §21.31 removed execution levels, and the `human` route is
honored only for pre-§21.12 legacy tasks. This amendment delegates the spec
stage to the §17.4 work-order lifecycle so the specifying agent reads the
repository before writing the contract, rewrites triage as the front-door
analyst that routes and frames, and sanctions simple non-normative diagrams
in spec prose. Eight changes; all other v1.33 decisions remain unchanged:

1. **Spec executes as an MCP work order (supersedes §21.4 change 2's fixed
   `in_process` spec routing; triage remains fixed in-process).** Spec joins
   implement and review as a stage type in the §17.4 leased work-order
   lifecycle. There is **no in-process fallback**, deliberately: a blind
   fallback would silently reintroduce the failure mode this amendment
   exists to remove. When no agent can serve a spec order it queues openly
   with the reason visible (§21.31 change 3), and `work_order_queue_timeout`
   remains the stall backstop. Hold (§21.31 change 2), leases, renewal, and
   stale recovery (§21.9), and snapshot pinning with queue re-entry refresh
   (§21.19, §21.32) apply to spec orders exactly as to implement orders.

2. **The spec work-order contract.** A *spec* order delivers the spec role
   prompt, the task context, the triage brief (change 5), artifact
   references, the repository and base branch, and — on gate-redirect
   regeneration — the declined revision plus every human-gate comment, the
   same feedback threading implement orders already receive. The specifying
   agent is expected to investigate the repository before writing; reading
   code is the point of the delegation. The order carries **no branch**: a
   spec run makes no edits, commits, or pushes, so branch assignment (§21.7)
   and the §21.8 worktree contract are untouched.

3. **`submit_spec` completes a spec order.** A new MCP tool accepts the same
   structured payload the in-process stage produced — `markdown`,
   `acceptance`, `decomposition` — and the §4/§4.1 validators are unchanged.
   A validation failure returns as the tool result so the agent corrects and
   resubmits in the same warm session; schema bounces stop being queue
   traffic. An accepted submission creates the next spec version and
   proceeds exactly as today: human gate when the spec gate is on,
   auto-approval when it is off, downstream GitHub projection unchanged. A
   gate redirect enqueues a regeneration spec order per change 2.
   `submit_spec` carries the submitter's self-reported agent and model,
   recorded on the spec version — the §21.4 change 3 independence-label
   pattern applied to spec provenance.

4. **No new claim constraint; spec→implement continuity is legitimate.** The
   self-review guard remains scoped to the implement/review pair of the same
   task. The session that authored a task's spec **may** claim its implement
   order: author-implementer continuity is a feature, not a conflict of
   interest, and provenance stays legible from the recorded identities. This
   is a decision, not an omission.

5. **Triage becomes route-and-frame (supersedes the §5.1 stage-1
   contract).** Triage remains the fixed in-process, always-on front door —
   intake must never depend on worker availability. Its output contract
   changes: `class` (bug/feature/chore) stays; `automatability` is
   **removed** — its consumer left with the §21.12/§21.31 level machinery;
   `route` keeps `{implement, spec, human, parked}` and **`human` is honored
   for every task**, transitioning to awaiting-input, restoring a live path
   that had decayed to legacy tasks only. New: triage produces a structured
   **brief** — the questions the spec must answer, suspected affected areas,
   and risks or ambiguities in the task body — recorded on
   `triage.completed` and delivered in the task's spec order, or in its
   implement order when triage routes straight to implementation. The brief
   is advisory and non-normative: it directs the downstream agent's
   investigation; no stage enforces it. Triage also proposes
   requirements-tree placement from the feature list supplied in its prompt,
   replacing substring matching as the source of `triage.feature_suggested`.

6. **Simple diagrams are sanctioned spec prose (§4.1 rule addition).**
   Non-`conveyor:*` fences are ordinary prose; Mermaid is the sanctioned
   notation for optional architecture and flow diagrams. Diagrams are
   **non-normative**: approval, review, and verification enforce prose
   sections, acceptance criteria, and Non-goals — never a picture. The
   dashboard renders Mermaid client-side best-effort, falling back to the
   fenced text on any render error, so a malformed diagram can never fail
   validation or break a gate view. GitHub renders Mermaid natively, so
   issue projection inherits diagrams for free. Role-prompt guidance: small
   (roughly fifteen nodes or fewer), and only where a diagram clarifies more
   than prose would.

7. **Configuration follows the execution move.**
   `execution_settings.control_plane` retains only triage. Spec gains a
   work-order execution context mirroring implementation —
   harness/model-policy resolution and timeout, served by the unchanged
   §21.13/§21.18/§21.19 machinery applied to a third stage type. Worker
   enrollment and per-setup serviceability extend to spec orders;
   serviceability remains advisory (§21.31 change 3). Normalization maps a
   stored `control_plane.spec` model/timeout onto the new context where
   unambiguous; legacy values never override contextual settings (§21.18
   change 2 pattern).

8. **Migration and compatibility.** Tasks queued for the spec stage at
   upgrade dispatch as spec work orders. An in-process spec call in flight
   at upgrade completes under the old contract and its result is accepted.
   Stored spec versions, gate history, and events are untouched. The working
   breakdown slots into docs/phase5-plan.md — spec delegation sequences
   after the Phase 5.1 worker (workers must claim and serve spec orders) and
   is independent of 5.2–5.4; this section is authoritative.

---

### 21.34 v1.35 — Task cancellation, setup re-freeze on recovery, stalled-task tray (July 21, 2026)

Dogfooding the §21.33 spec delegation on July 21, 2026 produced an
unrecoverable-but-immortal task. A task's frozen setup pinned a model its
harness cannot serve; every launch failed with the same provider rejection,
automatic retry was correctly suppressed (§21.21) — and then nothing more
was possible. Gate verdicts were refused because the task was not awaiting
(`reject` is a gate action, and the task was Running), operator recovery
would relaunch the same pinned model because snapshot refresh deliberately
preserves it (§21.32 change 2), and the corrected workspace configuration
could never reach the task because its setup contract froze at intake
(§21.13 change 7). The repair was a manual database edit — the second
§21.32-shaped dead end, one level up: config fixed, frozen state unable to
receive the fix, no supported exit. Separately, the task looked "Running"
in every list while dead; it surfaced in no inbox. This amendment adds a
lifecycle kill switch, makes operator recovery able to carry config fixes,
and makes dead tasks findable. Five changes; all other v1.34 decisions
remain unchanged:

1. **Cancel is a first-class lifecycle action, valid from any non-terminal
   state.** A new `cancel` intervention closes a task regardless of stage
   or gate state: UI action on the task header, CLI `conveyor task close
   <id>`, and REST, each requiring a reason and recording an audited
   intervention with the actor. Cancelling a terminal task is a no-op
   error. Cancel is deliberately **not** exposed as an MCP tool: agents
   file tasks (§21.5); they do not kill them. Cancel has no GitHub side
   effects in v1, and the task branch and any worktrees are agent-owned
   state (§21.7, §21.8) that cancel never touches.

2. **Cancel semantics at the protocol boundary.** Cancelling moves every
   non-terminal work order of the task to the `cancelled` state (admitted
   by the schema since Phase 4.7 and written for the first time here). An
   in-flight claimed session is not interrupted mid-execution: its next
   `renew_work_order`, `report_progress`, or `submit_*` call returns a
   terminal order-cancelled refusal — the §21.4 pattern that already
   governs spent budgets and clocks — and a worker supervising a child for
   a cancelled order terminates the child and records the attempt as ended
   by cancellation. `reject` remains exactly what it was: a human gate
   verdict on a spec or diff, valid only at a gate. Cancel is lifecycle,
   not judgment; the UI keeps the two visually and semantically distinct.

3. **Operator recovery may re-freeze the setup (refines §21.13 change 7,
   §21.21, §21.31 change 5, and §21.32).** When a human recovers a
   released, expired, retry-suppressed, or execution-timed-out work order,
   the recovery re-freezes the task's setup contract from the current
   definition of the same-named setup and re-pins the recovered order —
   harness, model, effort, and argv — from that re-frozen contract. A
   material re-freeze appends `task.setup.refrozen` carrying the prior and
   new contracts; an identical resolution appends nothing. If no
   same-named setup exists, the prior contract is retained unchanged —
   re-freeze never fails a recovery (§21.32 change 2's posture). Automatic
   queue re-entry keeps §21.32 semantics exactly: definition-only refresh,
   pinned model preserved. The freezing rationale of §21.13 is preserved,
   not weakened — frozen under automation, mutable only under explicit,
   audited human action, the same line §21.31 change 5 drew for hold.

4. **Stalled tasks surface in the inbox (refines §13.2).** A task with a
   retry-suppressed order, a queue-timed-out order, or a repeating
   dispatch-failure loop is **stalled**: it appears in the review inbox in
   a needs-operator tray alongside gate items, carrying the recover and
   cancel actions and the last failure evidence, and the task list badges
   it as stalled rather than an indistinguishable Running. Stalled is a
   derived presentation state, not a stored task state.

5. **Data and compatibility.** No new task state: cancel reuses `closed`,
   distinguished by the recorded intervention and event (`task.cancelled`).
   No schema migration is required for orders (`cancelled` already
   validates). Historical manual closes recorded as ad hoc events remain
   as-is. The working breakdown slots into docs/phase5-plan.md; this
   section is authoritative.

---

### 21.35 v1.36 — Audited task execution-setup reassignment (July 22, 2026)

The intake-time freeze in §21.27 remains the default: later workspace setup
edits, renames, and deletions never alter an existing task. This amendment
adds one deliberate operator transition for replacing that frozen routing
contract for work that has not run. It is distinct from §21.32 command-only
queue repair and from §21.34 same-name recovery re-freeze.

1. **Named, authenticated, and explicit.** An authenticated operator may
   change a non-terminal task to a currently defined named workspace setup by
   supplying a non-empty reason and request/idempotency key. Selecting the
   existing setup name is the explicit **Apply latest setup** path: the current
   workspace definition is normalized and frozen again by value. Unknown or
   deleted names fail without fallback. There are no per-task free-form
   harness, model, effort, timeout, review-execution, or seat overrides, and
   this mutation is not exposed through MCP.

2. **Active-attempt exclusion and serialization.** The transition is rejected
   while any work order for the task is claimed or executing. A task hold does
   not relax this rule. Claim and setup change acquire the same task-scoped
   serialization boundary, so exactly one wins and the loser performs no
   partial task, queue, review, or audit mutation. Terminal tasks are rejected.

3. **Future work only.** A successful transition stores the selected setup's
   current normalized value as the task's new frozen contract. Unclaimed queued
   implementation/spec work and future review dispatch metadata are rebuilt
   from it. Completed jobs, work orders, verdicts, events, and claimed attempt
   snapshots retain the contract and snapshots they actually used. §21.32
   continues to refresh only the command definition of an eligible pinned
   harness while preserving routing identity; it never changes this contract.

4. **Idempotency and audit.** The task contract, regenerated queue metadata,
   review transition, and `task.setup.changed` event commit atomically in both
   persistence implementations. The event records workspace, task, actor,
   request key, non-empty reason, previous and new setup names and normalized
   contracts, lifecycle boundary/stage, and the resulting queue/review action.
   Exact retries return the recorded result without another event; reuse of a
   key for different inputs fails closed.

5. **Interrupted review rounds use the task contract.** A setup change is
   allowed when the latest review round is interrupted only if no seat is
   claimed or executing. Recovery and retry snapshot their panel from the
   task's frozen setup contract — after this transition, the replacement — not
   from mutable workspace-level review configuration. Unclaimed queued seats
   are rebuilt with new harness/model/effort/timeout/argv snapshots; a
   superseded pinned snapshot can never reappear during later recovery.

6. **Compatible-result rule.** When old and new panels have equal cardinality,
   seats map by stable position. A completed verdict contributes to the
   resulting contract only when its effective harness, model, and effort are
   identical in both panels. Such verdicts remain immutable and are retained;
   changed completed assignments are historical-only and receive fresh work,
   while every incomplete seat is rebuilt under the new contract. Aggregation
   waits for one contributing verdict per resulting seat and never counts a
   historical-only verdict. Seat recovery/rebuild events record the prior and
   new named normalized setup contracts and whether the seat was retained or
   re-run.

7. **Panel shape changes.** If seat cardinality changes, or stable positional
   mapping is unsafe, the interrupted round is preserved unchanged and marked
   non-aggregatable. Conveyor creates a whole new round from the task's new
   frozen contract; no verdict from the old round contributes. The transition
   and new-seat events record both setup contracts and the whole-round reason.

8. **Operator surfaces.** Workspace-scoped REST and the matching CLI expose
   **Change execution setup** plus explicit **Apply latest setup**. Task detail
   shows the current frozen setup, selects only current named setups, previews
   implementation harness/model policy/effort/timeout and review
   execution/seats, states **affects future work only**, and explains terminal,
   in-flight, retained-seat, re-run-seat, or whole-round eligibility outcomes.
   Server validation is authoritative.

### 21.36 v1.37 — Setup reassignment while delivered work awaits review (July 22, 2026)

Operating experience with §21.35 surfaced a gap: a task whose implementation
attempt has been **submitted** — delivered, with its review round still
unclaimed — could not be reassigned, even while held. The operator's natural
moment to reroute review (hold the task, change the setup, release) was
blocked by an attempt that is no longer executing anything. This amendment
narrows §21.35 change 2 to what it named: active attempts.

1. **The exclusion binds to executing attempts.** The transition is rejected
   while any work order for the task is **claimed**, or while any review seat
   holds an in-flight verdict submission (a review-stage order in the
   submitted state). A spec or implementation work order in the **submitted**
   state — delivered and awaiting review or a gate — does not block the
   transition. Its own attempt snapshot remains immutable per §21.35 change 3;
   nothing about the delivered attempt is rewritten.

2. **Live rounds with unclaimed seats are future work.** When the latest
   review round holds only queued and completed seats, §21.35 changes 5–7
   apply unchanged regardless of the implementing attempt's submitted state:
   queued seats are rebuilt under the new contract, compatible completed
   verdicts are retained by the compatible-result rule, and a panel-shape
   change supersedes the round and creates a whole new one. An implementing
   session attached through the in-session review loop (§21.4 `await_review`)
   aggregates from the resulting round; on a later resubmit, subsequent
   rounds snapshot from the task's replacement contract as §21.35 change 3
   already requires.

3. **Hold semantics are unchanged.** A task hold still does not relax the
   exclusion in change 1: a claimed attempt blocks reassignment whether or
   not the task is held. Hold remains the reservation primitive (§21.31) that
   keeps unclaimed work unclaimed — which is precisely what makes
   hold-then-reassign reliable now that submitted attempts no longer block.

4. **Operator surfaces state the real blocker.** Task detail — including the
   compact sheet, which now carries the same **Change execution setup**
   control behind an expandable section — reports the specific exclusion
   (claimed attempt, or in-flight verdict) rather than a blanket in-flight
   refusal. Server validation remains authoritative.

---

### 21.37 v1.38 — Canonical lifecycle state machines (July 22, 2026)

Conveyor's lifecycles are state machines in fact but not in form. A task
moves through eight states and a work order through seven, yet no single
place in the system records which transitions are legal. The consequences
are measurable in the Phase 4.7 codebase:

- Task state is written from roughly **35 call sites** across dispatch,
  the HTTP API, the work-order service, and store internals. The two
  store primitives (`UpdateTaskState`, `SetTaskTransition`) and the
  dispatcher's `transition()` wrapper validate nothing — every write
  succeeds regardless of the task's current state.
- Work-order transition guards are **re-expressed at three layers** with
  no shared source: service-layer checks (`authorized()`, `enforce()`,
  and the hand-tuned `authorizedForAwait()` exception), transactional
  re-checks in `UpdateWorkOrder`, and raw `AND state='claimed'` SQL
  predicates. The stale/timed-out rejection alone appears in at least
  six places.
- `tasks.state` is a bare `text NOT NULL` column. Work orders have a
  CHECK constraint (§21.9 migration); tasks have none — the database
  will persist a state the code has never heard of.
- Several read paths reconstruct sub-state by folding the event log
  (merge-conflict episodes, latest review results). Folds are only
  sound if the event stream cannot contain illegal sequences — a
  guarantee nothing currently provides.

The §21.33 grounded-specs work made diagrams of these lifecycles
non-normative illustrations. This amendment makes the underlying
transition relation itself normative: one table per lifecycle, enforced
by one code path, checked by the schema, and mined against the existing
event corpus for confession before enforcement. Phase 5.2 (adversarial
review panel) and Phase 5.4 (evidence-gated `submit_for_review`) both
extend these lifecycles; the tables must exist before those phases grow
the surface. Seven changes:

1. **Every lifecycle state space carries a normative transition table.**
   A lifecycle is defined by `(state, command) → state'`, where a
   *command* is a named cause — the vocabulary below — not a free-form
   caller. The tables in changes 2 and 3 are the authoritative
   definition of the task and work-order lifecycles; the prose scattered
   through §17.4, §21.9, §21.23, §21.26, §21.30, §21.31, and §21.34
   remains authoritative for *semantics* (what a command means, who may
   issue it, what side effects accompany it) while this section becomes
   authoritative for *reachability* (which transitions may occur at
   all). A transition absent from the table is illegal by construction.
   `JobState` and the GitHub/review publication state spaces adopt the
   same formalism with their (much smaller) tables recorded in code
   under the same module; they are not reproduced here.

2. **The task lifecycle table.** States are the existing `TaskState`
   values: `claiming`, `queued`, `running`, `awaiting_human`,
   `approved`, `merged`, `closed`, `parked`. Terminal states are
   `merged` and `closed`; `parked` is quiescent but recoverable.

   | # | From | Command | To | Semantics anchor |
   |---|------|---------|----|------------------|
   | T1 | `claiming` | `intake.finalize` | `queued` | intake finalization (§21.5, §21.25) |
   | T2 | `queued` | `dispatch.start` | `running` | in-process stage begins (§21.4) |
   | T3 | `queued` | `order.claim` | `running` | agent/worker claims the stage's work order (§17.4; hold guard §21.31) |
   | T4 | `running` | `stage.advance` | `queued` | stage output accepted, next stage set (§4) |
   | T5 | `running` | `stage.bounce` | `queued` | redirect/bounce below cap, same stage re-queued (§21.17) |
   | T6 | `running` | `stage.bounce_limit` | `awaiting_human` | bounce cap reached (§21.17) |
   | T7 | `running` | `job.fail` | `awaiting_human` | in-process job failure, recovery stage recorded |
   | T8 | `running` | `triage.route_human` | `awaiting_human` | triage routes to human (§4.1) |
   | T9 | `running` | `triage.park` | `parked` | triage parks the task |
   | T10 | `running` | `gate.spec` | `awaiting_human` | spec gate on (§21.12); spec version awaits approval |
   | T11 | `running` | `gate.merge` | `awaiting_human` | review verdict lands, merge gate on (§21.12) |
   | T12 | `queued` | `dispatch.fail_retry` | `queued` | queue attempt failed, retry remains (§21.32); recovery stage recorded |
   | T13 | `queued` | `dispatch.fail_final` | `parked` | final queue attempt failed |
   | T14 | `awaiting_human` | `intervention.reject` | `closed` | human rejects (§13.2) |
   | T15 | `awaiting_human` | `intervention.approve_spec` | `queued` | spec approved → implement (§21.12) |
   | T16 | `awaiting_human` | `intervention.approve_review` | `approved` | review approved; merge readiness begins (§21.30) |
   | T17 | `awaiting_human` | `intervention.redirect` | `queued` | human redirects to recovery stage (§13.2) |
   | T18 | `approved` | `merge.confirm` | `merged` | merge confirmed on the forge (§21.30) |
   | T19 | `approved` | `refresh.review` | `queued` | base head moved; refresh review dispatched (§21.30 change 6) |
   | T20 | `approved` | `conflict.dispatch` | `queued` | merge conflict; fix order dispatched (§21.30 change 3) |
   | T21 | `parked` | `task.recover` | `queued` | operator recovery endpoint re-queues |
   | T22 | any non-terminal | `task.cancel` | `closed` | task cancellation (§21.34) |

   The table's commands map one-to-one onto event kinds (change 5);
   review-verdict re-queue under an off merge gate is `stage.advance`
   from `review`, not a distinct command. Guards referenced by the
   semantics anchors (hold at claim time, self-review, gate toggles,
   §21.36 reassignment rules) are preconditions *on issuing a command*;
   they live with the command's handler, not in the table. The table
   answers only whether the resulting transition is legal.

3. **The work-order lifecycle table.** States are the existing
   `WorkOrderState` values: `queued`, `claimed`, `submitted`,
   `completed`, `cancelled`, `stale`, `timed_out`. Terminal states are
   `completed` and `cancelled`; `stale` and `timed_out` are recoverable
   (§21.9, §21.23).

   | # | From | Command | To | Semantics anchor |
   |---|------|---------|----|------------------|
   | W1 | — | `order.create` | `queued` | dispatch creates the order (§17.4) |
   | W2 | `queued` | `order.claim` | `claimed` | claim; hold, self-review, serviceability guards (§21.31, §17.4, §21.27) |
   | W3 | `claimed` | `claim.renew` | `claimed` | lease extension (§21.9) |
   | W4 | `claimed` | `claim.release` | `queued` | worker releases (§21.13) |
   | W5 | `claimed` | `claim.expire` | `queued` | lease expiry returns the order to queue (§21.9) |
   | W6 | `claimed` | `submit_for_review` | `submitted` | implementation delivered (§17.4; configured evidence gate enforced by §21.44) |
   | W7 | `claimed` | `submit_spec` | `completed` | spec order completes (§21.33) |
   | W8 | `claimed` | `submit_review_verdict` | `completed` | review order completes (§17.4) |
   | W9 | `submitted` | `review.terminal` | `completed` | review round reaches a terminal verdict |
   | W10 | `submitted` | `review.revise` | `claimed` | in-session revision loop via `await_review` (§17.4, §21.23) |
   | W11 | `queued`, `claimed` | `order.timeout` | `timed_out` | order clock elapses (§21.9) |
   | W12 | `queued`, `claimed`, `submitted` | `order.stale` | `stale` | superseded — head moved or task advanced (§21.9) |
   | W13 | `timed_out`, `stale` | `order.recover` | `queued` | audited recovery (§21.23) |
   | W14 | `queued`, `claimed`, `submitted` | `order.redispatch` | `queued` | operator redispatch (§17.4) |
   | W15 | any non-terminal | `order.cancel` | `cancelled` | task cancellation cascades (§21.34) |

   W9/W10 codify what `authorizedForAwait()` currently expresses as a
   hand-tuned exception: `await_review` is legal in `claimed` and
   `submitted`, and the submitted order either completes on a terminal
   verdict or returns to `claimed` for the revision loop. Acceptance of
   this amendment includes reconciling W9/W10 against the §21.23/§21.26
   review-round recovery paths; any edge found in the event corpus
   (change 6) but absent here is resolved by table amendment before
   enforcement, never by silent code exception.

4. **One transition primitive per state space; direct writes are
   retired.** A `core`-level machine module holds each table as data
   (a Go map — no FSM library; §17.0's dependency discipline stands)
   and exposes a single guarded operation per space:
   `Transition(from, command) (to, error)`. The store primitives that
   today write state unconditionally (`UpdateTaskState`,
   `SetTaskTransition`, `transitionWorkOrderTx` call sites) are
   re-plumbed so the machine decides before the row is written; an
   illegal pair returns a typed `ErrInvalidTransition{Space, From,
   Command}` carrying the allowed alternatives. Existing sentinel
   errors (`ErrWorkOrderStale`, `ErrWorkOrderTimedOut`,
   `ErrReviewRetryConflict`) remain the wire-visible vocabulary —
   handlers translate, so MCP and REST surfaces are unchanged. The
   duplicated service-layer guards (`authorized`/`enforce`/
   `authorizedForAwait` state checks) collapse into machine consults;
   what remains of them is identity and clock checks, which are command
   preconditions, not transition rules. SQL `AND state=...` predicates
   and transactional re-checks remain as **defense in depth** — they
   restate the table's edge for the concurrent-writer case, they do not
   extend it.

5. **Transitions and events are one act.** Every legal transition
   already pairs a projection UPDATE with an `insertEvent` in the same
   transaction; this becomes definitional. Each command in the tables
   maps to exactly one event kind (`task.state_changed` payloads gain
   the command name; work-order kinds like `work_order.claimed`,
   `work_order.released`, `work_order.expired` already are the command
   vocabulary and are kept). The event log therefore records the
   machine's exact edge sequence, which is what makes the existing
   fold-over-events readers sound and makes change 6 possible.

6. **The event corpus audits the table before the table constrains the
   code.** Because events are append-only source of truth (§16), the
   real transition relation is minable: a migration-time audit folds
   every task's and order's event history and reports any observed edge
   absent from the tables. The audit runs before enforcement is
   switched on; discrepancies are resolved by amending the table (if
   the edge is intended) or recorded as historical corruption (if not
   — such rows are annotated, never rewritten). Enforcement ships only
   after a clean audit. Additionally, `tasks.state` gains the CHECK
   constraint work orders have had since §21.9, and both CHECK lists
   are generated from the machine module's state sets so schema and
   code cannot drift.

7. **Traceability and non-normative rendering.** The machine module is
   annotated `(spec §21.37)` per table row group, making the table the
   first spec artifact mechanically diffable against code. A generated
   state diagram (from the table data, per §21.33's non-normative
   diagram convention) is published into the requirements tree UI;
   the diagram illustrates, the table governs. Future lifecycle changes
   — Phase 5.2's review-panel states, Phase 5.4's evidence gate on W6
   — are made by amending the relevant table rows in this section,
   which is precisely the point: growth becomes a table edit plus
   handler work, not an archaeology of three guard layers.

**Explicitly out of scope.** No wire-protocol change: MCP tools, REST
routes, CLI, and the §17.4 lifecycle are untouched. No new states are
introduced and none removed; `TaskMode`'s vestigial parse-only role
(§21.31 change 6) is unaffected. Concurrency and locking are unchanged
by this amendment — serialization ergonomics are §21.38, which assumes
this amendment. The working breakdown slots into docs/phase5-plan.md
ahead of Phase 5.2; this section is authoritative.

---

### 21.38 v1.39 — Serialized task command plane (July 22, 2026)

Conveyor's concurrency substrate is shared Postgres — advisory locks, row
locks, transactions, leases — and that substrate is correct for a factory
whose actors are external, crash-prone, operator-owned agent processes.
At the system boundary the design is already actor-shaped: work orders
are messages, workers are actors, claim/renew/release is the mailbox
protocol (§17.4, §21.9), and reconciliation is supervision. This
amendment does not change that substrate and explicitly rejects an
in-process actor runtime. It builds on §21.37: the command plane is the
enforcement point where §21.37's tables are consulted.

What the substrate lacks is *ergonomics*: its guarantees are conventions,
not structures. In the Phase 4.7 codebase:

- `WithTaskLock` — the per-task serialization primitive — is **opt-in
  and rare**: four call sites (merge, refresh, conflict, redispatch
  paths in dispatch), while ~35 sites write task state. Nothing stops a
  handler from writing state without the lock; most do.
- The lock itself is subtle: a **session** advisory lock held on a
  dedicated pooled connection while the protected closure writes
  through *other* pool connections, with a hijack-and-close escape
  hatch when unlock fails. Correct, but every future caller must
  understand why.
- Reads mutate. `refreshWorkOrders` runs timeout/stale/expiry
  *transitions* inside `GetWorkOrder` and both list paths — listing
  work orders can change them, and expiry latency is coupled to read
  traffic rather than to a clock.
- Dispatch has two mailboxes: a 64-buffered in-memory channel, and
  River behind `UseDurableQueue()`. The in-memory path drops queued
  tasks on crash — exactly the failure mode the Postgres substrate
  exists to prevent.

The remedy is the virtual-actor pattern on the existing substrate: the
"actor" for a task is not a goroutine but an identity (the advisory-lock
key) that is activated on demand, processes exactly one command at a
time, and persists every step. One funnel receives commands, consults
§21.37's tables, and owns every state write; the type system makes
bypassing it a compile error. Seven changes:

1. **All task mutation flows through one command plane.** A new
   `internal/taskops` package exposes the single mutation entry point:

   ```go
   // Perform executes one command against one task, serialized
   // per task across all daemon instances (spec §21.38).
   func (p *Plane) Perform(ctx context.Context, taskID string, cmd Command) (Outcome, error)
   ```

   `Command` is the closed vocabulary of §21.37 change 2 (T1–T22),
   each carrying its typed payload (stage output, intervention record,
   recovery stage, cancellation reason). Internally `Perform` is the
   actor receive loop, one activation per call: acquire the per-task
   serialization (change 3) → load the task snapshot → check command
   preconditions (gates, hold, identity — the §21.37 "guards on
   issuing") → consult the §21.37 machine for legality → persist
   projection update and event in one transaction → run post-effects
   (queue insertion, notifications). The existing call-site taxonomy
   maps mechanically: `completeOutput`'s stage switch becomes
   `stage.advance`/`stage.bounce` commands, `HandleIntervention`'s
   action switch becomes `intervention.*` commands, the HTTP API's
   direct `UpdateTaskState` calls become `intake.finalize`, River's
   failure paths become `dispatch.fail_retry`/`dispatch.fail_final`.
   The dispatcher retires its private unconditional `transition()`
   wrapper. Work orders receive the same consolidation with their
   existing serialization (claim-scoped row locks and lease identity
   checks are already a per-order mailbox; they need §21.37's machine
   consult, not a new lock).

2. **Lock possession is a type, and store mutators demand it.** The
   plane is made unbypassable structurally, not by review discipline.
   `taskops` defines an unexported-constructor token:

   ```go
   // TaskLease proves the holder is inside Perform's serialized
   // section for this task. Only taskops can mint one.
   type TaskLease struct { /* unexported */ }
   ```

   Every store method that writes task lifecycle state — the successors
   of `UpdateTaskState` and `SetTaskTransition`, on both the Postgres
   and memory implementations — takes a `TaskLease` parameter. Code
   outside `taskops` cannot construct the token, so a state write
   outside the serialized section does not compile. This converts the
   current "four lock sites, thirty-five write sites" gap into an
   impossibility. Read paths take no token and acquire no lock.

3. **Transaction-scoped locks by default; session scope only across
   external side effects.** `Perform` serializes with
   `pg_advisory_xact_lock` inside the write transaction: lock and
   write share a connection, the lock cannot outlive a crashed
   transaction, and the hijack-on-unlock-failure dance disappears.
   The session-scoped variant survives under an intent-revealing name
   (`WithTaskSideEffectLock`, successor of today's `WithTaskLock`) for
   the one span that genuinely must cover an external call across
   transactions — forge merge confirmation (§21.30) — where the lock
   must be held from readiness check through merge API call through
   `merge.confirm`. Commands declare whether they carry a side-effect
   span; the plane picks the lock scope, callers never choose. The
   lock key remains workspace-scoped (§21.10).

4. **Reads are pure; the clock is a sender.** `refreshWorkOrders` is
   removed from `GetWorkOrder`/`ListWorkOrders`/`ListTaskWorkOrders`.
   In its place a periodic River job — the **order clock** — scans for
   elapsed deadlines and issues `order.timeout`, `order.stale`, and
   `claim.expire` commands through the work-order path of the plane,
   at the cadence the §21.9 clocks already define. Expiry becomes a
   message from a clock actor rather than a side effect of observation:
   read latency no longer hides transitions, list endpoints stop
   holding `FOR UPDATE` loops, and every expiry follows the same
   guarded, evented path as any other transition. Submission-time
   enforcement is unaffected — `enforce()`'s direct deadline
   comparison at command time remains, so a stale order is rejected
   even if the clock has not ticked yet; the tick merely makes the
   projection converge without waiting for a reader.

5. **One durable mailbox.** River becomes the only production dispatch
   queue; the in-memory channel survives solely as the memory-store
   test double behind the same interface, and `UseDurableQueue()`
   ceases to be a production toggle. A queue that forgets its contents
   when the daemon dies contradicts both the §16 source-of-truth
   posture and §21.31's one-queue model; after this change the mailbox
   has the same durability as the state it feeds. Queue insertion
   happens in `Perform`'s post-effect step keyed off the machine's
   outcome (a transition into `queued` enqueues), replacing the
   scattered enqueue-after-write pairs.

6. **Interaction contract with §21.37.** The machine module decides
   *legality*; the plane decides *admission* (preconditions) and owns
   *atomicity* (lock, transaction, event). Neither subsumes the other:
   the tables stay pure data usable by audits and diagram generation,
   and the plane stays the only writer. `ErrInvalidTransition`
   surfaces through `Perform` unchanged; handler-layer translation to
   wire vocabulary (`ErrWorkOrderStale` and friends) is unchanged from
   §21.37 change 4.

7. **Migration is staged and mechanically checkable.** Order of
   landing: (a) introduce the plane and route the four
   already-locked dispatch paths through it; (b) migrate the remaining
   write sites command by command — each is an enumerable, behavior-
   preserving move because §21.37's audit has already confirmed the
   edges; (c) flip the store mutator signatures to require `TaskLease`,
   which turns any missed site into a compile error and completes the
   retirement of direct writes; (d) land changes 4 and 5. Acceptance
   criteria: no non-test call path writes lifecycle state outside the
   plane (enforced by the token, verified by `make vet`); read
   endpoints issue no UPDATE (verified by query logging in tests); a
   crash between mailbox insert and daemon restart loses no queued
   task (River durability test); the §21.37 event-corpus audit stays
   clean when re-run over post-migration traffic.

**Explicitly out of scope and rejected.** No goroutine-per-task
runtime, no in-memory mailbox channels as a durability layer, no
supervision trees, no second protocol beside the §17.4 MCP lifecycle
(§21.13's thin-supervisor rule stands). Postgres remains the supervisor:
leases, clocks, and reconciliation are the restart semantics. Worker-
facing behavior is unchanged — claim/renew/release/submit semantics,
hold (§21.31), setup freezing (§21.27, §21.35), and review recovery
(§21.23, §21.26) are untouched; this amendment moves where their
invariants are enforced, not what they are. The working breakdown slots
into docs/phase5-plan.md ahead of Phase 5.2; this section is
authoritative.

---

### 21.39 v1.40 — GitHub-flavored task bodies (July 22, 2026)

Task `body` is GitHub-flavored Markdown, stored as the original string and
delivered unchanged to downstream consumers; existing plain-text bodies remain
compatible, and MCP intake identifies the format and encourages headings and
lists without adding rendered storage or enabling raw HTML.

---

### 21.40 v2.0 — Consolidated restatement (July 28, 2026)

Thirty-nine amendments accumulated against a v1.0 body written for a
design thesis the factory has since inverted: the base sections still
presented the sandbox execution plane, the escalation ladder, budget
enforcement, and pre-pivot Git ownership as normative, and a correct
reading of the specification required replaying the entire amendment log
against every section. That made the document authoritative in name but
archaeological in practice — for humans and for the agents that read it
as their work-order context. This amendment consolidates. Four changes:

1. **The body is restated.** Sections 1–20 are rewritten to state the
   v1.40 design directly: every accepted amendment through §21.39 is
   folded into the section that owns its subject, superseded v1.0–v1.39
   material is removed from the body, and section numbering is
   preserved so existing `(spec §N)` code annotations remain valid.
   This is a restatement, not a redesign: **no behavioral contract is
   added, removed, or altered by this amendment.** Where the v2.0 body
   and any earlier text are found to disagree, the discrepancy is a
   restatement defect to be fixed by a correcting amendment against the
   intent of the original accepted amendment.

2. **The body is again the single normative authority.** From v2.0, the
   consolidated §§1–20 govern, including the lifecycle transition
   tables (§3.3) and command-plane contract (§3.4) restated from
   §§21.37–21.38. Sections §21.1–§21.39 become the historical change
   record and design rationale — retained verbatim, valuable for the
   *why* behind every contract, no longer the place to look up the
   *what*.

3. **Cross-references are version-scoped.** Section references inside
   §§21.1–21.39 (e.g. "supersedes §13.1", "amends §8.2") refer to the
   body as it stood at that amendment's version, not to the v2.0 body.
   The v2.0 body cites amendments by number where the origin aids
   understanding; those citations are provenance, not delegation of
   authority.

4. **The amendment process is unchanged.** Future changes amend the
   v2.0 body by appending to this log with a version bump — never by
   silent edits — exactly as before. A future consolidation, if the log
   again outgrows the body, repeats this pattern as a new major
   version.

### 21.41 v2.1 — Supervision hygiene from external review; W14 restatement correction (July 28, 2026)

**Origin.** A comparative review of OpenAI's Symphony specification
(`openai/symphony` `SPEC.md`, Draft v1) — a single-stage, in-memory,
tracker-polling orchestrator for Codex agents. Architecturally the two
systems answer different questions and Symphony's core stances are ones
this specification has already explicitly rejected; but its subprocess
supervision and configuration hygiene are more thorough than ours in
six specific places, and grounding the comparison surfaced two defects
in our own text. This amendment adopts the six and fixes the two.
Everything here is supervision, determinism, or hygiene — nothing
revives budget enforcement (§21.6), managed execution (§21.4), or a
second protocol (§6.4).

**Changes:**

1. **Worker stall detection (§5.1, §5.3, §6.4).** The gap: between "the
   child process is alive" and the per-stage execution timeout —
   potentially hours — nothing bounded a hung harness. Symphony kills
   any run silent beyond a stall threshold, separately from its turn
   timeout; the same idea fits our worker supervision exactly. The
   registry entry gains optional `stall_timeout` (default 10m; literal
   `0` disables, the one sanctioned exception to the non-positive-
   timeout rule), snapshotted into the work order like every other
   launch value. The worker stops a child with no output for the bound,
   records the distinct outcome `stalled`, releases the claim, and
   consumes the bounded child-retry allowance — landing in the existing
   retry-suppressed/needs-operator path. Deliberately worker-side and
   activity-based: it watches silence, never token or USD spend, so it
   is not a budget breaker under §14. The timeline renders last agent
   activity so quiet-but-alive stays distinguishable from dead.

2. **Deterministic claim ordering (§6.3).** The spec previously stated
   no service order for `claim_work_order` — no FIFO guarantee, no
   priority, nothing; starvation under backlog was formally possible
   and untestable. Now specified: among eligible orders, oldest queue
   entry first (workspace-scoped, by the §3.2 queue clock); stage
   preference (the reserved review slot) sits above age; queue re-entry
   records a fresh queue entry so recovered orders rejoin behind the
   backlog. A task priority field was considered and **not** added —
   it would be the first scheduling knob agents could compete over;
   if demand materializes it arrives by amendment with its own abuse
   analysis.

3. **Worktree path safety (§2.1 validator, §8.2).** Symphony sanitizes
   workspace keys to `[A-Za-z0-9._-]` and requires resolved paths to
   stay inside the root; our deterministic sibling
   `../<repo>-task-<task-id>` specified blocking preconditions but no
   string-level rules, and repo `name` is operator-editable
   configuration. Now: repo `name` validates at write time to
   `[A-Za-z0-9._-]` (and not `.`/`..`) under the standard §2.1
   validator; task IDs already conform by construction; `conveyor
   checkout` verifies the resolved path is a true sibling and blocks on
   any separator or traversal in a component. Mostly an operator
   foot-gun fix, not an attack-surface fix — but two lines of invariant
   close it permanently. Existing repo entries validate on next
   configuration write; checkout enforces regardless.

4. **Pinned defaults (§2.1, §3.2, §3.3).** Symphony ends its config
   section with a full defaults table; comparing against it exposed two
   unpinned values here. The work-order claim lease had **no specified
   default** (the 15s figure in §6.4 is the worker *liveness* lease, a
   different object) — now 5 minutes, renewable, with the note that
   expiry is safe by design (W5 returns the order to `queued`). River
   dispatch retry (T12/T13) had no specified policy — now five attempts,
   exponential backoff from 10s doubling capped at 5m, then
   `dispatch.fail_final`. §2.1 gains a consolidated named-defaults
   table; each value's owning section governs, the table is an index.

5. **Forge error categories (§11.1).** Stable categories
   (`forge_request`, `forge_status`, `forge_response`,
   `forge_rate_limited`, `forge_permission`) recorded on forge-call
   failure events, replacing stringly-typed evidence in the
   needs-operator tray. Adopted from Symphony's adapter error-category
   contract, scoped to our one forge — this is a taxonomy, not an
   adapter interface. The original assumption that both Phase 5.3
   projections were outstanding is corrected by §21.43; categorized
   publication failures remain separately sequenced implementation work.

6. **Observational rate-limit telemetry (§14, §17.4).** `report_usage`
   MAY carry the provider's latest rate-limit status, persisted
   self-reported and rendered on worker/harness health surfaces, so an
   operator can see *why* a worker slowed down. Observational like
   every other usage value: nothing reads it for gating, and §21.6's
   no-enforcement posture is unchanged.

7. **W14 correcting fix (§3.3).** The v2.0 restatement admitted
   `order.redispatch` from `queued`, `claimed`, and `submitted` —
   broader than both §17.4 ("stale, never-claimed orders") and §21.9
   change 6, which explicitly rejects active claims and
   execution-timed-out orders. Under §21.40 change 1 that divergence is
   a restatement defect; W14 is corrected to `stale` → `queued` with
   the never-claimed guard, restoring the §21.9 contract. `order.recover`
   (W13) remains the audited operator path for `timed_out` and `stale`
   generally; redispatch remains the narrower agent-facing recovery for
   queue-timed-out orders.

8. **Decision log row 10 (§20)** records the adoption stance and, for
   the permanent record, what was reviewed and rejected.

**Considered and rejected**, with reasons on the record:

- **In-memory orchestrator state / restart-by-repolling.** Contradicts
  §3.4 ("Postgres is the supervisor") and §16; Symphony's own failure
  model concedes retry timers and live sessions die on restart.
- **Repo-owned workflow policy (`WORKFLOW.md`).** Conflicts with the
  settled §2.1 model — Postgres as running truth, one validator, audit
  events, `config export`/`import` for git-versioned backup. The
  legitimate remainder is already scoped as Phase 5.6 `.conveyor/`
  hints, advisory only; this amendment does not expand that scope.
- **Unbounded exponential retry.** Symphony retries failures forever
  (capped delay, uncapped attempts). Our 1s/2s/4s-then-retry-suppressed
  contract (§21.21) is the better fit for unconfined execution on
  operator machines: silent infinite retry burns real money invisibly;
  suppression makes the operator decide.
- **Server-side / per-state concurrency caps.** The pull model puts
  capacity where it lives — worker-side stage-aware slots (§6.4). A
  server cap would be a second, contradictory capacity authority.
- **Reconciliation that kills workers on external state change.** Our
  gentler equivalents are deliberate: cancel cascades at the next
  lifecycle call, terminal renewal responses stop the child (§21.26),
  hold never pauses in-flight work.

### 21.42 v2.2 — Worker first-activity liveness (July 28, 2026)

The claim lease proves that the worker supervisor can still reach the control
plane; it does not prove that the launched harness is making progress. The
worker therefore owns a separate, output-based time-to-first-activity signal
for worker-launched spec, implementation, and review children.

1. **Explicit execution policy.** Workspace execution policy gains
   `first_activity_timeout`, a duration with a canonical default of `2m`.
   Existing documents that omit it normalize to that explicit value. It must
   be positive and shorter than every configured MCP stage execution timeout;
   invalid deployment files and workspace writes fail validation. The
   normalized value is present in the effective worker configuration, so a
   worker has no hidden process-local policy default.

2. **First output is the sole signal.** The watchdog begins only after
   successful child-process launch. The existing redacted stdout and stderr
   destinations are wrapped by one concurrency-safe first-write signal; any
   non-empty write to either stream permanently disarms the watchdog. Output
   redaction, console forwarding, and the bounded failure tail are unchanged.
   The first-activity watchdog itself introduces no silence-between-outputs or
   total-duration heuristic. After first output, the distinct §21.41
   `stall_timeout` contract governs ongoing inactivity, while the fixed
   execution deadline remains the total-duration backstop.

3. **Conditional recovery through the existing path.** If the deadline
   arrives before output, the worker first preserves the precedence of an
   already-observed write, normal child exit, durable completion, cancellation,
   and claim-authority loss. It may act only while the child is still running
   and the same worker/session still owns the claimed order. The worker then
   terminates and reaps the child and releases that exact claim once as
   `child_failure` with the stable reason
   `harness produced no output before first_activity_timeout`. The existing
   conditional release, bounded retry/backoff, identical-failure suppression,
   and stale-claim protections remain authoritative.

4. **Existing clocks and observability remain authoritative.** Pre-launch
   checkout, MCP configuration, probes, and other setup remain governed by
   claim renewal and their existing bounds. The fixed execution deadline
   (§21.9, §21.21) remains the hard attempt backstop and is never extended or
   replaced. Existing `work_order.child_failed` events, failure fields, retry
   fields, and the §21.34 stalled-task tray expose detection, recovery, and
   retry suppression without a new lifecycle state or tray category.

**Explicitly out of scope.** No MCP heartbeat, server-side inference from MCP
calls, rollout files, CPU use, or session-file mtimes; no change to claim lease
semantics, execution deadlines, retry policy, review-round recovery, harness
snapshots, credentials, redaction, the separate §21.41 ongoing-stall policy,
or operator recovery authority; and no second worker protocol or later Phase
5/Phase 8 work.

### 21.43 v2.3 — Complete Phase 5.3 review projection (July 28, 2026)

PostgreSQL dogfooding exposed a transaction-ordering defect in the delivered
Phase 5.3 review publisher. A publication row receives PostgreSQL's transaction
timestamp, while its own `review.completed` event receives a later application
timestamp in the same transaction. Treating any later event as proof that the
comment was already represented therefore suppressed the publication's own
required aggregate comment and allowed `published` rows with `comment_id = 0`.
This amendment refines §11.1, §19, §21.12 change 5(c), §21.15, and §21.22:

1. **Durable identity, not time, controls idempotency.** The
   `review_work_order_id` publication record is the authoritative identity.
   Review round and seat are lifecycle context. Event or row timestamps never
   suppress the current publication. Every eligible publication upserts the
   one task-marked aggregate PR comment; panel seats, retries, reconciliation,
   and later rounds update that comment rather than creating another.

2. **Both projections are required.** An eligible publication completes only
   after the portable aggregate commit status and deterministic comment
   succeed. `review_publications.state = published` requires a nonzero
   `comment_id`; a missing or failed comment remains retrying or becomes failed
   under the existing bounded River policy. Startup reconciliation reopens and
   re-enqueues legacy `published`/zero-comment rows as the same durable
   publication identity. The durable verdict, bounce, round aggregate, and task
   gate remain authoritative and are never rolled back by an external
   publication failure.

3. **Resolution history is cumulative.** Requested changes remain visible as
   `unresolved` until a later approving round resolves them. That later round
   updates the same comment with the prior resolved entry and the new accepted
   approval. Single-seat and panel histories use the same deterministic task
   marker.

4. **Phase 5.3 is delivered.** Issue creation/update on spec approval, PR
   creation at `submit_for_review`, portable aggregate status, and deterministic
   verdict/resolution comment projection are shipped. §19 and the Phase 5
   documentation no longer describe both core Phase 5.3 capabilities as
   unimplemented.

5. **Forge taxonomy remains separately sequenced.** Parked task
   `260728-c0f858` owns implementation of the §21.41 forge error categories.
   This repair preserves the existing publication failure boundary and
   `last_error` evidence for that work but does not implement or rename the
   taxonomy.

Validation covers PostgreSQL transaction/event ordering, nonzero comment
persistence, single and panel publications, retry/reconciliation idempotency,
requested-changes-to-approval history, and preservation of internal authority
when GitHub publication fails.

### 21.44 v2.4 — Complete Phase 5.4 verification evidence (July 28, 2026)

Phase 5.4 delivers §12 and §21.12 change 6 without adding an automated verifier
or a second artifact system:

1. **The workspace policy is live configuration.** The versioned
   `execution.require_verification_evidence` boolean defaults off. Validated
   writes persist through the existing workspace configuration store, emit the
   ordinary `config.updated` audit event, and hot-reload through the existing
   configuration provider. It is intentionally not frozen into an execution
   setup.

2. **Eligibility is explicit and fail-closed.** Only a direct task link with
   role `verification_evidence` can satisfy the gate. Accepted screenshots are
   `image/png`, `image/jpeg`, and `image/webp` up to 10 MiB. Accepted short
   recordings are `video/mp4` and `video/webm` up to 25 MiB. MIME parameters and
   casing normalize at the control-plane boundary; filenames are never used as
   type evidence. Empty, unsupported, oversized, feature-only, cross-task, and
   cross-workspace artifacts are ineligible.

3. **Rejection has no lifecycle side effects.** After claim authorization and
   current configuration resolution, `submit_for_review` evaluates task-owned
   evidence before opening or updating a PR, recording a reviewed head,
   submitting the implementation order, advancing the task, or dispatching a
   review round. Rejection leaves the implementation claim active and tells the
   implementer the accepted types, limits, task ID, role, and attachment
   boundary.

4. **Every review seat receives authorized evidence.** Review work-order
   context repeats eligible evidence as immutable metadata plus that order's
   `read_artifact` reference. The artifact ID alone remains non-authorizing.
   The task detail and human review card render screenshots and recordings
   through the authenticated artifact download path, with accessible labels,
   explicit controls, and a download fallback.

5. **The GitHub mirror is deterministic and credential-free.** The reconciled
   PR lifecycle body contains one task-marked verification-evidence section.
   It publishes portable filename, normalized MIME type, byte size, and SHA-256
   identity, replacing the prior marked section on retry rather than
   duplicating it. Because the current artifact transport is private
   control-plane storage, the PR never receives bearer credentials, private
   URLs, or an inaccessible pseudo-link; media bytes remain available through
   each authorized Conveyor review surface.

Repository CI remains the mechanical verifier. Evidence is implementer-supplied
review context and cannot count as a passing check. The Phase 8 independent
Playwright/computer-use verifier, managed execution, runners, adapters,
snapshots, secret references, and broader artifact redesign remain out of
scope.

### 21.45 v2.5 — Complete Phase 5.6 monitor and advisory hints (July 28, 2026)

Phase 5.6 closes the original autonomous feedback loop while preserving every
existing authority boundary:

1. **The monitor is an observer and normal intake client.** Workspace
   configuration explicitly enables monitoring, selects configured
   repositories, and sets positive polling and startup-reconciliation
   windows. The GitHub boundary observes failed checks on recorded Conveyor
   lineage and direct pushes, merged external PRs, and reverts outside that
   lineage. A stable identity combines signal kind, repository, and an
   occurrence discriminator: redelivery, restart, polling, and later status
   reads reuse one task, while a later check attempt or new commit remains a
   distinct occurrence. Every created task goes through the one durable intake
   path and starts at triage with the workspace's current frozen setup and gate
   policy. Monitor code has no claim, implementation, verdict, merge, or
   deployment operation.

2. **Observation and drift are durable workspace projections.** Memory and
   PostgreSQL stores persist source URL, commit/check/PR identity, safe
   structured context, created/reused task link, deduplication count, and
   actionable health/backoff state. Out-of-pipeline changes additionally own a
   reconciliation row linked to the repository change, task, and feature when
   known. Workspace status exposes enabled state, last success, current
   categorized error/backoff, observations, and unresolved drift count and
   oldest age. Drift clears only through one of the audited terminal outcomes:
   `requirements_amended`, `conflict_resolved`, or `change_reverted`; merely
   acknowledging a change cannot make the metric healthy.

3. **Forge retry remains bounded and internally authoritative.** Startup and
   periodic observation re-read a configured time window and rely on durable
   occurrence identity for convergence. Forge calls use the §21.41 error
   taxonomy and bounded exponential backoff. External read failure updates
   operator-visible status but never removes or rewrites an internal task,
   observation, drift record, approved spec, or requirements node.

4. **Repository hints are strict advisory data.** The sole format is
   `.conveyor/hints.yaml`, version 1, read from the exact relevant commit. It
   permits named verification argv arrays, triage areas, ownership hints, and
   short workflow context. Known-field decoding, supported-version checks,
   non-shell argv validation, command-interpreter rejection, and
   revision/SHA-256 provenance fail closed. Loading performs no execution.
   Unknown capability fields and attempts to grant tools, credentials,
   network/filesystem access, model or worker routing, gates, setup changes, or
   execution authority are rejected as unknown fields.

5. **Authority precedence is deterministic.** Workspace security and
   configuration plus the task's frozen setup remain authoritative. Approved
   specs override conflicting advisory entries. Repository hints may only add
   otherwise-unclaimed advisory verification or triage context. A hint change
   affects only context subsequently loaded from that revision and cannot
   mutate an existing frozen task or approved spec.

6. **Operator surfaces are workspace-scoped.** The REST API, `conveyor monitor`
   CLI, Workspace configuration UI, and Monitor dashboard expose enablement,
   repository scope, last success, error/backoff, created/reused task links,
   and drift count/age. Explicit audited operator action records reconciliation
   outcomes. Secrets remain in the existing daemon/GitHub credential boundary
   and never enter task bodies, events, hint files, or logs.

Phase 6 memory, Phase 7 self-improvement, Phase 8 automated verification and
managed execution, provider adapters, the retired sandbox plane, automatic
requirements edits, and deployment remain out of scope.

*(Phase numbers in this entry are as of v2.5; §21.46 renumbers the
deferred phases.)*

---

### 21.46 v2.6 — Phase 5 closure; Phase 6: planning & the knowledge graph (July 29, 2026)

Phase 5 closes, and the next phase reorients the factory around
planning, dependencies, and lineage. The operating observation behind
the scope: dogfooding proved the pipeline as a per-task orchestrator,
but intake is one task at a time, the spec agent's `decomposition`
output is validated and then discarded, planning conversations
happen outside the factory — so their rationale never enters the
corpus — and the curated requirements tree accretes without driving
anything. The compounding value of a software factory is that task N is
cheaper than task 1; that requires owning planning and keeping the
structure the pipeline currently drops. Ten changes:

1. **Phase 5.5 closes; Phase 5 is complete.** `conveyor worker
   install` / `uninstall` / `status` shipped July 28, 2026 (PR #156)
   with tests and operations documentation; the operator accepted the
   phase July 29, 2026. §6.5 and the §19 table flip to complete. With
   §21.43–§21.45, every Phase 5 sub-phase is closed and
   docs/phase5-plan.md is a historical record.

2. **Phase 6 accepted: planning & the knowledge graph**, in three
   sub-phases — 6.1 blueprint materialization and dependency-gated
   claiming, 6.2 planning sessions, 6.3 lineage links and graph context
   assembly. Working breakdown in docs/phase6-plan.md; §19 is
   authoritative for scope and ordering. 6.1 lands first deliberately:
   it pays off with the existing MCP spec flow before any chat surface
   exists.

3. **Blueprint materialization (§4.1).** A spec with a non-empty
   `decomposition` is a blueprint; approval materializes each `SUB-n`
   into a child task transactionally — children inherit frozen gates
   and setup, carry `(spec version, SUB-n)` lineage, and enter at
   `implement` (their scope was just defined by an approved spec;
   re-triage is redundant and could re-route them). The decomposition
   must be a DAG — approval fails closed on cycles, closing the known
   validation gap. Materialization is idempotent per spec version. The
   parent takes no implement order; it is the batch anchor and closes
   by an audited control-plane transition when its last child is
   terminal — the §3.3 task machine gains exactly that edge, and the
   machine module and CHECK templates move together as §21.37 requires.
   A decomposition-free spec keeps the single-task flow byte-identical.

4. **Dependencies are ordering gates enforced at claim time (§6.3,
   §8.3).** `task_dependencies` edges, acyclic at write time. Blocked
   is a derived predicate — an unmerged dependency exists — never a
   stored lifecycle state, enforced in the same server-side layer as
   hold and the self-review guard; surfaces name the blocking tasks.
   Merge of a dependency re-nudges dependents' dispatch. The §21.31
   one-queue contract is untouched: no priority field, FIFO and the
   reserved review slot unchanged, orders queue openly with the reason
   visible. §8.3's stacked branches (cut from the dependency's branch,
   rebase on parent landing) are explicitly deferred: v1 dependents
   branch from a base that already contains their dependencies, which
   captures the ordering value without rebase-cascade coordination
   against agent-owned worktrees; reintroduction requires an amendment.

5. **Planning sessions: the factory owns planning (§9, §13.3,
   §17.3).** An in-product chat — the fifth intake surface — producing
   two artifact types: requirement documents (change 7) and blueprints.
   The agent
   runs in-process on the factory credential like triage, with read
   tools over the requirement corpus, approved specs, artifacts, and
   links, plus
   draft/revise/finalize; finalizing a blueprint creates the parent
   task and spec
   version through unchanged §4.1 validation and the unchanged §13.1
   spec gate — a session grants no approval authority. The transcript
   archives as an artifact linked to what it produced: the rationale —
   alternatives rejected, constraints surfaced — becomes lineage
   rather than evaporating in an external chat window. Rationale for
   in-product rather than delegated: context-switching to external
   agent sessions is a poor operating experience, planning quality is
   core factory value, and externally-drafted blueprints arrive with
   their provenance amputated. The §21.4 boundary is intact —
   implementation and review stay delegated; MCP (`create_task` +
   `submit_spec`) remains the headless twin converging on the same
   blueprint contract. Transport is the AI SDK UI-message protocol over
   SSE served by `conveyord`; the UI uses the stock chat component
   family. No new tier, no sidecar (§17.0 unchanged).

6. **Lineage links (§4.2 item 4, §16).** One polymorphic `links` table:
   typed edges deposited only by pipeline machinery at stage
   transitions — requirement `serves`/`relates_to` edges included —
   carrying `created_by_event` provenance, rebuildable as
   projections of `events`. Two layers by design: the append-only
   lineage graph (requirement → blueprint → work order → code
   → evidence) and the mutable alignment layer, maintained only where
   the pipeline already stops (review, planning reads, monitor drift)
   with in-repo `REQ-n` citations as the refactor-surviving anchor.
   Explicitly rejected: a graph database (Postgres per §17.0 at this
   scale), free-standing volunteered edges (they rot), asserted
   file/symbol maps (derived cache only), and embeddings as primary
   structure (recall without provenance).

7. **Requirements become living intent documents; the curated tree
   retires (§4.2, §13.3, §16).** The `features` tree was the factory's
   only hand-curated structure — filed taxonomy with no lifecycle, no
   content obligation, and no pipeline write-back — and dogfooding
   showed it accretes without driving anything, the failure mode this
   amendment's own doctrine predicts for volunteered structure. Its
   replacement is authored, not derived, because the intent layer needs
   a home in the operator's own language: a **requirement** is a
   versioned document generated from stated intent by the planning
   agent and revised the same way — plus by drift reconciliation, whose
   `requirements_amended` outcome now proposes a new requirement
   version for confirmation. Versioned and confirmed, never gated (the
   approval gate stays on blueprints); flat, never a hierarchy
   (`relates_to` links only); `REQ-n` statement IDs give citations,
   acceptance criteria, and verdicts a stable target that outlives any
   blueprint. Blueprint linkage is optional and loose by design —
   `serves` edges proposed by machinery (planning context, triage
   suggestions) and confirmed by humans; unlinked blueprints are legal
   forever; a requirement's accumulated blueprints are its delivery
   history. Staleness is surfaced per requirement, not discovered.
   **No epic entity**: the lineage chain is requirement → blueprint →
   code → evidence; the blueprint parent task remains the mechanical
   carrier (authoring, approval, events, child rollup) with no
   conceptual elevation and no separate surface. Migration: `features`
   nodes that accumulated approved specs seed requirement documents,
   empty nodes drop, `tasks.feature_id` assignments convert to history
   links, and `triage.feature_suggested` retires in favor of triage
   proposing a requirement link.

8. **Deferred phases renumber.** Memory → Phase 7 (rescoped in §15.1 to
   recall over the lineage graph, vector retrieval as secondary index;
   MCP transport decision of §21.12 unchanged); flywheel/graduation →
   Phase 8; managed-execution reintroduction and cross-repo
   coordination → Phase 9; enterprise SSO/SCIM/RBAC/HA → Phase 10.
   Body references updated in §§1–20; §21 entries keep their original
   numbering as history.

9. **Data model (§16).** New: `requirements` / `requirement_versions`,
   `task_dependencies`, `planning_sessions`,
   `links`; `tasks` gains the parent-blueprint link with `(spec
   version, SUB-n)` origin; `features` and `tasks.feature_id` are
   deprecated per change 7's migration. `events` remains append-only and
   authoritative; every new edge type commits with its event.

10. **Out of scope for Phase 6**, restated to prevent drift: branch
    stacking and rebase tasks (change 4); automatic edits to confirmed
    requirements or approved specs (§4.2 reverse-sync boundary
    unchanged — reconciliation proposes, humans confirm); memory
    MCP tools (Phase 7); a task priority field (§6.3, still
    amendment-gated); cross-repo dependency edges (Phase 9, §7.2);
    requirement hierarchy or any curated taxonomy (change 7); an epic
    entity or surface (change 7); any
    in-product implementation or review execution (§21.4).

---

### 21.47 v2.7 — Dependency semantics: unsatisfiable edges, cross-repo, stage scope, clock suspension (July 29, 2026)

Adopted from the Phase 6.1 implementation review (task `260729-b7df17`,
PR #160): the review surfaced four cases §6.3/§8.3 never contemplated,
and the operator decided each. Four changes:

1. **Unsatisfiable dependencies are surfaced and operator-resolved,
   never silent (§6.3, §13.2, §13.3).** The v2.6 predicate ("unmerged
   dependency exists") is correct but incomplete: a dependency that
   reaches a terminal state other than `merged` (cancelled, rejected)
   can never satisfy the edge, and the review found this produced a
   permanent, indistinguishable block with no escape hatch. Now: the
   dependent gets a `task.dependency_unsatisfiable` event, renders
   distinctly from ordinary waiting, and enters the needs-operator
   tray. Resolution is an explicit audited operator action — remove
   the edge (`task.dependency_removed`; UI/CLI/REST, deliberately not
   MCP, matching the cancel precedent: agents file work, humans decide
   its fate) or cancel the dependent. No automatic pruning: a
   dependency dying is a planning fact the operator should see, not
   one the machinery quietly absorbs.

2. **Dependency edges may span repositories (§8.3, §16).** The intake
   path's same-repo restriction is dropped; edges are workspace-scoped
   and acyclic, nothing more. §4.1's canonical decomposition example
   (`api` → `web`) was already cross-repo, and blueprint
   materialization already wrote such edges — intake and
   materialization now share one rule. Cross-*workspace* edges remain
   rejected (Phase 9 territory, §7.2).

3. **The dependency gate applies to implementation work orders only,
   at claim time only (§6.3).** Spec orders on a dependent stay
   claimable while dependencies are unmerged — dependencies order code
   landing, not design; a dependent's spec work is exactly what an
   operator wants underway while the dependency merges. And the gate
   is evaluated only at claim: an already-claimed order is never
   rejected mid-flight for blocking, so an edge added after claim (a
   re-materialization, a manual link) cannot invalidate in-progress
   work at `report_progress`/`submit_for_review` time.

4. **A blocked task's queue-timeout clock is suspended (§6.3).** v2.6
   promised blocked orders keep their original queue entry, but the
   §6.3 `work_order_queue_timeout` backstop (default 24h) would stale
   any order blocked longer than the timeout — guaranteed for serial
   chains under human-gated cadence — and recovery resets FIFO
   position, contradicting the promise. Resolution: while the blocking
   predicate holds, the queue clock does not run; it resumes on
   unblock. The backstop still protects unblocked orders unchanged.

Out of scope, unchanged: the derived-predicate design, one queue, no
priority field, FIFO ordering, hold semantics, branch-stacking
deferral, and every other §21.46 boundary.

---

### 21.48 v2.8 — Worktree containment, terminal cleanup, and checkout repository identity (July 29, 2026)

Accepted after task worktrees accumulated beside unrelated operator
projects and out-of-band directory removal left stale primary-checkout
registrations. Three changes:

1. **Implicit task worktrees are contained (§8.2).** The deterministic
   default moves from `../<repo>-task-<task-id>` to
   `../conveyor-worktrees/<repo>-task-<task-id>`. The container name is
   fixed; workspace-level `worktree_root` configuration is deliberately
   not introduced. Repository and task ID remain one safe path component,
   and canonical path resolution must keep the container directly beneath
   the primary checkout's parent and the destination inside it. `--path`
   remains the explicit placement override. Existing registered clean
   worktrees remain reusable by assigned branch at their current location;
   no migration, relocation, or bulk deletion is authorized.

2. **Checkout verifies repository identity before Git mutation
   (§8.2).** The assigned workspace repository's canonical configured
   identity is compared with the current checkout's unambiguous normalized
   `origin` before fetch, ref inspection, worktree reuse, or creation.
   Equivalent standard GitHub HTTPS and SSH forms compare equal. Missing,
   unreadable, ambiguous, or different identity fails closed with assigned
   and current identity context. Worker-environment and authenticated
   task-lookup assignments use the same rule; `--path` never bypasses it.
   The assigned repository name remains a directory label, not proof of
   identity.

3. **Terminal cleanup and primary-registration pruning are reconciled
   (§§8.1–8.2).** Every merged or closed task remains eligible on
   reconciliation passes until cleanup succeeds or is a no-op. The
   operation removes only a registered, clean, non-primary task worktree,
   retains every branch, refuses dirty or locked worktrees, treats missing
   registrations as success, and prunes registrations whose directories
   are already missing. It never resets, rebases, stashes, force-updates,
   or otherwise rewrites agent-owned history. Failures are logged with
   workspace, task, repository, and branch context and retried without
   changing terminal task state or blocking other repository work.
   Background `git worktree prune` continues across the control-plane
   cache and now also covers every available configured primary checkout;
   per-repository failures are isolated, and live worktrees, branches, and
   primary checkouts are outside prune semantics.

Non-goals: configurable implicit roots; migration or bulk deletion of old
worktrees; branch deletion or history rewriting; cleanup of non-terminal,
dirty, locked, or primary worktrees; and changes to worker protocol, task
lifecycle states, merge semantics, branch naming, or explicit `--path`
placement.

---

### 21.49 v2.9 — Blueprint presentation surface (July 30, 2026)

Adopted from operating the first materialized blueprint (task
`260730-c6750c`, the Phase 6.2 batch): rendering the anchor as an
ordinary task misdirects — "Queued" reads as waiting-for-a-worker,
Hold/Checkout and the assigned branch are inert on a task that takes no
orders and never receives a commit, and a stage column implies pipeline
motion the anchor will never make. The anchor is a contract with a
progress bar, not a work item. Three changes:

1. **Blueprints get a dedicated presentation surface (§13.3).** On the
   planning side, next to requirements: a list of blueprint anchors
   with child rollup and delivery state, and a detail view that leads
   with the approved spec, the materialized children in dependency
   order with per-child state, the batch timeline, and lineage
   (including `serves` links once Phase 6.2 lands). Anchor detail
   suppresses inert task affordances — checkout, assigned branch,
   hold — while cancel remains available as lifecycle.

2. **Anchors leave the stage-grouped feed.** The activity board is a
   view of work — things agents claim and execute; blueprints are
   intent artifacts. Children remain ordinary tasks in the feed; the
   anchor is reached through the Blueprints surface and through its
   children's parent references.

3. **Presentation only; the entity bar stands.** This supersedes the
   "no separate surface" clause of §21.46 change 7, which was aimed at
   (and still bars) an epic *entity*: no new table, no new lifecycle,
   no new noun in the data model. The blueprint parent task remains
   the sole carrier — authoring, approval, events, close — and
   "Blueprint" was already first-class §2 vocabulary (§21.46). What
   changes is which room it is shown in.

---

*End of specification. v2.9 accepted July 30, 2026 — the v2.0
consolidated restatement of v1.0–v1.40 (§21.40), supervision hygiene
(§21.41), worker-side first-activity liveness (§21.42), the completed
Phase 5.3 review projection (§21.43), Phase 5.4 verification evidence
(§21.44), and Phase 5.6 monitor and advisory hints (§21.45). §21.46
closes Phase 5 and accepts Phase 6 — planning & the knowledge graph:
requirement documents, blueprint materialization, dependency-gated
claiming, planning sessions, and lineage links — renumbering the
deferred phases. §21.47 clarifies dependency semantics from the Phase
6.1 implementation review: unsatisfiable edges surfaced with an audited
operator unlink, cross-repo edges legal, the gate scoped to
implementation claims, and queue-clock suspension while blocked. The
§21.48 amendment contains implicit task worktrees, verifies checkout
repository identity, and reconciles terminal cleanup plus
primary-checkout pruning. §21.49 moves blueprint anchors onto a
dedicated presentation surface — intent artifacts beside requirements,
out of the stage-grouped feed — with the data model unchanged. The
body (§§1–20) is the normative
authority; §21 is the change record. Subsequent changes proceed by
amendment with version bumps.*
