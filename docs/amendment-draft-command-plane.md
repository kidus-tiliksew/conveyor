# DRAFT amendment — §21.38 v1.39 — Serialized task command plane (actor ergonomics)

**Status: accepted July 22, 2026 — appended to
[conveyor-spec.md](../conveyor-spec.md) as §21.38 (v1.39).** The spec text
is authoritative; this file is retained as the review record, including
the non-normative code inventory appendix that was dropped on acceptance.

Depends on the companion draft
[amendment-draft-state-machines.md](amendment-draft-state-machines.md)
(§21.37, canonical lifecycle state machines): the command plane is the
enforcement point where §21.37's tables are consulted. §21.37 is landable
alone; this amendment is not meaningful without it.

---

### 21.38 v1.39 — Serialized task command plane (July 22, 2026)

Conveyor's concurrency substrate is shared Postgres — advisory locks, row
locks, transactions, leases — and that substrate is correct for a factory
whose actors are external, crash-prone, operator-owned agent processes.
At the system boundary the design is already actor-shaped: work orders
are messages, workers are actors, claim/renew/release is the mailbox
protocol (§17.4, §21.9), and reconciliation is supervision. This
amendment does not change that substrate and explicitly rejects an
in-process actor runtime.

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

## Appendix (non-normative): current-code inventory backing the motivation

Retained for reviewer convenience; drops away on acceptance.

- `WithTaskLock`: `internal/store/postgres/store.go:67-89` (session
  advisory lock, dedicated pooled conn, hijack-on-unlock-failure);
  memory twin `internal/store/store.go:702`. Call sites:
  `internal/dispatch/dispatch.go:141,:1010,:1141,:1225` — versus ~35
  task-state write sites (inventory in the §21.37 draft appendix).
- Read-path mutation: `refreshWorkOrders`
  (`postgres/store.go:1872-1915`) invoked from `GetWorkOrder:1526`,
  `ListWorkOrders:1537`, `ListTaskWorkOrders:1557`.
- Dual mailbox: in-memory `chan queuedTask` buffered 64
  (`dispatch.go:49`, drained at `:102-118`); River path
  `internal/dispatch/river.go` behind `UseDurableQueue()`.
- Existing per-order serialization this amendment builds on rather
  than replaces: claim-token + `FOR UPDATE` in
  `postgres/worker.go:119,:160` and `postgres/store.go:1960`;
  xact-scoped advisory locks precedent in
  `postgres/setup_change.go:30,:52`.
