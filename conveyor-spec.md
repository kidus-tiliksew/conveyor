# Conveyor: A Software Factory Platform

**Specification — v1.16**
**Date:** July 17, 2026
**Status:** Accepted — **Beta achieved July 15, 2026** (§19 exit criterion met); worker service packaging inserted as Phase 5.5 (§21.16), following the draft-PR drop (§21.15)
**Naming note:** "Conveyor" is a working title pending trademark clearance (known adjacent uses include Hydraulic's Conveyor packaging tool and the Konveyor modernization project). The CLI command, branch prefix (`conveyor/task-<id>`), paths, and issue labels are branded `conveyor`; a final-name change would require renaming these user-facing conventions, so clearance should happen before external users script against them.

---

## 1. Overview

Conveyor is an orchestration platform for automated software development. It runs coding-agent pipelines — triage, spec, implementation, review, verification, and monitoring — against one or more Git repositories, in destroyable-at-will containerized sandboxes (scoped to a task by default, §6.2) that execute either in the cloud or on a user's own hardware. Its purpose is to maximize the percentage of software changes that ship with zero or minimal human involvement, at the lowest possible cost per shipped change.

Conveyor does not implement its own coding agent. It orchestrates existing agent harnesses (Claude Code, OpenAI Codex CLI, OpenCode, Aider, and others) as pluggable executors. This lets the platform inherit each vendor's authentication — including consumer subscription auth, not just API keys — and benefit automatically as harnesses improve.

The design philosophy follows the "software factory" model: engineers do not primarily write code; they operate and improve a system that writes code. Every human intervention is treated as a signal to be recorded, analyzed, and engineered away over time.

### 1.1 Goals

1. Run multi-stage agent pipelines end to end: from an issue or trigger to a merged pull request, with human gates only where required.
2. Support any agent harness through a thin adapter interface; support subscription-based authentication (ChatGPT Plus/Pro for Codex, Claude subscriptions for Claude Code) in addition to API keys.
3. Execute identically on cloud infrastructure and on local machines, using containers built from the open devcontainer specification.
4. Treat cost as a first-class concern: per-task budgets, model routing, rate-limit-aware scheduling, transcript mining for waste, and cost attribution on every shipped change.
5. Support multi-repository workspaces, where a single task can modify several repos with coordinated branches and linked pull requests.
6. Provide computer-use verification: agents that run the built application and visually confirm it satisfies the spec before a human ever looks at it.
7. Manage secrets and environment variables centrally, injected at sandbox boot, never stored in task payloads or leaked into transcripts.
8. Provide provisioned CLI tooling (gcloud, aws, kubectl, terraform, gh, and similar) inside sandboxes, with credential scoping and a command policy layer.
9. Provide first-class Git worktree management so that many agents (and humans) can work the same repository in parallel, on any base branch, without interfering with one another.
10. Close the loop: a self-improvement engine mines transcripts and intervention records to reduce future human involvement and token waste.

### 1.2 Non-goals

- Building a novel coding agent or model. Conveyor is a harness orchestrator.
- Environment promotion machinery (dev/staging/prod tiers). Deferred; the unit of isolation is the branch and its sandbox, and deployment to production remains the responsibility of the repository's existing CI/CD.
- A hosted multi-tenant SaaS in v1. The initial target is a single team self-hosting the control plane.
- IDE features. Humans interact through the review UI, the CLI, and their existing editors via worktrees.

### 1.3 Guiding metrics

Two numbers define success and are instrumented from day one:

- **Automation rate:** the percentage of completed tasks shipped with zero human turns, and the distribution of tasks across escalation levels (§13).
- **Cost per shipped change:** total inference cost plus attributed human review time, per merged PR, per workspace, per harness.

---

## 2. Concepts and terminology

| Term | Definition |
|---|---|
| **Workspace** | A named set of repositories plus shared context (architecture notes, conventions, cross-repo contracts). The top-level unit of configuration. |
| **Task** | A unit of intended change (from an issue, Slack message, monitor alert, or manual entry). Tasks move through the factory pipeline. |
| **Job** | One execution of one pipeline stage for a task, in one sandbox, by one harness. A task typically spans many jobs. |
| **Harness** | An external agent CLI (Claude Code, Codex CLI, etc.) wrapped by a Conveyor adapter. |
| **Sandbox** | A container (or microVM) built from the workspace's devcontainer definition, holding the task's worktrees, injected secrets, and tools. Lives for one task by default (`sandbox_ttl`, §6.2) and is always safely destroyable. |
| **Runner** | A backend that provisions sandboxes: local Docker, Kubernetes, Fly Machines, etc. All runners implement one protocol. |
| **Worktree** | A Git worktree checked out from a shared bare clone, dedicated to one task in one repo. |
| **Escalation level** | The degree of human involvement a task class requires (L0 fully automatic through L3 interactive pairing). |
| **Agent role** | A pipeline stage persona: triage, spec, implementation, code review, verification, monitor, self-improvement. Roles are prompt-plus-policy configurations, not separate codebases. |
| **Platform scope** | Global configuration and agents that exist before and above any workspace: default prompt/policy pack, platform agents, credential pool, runners, secret backend. |
| **Prompt/policy pack** | The versioned bundle of role prompts, tool policies, and routing defaults that workspaces pin and upgrade deliberately, like a dependency. |

### 2.1 Configuration scopes and platform defaults

Configuration is layered across three scopes, with inheritance flowing downward. A new deployment is functional before any workspace exists, and a new workspace with zero overrides works correctly out of the box — there is no "build your global agent first" setup cliff.

**Platform scope.** The factory's firmware, shipped with the platform and versioned independently of workspaces:

- The default **prompt/policy pack**: role prompts, tool policies, and routing defaults for every pipeline stage. Workspaces pin a pack version and upgrade deliberately; the self-improvement engine proposes changes as diffs against the pack, and a silent global prompt change is treated as the regression class it is.
- **Platform agents** — agents that operate on the factory itself: environment inference and repair (§6.4), transcript mining, the self-improvement engine. These are pre-configured, not user-built, and they run through the same pipeline, sandboxes, audit log, and review queue as every other agent. There is no privileged side channel for "system" agents.
- Platform resources: the credential pool (each user's harness logins, §5.2), registered runners, the secret-backend connection, and the base Conveyor image.

**Workspace scope.** Inherits everything from platform scope and overrides what is specific: repos, memory entries, routing tweaks, escalation-level assignments, secret sets, and custom pipeline stages. *(Storage and mutation of this scope are amended by §21.3: workspace configuration is Postgres-backed, edited through the authenticated API/UI with validation and audit events; the deployment file becomes the bootstrap seed. Boot-time deployment settings and the credential pool remain file-based.)*

**Task scope.** Per-task parameters only: base branch, budget override, escalation override.

Organizational knowledge is deliberately *not* a required setup input. Workspace memory may optionally be seeded with architecture docs at creation, but it is primarily fed by real transcripts and intervention reason codes accumulated through normal operation (§15).

### 2.2 Pack contents and structure

The prompt/policy pack is a versioned directory of declarative files, treated like a dependency: workspaces pin a version and upgrade deliberately. Its contents, in full:

**Role prompts** — the behavioral definition of every agent, pipeline and platform alike:

```
pack/
  roles/
    triage.md          # classification + automatability estimation
    spec.md            # spec-first duties; expand/contract decomposition preference (§4, §7.1)
    implement.md
    code-review.md     # includes the decomposition mirror rule (§7.1)
    verify.md
    monitor.md
    env-inference.md   # platform agents (§6.4)
    env-repair.md
    transcript-mining.md
  tools/
    network-policy.yaml    # per-stage network access (§18)
    command-policy.yaml    # read / safe-write / destructive classifications for the §11.2 shim
  routing.yaml             # stage → harness/model/budget defaults (§5.3)
  pack.yaml                # version, changelog, minimum platform version
```

**Tool policies** — per-stage network access rules and the command classification lists consumed by the command-policy shim. Note the split: the *classifications* (which verbs are destructive) are pack content and thus tunable; the *shim that enforces them* is platform code and is not.

**Routing defaults** — the stage-to-harness/model/budget mapping that workspaces override.

**What is never in the pack (invariants).** Behaviors live in the pack and are overridable down the scope chain; invariants live in the orchestrator and job shim and are not: the linked-PR gating rules (§7.1), per-job credential injection and rotation (§6.2, §10.2), transcript redaction (§10.3), the path jail (§8.5), budget circuit-breaker enforcement (§14), and the **no-circumvention rule**: Conveyor respects vendor rate limits by backing off and rerouting rather than evading them, never disguises automated traffic as interactive or spoofs client fingerprints, and never pools personal credentials (§5.2). The self-improvement engine can propose diffs only against pack content; no proposal path exists for invariants.

**One default pack, v1.** The platform ships exactly one default pack; workspaces customize via overrides (§2.1), not by choosing among named pack variants. Named variants (e.g. a "conservative" vs. "aggressive automation" pack) are just pre-bundled override sets and are deferred until real workspaces demonstrate recurring override clusters worth productizing.

**Upgrades are gated by shadow runs.** Pack changes fail behaviorally, not mechanically — a reworded triage prompt still returns well-formed output while quietly misrouting tasks — so no pack version (whether an upstream release or an approved self-improvement diff) becomes adoptable in a workspace until it passes a **shadow run**: the candidate pack is replayed against the workspace's curated set of completed tasks in sandboxes, merging nothing, and compared with recorded outcomes on routing decisions, spec-block validity, bounce counts, and cost. This reuses the §15.2 eval rig — upstream releases are simply fed through the same machinery as self-improvement proposals. On shadow pass, the workspace re-pins; on regression, the candidate is rejected with the comparison attached. Every job records the `pack_version` it ran under, so post-adoption regressions surface in the standard dashboards attributable to the version change, and rollback is a re-pin to the prior version. Live-traffic canarying is deliberately not part of the lifecycle: shadow-gate plus post-adoption observability keeps the mechanism simple and works at any task volume.

---

## 3. Architecture

Conveyor is split into a **control plane** and an **execution plane**.

The control plane is the durable brain: it owns pipeline state, the task queue, routing and budget policy, the memory store, the review UI, and the audit/transcript store. It never checks out code and never holds plaintext secrets.

The execution plane is a fleet of sandboxes provisioned by runners, each scoped to a task by default (§6.2). Sandboxes hold everything ephemeral: worktrees, resolved secrets, harness processes, build artifacts. A sandbox can be destroyed at any time and rebuilt from control-plane state.

```
 Triggers (GitHub, Slack, cron, monitor agent)
      │
      ▼
 ┌───────────────────────── Control plane ─────────────────────────┐
 │  Orchestrator · Task queue · Router · Memory store · Review UI  │
 │  Audit log and transcript store (append-only)                   │
 └───────────────┬──────────────────────────────────────────────────┘
                 │ dispatch jobs (runner protocol)
                 ▼
 ┌──────────────────────── Execution plane ────────────────────────┐
 │  Sandbox: worktrees + secrets + tools + harness process         │
 │  (local Docker · Kubernetes · Fly · Firecracker — same image)   │
 └───────────────┬──────────────────────────────────────────────────┘
                 │ PRs, transcripts, artifacts, cost telemetry
                 ▼
        Self-improvement engine ──↻── feeds prompts/memory back
```

### 3.1 Control plane components

**Orchestrator.** A state machine per task. Advances tasks through pipeline stages, dispatches jobs to runners, handles retries, enforces stage timeouts, and applies escalation policy. Written as an event-sourced service: every transition is an append-only event, which doubles as the audit log.

**Task queue and router.** Prioritized queue with routing policy. The router selects, per job: which harness, which model tier, which runner, and which budget. Routing inputs include task class, historical success rates, current subscription rate-limit status per credential, and cost policy. Rate-limit-aware scheduling is a core feature: if the Claude subscription pool is throttled, the router may delay the job or route to Codex, per policy.

**Memory store.** Workspace-scoped knowledge injected into agent contexts: architecture decision records, domain rules, conventions, "lessons" produced by the self-improvement engine, and approved specs. Backed by Postgres + pgvector; retrieval is hybrid keyword/vector with per-role context budgets.

**Review UI.** The human gate (§13). A single inbox of items awaiting human decision, each showing the diff, the spec, verification evidence (screenshots/video), cost so far, and the agent's own summary. Actions (amended by §21.11): approve, request changes (wire action `redirect`; re-dispatches the task rather than requiring the human to fix it), or reject. Local work starts from `conveyor checkout` (§8.4), surfaced in the task header; pull-to-local is no longer offered as a gate action.

**Audit log and transcript store.** Append-only record of every job: full harness transcript (post-redaction, §10.3), tool calls, token counts, costs, and every human intervention with a structured reason code. This store is the raw material for the self-improvement engine and the compliance story.

### 3.2 Execution plane

Runners implement a single protocol:

```
StartJob(image, worktreeSet, secretRefs, prompt, budget, policy) → jobHandle
StreamLogs(jobHandle) → event stream (tool calls, tokens, stdout)
Signal(jobHandle, pause|resume|kill)
CollectArtifacts(jobHandle) → commits, files, screenshots, reports
```

v1 ships two runners: `LocalDockerRunner` (a daemon on a developer machine that polls the control plane for jobs) and `K8sRunner` (Kubernetes Jobs). Kubernetes is chosen for vendor neutrality: the same runner works against any managed cluster (EKS, GKE, AKS, and others) or self-hosted k3s, so hosting is the user's choice, not the platform's. Isolation hardening is a per-cluster option via RuntimeClasses (gVisor or Kata Containers, the latter providing Firecracker-class microVM isolation through the standard K8s API). Sandboxes needing to run containers themselves (image builds, testcontainers) default to rootless BuildKit/Podman; privileged Docker-in-Docker is permitted only on explicitly labeled node pools. Other backends (e.g. Fly Machines) remain possible as thin adapters against the runner protocol but are not v1 targets. Because all runners consume the same devcontainer-built image, "cloud or local" is a per-job scheduling decision, not an architectural one. Local mode is a first-class product surface: users can point their own subscription credentials and their own hardware at the queue and pay nothing for cloud compute.

> **Phase 1 amendment:** the volatile Phase 1 control plane and
> LocalDockerRunner run co-process; the durable standalone poll/claim boundary
> begins in Phase 2 (§21.1).

---

## 4. The factory pipeline

The default pipeline mirrors the factory workflow: triage → (spec) → implement → code review → verify → human gate → merge → monitor. Each stage is an agent role with its own prompt, model tier, tool policy, and budget. Pipelines are defined per workspace in configuration and can be customized (stages skipped, reordered, or added).

1. **Triage agent.** Reads the incoming task, attempts to reproduce/understand it, classifies it (bug, feature, chore), estimates automatability, and routes: straight to implementation, to spec first, to human input, or to parked. Runs on a strong model tier: triage decisions gate everything downstream, and a single misroute wastes an entire implementation or spec run — far more than the triage tokens saved by economizing here.
2. **Spec agent** (conditional). Produces a written spec from the task plus workspace memory. Specs are first-class artifacts: human-reviewable, versioned, and later used as the verification contract. This is the spec-first principle — business intent is captured and approved before code exists. For multi-repo tasks, the spec agent's role prompt (part of the default prompt/policy pack, §2.2) directs it to *prefer* an **expand/contract decomposition** where the change permits one: an ordered list of subtasks, each independently safe to merge, with earlier steps remaining backward-compatible until a later cleanup step removes the old path. The orchestrator materializes the decomposition as stacked tasks with dependency edges (§8.6, §7.1). This is a default, not a mandate — overridable per the usual scopes (§7.1).
3. **Implementation agent.** A full harness run in a sandbox with the task's worktrees. Produces commits on the task branch.
4. **Code review agent.** A separate harness run (ideally a different model than the implementer) reviewing the diff against the spec, conventions from memory, and static-analysis output. May bounce the task back to implementation with structured feedback.
5. **Verification agent.** Builds and runs the application from the worktree, then exercises it: scripted checks via Playwright, plus computer-use runs where a vision-capable model operates the real UI and judges it against the spec (§12). Evidence attaches to the PR.
6. **Human gate.** Per the task's escalation level (§13). L0 tasks skip this entirely.
7. **Merge and CI/CD.** Merging the PR triggers the repository's existing CI/CD unchanged. Conveyor never deploys directly.
8. **Monitor agent.** Watches post-merge signals (CI, error trackers, logs it has read access to) and files new tasks when regressions appear — closing the loop. It also watches for changes landing *outside* the pipeline (direct pushes, externally merged PRs, reverts) and triggers spec reconciliation tasks (§4.2).

Any stage can fail back to an earlier stage a bounded number of times before escalating to a human, and every such bounce is recorded with a reason code.

### 4.1 Spec format

A spec is a markdown document — prose for intent, context, approach, and rationale — containing a small number of schema-validated fenced blocks that machines own. Prose serves the human approver and the implementation agent (which works better from rich context than from skeletal schemas); the fenced blocks serve the verifier and orchestrator, which need mechanically enumerable content with stable IDs.

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

Rules: (1) blocks are validated at spec-agent output time; a malformed block bounces the spec back automatically, invisibly to humans. (2) Each acceptance criterion carries a **verification method**; the verifier plans its run by walking the list — `test` criteria dispatch to the suite, `playwright`/`computer-use` to the browser sandbox, and `human`-tagged criteria surface on the review card as explicit checkboxes rather than being pretend-verified. (3) Verdicts and evidence attach to `AC-n` IDs, feeding the PR check, the activity-view badge (§13.3), and criterion-level metrics for the self-improvement engine. (4) The `Non-goals` section is what the code-review agent enforces when emitting `scope-creep` reason codes. (5) The *approved* spec version is the verification contract: a redirect that changes criteria produces a new spec version requiring lightweight re-approval, so the verifier never judges against text nobody signed off. (6) The block schema is deliberately minimal — `acceptance` and `decomposition` only at v1 — because every schema field is friction taxed on every task forever.

### 4.2 Coherence: spec–code drift governance

The forward pipeline (intent → spec → code → verification) is only half of coherence; without a reverse loop, every change that bypasses it — a human hotfix pushed to main, an emergency revert, a merged PR from outside Conveyor — silently turns approved specs into fiction, and agent-scale code generation industrializes that drift. Governance mechanisms:

1. **The spec corpus.** Approved specs accumulate in the workspace memory store (§15.1) as the current statement of intended system behavior, not as orphaned per-task artifacts. New specs touching the same behavior *amend* the corpus (superseding or extending prior criteria), so there is always one queryable answer to "what is this system supposed to do here."
2. **Reverse sync.** The monitor agent watches for changes landing outside the pipeline (direct pushes, externally merged PRs, reverts). Each one triggers a **reconciliation task**: a cheap agent run that diffs the change against the corpus and either proposes a spec amendment reflecting the new reality (hotfix made behavior X; update or supersede AC-n) or flags a genuine conflict for human decision. Documentation written after the fact doesn't count as provenance, so reconciliation tasks carry the same audit chain as any other task.
3. **Criteria re-verification.** When a hotfix touches code covered by existing acceptance criteria, the affected `AC-n` entries re-verify against the patched build — catching hotfixes that quietly broke an adjacent guarantee.
4. **Drift as a metric.** The dashboard tracks the count and age of unreconciled out-of-pipeline changes per workspace. The healthy value is zero; a rising number is an early warning that the factory is being routed around, which is itself a signal worth a human conversation.

---

## 5. Harness layer

### 5.1 Adapter interface

Each harness is wrapped by an adapter that normalizes it to one interface:

```
Run(workdir, prompt, contextFiles, toolPolicy, budget) → event stream
  events: assistant_text | tool_call | tool_result | token_usage | done | error
Resume(sessionRef, feedback) → event stream          // continue a prior session
Capabilities() → { multiRepo, resume, jsonStream, authModes[], nativeSandbox }
```

Adapters run the harness CLI in its headless / non-interactive mode and parse its structured (JSON) output stream into Conveyor events. v1 adapters: **Codex CLI** (first) and **Claude Code**. The adapter surface is deliberately small so community adapters (OpenCode, Aider, Gemini CLI) are cheap to add. Exact invocation flags are pinned per harness version in the adapter and verified against vendor docs at upgrade time (for Claude Code, https://docs.claude.com/en/docs/claude-code/overview).

### 5.2 Authentication: every credential class is first-class

Because Conveyor executes vendor CLIs rather than calling model APIs directly, it inherits each CLI's auth. Conveyor supports all three credential classes on equal footing — the router treats them as interchangeable capacity with different cost and policy profiles:

- **Personal subscriptions.** The user completes the harness's interactive login once (locally or via a guided flow); the resulting credential directory (e.g. `~/.claude`, `~/.codex`) is stored encrypted and mounted read-only into sandboxes that job runs under that credential. Near-zero marginal token cost up to plan limits.
- **Team / enterprise seats.** Same mechanics, but with terms actually written for organizational and programmatic use — the preferred class for companies, and the one with the least terms risk.
- **API keys / OpenRouter.** Terms designed for automation, metered cost. Serves harnesses and models with no subscription path, and acts as universal overflow.

Credential model rules:

1. Credentials belong to a **user** (or, for team seats, an org), not to the platform. Conveyor never pools one personal subscription across multiple people. This must be preserved in any future multi-user deployment.
2. The router tracks per-credential rate-limit state (from harness error signals and usage telemetry) and schedules around it: queue, reroute to another harness or credential class, or overflow to API, per workspace policy.
3. Marginal cost accounting: subscription-backed jobs are costed at zero marginal token cost up to their limits, which the cost dashboard reflects honestly (amortized subscription cost shown separately).

**Subscription-terms drift management.** Subscription routing rests on vendor terms that change unilaterally, so it is managed as a live risk with four mechanisms:

- **Per-vendor policy registry** (platform scope): for each (vendor, harness, auth mode), a data record — `subscription_headless: allowed | restricted | disallowed | unknown`, last-reviewed date, source link to governing terms. Data, not code: updatable the moment terms change, no deploy required. The router consults it on every dispatch.
- **Kill switch with graceful degradation.** Flipping a vendor's flag halts subscription routing for that harness instantly; the router's overflow logic reroutes affected jobs to another credential class or harness, nothing stops, and the cost dashboard surfaces the economic delta so the change is visible rather than silent.
- **Informed consent at credential registration.** Connecting a personal subscription shows the registry's current status for that vendor and makes explicit that the account is the user's own, used under that vendor's terms. Conveyor never turns a user into an unwitting terms violator; if a vendor's flag moves to `restricted`, affected credential owners are notified, not silently kept in rotation.
- **Structural hedge, stated plainly:** if consumer-subscription routing disappeared entirely, Conveyor degrades to an API- and enterprise-routed factory with strong cost optimization — worse economics, same product. The subscription lane is a pricing advantage, never a load-bearing wall.

### 5.3 Routing policy

Routing is declarative per workspace, with sane defaults:

```yaml
routing:
  triage:     { harness: claude-code, model: strong,  budget: $0.75 }
  spec:       { harness: claude-code, model: mid,     budget: $0.50 }
  implement:  { harness: any-subscription, fallback: api, budget: $3.00 }
  review:     { harness: different-from-implementer,  budget: $0.75 }
  verify:     { harness: computer-use-capable,        budget: $1.50 }
```

The self-improvement engine may propose routing changes (e.g., "Codex outperforms on frontend tasks in this workspace") which take effect only after human approval.

---

## 6. Execution environments

### 6.1 Devcontainer as the single environment spec

Every repository describes its toolchain in `devcontainer.json` (open Dev Container spec). Conveyor builds and caches images from these definitions. A **base Conveyor image** provides the common layer: git, the harness CLIs, the Conveyor job shim, browsers for verification, and the standard cloud toolbelt (§11). Repo devcontainers extend it.

Benefits: one definition works for cloud runners, local runners, and humans opening the repo in their own editor; environment drift between "agent's machine" and "human's machine" disappears.

### 6.2 Sandbox lifecycle

```
build/cache image → provision container → mount worktree set (rw)
→ mount bare-repo cache (ro) → inject secrets (§10) → start job shim
→ run job → collect artifacts → pause          (sandbox_ttl: task, default)
   …later jobs for the same task resume the same sandbox…
→ task merged/closed or TTL expires → destroy  (worktree persists, §8.3)
```

**Sandbox lifetime is a policy knob**, configurable per workspace and per runner:

```yaml
sandbox_ttl: task        # job | task | <duration>; default: task
```

- **`task` (default).** One sandbox per task, kept alive (paused) across the review loop and destroyed when the task merges, closes, or goes stale. Successive jobs — implementation, review bounce, redirect round two — resume a warm sandbox: running dev servers, in-memory state, hot build caches, and the harness's live session all survive, which compounds the token and wall-clock savings of persistent worktrees (§8.3). The cost is held resources per open task; runners may checkpoint or stop paused sandboxes under pressure and cold-resume them from the worktree.
- **`job`.** Fully disposable: destroy after every job. Strongest isolation and zero idle resource cost; appropriate for untrusted or high-sensitivity workspaces and for elastic cloud fleets under heavy parallelism.
- **`<duration>`.** Time-boxed persistence for intermediate policies.

**Why compute stays disposable at all** (rather than long-lived agent machines): persistent machines accumulate state — global installs, mutated config, leftover daemons, credential residue — until failures stop being reproducible and one job's compromise or breakage silently affects the next. Conveyor's model is *disposable compute, persistent state*: everything durable lives in checked-in environment specs, persistent worktrees, and job artifacts, so any sandbox can be destroyed at any time and rebuilt bit-for-bit. The `task` default trades a bounded amount of isolation (one task's jobs share a container) for substantial warmth, while keeping the blast radius confined to a single task and preserving full reproducibility — nothing in a paused sandbox is load-bearing that isn't reconstructible from spec plus worktree. Two invariants hold at *every* setting: secrets and cloud credentials are injected and rotated **per job**, with expired credentials removed on resume (a persistent sandbox never extends a credential's life), and destroying a sandbox is always safe.

Sandbox reliability is treated as a product feature, not plumbing. "Sandbox failed to boot" is a first-class job state with structured diagnostics (image build log, devcontainer validation errors, missing env template variables), because flaky environments are historically where the majority of engineering effort in systems like this actually goes. Cold-resume after checkpoint/eviction gets the same first-class treatment.

### 6.3 Job shim

A small supervisor inside every sandbox that: resolves nothing itself (secrets arrive resolved), enforces the tool policy (§11.2), meters resource usage, streams logs to the control plane with redaction applied at the edge, enforces the path jail (§8.5), records every runtime package install for later mining (§6.4), and handles pause/resume/kill signals.

### 6.4 AI-generated environments: inference, repair, and install mining

Environment definitions are AI-*authored* but deterministically *built*. The boundary: models touch only the spec layer (`devcontainer.json`, Dockerfile fragments), and everything they produce flows through the normal pipeline — PR → review → merged spec → deterministic build → cached image. The control plane never builds anything itself and never executes a model-invented definition that isn't checked in. Fully dynamic per-job environment generation is explicitly rejected: it destroys image caching, reproducibility of failures, and human/agent environment parity.

Three mechanisms, all platform-scoped agents (§2.1):

1. **Environment inference (repo onboarding).** For repos without a devcontainer, an inference agent scans lockfiles, `.tool-versions`, CI configs, Dockerfiles, and READMEs; proposes a `devcontainer.json`; then proves it by building the image and running the repo's build and tests inside it, iterating until green. The result ships as an ordinary PR through the review queue. Workspace creation automatically dispatches these as the workspace's first tasks (§17.3), removing the largest onboarding friction.
2. **Environment repair.** When a sandbox fails to boot or a job dies on a missing-dependency error class, the orchestrator dispatches a repair agent whose input is the boot diagnostics and whose output is a proposed devcontainer fix — as a PR, never a live mutation.
3. **Install mining.** Agents may install tools freely *inside* their disposable, path-jailed sandboxes (network policy already constrains registries). The job shim records every install; the self-improvement engine mines the records and proposes image additions ("9 of the last 12 jobs on acme/api installed `jq` at runtime"). Runtime installs are exploration; image updates are the learned, cached result.

Guardrails on generated specs — a container definition is code with build-time root: base images pinned to an allowlist (base Conveyor image plus approved language images); build-time network restricted to allowlisted registries; lint rejection of `curl | bash` from arbitrary domains, secrets baked into layers, and floating `latest` tags; builds executed only in the runner's build service, never on control-plane hosts. Inference and repair PRs start at escalation L2 and may graduate to L1/L0 as their measured success rate proves out (§13.1).

---

## 7. Multi-repo workspaces

A workspace declares its repositories and shared context:

```yaml
workspace: acme
repos:
  - github.com/acme/api        # roles: [backend]
  - github.com/acme/web        # roles: [frontend]
context:
  - "Changes to API contracts in acme/api require matching updates in acme/web."
  - docs/architecture.md
```

For each task, the sandbox receives a **worktree set**: one worktree per repo the task may touch, checked out side by side under the job directory, all on the same branch name. The harness works across them as a multi-root workspace. On completion, the orchestrator opens PRs only in repos with commits and links them.

### 7.1 Cross-repo merge coordination

Atomic cross-repo merges do not exist — Git has no transaction spanning repositories — so coordination makes non-atomicity safe rather than pretending to eliminate it. Two mechanisms, in order of preference:

**Decomposition first (the default).** The spec agent decomposes multi-repo work into expand/contract-sequenced subtasks (§4 step 2): e.g. *add the new API field alongside the old (mergeable alone) → switch the consumer to it (mergeable alone) → remove the old field (mergeable alone)*. Workspace contract rules in memory ("changes to API contracts in acme/api require matching updates in acme/web") tell it when this applies; the orchestrator executes the sequence via stacked-task dependency edges (§8.6); the code-review agent's role prompt carries the mirror rule and flags PRs that break the pattern (e.g. removing a field other repos still reference). Under decomposition, no single PR is ever unsafe to merge alone, and the coordination problem degrades from *atomic* to merely *ordered*. Bad decompositions that slip through produce `broken-pair` reason codes, feeding pack improvements through the standard flywheel.

**Decomposition is a default preference, not a hard policy.** Overrides exist at every scope: the spec agent itself emits a single gated spec when a change genuinely cannot be split (lockstep protocol changes, security fixes where the compatible intermediate state is the vulnerability); the human editing the spec at review can collapse or rewrite the decomposition, or direct gating in a redirect comment; the workspace can weaken, strengthen, or disable the preference via pack overrides (§2.1); and a single task can carry `coordination: gated`. The only non-overridable element is the safety floor: any task that ends up with linked PRs, by any route, is subject to the four gating rules below.

**Linked-PR gating (the fallback)** for changes that genuinely cannot be decomposed. Four rules, enforced by the orchestrator:

1. The task declares a merge order across its linked PRs.
2. No linked PR merges until *all* are green and approved.
3. Immediately before each merge, the PR is re-validated against the current tip of its base branch (freshness check), and the set merges back-to-back to minimize the inconsistent window.
4. If a later merge in the sequence fails after an earlier one landed, the orchestrator automatically files a fix-forward task (or reverts the landed PR, per workspace policy) and flags the incident.

**Explicitly deferred: a cross-repo merge train** (speculative integration branches testing the combined future state of all repos before landing). Native forge merge queues are per-repo and cannot coordinate across repositories, so a cross-repo train is custom infrastructure. It is built only if evidence demands it: gating rule 4 and the monitor agent record every broken-pair or stale-green incident on main, and a sustained incident rate — not anticipation — is the trigger. This is the factory ethos applied to the factory: ship the cheap mechanism, instrument the failure it doesn't cover, let data justify the expensive one.

---

## 8. Worktree and branch management

Branch-per-task is the core Git model. There are no environment tiers; users simply choose the base branch, and isolation comes from branches plus sandboxes.

### 8.1 Storage layout: one bare clone, many worktrees

Each runner host maintains a shared cache of bare mirror clones, one per repo:

```
/conveyor/cache/github.com/acme/api.git      # bare, shared, fetch-only
/conveyor/jobs/task-123/api/                 # worktree on conveyor/task-123
/conveyor/jobs/task-123/web/                 # worktree on conveyor/task-123
```

Worktrees share the object database with the bare clone, so creating a task workspace costs a checkout, not a clone — seconds instead of minutes on heavy repos, multiplied across every job. In containers, the bare cache mounts read-only and the job's worktrees mount read-write.

> **Phase 1 amendment:** isolated task clones are seeded from this cache and
> the cache is not mounted at all. This enforces the same read-only boundary
> without linked-worktree write dependencies (§21.1).

Fetches into a bare cache are serialized with a per-repo lock; concurrent fetches into one bare repo are forbidden (ref corruption risk).

### 8.2 Job = branch = worktree

Dispatching a task atomically creates: the branch `conveyor/task-<id>` cut from the chosen base (default `main`, selectable per task — this is how users "switch between branches": long-lived feature work simply becomes the base for its tasks), the worktree set, and the sandbox. They are torn down together.

> **Current contract:** §21.7 supersedes this factory-owned branch creation
> statement for Phase 4.7's operator-owned execution model. The paragraph is
> retained as the historical v1.0 contract; current intake assigns branch and
> base metadata only.

### 8.3 Worktree persistence across the review loop

Worktrees outlive individual jobs. When a human redirects a task with comments, the re-dispatched implementation job lands in the same worktree, preserving build artifacts, dependency installs, and — where it restores cleanly — the harness's session state. This keeps multi-round review cheap in both wall-clock time and tokens (no cold re-exploration of the repo).

**Handoff snapshots (the continuity contract).** Agent memory across jobs uses a two-layer scheme with a clear hierarchy:

- **Handoff snapshot — always.** At the end of every job, one additional prompt has the agent write a structured handoff document: current state of the work, key decisions and their rationale, files touched and why, known gotchas, remaining todos. It is stored in the control plane as a job artifact and injected into the successor job's opening context alongside any human redirect comments. Because it is plain structured text, it is portable across hosts and sandboxes, immune to harness version drift, works across *different* harnesses (enabling mid-task rerouting), and stays compact. It is lossy by construction — it captures what the agent judged important — which is why it is the guaranteed floor, not the ceiling.
- **Native session resume — an optimization on top.** Where the adapter's `Capabilities()` reports resume support, the session directory is archived as a job artifact and restored into the successor sandbox, and `Resume(sessionRef, feedback)` is attempted. (Under the default `sandbox_ttl: task`, successive jobs usually resume inside the *same* paused sandbox, making this the trivial case; the archive/restore path covers `job`-TTL workspaces, evictions, and cross-host moves.) If the session fails to load (host/path differences, harness version bump, corruption), the job degrades gracefully to snapshot-only rather than failing. Known fragility modes: sessions embed absolute paths and environment details; session formats are internal and drift across harness versions; long-session compaction silently drops rationale; and native sessions never transfer across harnesses. A resumed session is also not automatically better — bloated contexts can underperform a well-briefed cold start — so the self-improvement engine A/B-measures resume-plus-snapshot vs. snapshot-only per harness and task class, and routing policy follows the data.

Implementation notes (validated against prior art — Orca, stablyai/orca, which built per-harness native resume and hit these issues): (1) adapters capture session IDs and transcript paths via each harness's *hook* mechanism during the run, pushed authoritatively, rather than scraping session directories afterward — harness updates have already broken ID-to-filename correspondence in the wild; (2) each harness's state directory is redirected via environment (e.g. `CODEX_HOME`, or controlling `HOME` itself inside the sandbox) into the job directory, making session state a job artifact by construction rather than something to be exfiltrated from a home directory; (3) in-sandbox worktree mount paths are deterministic per task (`/conveyor/jobs/task-<id>/…` on every host), a hard requirement because some harnesses key sessions to their working directory and a moved path orphans the session; (4) resume support is expected to be the minority case across the harness ecosystem — of ~25 CLI agents Orca supports, only nine are resumable at all — reinforcing that the snapshot, not resume, is the contract.

Garbage collection: worktrees are removed on merge, on task close, or after a staleness TTL (default 14 days). A background chore runs `git worktree prune` and enforces per-host disk quotas. Orphaned-worktree disk creep is an explicitly monitored metric.

### 8.4 The human escape hatch

The CLI extends worktree management to humans:

```
conveyor checkout task-123   # adds the task branch as a worktree in the
                            # human's local clone, beside their own work
conveyor done task-123       # removes it (and optionally re-dispatches)
```

Under the current §21.7 contract, `checkout` is a post-push human escape hatch.
It is unavailable while the task has only an assigned branch name and no
pushed remote ref; the implementing agent creates and pushes that ref first.

Intervening on a stuck agent never requires stashing or switching the human's own branch: open the task worktree in a second editor window, fix or nudge, push, re-dispatch. Lowering the cost of intervention is what makes the semi-automated middle ground livable.

### 8.5 Confinement: the path jail and local-runner security

Agents are confined to their own worktree set: the bare cache and sibling job directories are never writable, and nothing outside the job directory is visible. Cloud sandboxes get this from container mounts. Local runners — where the same agent process, unconfined, would run with the user's reach into `~/.ssh`, browser stores, cloud CLI sessions, and other repos — get a **three-tier confinement model**. Every job records which tier actually applied in its audit record, so "was this PR produced by an unjailed agent" is always a queryable fact.

- **Tier A — containerized local (default).** LocalDockerRunner runs jobs in containers: job worktrees mounted rw, bare cache ro, and *nothing else* — never the home directory, never the Docker socket (nested builds use rootless BuildKit, §3.2). Rootless Podman/Docker preferred, since a root-mode daemon is itself a root-equivalence hazard on a dev machine. Container network namespaces apply the same per-stage egress policy as cloud.
- **Tier B — harness-native or OS-primitive confinement.** For native (non-container) execution, the preferred mechanism is the **harness's own sandbox** where the adapter reports it robust — e.g. Codex CLI's read-only/workspace-write modes backed by Seatbelt (macOS) and Landlock/seccomp (Linux), or Claude Code's permission system. Harnesses lacking a robust native sandbox are wrapped by the job shim acting as *launcher*, applying the strongest OS primitive the host offers (Landlock or bubblewrap on Linux, `sandbox-exec` on macOS, restricted tokens on Windows). Known limitation, documented rather than papered over: native network-egress control is leakier than a container namespace — one more reason Tier A is the default rather than an equal option.
- **Tier C — native, unconfined ("trusted local").** The rare intersection of no container, no OS primitive, and no usable harness sandbox. Conveyor builds no machinery for this state beyond refusing to be silent about it: it requires an explicit per-runner opt-in flag, every job it runs is labeled unconfined in the audit log, and it receives **no workspace secrets** — only the user's own harness credentials, which already live on that machine, making the marginal exposure zero.

**Responsibility seam with harnesses.** Harness vendors own the confinement *mechanisms*; Conveyor owns configuring them correctly for unattended use — because headless operation is precisely what removes the human from the harness's permission prompts, and the party that pre-answers those prompts inherits responsibility for what replaces them. Concretely, this is an **adapter duty**: each job's tool policy (from the pack) is mapped into the harness's native permission configuration per job — sandbox mode, allowlists, denied commands — rather than relying on whatever the user configured for their own interactive use. Adapters report native-sandbox robustness in `Capabilities()`, which is what tier selection consults.

**Cross-cutting policy hooks:** secret sets carry a `local_eligible` flag (§10.1), so production-adjacent credentials can be cloud-only regardless of which runner claims a job; workspaces can set a confinement floor (`local_min_confinement: tierA | tierB`) to keep sensitive repos off weaker runners entirely. On shared machines, runners register per-user, caches and worktrees live under per-user paths with filesystem permissions, and one user's daemon never executes jobs under another user's credentials.

### 8.6 Stacked tasks

A task may declare a dependency on another task; its branch is then cut from the dependency's branch instead of the base. The orchestrator tracks the edge and triggers a rebase job when the parent lands. Full stacked-diff tooling is out of scope for v1, but the model never blocks it.

---

## 9. Triggers

Tasks enter the queue from:

- **GitHub / GitLab:** issue labels (`conveyor:ready` → dispatch), @-mentions in issue or PR comments, review comments on Conveyor PRs (auto-converted to redirect feedback).
- **Chat:** a Slack (or similar) app that converts messages into tasks with a confirmation step.
- **Cron:** scheduled tasks (dependency bumps, routine chores).
- **Monitor agent:** tasks filed from CI failures and error-tracker signals (§4, step 8).
- **CLI / API / Review UI:** manual entry.

Every trigger records provenance on the task for audit and for the automation-rate metric (which trigger sources produce the most automatable work).

---

## 10. Secrets and environment variables

### 10.1 Model: references in the control plane, values at the edge

Secrets live in a dedicated secret backend — pluggable: HashiCorp Vault, GCP Secret Manager, AWS Secrets Manager, or SOPS-encrypted files for fully local deployments. The control plane stores only references:

```
secretref://acme/default/DATABASE_URL
```

Scoping is workspace × secret-set (a named bundle, e.g. `default`, `integration-tests`). Secret sets carry a `local_eligible` flag: sets marked false are delivered only to cloud runners, regardless of which runner claims the job (§8.5). The runner resolves references at sandbox boot and injects values as environment variables or mounted files. Plaintext secrets never appear in the control-plane database, job payloads, or queue messages.

### 10.2 Short-lived credentials preferred

Where the provider supports it, the runner mints per-job credentials with a TTL slightly exceeding the job budget: GCP workload identity federation, AWS STS, GitHub App installation tokens (never long-lived PATs). Static secrets are supported but flagged in the UI as tech debt.

### 10.3 Transcript redaction (non-negotiable)

Because transcripts are persisted and later mined by the self-improvement engine, any leaked secret would otherwise be stored and re-fed into future model contexts. The job shim redacts the log stream **before** it leaves the sandbox:

1. Exact-match scrubbing of every injected secret value (and common encodings: base64, URL-encoded).
2. Pattern/entropy detection for secrets from other sources: JWTs, `AIza…`, `sk-…`, `ghp_…`, PEM blocks, high-entropy strings in assignment contexts.

Redaction events are themselves logged (count and pattern class, never the value).

### 10.4 Env templates

Each repo/workspace may define an env template (variable names, which secret-set supplies them, safe defaults for tests). Sandboxes boot with a complete `.env`, eliminating the classic token sink of agents debugging missing-variable startup failures.

---

## 11. CLI tooling inside sandboxes

### 11.1 Provisioning

The base Conveyor image ships the standard toolbelt: `gcloud`, `aws`, `kubectl`, `terraform`, `gh`, `docker` (client), plus language toolchains per the repo devcontainer. Cloud CLI auth flows through §10: the runner injects a scoped credential (e.g., a federated token with `GOOGLE_APPLICATION_CREDENTIALS` set) so tools work non-interactively.

### 11.2 Control: three layers

1. **IAM is the primary control.** Conveyor never relies on command filtering to protect infrastructure; it scopes the credential. The service account a job receives has only the roles that job class needs. There is no command an agent can type to exceed its IAM grant.
2. **Command policy shim.** Wrapped binaries on `$PATH` classify invocations: reads pass silently; known-safe writes pass with logging; destructive verbs (`delete`, `destroy`, `apply` against protected targets) block or pause the job and push an approval card to the review queue. This yields an audit trail even where IAM would have permitted the action, and a human-in-the-loop for the gray zone.
3. **Plan/apply split for infrastructure.** Agents may always `terraform plan`; `apply` output is a reviewable artifact routed through the human gate like any code diff.

---

## 12. Verification and computer use

The verification agent runs in a sandbox with the task's worktree and a display-capable environment (headless browser + virtual display):

1. **Build and launch** the application per the devcontainer's run configuration.
2. **Scripted checks:** existing test suites, plus Playwright flows generated from the spec where feasible.
3. **Computer-use judgment:** a vision-capable model operates the running UI — clicking through the changed flows, taking screenshots — and produces a structured verdict against the spec: pass/fail per acceptance criterion, with screenshot or short video evidence.
4. **Evidence attachment:** verdicts and media attach to the PR and appear in the review UI.

The explicit goal is to compress human review time — the true bottleneck — by letting the reviewer confirm evidence rather than reproduce behavior. Verification runs against whatever is in the worktree; no deployment/promotion machinery is involved.

---

## 13. Human review queue and escalation ladder

### 13.1 Escalation levels *(superseded by §21.12)*

*The L0–L3 ladder is replaced by Auto/Manual execution modes with two
independent human-gate toggles — spec approval and merge approval — per
§21.12 change 2. The table below remains the v1.0–v1.11 historical record;
existing task records keep their recorded levels.*

| Level | Meaning | Examples (initially) |
|---|---|---|
| **L0** | Fully automatic; auto-merge on green verification | Dependency bumps, lint/format fixes |
| **L1** | Automatic with a one-click human approve | Small bugs with strong test coverage |
| **L2** | Human reviews the spec before implementation; reviews the PR after | Features, cross-repo changes |
| **L3** | Human pairs interactively (task pulled off the line) | Ambiguous, architectural, or novel work |

Task classes are assigned a level per workspace; classes **graduate downward** as their measured success rate over a trailing window crosses thresholds, and are demoted on regressions. Graduation proposals come from the self-improvement engine and require human sign-off. The distribution of tasks across levels is the automation-rate metric made concrete.

### 13.2 The review inbox

One queue, one card per pending decision. Each card shows: the diff, the governing spec, verification evidence, cost so far vs. budget, the agent's summary and self-assessment, and prior bounce history. Actions (amended by §21.11):

- **Approve** (merge or advance) — one click; the reason code is derived,
- **Request changes** (wire action `redirect`) — the human writes feedback, not code; the task re-dispatches into its existing worktree (§8.3),
- **Reject** (close).

Pull-to-local was retired from the review UI by §21.11: under the MCP pull model (§21.4), local work starts from `conveyor checkout <task-id>` (§8.4, §21.8), which stays surfaced in the task header rather than as a gate decision. The `pull_to_local` wire action and its recorded interventions remain for the historical record.

Every action records a structured **reason code** on the wire. Since v1.11 the dashboard derives it from the action (approve → `approved`, redirect → `changes-requested`, reject → `rejected`) and the operator's free-text comment carries the nuance; agents and integrations may still record curated codes (spec-wrong, hallucinated-API, style, flaky-env, scope-creep, broken-pair, …), which remain the training signal for self-improvement where present (§15.2, §21.11).

### 13.3 Activity view

The primary UI surface (Phase 2) is an activity view modeled on stage-grouped task feeds, with three fixed elements:

1. **Stage-grouped feed.** Tasks grouped by pipeline stage — Triage / Spec / Implementing / Reviewing / Verifying / Awaiting human — as collapsible sections with counts. The distribution of work across stages is the factory's health made visible: a pile-up in any stage is immediately apparent. Each row shows the task ID, title, escalation-level badge, provenance chips (source channel, ticket ref, PR number), a "Needs attention" badge where a human gate or circuit breaker has fired, and recency. "Needs attention" is the only alarm color on the page; visual economy is a design requirement, since the product's goal is fewer human touches, and attention states must read as exceptions.
2. **Costed event timeline.** The task detail panel narrates the task's history as a timeline: one entry per pipeline stage, each showing the agent's summary of what it did, wall-clock duration, cost, and which harness / model tier / auth mode ran it (e.g. "4m 03s · $0.94 · codex / subscription"). This makes per-stage cost something operators absorb passively during normal review rather than a separate dashboard, and it is the audit log (§3.1) rendered as a story. The task header shows budget consumed vs. allocated and a verification badge with pass count against the spec's acceptance criteria.
3. **Review actions in place.** For tasks at a human gate, the detail panel carries the §13.2 actions directly as a verdict-first card (§21.11): a headline and tone matched to the gate state ("Ready to merge" on an approved task; amber reserved for genuinely stuck states), a one-click context-matched approve, and request-changes / reject as secondary decisions. A reviewer never leaves the timeline context to act on what it shows.

The cost dashboard (§14) remains the aggregate view; the activity view is deliberately per-task and narrative.

---

## 14. Cost controls and token efficiency

1. **Budgets with circuit breakers.** Every job carries a budget. At 100% the job pauses and surfaces to the queue; an anomaly breaker also pauses jobs exceeding 2× the trailing median cost for their task class — runaway loops are the dominant waste mode.
2. **Model routing** per stage (§5.3): strong models where decisions gate downstream cost (triage), subscription-backed harnesses for implementation, API overflow only when policy allows.
3. **Rate-limit-aware scheduling** across the credential pool (§5.2).
4. **Transcript mining.** A scheduled cheap-model job scans recent transcripts for waste signatures — repeated file re-reads, failed tool-call retry storms, oversized contexts, redundant exploration — and files improvement proposals (prompt edits, context-injection changes, routing changes).
5. **Warm-state reuse.** Persistent worktrees, handoff snapshots, and native session resume where it proves out (§8.3) avoid paying repo-exploration costs on every review round.
6. **Cost attribution everywhere.** Dashboard slices: cost per merged PR, per task class, per harness, per workspace; subscription amortization shown separately from marginal API spend.

---

## 15. Memory and self-improvement

### 15.1 Memory store

Workspace-scoped, structured entries: architecture decisions, domain rules, conventions, the **spec corpus** — approved specs as the amendable, queryable statement of intended system behavior (§4.2) — and **lessons** ("computer-use verification of the checkout flow requires seeding a test payment token — see task-841"). Each agent role has a context budget; retrieval is hybrid search over the store, filtered by role relevance. Entries carry provenance and expiry review dates so the store stays curated rather than becoming a landfill.

### 15.2 Self-improvement engine

Consumes: transcripts, reason codes, bounce histories, cost telemetry. Produces, as human-reviewable proposals (never auto-applied in v1):

- Prompt and policy edits per agent role,
- New or amended memory entries,
- Routing changes and escalation-level graduations,
- Image additions mined from recorded runtime installs (§6.4),
- Prompt/policy pack diffs, versioned so workspaces roll forward on their own schedule (§2.1),
- Harness/prompt A/B eval runs: candidate configurations are replayed against a curated set of historical tasks, scored on success and cost, before promotion.

The design intent is a measurable flywheel: every human intervention makes the next similar task less likely to need one.

---

## 16. Data model (core tables, abridged)

```
workspaces(id, name, config_yaml, config_version, created_at)   -- config governance §21.3
repos(id, workspace_id, url, default_base, devcontainer_path)
secret_sets(id, workspace_id, name, backend_ref)
users(id, identity_provider_ref, role)      -- roles defined in v1, enforced later (§18.1)
credentials(id, owner_id, owner_kind[user|org], kind[personal_sub|team_sub|api], harness, enc_ref, rl_state, policy_flag)
tasks(id, workspace_id, source, title, class, escalation_level,
      base_branch, branch, state, parent_task_id, created_at)
task_repos(task_id, repo_id, has_commits, pr_url)
jobs(id, task_id, stage, harness, model_tier, runner, sandbox_ref,
     worktree_ref, pack_version, confinement_tier, budget_usd, cost_usd,
     tokens_in, tokens_out, state, started_at, ended_at)
events(id, task_id, job_id, kind, actor_id, actor_role, payload_json, at)  -- append-only
transcripts(job_id, uri, redaction_stats)
interventions(id, task_id, job_id, actor_id, actor_role, action, reason_code, comment, at)
memory_entries(id, workspace_id, kind, body, embedding, provenance, review_by)
proposals(id, kind, diff, evidence, state[open|approved|rejected], at)
worktrees(id, host, task_id, repo_id, path, state, last_used_at)
```

`events` is the source of truth for task state (event-sourced); `jobs`, `tasks` carry denormalized current state for querying.

---

## 17. Interfaces

### 17.0 Implementation stack

**Backend — Go**, everywhere: control plane services, runners, adapters, CLI, and job shim. The decisive properties: single static binaries for the three components where distribution friction is fatal — the CLI, the local runner daemon (the "point your own hardware at the queue" story), and the job shim, which is injected into every sandbox image regardless of the repo's language and therefore must be dependency-free; goroutine-native concurrency for the orchestrator's long-lived state machines and the adapters' subprocess supervision; and `client-go` for the K8sRunner. Named picks: `net/http` + chi (API), **pgx + sqlc** (typed Postgres access over the event-sourced schema), **River** (Postgres-backed job queue — collapsing queue and database into one stateful dependency, with job enqueues transactionally consistent with event commits), cobra (CLI).

**Frontend — Vite + React + TypeScript** with **TanStack Router** (typesafe routes; `/tasks/$taskId` deep links from PR comments and chat) and **TanStack Query** for all server state, Tailwind + shadcn/ui for components. The activity view (§13.3) maps directly onto Query's cache-and-background-refetch model; live surfaces (job logs, timeline updates) stream over the SSE endpoints into the Query cache via stream-aware queries or key invalidation. The dashboard is a pure SPA against the Go API, embedded into the control-plane binary via `go:embed` — the entire control plane, API and UI, deploys as one binary plus Postgres. **TanStack Start was considered and rejected:** its differentiators (SSR, server functions) require a JS server tier, which would add a Node runtime to every self-hosted deployment and a second backend competing with the Go API, for features an authenticated internal dashboard doesn't use. Because Start is built on TanStack Router, this decision is cheaply reversible if a genuine SSR need ever appears.

**Permitted exception:** the Phase 5 analysis side of the self-improvement engine may run as a small Python sidecar service if transcript mining outgrows what's comfortable in Go; everything else stays in the two stacks above.

### 17.1 CLI (primary human surface alongside the review UI)

```
conveyor task new "fix flaky auth test" --repo api --base main [--level L2]
conveyor task list | show <id> | redirect <id> -m "…" | approve <id>
conveyor checkout <id>          # human worktree escape hatch
conveyor done <id> [--redispatch]
conveyor runner start --local   # start local daemon, register credentials
conveyor secrets set acme/default/DATABASE_URL --from-stdin
conveyor costs [--by harness|class|workspace]
```

> In Phase 1, `runner start --local` starts the combined control-plane/local
> runner process described by the §21.1 amendment.

### 17.2 First-run sequence

The user never builds or configures an agent; platform defaults are live on deployment.

1. Deploy the control plane → platform scope (prompt/policy pack, platform agents) is active immediately.
2. Connect credentials: run each harness's login once (Claude Code, Codex); register at least one runner — `conveyor runner start --local` is the zero-infrastructure path.
3. Configure the secret backend, or accept the local SOPS default.
4. Create a workspace and point it at repos.
5. Workspace onboarding runs *as conveyor tasks*: the environment-inference agent (§6.4) is dispatched automatically against each repo lacking a devcontainer, and its proposals land in the review queue as PRs. The user's first review-inbox experience is approving their own environment specs — which doubles as a tutorial for the review model.

### 17.3 HTTP API

REST + SSE mirroring the CLI: task CRUD, job log streaming, review actions, runner registration, webhook ingestion (GitHub, Slack, error trackers), and workspace configuration read/write (§21.3). All mutating endpoints require auth and are recorded in `events`.

### 17.4 MCP task intake and work orders

`conveyord` exposes one authenticated MCP server for agent-facing intake and
execution. `create_task` accepts a title, repository, optional task context and
escalation level, plus a required workspace-scoped idempotency key. It creates
the same durable task as HTTP/CLI intake, queues its existing triage stage, and
returns immediately; retries with the same key and input return the original
task, while reuse for different input is rejected. Triage remains the
server-owned in-process stage — MCP intake is not a second triage
implementation (§21.5).

Implementation and review use the leased work-order lifecycle defined by
§21.4: `list_work_orders`, `claim_work_order`, `get_work_order`,
`report_progress`, `report_usage`, `upload_transcript`, `submit_for_review`,
`await_review`, and `submit_review_verdict`. The MCP bearer is authenticated as
an agent actor; every resulting task, event, claim, report, and verdict follows
the same durable audit path as the corresponding HTTP or pipeline action.

---

## 18. Security summary

- Plaintext secrets are resolved only by the trusted runner at sandbox boot and exist only in runner-process memory and the sandbox; redaction attaches at the sandbox edge (§10.1, §10.3).
- Per-user credentials, never pooled subscriptions (§5.2).
- IAM-scoped, short-lived cloud credentials per job; command-policy shim as a second layer (§11.2).
- Path-jailed agents under tiered confinement (container / harness-native or OS primitive / labeled opt-in unconfined), applied tier recorded per job; read-only bare caches; serialized fetches (§8.5).
- Append-only audit of every agent action and human decision (§3.1).
- Prompt-injection posture: content fetched from the web or from issue bodies is treated as untrusted data; tool policies restrict outbound network access per stage (triage: read-only; implementation: package registries + declared APIs; verification: app under test only).

### 18.1 Enterprise path

Conveyor's enterprise strategy is bottom-up: a platform team or product group self-hosts Conveyor for its own repos, proves the automation-rate and cost metrics, and becomes the internal champion for broader adoption. Self-hosting is the wedge — the control plane runs in the customer's infrastructure, code never leaves their network, and model traffic can flow through enterprise contracts they already hold (enterprise seats and enterprise API endpoints are first-class credential classes, §5.2; harnesses that support Bedrock/Vertex-style endpoints inherit those agreements). The audit chain, per-job credential scoping, redaction, and spec→PR→verification→merge provenance are core architecture rather than enterprise add-ons, which is most of what a security review asks for.

v1 is explicitly *not* enterprise-ready — no SSO/SAML, no SCIM, no RBAC, no HA/backup story, and none of the organizational apparatus (certifications, SLAs, procurement support) that enterprise sales requires. What v1 must do instead is avoid foreclosing enterprise-readiness, via three cheap-now / miserable-to-retrofit requirements:

1. **Identity abstraction from day one.** All authentication flows through a pluggable identity provider interface; v1 ships local accounts, and SSO/OIDC becomes an adapter rather than a rewrite.
2. **Roles on the actor model from day one.** Every actor carries a `role`, and every event in the audit log records it. v1 defines the roles without enforcing fine-grained permissions; later RBAC (per-workspace approver rights, policy-registry administration) is enforcement added to data that already exists.
3. **Boring, documented operations.** One Postgres as the source of truth, a documented backup/restore procedure, and a versioned upgrade path — the substrate an HA story can later be built on.

SSO/OIDC, SCIM, and RBAC enforcement are roadmap items for a post-Phase-5 enterprise phase, triggered by demand rather than scheduled.

---

## 19. Roadmap

| Phase | Deliverable | Proves |
|---|---|---|
| **1** *(complete)* | Codex adapter (ChatGPT subscription auth), LocalDockerRunner, bare-clone + worktree manager, secrets injection, base image, handoff snapshots + resume-fidelity experiment, GitHub issue → PR, logs only | The core loop |
| **2** *(complete)* | Claude Code adapter, credential pool + router, Postgres state + events, activity view + review queue with reason codes (§13.3), redaction | Multi-harness + human gate |
| **3** | Full pipeline: multi-stage orchestration with per-stage gates and bounded bounces; triage, spec, and code-review agents; spec format machinery (§4.1) with versioned, approved specs as the implementation contract; role prompts and tool policies as versioned files (proto-pack, §2.2); per-repo sandbox images (including a Go-toolchain image so Conveyor can build itself); PR review comments → redirect feedback (§9); per-job budget circuit breaker and job timeouts (§14.1) | The full pipeline runs |
| **4** *(complete)* | UI rewrite: ground-up, polished implementation of the §13.3 activity view on Tailwind + shadcn/ui (§17.0) — stage-grouped feed, costed event timeline, spec and diff review surfaces, review actions in place; app shell with home/workspace/settings surfaces, task intake, and a read-only workspace snapshot | The factory is operable from one screen |
| **4.5** *(complete)* | Dynamic workspace configuration (§21.3): workspace config moves to Postgres as source of truth with the deployment file as bootstrap seed; authenticated, validated config read/write API with optimistic concurrency and `config.updated` audit events; hot reload of routing, repos, budgets, and bounce limits without a control-plane restart; Workspace UI becomes editable for stage routing, workspace basics, and repos & environments (tool policy, images, secret *references*) | The factory is steerable from its own control surface |
| **4.7** *(complete)* | MCP execution pivot (§21.4–§21.5): retire the sandbox execution plane; triage and spec become in-process API calls on one deployment key; agent-discovered work enters through idempotent MCP task intake; implementation *and code review* delegate to the operator's own agents over the MCP work-order server (stage-typed work orders, no self-review, in-session review loop via `await_review`); requirements tree as the organizing UI for the spec corpus; artifacts for context files | **Beta: Conveyor develops Conveyor** |
| **5.1** *(post-Beta)* | Worker & execution modes (§21.12): `conveyor worker run` — enrollment via pairing token, heartbeat, harness registry with health probes, headless harness dispatch over the unchanged §17.4 lifecycle; Auto/Manual execution modes plus independent gate toggles replacing L0–L3; mode surfaces in UI/CLI/intake | Unattended execution on operator hardware |
| **5.2** *(post-Beta)* | Adversarial review panel (§21.12): per-seat pinned reviewer models, unanimous-approve aggregation, merged bounce feedback, `model_enforcement` independence labels | Reviewer independence, enforced rather than asserted |
| **5.3** *(post-Beta, parallelizable with 5.2)* | GitHub coordination (§21.12, §21.15): issue created or reused on spec approval, review verdicts and resolutions mirrored to the PR; PR creation stays at `submit_for_review` (§21.15) | The task's trail is legible on GitHub alone |
| **5.4** *(post-Beta)* | Verification evidence (§21.12): evidence-gated `submit_for_review`, evidence artifacts in review work orders, on the review card, and on the PR | Reviewers confirm evidence rather than reproduce behavior |
| **5.5** *(post-Beta)* | Worker service packaging (§21.16): `conveyor worker install` / `uninstall` / `status` — launchd agent (macOS) / systemd user unit (Linux) wrapping the existing `worker run`, restart-on-failure, start-on-boot, documented log location | Auto capacity survives reboots without operator ritual |
| **5.6** *(post-Beta)* | Platform agents & policy *(renumbered from Phase 5 by §21.12, from 5.5 by §21.16)*: monitor agent — CI/post-merge signals → tasks, out-of-pipeline reverse sync (§4, §4.2); repo-resident `.conveyor/` hints *(shim approval cards and environment inference retired with the sandbox lane, §21.4)* | The factory guards and onboards itself |
| **6** *(post-Beta)* | Memory store (§15.1): Postgres + pgvector, workspace knowledge and lessons, the spec corpus as amendable intent (§4.2), hybrid retrieval with per-role context budgets | Agents work from accumulated context |
| **7** *(post-Beta)* | Flywheel: transcript mining, self-improvement proposals, escalation-level graduation, pack versioning with the eval rig and shadow runs (§2.2, §15.2) — consuming the transcript corpus Beta accumulates | The flywheel |
| **8** *(demand-triggered)* | Verification agent (Playwright + computer use, §12), K8sRunner, multi-repo worktree sets + linked-PR gating (§7.1), aggregate cost dashboard and budget policy (§14) *(per §21.4, this phase is the reintroduction of managed execution — until then, repo CI is the mechanical verifier)* | Trust + scale |
| **9** *(demand-triggered)* | SSO/OIDC adapter, SCIM, RBAC enforcement, HA/backup hardening (§18.1) | Enterprise-ready |

The Phase 1 runner and checkout storage mechanics in this table are amended by
§21.1; phases 3–9 are restructured by §21.2, which records the Beta milestone,
its exit criterion, and the deferral rationale. Phase 4.5 is inserted by §21.3,
which moves workspace configuration into the control plane. Phase 4.7 is
inserted by §21.4, which retires the sandbox execution plane in favor of the
MCP work-order model and re-gates Beta entry on it; §21.4 also amends the
Phase 5 and Phase 8 rows. §21.5 extends that same MCP surface with durable,
idempotent task intake without changing the Beta gate. §21.12 replaces the
former Phase 5 row with phases 5.1–5.5 — worker execution and Auto/Manual
modes, adversarial review, GitHub coordination, verification evidence, and
the renumbered platform-agents phase — all post-Beta; the Beta gate and its
exit criterion below are unchanged and run first. §21.16 inserts worker
service packaging as Phase 5.5 and renumbers platform agents to 5.6.

**Beta exit criterion (§21.4):** five consecutive real tasks on the Conveyor
repository shipped through the full pipeline — issue → triage → approved spec
→ implement work order claimed over MCP by the operator's own agent →
`submit_for_review` → review work order claimed by a *different* agent
session → PR → merge — with at least one task completing a
`changes_requested` round inside the implementing agent's session, zero manual
git operations outside the agents' own workflows, and all human actions taken
through the UI or CLI.

**Met July 15, 2026.** The operator ran the live exit flow and the five-task
sequence from real MCP sessions on this repository; the merged
`conveyor/task-*` pull requests (#3–#7, #9, #10) are the recorded trail.
Beta is achieved and the post-Beta phases 5.1–5.5 (§21.12) are unblocked.

---

## 20. Decision log (formerly open questions)

1. ~~Cloud runner choice for v1.~~ **Resolved:** Kubernetes Jobs (`K8sRunner`) for vendor neutrality — any managed cluster or self-hosted k3s — with optional RuntimeClass isolation hardening (gVisor / Kata-Firecracker). Fly Machines and similar vendor-specific backends deferred to optional adapters (§3.2).
2. ~~Harness session-resume fidelity.~~ **Resolved:** hybrid scheme — a harness-agnostic handoff snapshot is the guaranteed continuity contract on every job, with native session resume attempted as an optimization where supported and cleanly restorable (§8.3). A Phase 1 experiment calibrates per-harness defaults: kill/resume each harness (same sandbox, fresh sandbox on another host, and after a minor version bump), scored by rationale-recall probes and by token cost of the resumed continuation vs. a snapshot-briefed cold start.
3. ~~Cross-repo merge coordination.~~ **Resolved:** expand/contract decomposition by the spec agent as the default (no PR is ever unsafe alone), linked-PR gating with ordered, freshness-checked, back-to-back merges as the fallback, and automatic fix-forward on partial landing. A cross-repo merge train is deferred until monitored broken-pair/stale-green incident rates justify it (§7.1).
4. ~~Spec format.~~ **Resolved:** markdown prose for intent and rationale, plus two schema-validated fenced blocks (`conveyor:acceptance`, `conveyor:decomposition`) with stable IDs and per-criterion verification methods; the approved spec version is the verification contract (§4.1).
5. ~~Subscription-terms drift.~~ **Resolved:** all three credential classes (personal subscription, team/enterprise seat, API key) are first-class routing capacity; a per-vendor policy registry (data, not code) gates subscription routing, a kill switch degrades gracefully to other classes with the cost delta surfaced, credential owners get informed consent and notification on flag changes, and a no-circumvention invariant (§2.2) forbids evading limits, disguising traffic, or pooling credentials (§5.2).
6. ~~Local-runner security posture.~~ **Resolved:** three confinement tiers — containerized local (default), harness-native or OS-primitive sandboxing, and opt-in unconfined mode that is audit-labeled and receives no workspace secrets — with the applied tier recorded per job, tool policy mapped into each harness's native permission config by the adapter, `local_eligible` on secret sets, and per-workspace confinement floors (§8.5).
7. ~~Pack upgrade mechanics.~~ **Resolved:** shadow runs are the adoption gate — every candidate pack version replays against the workspace's curated task set via the §15.2 eval rig before re-pinning, jobs record `pack_version` for post-adoption attribution and one-step rollback, and live-traffic canarying is deliberately omitted (§2.2).

---

## 21. Amendments

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
   ad hoc triage answer.
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

*End of specification. v1.16 accepted July 17, 2026; all seven originally open questions resolved (§20), Phase 1 closure boundaries amended (§21.1), phases 3–9 restructured for the Beta milestone (§21.2), workspace configuration moved into the control plane (§21.3), execution pivoted to the MCP work-order model with requirements tree and artifacts, Phase 4.7 gating Beta (§21.4), durable MCP task intake added without a parallel triage path (§21.5), budget allocation/enforcement removed while usage telemetry remains observational (§21.6), operator-owned branch creation plus the repository Codex plugin made explicit (§21.7), dedicated local task worktrees made the safe default (§21.8), work-order queue, execution, and lease clocks separated with audited stale recovery (§21.9), explicit multi-workspace control-plane isolation added (§21.10), the human gate rebuilt verdict-first with derived reason codes, pull-to-local retired from the review UI, and redirect surfaced as "Request changes" (§21.11), and unattended execution productized post-Beta — the worker, Auto/Manual execution modes replacing the escalation ladder, adversarial review panels, factory-coordinated GitHub, and evidence-gated review (§21.12), with the worker execution contract — route-selected harnesses, route-scoped health gating, worker-control lease endpoints, token-exchange enrollment, stage-aware capacity, and the gate truth table with intake-time resolution — fixed by §21.13, deterministic field-local harness-template expansion fixed by §21.14, draft-PR-on-first-push dropped in favor of the existing PR-at-submit behavior (§21.15), and worker service packaging inserted as Phase 5.5 with platform agents renumbered to 5.6 (§21.16). Subsequent changes proceed by amendment with version bumps.*
