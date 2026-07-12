# Beta plan: phases 3–4.5

The roadmap authority is [conveyor-spec.md](../conveyor-spec.md) §19 (v1.3),
restructured by §21.2 and extended by §21.3. This document is the working
breakdown: what each pre-Beta phase contains, its dependencies, and its exit
criterion. Phases 1–2 are complete and validated; nothing here reopens them.

**The milestone:** Beta = Conveyor develops Conveyor. A GitHub issue on the
Conveyor repository flows issue → triage → approved spec → implementation →
PR → (redirect rounds) → merge, with human involvement limited to gate
decisions and merges, all taken through the UI or CLI.

**Beta exit criterion (spec §19):** five consecutive real tasks shipped
through the full pipeline, at least one completing a redirect round, zero
manual git operations.

Pre-Beta is deliberately three phases — the pipeline, the UI to operate it,
and the configuration surface to steer it (§21.3). Everything else (platform
agents, memory store, flywheel) is sequenced post-Beta, built with and
increasingly by the factory itself.

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
8. **Safety for unattended runs:** per-job budget circuit breaker (pause at
   100%, surface to queue — §14.1; the anomaly breaker stays deferred) and
   job wall-clock timeouts.

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
  duration, cost, harness/model/auth-mode; budget consumed vs. allocated;
  acceptance-criteria badge fed by the §4.1 blocks.
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
   (budgets/timeouts/tool policy immutable per job, §14.1).
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
and leaves state untouched. Beta entry follows: five consecutive real tasks,
one redirect round, zero manual git ops, all human actions through the UI or
CLI.

---

## Post-Beta sequence (context, not scope)

Built through the factory, prioritized by observed operational load:

- **Phase 5 — platform agents & policy:** command-policy shim + approval
  cards (§11.2), environment inference & repair (§6.4), monitor agent
  (CI → task, reverse sync §4.2).
- **Phase 6 — memory store** (§15.1): pgvector retrieval, lessons, the
  spec corpus, per-role context budgets.
- **Phase 7 — flywheel:** transcript mining, self-improvement proposals,
  escalation graduation, pack versioning + eval rig (§2.2, §15.2).
- **Phase 8 (demand-triggered):** verification agent, K8sRunner, multi-repo
  sets, aggregate cost dashboard. **Phase 9 (demand-triggered):** enterprise.

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
