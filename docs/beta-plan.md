# Beta plan: phases 3–4.7

The roadmap authority is [conveyor-spec.md](../conveyor-spec.md) §19 (v1.6),
restructured by §21.2 and extended by §21.3–§21.6. This document is the
working breakdown: what each pre-Beta phase contains, its dependencies, and
its exit criterion. Phases 1–2 are complete and validated; nothing here
reopens them.

**The milestone:** Beta = Conveyor develops Conveyor. A GitHub issue on the
Conveyor repository flows issue → triage → approved spec → implementation →
PR → (redirect rounds) → merge, with human involvement limited to gate
decisions and merges, all taken through the UI or CLI.

**Beta exit criterion (spec §19, restated by §21.4):** five consecutive real
tasks where the operator's own agent claims each work order over MCP, at
least one completing a `changes_requested` round in-session, zero manual git
operations outside the implementing agent's workflow, all human actions
through the UI or CLI.

Pre-Beta is four phases — the pipeline, the UI to operate it, the
configuration surface to steer it (§21.3), and the MCP execution pivot that
changes who runs the code (§21.4). Everything else (monitor agent, memory
store, flywheel) is sequenced post-Beta, built with and increasingly by the
factory itself.

---

## Phase 3 — Full pipeline

*Proves: the full pipeline runs.*

**Complete, including the live dogfood exit run.** Operational steps and the
recorded task/PR evidence are in [phase3.md](phase3.md).

The structural core is multi-stage orchestration; the agents are
configurations on top of it. Suggested order:

1. **Dogfood enablement first** (small, unblocks live testing of everything
   after it):
   - Per-repo sandbox image override (`repos[].image`).
   - `images/conveyor-dev/`: base image + Go toolchain (match `go.mod`
     version) + node, so sandboxes can run `make build/test/vet` on Conveyor.
2. **Multi-stage orchestration.** Task state machine: stage sequence per
   escalation level (L2: triage → spec → gate → implement → gate; lower
   levels skip stages per §13.1). Per-stage jobs with per-stage routing
   (`Router.Select` already takes a stage), per-stage gates reusing the
   intervention machinery, and **bounded bounces** (stage N fails back to
   stage N−1 with a structured reason, capped, every bounce recorded — §4).
3. **Role prompts as files** (proto-pack, §2.2): `pack/roles/*.md` checked
   in and loaded per stage, replacing the hardcoded `buildPrompt`. No
   versioning/pinning machinery yet (that is Phase 7); the point is that
   prompts become reviewable artifacts the self-improvement engine can
   later diff against.
4. **Triage agent.** Classification (bug/feature/chore), automatability
   estimate, route decision (straight to implement / spec first / human /
   parked). Strong model tier per §4. Output schema-validated; malformed
   output bounces invisibly.
5. **Spec agent + §4.1 machinery.** Markdown spec with validated
   `conveyor:acceptance` and `conveyor:decomposition` fenced blocks;
   malformed blocks auto-bounce. Specs stored as versioned artifacts;
   the *approved* version is injected into the implement prompt as the
   contract. Decomposition blocks parse and display but do not materialize
   stacked tasks yet (single-repo Beta; materialization joins Phase 8's
   multi-repo work).
6. **Code-review agent.** Reviews the diff against the approved spec and
   `Non-goals` (scope-creep reason codes, §4.1 rule 4); harness routed
   different-from-implementer (§5.3); bounces to implement with structured
   feedback.
7. **PR review comments → redirect** (§9): review comments on Conveyor PRs
   convert to redirect interventions, so diff review happens on GitHub
   without losing the feedback loop.
8. **Safety for unattended runs:** job wall-clock timeouts. Phase 3
   historically included a spending circuit breaker; §21.6 removes that
   allocation and enforcement surface while preserving timeouts.

**Exit criterion:** one real task traverses triage → spec (approved via
API/CLI) → implement → code-review bounce → PR on the Conveyor repo, with
the sandbox running Conveyor's own test suite.

## Phase 4 — UI rewrite → Beta

*Proves: Beta readiness.*

Ground-up rewrite of `web/` — polished, professional, enticing — on
Tailwind + shadcn/ui (the §17.0 stack; TanStack Router + Query stay).
Designed against the full pipeline's data model from Phase 3; post-Beta
phases extend it (approval cards with Phase 5, memory surfaces with
Phase 6) rather than reshape it. Surfaces, per §13.3:

- **Stage-grouped feed:** collapsible sections per pipeline stage with
  counts; escalation badges, provenance chips, recency; "Needs attention"
  as the single alarm color on the page.
- **Costed event timeline:** per-stage entries with agent summary,
  duration, observational cost, harness/model/auth-mode, and the
  acceptance-criteria badge fed by the §4.1 blocks. The task header no longer
  shows spending allocation or remaining balance (§21.6).
- **Review in place:** approve / reject / redirect-with-comment /
  pull-to-local on the task detail; spec review card (rendered markdown +
  acceptance criteria checklist); diff summary with a deep link to the PR.
