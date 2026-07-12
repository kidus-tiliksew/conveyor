# Conveyor: A Software Factory Platform

**Specification — v1.3**
**Date:** July 12, 2026
**Status:** Accepted — dynamic workspace configuration amendment applied (§21.3)
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

**Review UI.** The human gate (§13). A single inbox of items awaiting human decision, each showing the diff, the spec, verification evidence (screenshots/video), cost so far, and the agent's own summary. Actions: approve, reject, redirect-with-comment (re-dispatches the task rather than requiring the human to fix it), or pull-to-local (creates a worktree in the human's checkout, §8.4).

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

### 13.1 Escalation levels

| Level | Meaning | Examples (initially) |
|---|---|---|
| **L0** | Fully automatic; auto-merge on green verification | Dependency bumps, lint/format fixes |
| **L1** | Automatic with a one-click human approve | Small bugs with strong test coverage |
| **L2** | Human reviews the spec before implementation; reviews the PR after | Features, cross-repo changes |
| **L3** | Human pairs interactively (task pulled off the line) | Ambiguous, architectural, or novel work |

Task classes are assigned a level per workspace; classes **graduate downward** as their measured success rate over a trailing window crosses thresholds, and are demoted on regressions. Graduation proposals come from the self-improvement engine and require human sign-off. The distribution of tasks across levels is the automation-rate metric made concrete.

### 13.2 The review inbox

One queue, one card per pending decision. Each card shows: the diff, the governing spec, verification evidence, cost so far vs. budget, the agent's summary and self-assessment, and prior bounce history. Actions:

- **Approve** (merge or advance),
- **Reject** (close with reason code),
- **Redirect** — leave comments; the task re-dispatches into its existing worktree (§8.3); the human writes feedback, not code,
- **Pull to local** — emits the `conveyor checkout` command / deep link (§8.4).

Every action records a structured **reason code** (spec-wrong, hallucinated-API, style, flaky-env, scope-creep, broken-pair, …). Reason codes are the primary training signal for self-improvement.

### 13.3 Activity view

The primary UI surface (Phase 2) is an activity view modeled on stage-grouped task feeds, with three fixed elements:

1. **Stage-grouped feed.** Tasks grouped by pipeline stage — Triage / Spec / Implementing / Reviewing / Verifying / Awaiting human — as collapsible sections with counts. The distribution of work across stages is the factory's health made visible: a pile-up in any stage is immediately apparent. Each row shows the task ID, title, escalation-level badge, provenance chips (source channel, ticket ref, PR number), a "Needs attention" badge where a human gate or circuit breaker has fired, and recency. "Needs attention" is the only alarm color on the page; visual economy is a design requirement, since the product's goal is fewer human touches, and attention states must read as exceptions.
2. **Costed event timeline.** The task detail panel narrates the task's history as a timeline: one entry per pipeline stage, each showing the agent's summary of what it did, wall-clock duration, cost, and which harness / model tier / auth mode ran it (e.g. "4m 03s · $0.94 · codex / subscription"). This makes per-stage cost something operators absorb passively during normal review rather than a separate dashboard, and it is the audit log (§3.1) rendered as a story. The task header shows budget consumed vs. allocated and a verification badge with pass count against the spec's acceptance criteria.
3. **Review actions in place.** For tasks at a human gate, the detail panel carries the §13.2 actions directly: approve, reject, redirect-with-comment, and pull-to-local. A reviewer never leaves the timeline context to act on what it shows.

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
| **4.5** | Dynamic workspace configuration (§21.3): workspace config moves to Postgres as source of truth with the deployment file as bootstrap seed; authenticated, validated config read/write API with optimistic concurrency and `config.updated` audit events; hot reload of routing, repos, budgets, and bounce limits without a control-plane restart; Workspace UI becomes editable for stage routing, workspace basics, and repos & environments (tool policy, images, secret *references*) | **Beta: Conveyor develops Conveyor** |
| **5** *(post-Beta)* | Platform agents & policy: command-policy shim with review-queue approval cards (§11.2); environment inference & repair agents (§6.4); monitor agent — CI/post-merge signals → tasks, out-of-pipeline reverse sync (§4, §4.2) | The factory guards and onboards itself |
| **6** *(post-Beta)* | Memory store (§15.1): Postgres + pgvector, workspace knowledge and lessons, the spec corpus as amendable intent (§4.2), hybrid retrieval with per-role context budgets | Agents work from accumulated context |
| **7** *(post-Beta)* | Flywheel: transcript mining, self-improvement proposals, escalation-level graduation, pack versioning with the eval rig and shadow runs (§2.2, §15.2) — consuming the transcript corpus Beta accumulates | The flywheel |
| **8** *(demand-triggered)* | Verification agent (Playwright + computer use, §12), K8sRunner, multi-repo worktree sets + linked-PR gating (§7.1), aggregate cost dashboard and budget policy (§14) | Trust + scale |
| **9** *(demand-triggered)* | SSO/OIDC adapter, SCIM, RBAC enforcement, HA/backup hardening (§18.1) | Enterprise-ready |

The Phase 1 runner and checkout storage mechanics in this table are amended by
§21.1; phases 3–9 are restructured by §21.2, which records the Beta milestone,
its exit criterion, and the deferral rationale. Phase 4.5 is inserted by §21.3,
which moves workspace configuration into the control plane and re-gates Beta
entry on it.

**Beta exit criterion (§21.2):** five consecutive real tasks on the Conveyor
repository shipped through the full pipeline — issue → triage → approved spec
→ implementation → PR → merge — with at least one task completing a redirect
round, zero manual git operations, and all human actions taken through the
Phase 4 UI or CLI.

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

---

*End of specification. v1.3 accepted July 12, 2026; all seven originally open questions resolved (§20), Phase 1 closure boundaries amended (§21.1), phases 3–9 restructured for the Beta milestone (§21.2), workspace configuration moved into the control plane with Phase 4.5 gating Beta (§21.3). Subsequent changes proceed by amendment with version bumps.*
