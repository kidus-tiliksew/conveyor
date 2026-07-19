# Phase 5 plan: worker execution & autonomy (phases 5.1–5.5)

The roadmap authority is [conveyor-spec.md](../conveyor-spec.md) §19 (v1.28),
amended by §21.12; the Phase 5.1 execution contract is fixed by §21.13 and
its harness-template expansion and transport rules are clarified by §21.14
and §21.20; worker-attempt recovery is fixed by §21.21, portable review
publication by §21.22, terminal review-round recovery by §21.23,
reconnect-safe worker plus interrupted-seat recovery by §21.26, and
execution setups by §21.27. These are
authoritative over this file. This document is the working breakdown: what
each phase contains, its dependencies, and its exit criterion. All of it is
post-Beta scope; the gate has cleared — **Beta was achieved July 15, 2026** (§19 exit
criterion met over the Manual MCP pull flow), so this plan is active.

**The milestone:** Auto mode. A GitHub issue becomes a merged PR with no
human touch except the gates the operator chose to keep — the worker claims
the work orders, the operator's own harnesses do the work, and the factory
coordinates everything on GitHub.

**The boundary that does not move:** the worker is not the sandbox plane
returning. No containers, no adapter interface, no credential pooling. It
runs the operator's own harness CLIs (`claude -p`, `codex exec`) under the
operator's own login on the operator's own machine, over the unchanged §17.4
MCP work-order lifecycle. What is given up is recorded plainly per §21.12
change 1: unconfined execution on a real machine, mitigated by explicit
enrollment, Auto-only claiming, dedicated worktrees (§21.8), and
gates-on-by-default.

---

## Phase 5.1 — Worker & execution modes

*Proves: unattended execution on operator hardware (spec §21.12 changes 1–3).*

Suggested order:

1. **Harness registry + stage routing** (data-only, unblocks everything
   after it): workspace config gains `harnesses: [{name, mcp_transport,
   command, model_args, effort_args?, probe_command, probe_timeout}]` — argv arrays, never
   shell-evaluated. Per §21.14, `command` contains exactly one `{prompt}`
   and one `{mcp_config}`; `model_args` is appended to it and may use only
   `{model}`; `effort_args` maps `low`, `medium`, and `high` to literal,
   placeholder-free adapter argv appended only when a seat requests that
   exact effort (§21.19); `mcp_transport` is `json_file` or the secret-free
   `toml_override` (§21.20); `probe_command` is standalone and accepts no placeholders.
   Placeholders substitute as whole elements and invalid field/placeholder
   combinations are rejected at write time — under the standard §21.3
   mechanics: validated writes, `config.updated` events, hot reload. The
   **implement and review** stage routes select a harness by registry name
   (§21.13 change 1; an `in_process` review route takes none; 5.2 panel
   seats later override the review route per seat); validation enforces
   referential integrity both ways. With one registered harness the route
   field may be omitted and inherits it; with several it is required. A
   route's harness binds worker dispatch only — a Manual claim cannot be
   forced through a harness — and is surfaced enforced vs. advisory
   exactly like `model_enforcement`. No per-task harness override.
   Registry editor on the Workspace page. No adapter interface; §5.1 stays
   retired.
2. **Execution modes + gate toggles:** task-level `mode: auto | manual`;
   workspace toggles for spec approval and merge approval with per-task
   override (Auto + both gates on is the shipped default); legacy L0–L3
   display mapping per §21.13 change 7 (L3 ≈ Manual; L2 ≈ Auto + both
   gates; L1 ≈ Auto + merge gate only; L0 ≈ Auto + gates off) — existing
   records keep their levels and in-flight legacy tasks finish under
   them; mode chip replaces the
   escalation badge in the feed and task detail; `conveyor task new --mode`
   replaces `--level`; MCP `create_task`'s optional escalation level becomes
   an optional mode. Gate behavior follows the §21.13 change 7 truth
   table: the spec gate forces the spec stage when on and auto-approves
   generated specs when off; the merge gate holds approved reviews for a
   human when on and auto-merges on green when off; effective mode and
   gates are resolved and persisted at intake, so workspace edits never
   change an in-flight task.