- Live updates over the existing SSE endpoints into the Query cache.

**Exit criterion:** the §13.3 surfaces render the full pipeline's data model
and all review actions work in place. *(Met; Beta entry moved to the Phase 4.5
exit by §21.3.)*

## Phase 4.5 — Dynamic workspace configuration → Beta

*Proves: the factory is steerable from its own control surface (spec §21.3).*

Workspace-scope config moves from boot-loaded `conveyor.yaml` into Postgres,
mutable through the authenticated API and the Workspace UI. Suggested order:

1. **Storage + bootstrap.** `workspaces.config_yaml` + `config_version`
   become the running truth; generalize `BootstrapConfig` to import the
   file's workspace sections on first boot against an empty row, ignore
   them (with a startup notice) thereafter. Boot-time deployment settings
   (database, listen addr, pack dir, secrets backend, cache/jobs dirs) stay
   file-only.
2. **Config API.** `GET /v1/workspace/config` (document + version) and
   `PUT /v1/workspace/config` (full document, `If-Match` on version).
   Reuse the `config.Load` validators — one validator, two entry points;
   structured field errors on 422. Every accepted write appends
   `config.updated` (actor, version pair, section-level diff summary).
   The read-only `GET /v1/workspace` snapshot from Phase 4 remains for
   unauthenticated display.
3. **Hot reload.** Dispatcher/router/trigger read a config snapshot
   refreshed on change; routing and repo changes apply from the next
   dispatched job. In-flight jobs keep their dispatch-time snapshot
   (timeouts and tool policy immutable per job).
4. **CLI round-trip.** `conveyor config export` / `import` against the API,
   for git-versioned backups and recovery.
5. **UI.** The Workspace page's routing table, workspace basics, and repo
   cards become editable forms (operator token required); inline
   validation errors from the API; every save confirms with the recorded
   event. Credential pool and vendor policies stay read-only file-based
   surfaces (migrate no earlier than Phase 5, §21.3 change 5).

**Exit criterion (spec §21.3):** a stage-routing change and a repo addition
made through the UI take effect on the next dispatched job without a
control-plane restart, each recorded as a `config.updated` event with actor
identity; a rejected invalid write surfaces its validation error in the UI
and leaves state untouched. *(Met; Beta entry moved to the Phase 4.7 exit by
§21.4.)*

## Phase 4.7 — MCP execution pivot → Beta

*Proves: the factory orchestrates; the operator's own agents implement
(spec §21.4).*

The sandbox execution plane retires; implementation delegates to
operator-owned agents over MCP; the spec corpus gets its organizing UI;
context files become first-class. Task intake assigns branch metadata only; the
implementing agent resolves a task-dedicated local worktree, safely creates or
adopts and pushes the exact assigned branch there, and leaves the primary
checkout untouched (spec §21.8). Suggested order:

1. **Pipeline agents in-process.** Replace harness-dispatched triage and
   spec jobs with direct API calls inside `conveyord` on
   `CONVEYOR_API_KEY`: per-stage `{model, timeout, execution}`
   from the §21.3 config document (triage/spec fixed `in_process`; review
   defaults `mcp` with `in_process` as fallback), exact token metering
   retained as observational audit telemetry, §4.1 output validators
   unchanged, full transcripts persisted through the §10.3 redaction path. This lands
   *before* the demolition so the pipeline never stops working.
