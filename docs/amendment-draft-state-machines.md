# DRAFT amendment — §21.37 v1.38 — Canonical lifecycle state machines

**Status: accepted July 22, 2026 — appended to
[conveyor-spec.md](../conveyor-spec.md) as §21.37 (v1.38).** The spec text
is authoritative; this file is retained as the review record, including
the non-normative code inventory appendix that was dropped on acceptance.

Companion draft: [amendment-draft-command-plane.md](amendment-draft-command-plane.md)
(§21.38, serialized task command plane). This amendment is independently
landable; the companion assumes it.

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
   | W6 | `claimed` | `submit_for_review` | `submitted` | implementation delivered (§17.4; evidence gate arrives in Phase 5.4) |
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
by this amendment — serialization ergonomics are the companion draft
(§21.38), which assumes this amendment but is separately acceptable.
The working breakdown slots into docs/phase5-plan.md ahead of Phase 5.2;
this section is authoritative.

---

## Appendix (non-normative): current-code inventory backing the motivation

Retained for reviewer convenience; drops away on acceptance.

- Task-state write sites: `internal/dispatch/dispatch.go` (~20, incl.
  `completeOutput` switch at :695, `HandleIntervention` at :892, merge/
  refresh/conflict paths at :989/:1190/:1261/:1318), `internal/dispatch/
  river.go:357,:360`, `internal/httpapi/server.go:164,:466`,
  `internal/workorder/service.go:578`, claim-flow internals in
  `internal/store/postgres/store.go:1179,:1293`.
- Unguarded writers: `UpdateTaskState` (`postgres/store.go:518`),
  `SetTaskTransition` (`:542`), `Dispatcher.transition`
  (`dispatch.go:882`), `transitionWorkOrderTx` (`postgres/store.go:1932`).
- Duplicated guards: `workorder/service.go:811` (`authorized`), `:834`
  (`enforce`), `:685` (`authorizedForAwait`), `postgres/store.go:1965`
  (`UpdateWorkOrder` re-checks), memory twins in `store/store.go:588-617`,
  SQL predicates at `postgres/worker.go:141,:167,:219`,
  `postgres/store.go:1918`.
- Schema: `tasks.state` unconstrained (`migrations/001_phase2.sql:66`);
  work-order CHECK at `migrations/009_phase47_mcp.sql:52` widened by
  `migrations/013_work_order_clocks.sql`.
- Event-fold readers relying on legal sequences:
  `currentMergeConflictEpisode` (`dispatch.go:1080`),
  `latestReviewResult` (`workorder/service.go:707`),
  `reviewedHeadFromEvents` (`dispatch.go:941`).