3. **Worker enrollment + heartbeat:** a short-lived, single-use pairing
   token (issued via UI/CLI) is exchanged at enrollment for a revocable,
   workspace-scoped worker credential stored server-side only as a hash;
   revocation is an operator action on the Workspace page. Liveness is a
   server-issued lease (default 15s, §21.13 change 3) refreshed by
   heartbeat;
   heartbeats carry harness probe results (binary present, authenticated,
   trivial invocation succeeds); worker and per-harness health render on
   the Workspace page.
4. **Worker claim loop:** `conveyor worker run` long-polls
   `list_work_orders` / `claim_work_order` for **Auto orders only** (one
   queue — any authenticated agent may still claim any order manually),
   spawns the configured harness headless with the Conveyor MCP config
   attached, and supervises asynchronously with **stage-aware capacity**
   (§21.13 change 6): configurable implement concurrency plus at least one
   reserved, prioritized review slot — implementers blocking in
   `await_review` must never occupy every slot, or the review orders that
   would unblock them sit unclaimed. Each order runs under a fresh
   identity/token pair so the self-review guard and independence labels
   hold. Supervision means lease renewal while a child is alive,
   exit-status capture, durable bounded 1s/2s/4s child retry, and active claim
   release on failure. Release or lease expiry ends that execution attempt,
   clears its active clocks/ownership, and requires the audited recovery path
   after cancellation, expiry, or retry suppression — additive
   worker-control endpoints (§21.13 change 4); the agent-facing §17.4
   lifecycle is unchanged, and renewal never extends the attempt's §21.21
   execution deadline. The spawned session performs the standard flow itself
   (`conveyor checkout` → §21.7 branch adoption → implement → push →
   `submit_for_review` → `await_review`). §21.21 lease-expiry cleanup and
   audited recovery are the backstop when a worker dies outright.
   `conveyord --worker-retry-delay` and `--worker-retry-max` configure the
   bounded delay window; startup rejects a maximum below the initial delay.
5. **Health-gated Auto:** Auto offered only while a worker holds a live
   liveness lease and **every harness referenced by the applicable
   implement/review routes** probes healthy (§21.13 change 3; an
   `in_process` review route is exempt) — an unrelated healthy harness
   must not enable Auto while the routed one is down. While unhealthy, an
   explicitly requested `mode: auto` is rejected (409 / MCP error) and a
   workspace-default Auto resolves to Manual with a recorded fallback
   event; the "default new tasks to Auto" toggle greys out. Nothing queues
   silently against a dead worker.

**Exit criterion:** a task created in Auto mode is claimed and completed
end-to-end by the worker — checkout, implement, push, submit, review round,
merge — with no human touch except the configured gates; killing the worker
makes Auto unavailable within one liveness lease (explicit Auto refused,
default Auto falling back to Manual with a recorded event); the Manual
flow is byte-for-byte unchanged.

## Phase 5.2 — Adversarial review panel

*Proves: reviewer independence, enforced rather than asserted (§21.12
change 4). Depends on 5.1 for the enforcement path.*

1. **Panel config:** workspace review setting becomes
   `{seats: [{model, harness?, effort?}]}` — operator-chosen count, model
   pinned per seat, with optional vendor-neutral `low | medium | high` effort.
2. **Panel dispatch:** `submit_for_review` enqueues one review work order
   per seat; the self-review guard applies to every seat; seats must be
   distinct sessions from one another.
3. **Aggregation:** unanimous-approve. `await_review` returns once all
   verdicts arrive; any `changes_requested` bounces with all reviewers'
   feedback merged into one structured round — one bounce against the §21.2
   cap (retuned to check-in semantics by §21.17) regardless of panel size.
4. **Enforcement labels:** independence labels gain
   `model_enforcement: worker-pinned | self-reported`; worker-executed seats
   are invoked with their pinned model and labeled enforced; the review card
   and timeline render the difference honestly.

**Exit criterion:** a two-seat panel with different pinned models produces
two recorded verdicts with enforced labels; one `changes_requested` verdict
bounces the task with merged feedback delivered to the waiting implementer;
unanimous approval advances to merge.