2. **MCP intake and work-order server (§17.4).** `create_task` accepts
   agent-discovered work with a required workspace-scoped idempotency key,
   creates the normal durable task, and enqueues existing triage without a
   parallel triage path (§21.5). Stage-typed work orders cover implement and
   review. Lifecycle tools: `list_work_orders`, `claim_work_order`
   (leases; expiry returns the claim to queue; a review order for task T
   is unclaimable by the token/session that claimed T's implement order),
   `get_work_order` (implement: approved spec, assigned branch name, base, bounce
   history, prior feedback, artifact refs; review: diff/PR ref, approved
   spec, bounce history, review role prompt), `report_progress` /
   `report_usage` / `upload_transcript` (self-reported, marked as such). The
   implementing agent owns safe branch setup and pushes before
   `submit_for_review` (which opens the PR if absent, then dispatches review
   per execution mode), `await_review` (long-poll so the implementer's
   session receives the verdict in-session when a reviewer claims
   promptly), and `submit_review_verdict` (§4.1-validated verdict +
   structured feedback; reviewer identity plus self-reported agent/model
   recorded on the intervention → independence labels:
   `reviewer_session`, `reviewer_model`, `same_model_as_implementer`). A
   completed verdict is recorded internally before a durable River job
   publishes or updates the `Conveyor / Code review` Check Run and the single
   Conveyor factory PR comment; GitHub retries cannot roll back the verdict or
   bounce decision. `await_review` remains authorized to the submitting
   implementation session after submission even when its claim lease expires.
   On `changes_requested`, that warm session claims the newly queued
   implementation order before editing and resubmitting the existing branch
   and PR. Clock enforcement at the protocol boundary separates queue
   retention, execution, and lease clocks: the stage timeout starts on the
   first successful claim, lease renewal cannot extend it, and an unclaimed
   order becomes explicitly stale after `work_order_queue_timeout` (default
   `24h`) until `redispatch_work_order` resets its queue clock (§21.9). Jobs are recorded `harness: external-mcp,
   confinement: none, auth: byoa`.
3. **Demolition.** Delete `internal/runner`, `internal/adapter`, the
   credential pool/router, `cmd/conveyor-runner`, `cmd/conveyor-shim`,
   `images/`, snapshot/resume machinery; strip credentials, vendor
   policies, tool policies, per-repo images, and secret refs from the
   config document (§21.4 change 8). `gitx` bare-clone cache stays for
   pushed-branch diffs. The local `conveyor checkout` helper is agent-owned and
   may safely create a missing implementation branch from its fetched base;
   the factory does not create or reset that branch. Mechanical verification is the
   repo's own CI on the PR.
4. **Requirements tree.** `features` table (hierarchical) +
   task→feature assignment (triage suggests, human reassigns); a
   Requirements UI module rendering each node's accumulated approved
   specs with linked tasks, PRs, and events.
5. **Artifacts.** Content-addressed, size-bounded file storage;
   upload/browse UI; attach to features and tasks; injected into
   pipeline-agent context; listed with fetch access in `get_work_order`.
6. **UI/CLI updates.** Workspace page reflects the slimmed config;
   work-order state surfaces in the feed and task panel (claimed-by,
   lease, self-reported usage); review independence labels on the review
   card and timeline entry (§21.4 change 3); assigned branch state and
   dedicated-worktree checkout guidance (§21.8); MCP connection instructions in Settings;
   `conveyor config export/import` unchanged.

**Exit criterion (spec §21.4):** one real task flows issue → triage → spec
(approved in UI) → implement work order claimed by the operator's Claude
Code over MCP → safe creation or adoption of the assigned branch in a dedicated
sibling worktree with the primary checkout left untouched → implementation and
push on the operator's machine → `submit_for_review` → review work order claimed
by a *fresh* agent session → `changes_requested` verdict delivered to the waiting implementer via
`await_review` → fix in the same session → resubmit → approve → PR merged,
with the full lifecycle audited and the self-review guard verified (the
implementing session cannot claim the review order). Beta entry follows:
five consecutive such tasks per the §19 criterion.

**Implementation status (July 14, 2026): complete.** The Phase 4.7 code,
including the v1.5 MCP task-intake amendment, v1.6 budget removal, v1.7
operator-owned branch contract, v1.8 dedicated-worktree default, v1.9
independent work-order clocks and stale recovery, repository
Codex plugin, and operator
surfaces are implemented and repository validation passes. The live
dogfood exit flow and subsequent five-task Beta proof above are still pending;
they must be recorded from real MCP sessions and are not inferred from tests.

---

## Post-Beta sequence (context, not scope)

Built through the factory, prioritized by observed operational load:

- **Phase 5 — platform agents & policy:** monitor agent (CI → task,
  reverse sync §4.2); repo-resident `.conveyor/` hints (verify commands,
  triage area hints — advisory only, never capability grants). The
  command-policy shim + approval cards and environment inference/repair
  retired with the sandbox lane (§21.4).
- **Phase 6 — memory store** (§15.1): pgvector retrieval, lessons,
  per-role retrieval context limits; extends the Phase 4.7 requirements tree
  and artifacts rather than introducing the corpus.
- **Phase 7 — flywheel:** transcript mining, self-improvement proposals,
  escalation graduation, pack versioning + eval rig (§2.2, §15.2) — over
  native pipeline-agent transcripts plus self-reported MCP transcripts.
- **Phase 8 (demand-triggered):** the reintroduction of managed execution
  (§21.4): verification agent, K8sRunner, multi-repo sets, aggregate cost
  dashboard. **Phase 9 (demand-triggered):** enterprise.

## Standing notes

- **Soft dogfooding starts at Phase 3 exit**, deliberately: Phase 4 work
  items that are low-risk and well-specified should be filed as Conveyor
  tasks and run through the pipeline, so Beta entry is a milestone on a
  loop that is already turning, not a first flight.
- **Manual deploys during Beta:** merged PRs change the running factory;
  the operator rebuilds and restarts deliberately (spec §21.2 note).
  Conveyor never deploys itself.
- **Flywheel expectations:** Phase 7 consumes the transcripts, reason
  codes, and bounce histories Beta generates. Every redirect during Beta is
  training signal — write redirect comments accordingly.
- **Deferred-item triggers:** verification agent when GitHub review stops
  scaling as quality control; K8sRunner/multi-repo when a second machine or
  second repo actually joins; anomaly breaker when task volume makes the
  trailing-median meaningful; enterprise on demand (§18.1).
- **Future intake governance:** evaluate an audited `update_task`,
  `add_task_context`, or spec-amendment operation so accepted context can be
  appended after task creation without opening a duplicate task. This is not
  part of the pre-Beta worktree implementation.