## Phase 5.2.1 — Execution setups

*Proves: per-task-class execution contracts without per-task free-form
overrides (§21.27). Depends on 5.1 (harness registry, modes, health
gating) and 5.2 (panel seats); parallelizable with everything after —
config, intake, and UI work with one dispatch-logic change.*

Suggested order:

1. **Setup schema + normalization** (data-only, unblocks the rest):
   workspace config gains `setups: [{name, execution_settings, review}]`
   plus `default_setup`, under the standard §21.3 mechanics (validated
   writes, `config.updated`, hot reload). Normalization folds a v1.27
   document's top-level `execution_settings`/`review` into a single setup
   named `default`; legacy top-level fields stay readable as a projection
   of the default setup (§21.18 change 2 pattern, extended). Each setup
   validates independently under the existing §21.18–§21.20 rules against
   the shared harness registry; harness delete-protection extends to
   references from any setup. The workspace must always retain ≥1 setup
   and a valid `default_setup`.
2. **Intake selection + freeze:** REST `createTaskReq` and MCP
   `create_task` gain optional `setup`; unset resolves to `default_setup`;
   unknown name is 400 / MCP error, never a silent fallback. The resolved
   setup is normalized and persisted **by value** on the task at intake
   (the §21.13 change 7 rule extended from mode/gates), so later setup
   edits, renames, and deletes never touch in-flight tasks. Task records
   carry the setup name for display plus the frozen contract for dispatch.
3. **Dispatch sourcing:** implementation dispatch and `BuildReviewRound`
   read harness/model/effort/timeout from the task's frozen setup instead
   of the workspace singleton. Work-order snapshots, claim validation,
   self-review guard, and enforcement labels are mechanically unchanged
   (§21.27 change 5) — this step is a sourcing swap, not a protocol
   change.
4. **Setup-scoped health gating:** the Auto-availability check and
   intake-time auto→manual fallback evaluate the harness set required by
   the *task's* setup (implementation harness + effective seat harnesses;
   `in_process` review exempt) rather than one workspace-wide route set.
   An unrelated setup's broken harness must not disable Auto for tasks
   that don't use it. Serviceability reporting becomes per-setup; the
   fallback event records which setup's requirements failed.
5. **Operator surface:** Workspace UI setup manager — create, duplicate,
   edit, set default, delete — rendering the existing contextual layout
   per setup; task intake (UI, CLI `conveyor task new --setup`, REST, MCP)
   gains a setup selector defaulting to the workspace default, composition
   shown as secondary detail (tooltip/expand), not inline jargon; task
   detail shows the frozen setup name and composition.

**Exit criterion:** two setups with different implementation harnesses and
panels; tasks created under each run end-to-end under their own contract
with correctly labeled seats; editing a setup mid-flight changes nothing
for the in-flight task; breaking one setup's harness disables Auto only
for tasks selecting that setup, with the other setup's Auto unaffected; a
v1.27 single-config workspace upgrades with byte-for-byte identical
behavior as setup `default`.

## Phase 5.3 — GitHub coordination

*Proves: the task's trail is legible on GitHub alone (§21.12 change 5).
Independent of 5.1/5.2 — parallelizable with 5.2.*

1. **Issue on spec approval:** the factory creates a GitHub issue carrying
   the approved spec (intent + acceptance criteria) and links it to the
   task; a task that originated from an issue (§9) updates that issue
   instead of duplicating it; the eventual PR carries `Closes #N`.
2. **Verdict mirroring:** review verdicts and their resolutions post to the
   PR, extending the existing aggregate commit-status + factory-comment machinery into a
   complete review trail; redirect rounds show as review threads with their
   resolutions.

PR creation is deliberately *not* part of this phase: the factory opens the
PR at `submit_for_review`, the behavior already specified and implemented
(§21.4 change 4, §21.7 change 5). Draft-PR-on-first-push was dropped by
§21.15 — in the push-once-then-submit flow the draft window is seconds and
its machinery (push-event matching, draft→ready flips, orphan cleanup)
serves nobody. The reaffirmed boundaries hold: no ref creation at intake
(§21.7), no PR before the first push, no stub commits.

**Exit criterion:** a task's full lifecycle is reconstructible from GitHub
alone — issue with spec → PR at submit → review verdicts with
resolutions → merge that closes the issue.

## Phase 5.4 — Verification evidence

*Proves: reviewers confirm evidence rather than reproduce behavior (§21.12
change 6). Follows 5.3 for the PR-mirroring path.*

1. **Evidence gate:** workspace toggle; with it on, `submit_for_review` is
   refused until at least one verification-evidence artifact (screenshots or
   short recording of the exercised change) is attached via the §21.4
   artifacts machinery.
2. **Evidence surfaces:** evidence artifacts listed in review work orders,
   rendered on the review card, mirrored to the PR.

The independent verification agent (scripted flows + computer-use verdicts
per acceptance criterion, §12) remains Phase 8; this phase is deliberately
just the evidence-attachment contract.

**Exit criterion:** with the toggle on, a submit without evidence is refused
with a clear error; with evidence attached, the reviewer sees it on the
review card and the PR without leaving either surface.

## Phase 5.5 — Worker service packaging

*Proves: Auto capacity survives reboots without operator ritual (§21.16).
Placed after 5.4 by deliberate prioritization; technically independent of
5.2–5.4.*

1. **`conveyor worker install`:** writes and loads a launchd agent (macOS)
   or systemd user unit (Linux) wrapping `conveyor worker run` — restart on
   failure, start on boot/login. Requires existing enrollment; refuses with
   guidance when the credential file is absent. Records the unit path.
2. **`conveyor worker uninstall` / `status`:** uninstall stops and removes
   the unit idempotently; status reports service state, enrollment
   identity, last heartbeat, and per-harness probe results.
3. **Logging:** service stdout/stderr to a documented log location,
   surfaced by `status`.

Supervision only (§21.16): no new protocol, endpoints, or behavior beyond
the foreground command the service wraps; pairing/enrollment unchanged;
interactive `worker run` stays fully supported.

**Exit criterion:** on a rebooted machine with the service installed, Auto
mode is available again with no operator action — the worker heartbeats
within one liveness lease of login; `install`/`uninstall` round-trip
cleanly, with `status` accurate in both states.

## Phase 5.6 — Platform agents & policy

*Renumbered from Phase 5 (§21.12 change 8) and from 5.5 (§21.16); scope
unchanged, deliberately
sequenced after the worker: monitor-filed tasks plus Auto dispatch is the
original autonomous loop completed.*

- Monitor agent: CI failures and post-merge signals → tasks; out-of-pipeline
  reverse sync (§4, §4.2).
- Repo-resident `.conveyor/` hints (verify commands, triage area hints —
  advisory only, never capability grants).

---

## Deferred, explicitly

- **Memory (Phase 6):** transport decided — control-plane MCP tools
  (`get_memories`, `store_memory`) on the §17.4 server, available to any
  connected session; the worker is uninvolved (§21.12 change 7). Scope
  otherwise unchanged and not pulled forward.
- **Independent verification stage (Phase 8):** pull forward only if 5.4's
  evidence contract proves too gameable.
- **Graduation (Phase 7):** operates on gate toggles and mode defaults
  instead of ladder levels when it arrives.

## Standing notes

- **Build it through the factory.** Every 5.x work item that is well-specified
  should be filed as a Conveyor task and run through the Beta pipeline —
  the worker should be the factory's own product before it is the factory's
  own dispatcher.
- **Parallelization:** 5.1 → {5.2 ∥ 5.3} → 5.4 → 5.5 → 5.6; 5.2.1
  (setups) follows 5.2 and runs parallel with any of 5.3–5.5.
- **Honesty labels are load-bearing:** `dispatch: worker`,
  `model_enforcement`, and `confinement: none` are the operator's view into
  what the automation actually guarantees. They are not optional polish.
- **Manual stays first-class:** the worker never becomes a requirement for
  operating Conveyor; every flow must remain completable by an
  operator-attached agent over plain MCP.
