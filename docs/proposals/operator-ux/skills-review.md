# Skills and harness-neutrality review

Read-only review of the Conveyor skills, playbooks, managed guidance, and product couplings against the goal "Conveyor coordinates; it does not launch agents".
All repository citations are `origin/main` at 1c70303e unless marked HEAD.
Line numbers come from `git show origin/main:<path>`.
Corpus IDs (REQ, AC, DEC, component names) are quoted from code comments and playbooks and are unverified against the live corpus.
Web findings about other tools' skill directories are cited by URL; anything not read from a primary source is marked [INFERENCE].

## Contents

1. [Inventory](#1-inventory)
2. [Harness, model, and launcher assumptions in the skills](#2-harness-model-and-launcher-assumptions-in-the-skills)
3. [Product-side couplings](#3-product-side-couplings)
4. [Harness-neutral structure](#4-harness-neutral-structure)
   - 4a. [Skill set](#4a-skill-set)
   - 4b. [Harness capability contract](#4b-harness-capability-contract)
   - 4c. [Where per-operator setup lives](#4c-where-per-operator-setup-lives)
   - 4d. [CLI and MCP gaps](#4d-cli-and-mcp-gaps)
   - 4e. [Install targets](#4e-install-targets)
5. [Prioritized change list](#5-prioritized-change-list)
6. [Open questions for the operator](#6-open-questions-for-the-operator)

## 1. Inventory

Sizes are `wc -l` on `git show origin/main:<path>`, and plain `wc -l` for the pasted file.
Each installed skill is a thin `SKILL.md` wrapper plus one canonical playbook copied from `docs/playbooks/` (`cmd/conveyor/skills.go:22-25`, `:39-118`).
The `skills_assets` playbook copies are byte-identical to `docs/playbooks/` (equal MD5 observed), and a test fails on drift (`cmd/conveyor/skills_test.go:30-35`).
The `.claude/skills/*/SKILL.md` repo sources are byte-identical to the `skills_assets` wrappers (equal MD5 observed).

| Skill / file | Purpose | Trigger (frontmatter or role) | Lines |
| --- | --- | --- | --- |
| `conveyor-work` SKILL.md + `conveyor-work.md` | Claim, contract, checkout, lease, submit, release; self-claimed delivery loop; human gates; bounded-queue rules | "work on or implement a Conveyor task ID" (`skills_assets/conveyor-work/SKILL.md:3`) | 52 + 605 |
| `conveyor-file-tasks` + `conveyor-task-filing.md` | Investigate, then file tasks or dependency-ordered sets via MCP `create_task` | "file a task, a fix wave, or a phase's work breakdown" (`skills_assets/conveyor-file-tasks/SKILL.md:3`) | 27 + 174 |
| `conveyor-plan` + `conveyor-planning.md` | Draft and push requirement, System Design, DEC, and reference documents as proposals over REST | "plan features, write or revise any document tier" (`skills_assets/conveyor-plan/SKILL.md:3`) | 18 + 306 |
| `conveyor-kit` + `conveyor-kit.md` | Author verification kits and manifests under an implementation claim | "Author or extend repository verification kits" (`skills_assets/conveyor-kit/SKILL.md:3`) | 13 + 332 |
| `conveyor-kit-verify` + `conveyor-kit-verify.md` | Execute kits and obligations under a verify claim and submit the result | "Execute selected kits ... under a live verify claim" (`skills_assets/conveyor-kit-verify/SKILL.md:3`) | 14 + 488 |
| `conveyor-testing-doc` + `conveyor-testing-doc.md` | Write a testing-strategy System Design document | "Create or revise a testing-strategy System Design document" (`skills_assets/conveyor-testing-doc/SKILL.md:3`) | 16 + 148 |
| `docs/playbooks/checkpoint-recovery.md` | Recover a preserved implementation checkpoint | No skill; not in the embedded manifest (`cmd/conveyor/skills.go:39-118`) | 82 |
| `skills_assets/conveyor-repo-init/AGENTS.md` | Managed AGENTS/CLAUDE section written by `conveyor repo init` | Always loaded by AGENTS.md-aware tools | 13 |
| `CLAUDE.md` (`AGENTS.md` is a symlink to it) | This repository's own agent notes plus the managed section | Always loaded | 85 |
| `plugins/conveyor/skills/conveyor-operator/SKILL.md` | Codex plugin skill: create/triage, implement, review via MCP | "Use when Codex must create or triage ..." (`:3`) | 176 (+4 `agents/openai.yaml`, 38 `.codex-plugin/plugin.json`) |
| `conveyor-coordinate` (pasted, conveyor-coordinate skill (not included)) | Supervise many tasks: setups, threads, approvals, review admission, merge, release | "coordinate these tasks", "keep this queue moving" (`:4`) | 58 |

### Overlap

- `conveyor-work.md:116-156` ("Coordinate a bounded queue") and `conveyor-work.md:272-584` (self-claimed loop, stage preferences, launch examples, gates) already contain most of what `conveyor-coordinate` does for one task.
- `conveyor-coordinate:44-49` (review admission, merge) re-states `conveyor-work.md:116-148` with a wider authority claim (see §2.3).
- `conveyor-operator/SKILL.md:41-84` duplicates the claim and implement steps of `conveyor-work.md:35-73`, and `:139-159` duplicates the review stage.
- `conveyor-operator/SKILL.md:25-39` duplicates `conveyor-task-filing.md:52-64` for `create_task`.
- `conveyor-operator/SKILL.md:93-123` is a manual-git fallback for `conveyor checkout` that no other skill has.
- The "Claim identity" text appears twice: `conveyor-work.md:75-93` and `conveyor-kit-verify.md:45-57`.
- `conveyor-kit.md` and `conveyor-kit-verify.md` share permission and toolchain sections (`conveyor-kit.md:142-199` vs `conveyor-kit-verify.md:201-336`, compared by section headings only) [INFERENCE: overlap judged from headings, not a line diff].
- `conveyor-planning.md` and `conveyor-testing-doc.md` overlap on document push; the testing-doc playbook defers to planning for mechanics (`conveyor-testing-doc.md:147`).
- `checkpoint-recovery.md` overlaps the checkout and release rules of `conveyor-work.md:54-61` but is not installed, so agents in other repositories never see it.

## 2. Harness, model, and launcher assumptions in the skills

### 2.1 Explicit assumptions

| Where | Assumption | Why it matters |
| --- | --- | --- |
| `conveyor-work.md:16-20`; `skills_assets/conveyor-work/SKILL.md:28-31` | A "launched session" mode exists, owned by `conveyor run` or a worker | Makes the launcher a first-class protocol actor in the main skill. |
| `conveyor-work.md:21-23` | Names Claude Code, Codex, OpenCode, Cursor as example self-claimers | Harmless as examples, but it omits OMP and T3 Code and reads as a support list. |
| `conveyor-work.md:82-89`; `conveyor-kit-verify.md:51-57` | `agent` values `claude-code`, `codex`, `opencode`, `cursor`; model example `claude-opus-5-5`; "as Cursor Auto does" | Fine as examples; the duplicated block will drift. |
| `conveyor-work.md:91-93`; `web/src/lib/model-logo.ts:7`, `:24-27` | Dashboard falls back to a harness logo from a fixed alias list | `omp` and `t3code` get no harness attribution in the UI. |
| `conveyor-work.md:108-109` | `worker_fallback` usage source "belongs to workers" | A worker-only telemetry path inside the agent playbook. |
| `conveyor-work.md:175-178` | Launcher renews every ten seconds | Launcher internals in an agent document. |
| `conveyor-work.md:222-225`, `:465-466`, `:495-497` | Launcher owns verdicts, successors, and worktree removal | The playbook branches on who launched the session. |
| `conveyor-work.md:264-267` | `CONVEYOR_GIT_ASKPASS_*` are launcher-only variables | Launcher internals in an agent document. |
| `conveyor-work.md:261-263` | CLI sign-in must be the same user "whose MCP registration made the claim" | Assumes the claim came through an MCP registration. |
| `conveyor-work.md:301-311`, `:347-356` | Launch prompts say "Use the Conveyor MCP registration for that server" | Assumes the child has native MCP and the same registration. |
| `conveyor-work.md:318-321` | Stage preference lives in "the session's own agent memory"; never in a Conveyor-owned file (DEC-44) | Requires persistent memory, and forbids the obvious harness-neutral store. |
| `conveyor-work.md:333-337` | With no preference and no operator, "start an isolated subagent of the session's own harness" (AC-3.5) | Requires a subagent facility. |
| `conveyor-work.md:364-382` | Per-harness launch table: Claude Code `Agent` tool with `subagent_type`, `claude -p ... --allowedTools 'mcp__<registration>__*'`, Codex multi-agent tool and `codex exec`, OpenCode `task` tool and `opencode run`, Cursor subagent and `cursor-agent -p` | Tool-name syntax and CLIs of four harnesses inside the canonical playbook. |
| `conveyor-work.md:379-382` | Claude Code and Codex use a header helper; OpenCode and Cursor use an env var | MCP registration style per harness. |
| `conveyor-kit-verify.md:40-42`; `cmd/conveyor/kit_verify.go:47` | `conveyor verify` needs "launcher-provided" `CONVEYOR_CLIENT_TOKEN` in the environment | A self-claimed verifier must put its client token into a shell command, which `conveyor-work.md:44-45` and `:268-270` forbid. |
| `checkpoint-recovery.md:20-30`, `:37-40` | Recovery must go "through the launcher, not a standalone checkout"; requires launcher-set writer variables | A self-claimed harness has no supported recovery path. |
| `conveyor-planning.md:3` | "You are the headless planning twin" | Frames the agent relative to the in-product chat, not a protocol role. |
| `conveyor-planning.md:10-11` | `Authorization: Bearer $CONVEYOR_API_TOKEN` from the repo `.env`; "Single workspace today: `demo`" | Repo-local secret and a hard-coded workspace, both wrong when installed into another repository; `conveyor auth token` exists for this (`cmd/conveyor/auth.go:115`). |
| `conveyor-planning.md:118-122`, `:164-171`, `:249-258` | Document pushes are raw REST calls | Requires an HTTP client plus token handling; no CLI or MCP equivalent is named. |
| `conveyor-task-filing.md:19-24` | "Use the server-specific native MCP connection" | Requires native MCP for read and file. |
| `skills_assets/conveyor-repo-init/AGENTS.md:11` | Lists only `conveyor-plan`, `conveyor-file-tasks`, `conveyor-work` | Kit, kit-verify, and testing-doc are installed but not routed. |
| `cmd/conveyor/repo_init.go:150` | Managed section says "Select a native MCP connection whose endpoint matches this server" | MCP is the only access path offered. |
| `CLAUDE.md:55-56` | "`.claude/skills/` wraps them for Claude Code"; AGENTS.md symlink for Codex | Names two harnesses as the audience. |
| `conveyor-operator/SKILL.md:3`, `:37`, `:141` | "Use when Codex must ..."; "do not run a second triage path in Codex"; "Review in a fresh Codex session" | Codex-only skill. |
| `conveyor-operator/SKILL.md:14-17` | Setup via `conveyor mcp install --tool <client>` | Only the four supported clients can follow it (`cmd/conveyor/mcp_install.go:198-209`). |
| `conveyor-operator/SKILL.md:38-39` | "If no task-status tool is available, direct the user to Conveyor's dashboard or worker logs instead of inventing polling" | Contradicts `conveyor-work.md:570-584`, which waits with `conveyor task wait`. |
| `conveyor-operator/SKILL.md:98` | `CONVEYOR_TASK_REPO_URL` "in a worker session" | Worker environment inside an agent skill. |
| `conveyor-operator/SKILL.md:172-176` | "Workers" section on enrollment credentials | Launcher concern. |
| `conveyor-coordinate:4`, `:7`, `:25` | "run each task's stages in separate agent threads inside the harness"; "Use whatever thread facility your harness provides" | Requires a thread or subagent facility. |
| `conveyor-coordinate:9`, `:21-22` | Setup preferences come from "your own agent memory"; or the "Saved coordination preference:" sentence convention for a memory pass | Requires memory; the sentence convention fits one harness family's conversation-mining memory [INFERENCE: which harnesses mine conversations is not verified]. |
| `conveyor-coordinate:17-20` | Setup = host, harness/model/effort per role, concurrency | Operator setup with no Conveyor-side home other than memory. |
| `conveyor-coordinate:23` | "A thread cannot ask the operator; it reports to you" | Assumes the coordinator–thread topology. |
| `conveyor-coordinate:25` | Threads reach Conveyor "through the harness's own installed MCP registration" | Assumes children inherit MCP. |
| `conveyor-coordinate:25`, `:42` | "instead of through `conveyor run`"; record an operator preference for `conveyor run` or a worker | Launcher still named as an alternative path. |
| `conveyor-coordinate:28` | Base prompt "Use the conveyor-work skill ..." | Assumes the child harness has the skills installed and invokes them by name. |
| `conveyor-coordinate:38` | `conveyor task wait` "where the installed CLI has it" | The command exists on origin/main (`cmd/conveyor/task_wait.go:20-22`, `:120`); the hedge is stale. |
| `conveyor-coordinate:39` | "continue the same implementer thread" | Requires resumable threads. |
| `conveyor-coordinate:15` | Uses `references/coordination-record.md` | The reference file was not supplied with the pasted skill; it dangles. |

### 2.2 Implicit assumptions

| Assumption | Where it is relied on | Why it matters |
| --- | --- | --- |
| Native MCP client | Every lifecycle step names MCP tools: `list_work_orders`, `claim_work_order`, `get_work_order`, `read_artifact`, `report_progress`, `renew_work_order`, `await_review` (`conveyor-work.md:37-71`, `:179-185`, `:386-399`) | The CLI has no claim, renew, contract, progress, plan-submit, or verdict command (CLI command list in `cmd/conveyor/main.go:322-752`, `cmd/conveyor/submit.go:26`, `cmd/conveyor/task_wait.go:120`), so a shell-only harness cannot comply. |
| Fresh-context sessions on demand | `conveyor-work.md:287-299`, `:325-337`; `conveyor-coordinate:36-37` | The self-claimed loop cannot get independent review without a second session. |
| Persistent memory | `conveyor-work.md:318-340`; `conveyor-coordinate:21-22` | Without memory the operator is asked every session. |
| Long blocking tool calls (up to 600 s) | `await_review` (`conveyor-work.md:386-392`; default 300 s at `internal/httpapi/mcp.go:494-499`); `conveyor task wait --timeout 5m` (`conveyor-work.md:573-579`) | Some harnesses cap tool or command duration [INFERENCE: caps not verified per harness]. |
| User input can interleave with a blocking wait | "Handle a reply the operator types between waits before the next wait" (`conveyor-work.md:584`) | Only true where a harness can queue user messages while a command runs [INFERENCE]. |
| The agent can keep a secret out of its own transcript | `conveyor-work.md:44-45`, `:268-270` | A harness that logs every tool argument records the `client_token` passed to `claim_work_order` [INFERENCE]. |
| The agent knows its own harness name and model ID | Required `agent` and `model` in the claim schema (`internal/httpapi/mcp.go:909`) | Fine with the `auto` escape (`conveyor-work.md:86-89`); T3 Code users may report the provider rather than T3 Code [INFERENCE]. |
| Shell, Git, and the `conveyor` CLI in the same environment as the agent | `conveyor checkout`, `submit`, `done`, `verify` (`conveyor-work.md:54-61`, `:205-213`, `:441-450`) | A sandboxed or cloud harness without the CLI or host Git credential cannot deliver. |
| Lease management by milestone | `lease_seconds` up to 3600 and renew at milestones (`conveyor-work.md:179-185`) | A single step longer than one hour loses the claim (see proposal A10, `proposal.md:110-120`). |
| Skill-to-skill invocation by name | "Follow the `conveyor-kit-verify` skill" (`conveyor-work.md:214-215`, `:304-305`) | Requires the child harness to have the same skill set installed. |

### 2.3 Conflicts between the pasted coordinate skill and the repository

- `conveyor-coordinate:12` and `:14` treat a coordination request as standing authority to approve plans, proposals, review admission, and merge across turns.
- `conveyor-work.md:521-526` and `skills_assets/conveyor-work/SKILL.md:21-23` allow recording an operator decision only "on the operator's direct instruction in the same conversation, for its own task, with the operator's own credential (DEC-45)".
- These two cannot both hold; one of them needs a DEC change or the coordinate text must narrow.
- `conveyor-coordinate:31` tells the implementer thread to submit and end without waiting for verdicts.
- `skills_assets/conveyor-work/SKILL.md:31-37` and `conveyor-work.md:238-240` tell every self-claimed implementation session to run the delivery loop instead of exiting.
- A coordinator-started implementer is self-claimed (it calls `claim_work_order` itself), so it receives contradictory instructions from the two skills.
- `conveyor-coordinate:21-22` (memory) agrees with DEC-44 as quoted at `conveyor-work.md:318-321`, so moving setup out of memory (§4c) needs a DEC-44 revision.

## 3. Product-side couplings

Legend: Declarative = server states a requirement or records a self-report for audit; Stay = keep as is; Retire = remove.

| # | Coupling | Code | Recommendation |
| --- | --- | --- | --- |
| P1 | Server-pinned execution fields on every work order: `RequiredModel`, `RequiredHarness`, `RequiredEffort`, `RequiredHarnessConfig` | `internal/core/types.go:631-634` | Retire: they are already cleared on re-queue "(req-worker AC-2.2, AC-2.3; DEC-56)" (`internal/core/types.go:689-697`) and ignored by the worker (`cmd/conveyor/run_cmd.go:1044-1045`), so only legacy in-flight orders carry them. |
| P2 | Review-claim model enforcement: a worker claim is refused when its model differs from the pinned seat model; labels `worker-pinned` vs `self-reported` | `internal/store/postgres/work_orders_store.go:1196-1202`; mirrors at `internal/store/work_orders_store.go:2048-2055`, `internal/store/singlestore/work_orders.go:967`, `:1023-1025` | Declarative: drop the refusal and replace the label with the identity source (`self_reported` from the claim, `launcher_reported` when a client process attests it). |
| P3 | Worker claims record `agent = "worker"` and `model = RequiredModel` | `internal/worker/service.go:791-792` | Declarative: record the launched harness name and model as `conveyor run` already does (`cmd/conveyor/run_client.go:101-102`). |
| P4 | `report_continuation` is reserved: agent credentials are refused, and the tool says only the launching worker or `conveyor run` may call it | `internal/httpapi/mcp.go:145-146`, `:553`, `:919`; `internal/workorder/service.go:1286-1288` | Declarative: let any claimant report its own resumable-session handle for audit, since resume eligibility already stays advisory (`internal/core/types.go:35-36`, `:756-765`), which is also the hook for proposal A11 (`proposal.md:122-126`). |
| P5 | Continuation harness must equal the claim's `agent` or the pinned harness | `internal/store/work_orders_store.go:512-517`; `internal/store/postgres/worker.go:302`; `internal/store/singlestore/workers.go:352` | Stay, once P1 removes the pinned fallback; matching the claim's own `agent` is a consistency check, not control. |
| P6 | Server publishes "active harnesses" probe targets (built from `RequiredHarnessConfig` snapshots) to workers | `internal/worker/service.go:456-482`; `internal/httpapi/worker.go:197` | Retire with P1. |
| P7 | Task view shows worker availability and `RequiredHarnesses` | `internal/worker/service.go:70-77`, `:603-604`; `internal/httpapi/server.go:2174-2181` | Declarative: keep it as advisory "is anyone claiming" state (pull-only workspaces already exist, `internal/worker/service.go:79-80`) and retire the always-empty `RequiredHarnesses` field. |
| P8 | Harness/model failure memory keyed by pinned harness and model | `internal/store/work_orders_store.go:616-619` | Declarative: key it by the claim's self-reported `agent`/`model`. |
| P9 | Rate-limit health groups by pinned harness, falling back to the claim | `internal/worker/service.go:497-503` | Declarative: group by the claim only. |
| P10 | `same_model_as_implementer` derives the implementer model from job `ModelTier` | `internal/dispatch/dispatch.go:1067-1078` | Declarative: compare self-reported claim models [INFERENCE: `ModelTier` is likely empty for self-claimed work, making the field `unknown`]. |
| P11 | `conveyor run` explicitly claims and launches a harness child | `cmd/conveyor/run_cmd.go:31-35` | Stay as an optional client adapter, but remove it from the skills' protocol, where a skill only needs "if another process claimed this order for you, report and exit". |
| P12 | `conveyor worker run` heartbeats, auto-claims, and supervises harnesses; worker-only routes | `cmd/conveyor/worker_cmd.go:66`; `internal/httpapi/server.go:158-183` | Stay as an optional adapter, but give every worker-only capability a claimant-neutral equivalent: attempt checkpoint (`:172`), attempt observability (`:175`), worktree handoff (`:176`), worktree cleanup (`:178-179`). |
| P13 | Checkpoint recovery needs the launcher's writer lock and environment | `docs/playbooks/checkpoint-recovery.md:20-40`; `cmd/conveyor/main.go:684-685` | Declarative: let `conveyor checkout` fetch predecessor evidence from the server for a self-claimed successor instead of requiring launcher-set variables. |
| P14 | `conveyor verify` and `conveyor submit` take claim identity only from environment variables | `cmd/conveyor/kit_verify.go:47`; `cmd/conveyor/submit.go:35`, `:57` | Declarative: add a CLI claim that stores the session and token in a local 0600 file, so no secret appears in a command line (§4d G1, G2). |
| P15 | Client-local execution setup holds harness argv, MCP transport, model and effort args | `cmd/conveyor/execution_setup.go:3-7`; `internal/config/config.go:333-348` | Stay: it is already client-only ("deliberately has no HTTP client") and is the natural home for per-operator setup (§4c). |
| P16 | Harness template catalog (codex, claude, grok, cursor, opencode) | `internal/config/harness_templates.go:13-16`; callers only in `cmd/conveyor/execution_setup.go:361`, `:399-400`, `:935`, `:963` and `named_execution_setup.go:637` | Stay (client-only); fix the stale "used by the operator UI" comment at `:13-14`. |
| P17 | `create_task` rejects `harness`, `model`, `effort`, `argv` as "retired execution detail" | `internal/httpapi/mcp.go:209-211` | Stay: this is the precedent the rest should follow. |
| P18 | Fixed execution deadline and lease cap | `conveyor-work.md:169-173`; default stage timeout `internal/config/config.go:550` | Declarative per proposal A10 (`proposal.md:110-120`). |

## 4. Harness-neutral structure

### 4a. Skill set

| Skill | Action | Content |
| --- | --- | --- |
| `conveyor-work` | Narrow | One claimed order end to end (find, claim, contract, artifacts, checkout, lease, validate, submit or release, exit or hand back), keeping claim identity, usage, and the authority boundary, moving the multi-order loop and gates out, and replacing the two session modes with one rule: "if a launcher claimed this order for you, report and exit after submission". |
| `conveyor-coordinate` | Add to the repo | The loop across orders and tasks (setup resolution, starting sessions per role, verdict waiting, bounces, human gates, bounded-queue policy, cleanup and fast-forward), absorbing `conveyor-work.md:116-156` and `:272-584`. |
| `conveyor-plan` | Keep, fix | Replace the `.env` token and `demo` workspace (`conveyor-planning.md:10-11`) with `conveyor auth token` and the managed section's workspace; move to CLI commands when G3 lands. |
| `conveyor-file-tasks` | Keep | Add a CLI path when MCP is absent (`conveyor task new` exists, `cmd/conveyor/main.go:328`) [INFERENCE: whether `task new` covers `depends_on` and gate fields was not checked]. |
| `conveyor-kit`, `conveyor-kit-verify`, `conveyor-testing-doc` | Keep | Point kit-verify's claim identity at the shared reference; fix the client-token handling with G1. |
| `checkpoint-recovery` | Fold into `conveyor-work` | As a "Recover a preserved attempt" section once P13 lands; until then it stays launcher-only and should say so. |
| `conveyor-operator` (Codex plugin) | Retire | It duplicates `conveyor-work` and `conveyor-file-tasks`, is Codex-named, and contradicts the wait rule (`conveyor-operator/SKILL.md:38-39`); if the Codex marketplace entry must stay, make the skill a pointer to the installed skills. |
| Shared `references/protocol.md` | Add | One copy of claim identity, the authority boundary, and the capability contract (§4b), shipped inside each skill directory by the installer so wrappers stay thin. |

What `conveyor-coordinate` must drop or reword before it enters the repo:

- "separate agent threads inside the harness" (`:4`, `:7`, `:25`) becomes "a separate session per role; if your harness can start sessions itself, start them; otherwise print the launch prompt for the operator to start one".
- "Read the operator's saved setup preferences from your own agent memory" (`:21`) becomes "read the local setup (§4c); fall back to your harness's memory if it has one; otherwise ask once per session".
- The "Saved coordination preference:" sentence convention (`:22`) is removed; it encodes one memory implementation.
- "instead of through `conveyor run`" and the launcher-preference sentence (`:25`, `:42`) are removed; the launcher is not part of the protocol.
- "where the installed CLI has it" (`:38`) is removed; `conveyor task wait` exists.
- "continue the same implementer thread" (`:39`) becomes "if your harness can resume a session, resume it for the successor order with a fresh session ID; otherwise start a new session; the successor's contract carries the feedback".
- "through the harness's own installed MCP registration" (`:25`) becomes "through the Conveyor connection the capability contract names (MCP or CLI)".
- The standing-approval language (`:12`, `:14`, `:47`) is reconciled with DEC-45 (§2.3, Q1).
- The implementer-exits rule (`:31`) moves into `conveyor-work` as an explicit third role, "delegated executor", alongside the delegated planner, verifier, and reviewer that already exit (`conveyor-work.md:226-232`).
- `references/coordination-record.md` (`:15`) ships with the skill or the sentence goes.
- Release and deployment (`:58`) leave the skill; they are operator procedure, not coordination protocol.

### 4b. Harness capability contract

Required:

- R1. A shell that can run `git` and a `conveyor` CLI whose version matches the server, signed in with `conveyor auth login` (`cmd/conveyor/auth.go:19-21`).
- R2. A Conveyor tool path: a native MCP registration today; the CLI alone once G1 lands.
- R3. A checkout of the registered repository where the CLI can create task worktrees (`conveyor checkout`, `cmd/conveyor/main.go:577`).
- R4. The agent can state its harness name and its model ID, or the harness's reported value such as `auto` (`conveyor-work.md:82-89`).

Optional, with degradation:

| Capability | Used by | Without it |
| --- | --- | --- |
| O1. Start a separate fresh-context session (subagent, new thread, or headless CLI process) | coordinate, work (verifier/reviewer) | Submit, print the exact launch prompt, and stop so the operator starts the session in any harness, never reviewing your own work (DEC-11 guard, `conveyor-work.md:216-218`). |
| O2. Run several sessions in parallel | coordinate | Run tasks one at a time in dependency order. |
| O3. Persistent memory | coordinate, work | Read the local setup file (§4c); if absent, ask once per session and say the answer does not persist. |
| O4. Long blocking calls (5–10 min) | work, coordinate | Use shorter `--timeout` values in a loop, or end the turn with the exact resume command (`conveyor task wait <id>`). |
| O5. Resume an earlier session | coordinate | Start a fresh session for the successor order; the contract carries the feedback. |
| O6. Accept operator input during a wait | work, coordinate | The wait timeout is the interleave point; check for operator input between waits. |
| O7. Token counts | work, kit-verify | Skip `report_usage`; never invent figures (`conveyor-work.md:110`). |
| O8. Keep a secret out of its own logs | work, kit-verify | Use CLI commands that read the claim from a local session file (G1) instead of passing the token as an argument. |

### 4c. Where per-operator setup lives

Options:

| Option | Server stays harness-unaware | Works without memory | Shared across harnesses on one host | Cost |
| --- | --- | --- | --- | --- |
| Agent memory (today, DEC-44 per `conveyor-work.md:318-321`) | Yes | No | No; each harness keeps its own copy (`conveyor-coordinate:22`) | None |
| Local Conveyor config file | Yes; the setup code already has no HTTP client (`cmd/conveyor/execution_setup.go:3-7`) | Yes | Yes | DEC-44 revision; a schema addition |
| Server-side user preferences | No; the server would store harness and model choices | Yes | Yes, across hosts | Contradicts P1/P17 direction |

Recommendation: the local Conveyor config file.
The file already exists and is operator-selected: `--config`, `CONVEYOR_CONFIG`, or the user default, outside any checkout (`conveyor-kit-verify.md:211-213`, `:237-243`).
It already has named setups and review seats (`cmd/conveyor/named_execution_setup.go:23-150`).
Add an advisory `roles` section (host, harness, model, effort per role, concurrency) that needs no launch argv, and expose it with a read command such as `conveyor setup show --json`.
Skills read that command first, then memory, then ask.
The server never receives the section, which keeps "the server states requirements; the agent self-reports" intact.
`conveyor run` and the worker can keep reading the same file, so there is one setup store on the host.

### 4d. CLI and MCP gaps

| Gap | Effect today | Overlap with proposal |
| --- | --- | --- |
| G1. No CLI for claim, renew, contract, artifact, progress, usage, release, `submit_plan`, `submit_review_verdict`, `await_review` (CLI command list, `cmd/conveyor/main.go:322-752`) | A harness without native MCP cannot work an order. | New (not in B1/B3). |
| G2. `conveyor verify` and `conveyor submit` read claim identity from environment variables (`cmd/conveyor/kit_verify.go:47`, `cmd/conveyor/submit.go:57`) | A self-claimed verifier must type its client token into a shell command. | New. |
| G3. No CLI or MCP for document proposals; planning uses raw REST with a token (`conveyor-planning.md:10`, `:118-122`) | Every harness needs an HTTP client and token handling. | New. |
| G4. No task status or operator inbox (`conveyor task show` exists, `cmd/conveyor/main.go:373`; no `status`/`inbox`) | Agents reconstruct "what is pending" from `get_task`, `get_task_context`, `list_work_orders` (`conveyor-work.md:501-507`). | B1 (`proposal.md:132-137`). |
| G5. `conveyor task wait` exists but has no `--until operator|terminal|stage` (`cmd/conveyor/task_wait.go:20-22`) | Callers loop and re-read. | B1, last bullet (`proposal.md:137`); the proposal treats `task wait` as new, but origin/main already has the base command. |
| G6. No `order recover` / `order redispatch` / `order grant` CLI | Operator acts need the dashboard or MCP. | B3 (`proposal.md:144-147`); `conveyor task merge` already exists on origin/main (`cmd/conveyor/main.go:469`), so B3 shrinks. |
| G7. `report_continuation` refused for agent credentials (`internal/httpapi/mcp.go:145-146`, `:553`) | Self-claimed sessions cannot record a resumable handle or harness identity. | A11 (`proposal.md:122-126`). |
| G8. Self-claimed checkpoint recovery is unsupported (`checkpoint-recovery.md:20-30`) | A self-claimed harness that releases at a checkpoint cannot resume through the supported path. | Related to A5 checkpoint handling [INFERENCE: A5 text not re-read here]. |
| G9. Lease cap of 3600 s for self-claims (`conveyor-work.md:179-180`) | One long step loses the claim. | A10 (`proposal.md:110-120`). |
| G10. `conveyor mcp install` supports four clients (`cmd/conveyor/mcp_install.go:198-209`) | OMP and other MCP clients need manual registration. | New: add a `--print` generic snippet (endpoint plus header from `conveyor auth token`). |

### 4e. Install targets

What the code does:

- `conveyor skills install` detects tools by binary on `PATH` and writes the user-global directory by default (`cmd/conveyor/skills.go:204`, `:242-263`).
- Targets are `.claude/skills`, `.codex/skills`, `.cursor/skills` (detected by the `cursor-agent` binary), and `.config/opencode/skills` or `.opencode/skills` (`cmd/conveyor/skills.go:152-159`).
- `conveyor repo init` writes all four project roots without detection (`cmd/conveyor/repo_init.go:380-383`), so 4 × 12 manifest files land in each repository (`cmd/conveyor/skills.go:39-118`; product asserted at `cmd/conveyor/skills_test.go:1095` for install).
- `skillsDestination` still hard-codes `.claude/skills` (`cmd/conveyor/skills.go:234-240`).
- No `.agents/skills` target exists.

What the tools read:

| Tool | Project skill dirs read | Source |
| --- | --- | --- |
| Cursor | `.cursor/skills`, `.agents/skills`, `.claude/skills`, `.codex/skills`; user `~/.cursor/skills`, `~/.agents/skills` | https://cursor.com/docs/skills (via search summary) |
| Codex | `.agents/skills` (repo), `~/.agents/skills` (user); `~/.codex/skills` deprecated | https://github.com/openai/codex/blob/main/codex-rs/ext/skills/src/host_roots.rs (via search summary) |
| OMP | `.claude/skills`, `.agents/skills`, `.agent/skills`, plus user equivalents | https://github.com/can1357/oh-my-pi/blob/main/docs/skills.md (via search summary) |
| T3 Code | A control surface that drives Codex, Claude Code, Cursor, OpenCode providers | https://github.com/pingdotgg/t3code; skills come from the driven provider [INFERENCE] |
| Claude Code | `.claude/skills` | [INFERENCE: not re-verified; whether it reads `.agents/skills` is unknown] |
| OpenCode | `.opencode/skills` per Conveyor's own table (`cmd/conveyor/skills.go:158`) | Other dirs [INFERENCE] |

Observed on this host: the operator's skills exist as separate copies in `~/.agents/skills`, `~/.claude/skills`, `~/.codex/skills`, and `~/.omp/agent/skills` (listed with `ls`), which is the drift risk in practice.
OMP (`~/.omp/agent/skills`) is not a Conveyor install target.
`repo init`'s `.codex/skills` project root may not be read by current Codex, which uses `.agents/skills` [INFERENCE from the Codex source summary above].

Recommendation: one canonical project location, `.agents/skills/<name>/`, plus `.claude/skills/<name>/` only for Claude Code (and T3 Code driving Claude Code).
Drop `.cursor/skills`, `.codex/skills`, and `.opencode/skills` from `repo init` because Cursor, Codex, and OMP read `.agents/skills`; keep OpenCode's root only if it does not [INFERENCE].
Make the `.claude/skills` copy byte-identical, not a pointer, because Cursor and OMP read both directories and identical content keeps duplicate names harmless [INFERENCE: duplicate-name handling not verified per tool].
Avoid symlinks: `ensureSafeInstallPath` refuses symlinked roots (`cmd/conveyor/skills.go:765-773`).

## 5. Prioritized change list

Corpus IDs are quoted from code comments and playbooks and are unverified.

| # | Size | Change | Files | Corpus impact |
| --- | --- | --- | --- | --- |
| 1 | S | Fix the planning playbook's `.env` token and `demo` workspace; use `conveyor auth token` and the managed section's workspace. | `docs/playbooks/conveyor-planning.md`, `cmd/conveyor/skills_assets/conveyor-plan/conveyor-planning.md` | None expected. |
| 2 | S | Route all six skills from the managed section. | `cmd/conveyor/skills_assets/conveyor-repo-init/AGENTS.md`, `CLAUDE.md` | req-agent-guidance-install (template text) [unverified]. |
| 3 | S | Move the per-harness launch table and header-helper notes out of the canonical playbook into a non-normative `references/harness-notes.md`. | `docs/playbooks/conveyor-work.md:362-382`, `cmd/conveyor/skills.go` manifest | req-agent-skills AC-3.4/AC-3.5 cite launch behavior [unverified]. |
| 4 | S | Dedupe claim identity into one shared reference. | `docs/playbooks/conveyor-work.md:75-93`, `docs/playbooks/conveyor-kit-verify.md:45-57`, `cmd/conveyor/skills.go` | None expected (AC-3.2 text unchanged). |
| 5 | S | Retire the Codex `conveyor-operator` skill or reduce it to a pointer. | `plugins/conveyor/**`, `.agents/plugins/marketplace.json` | None expected. |
| 6 | M | Split `conveyor-work` and add `conveyor-coordinate` with the §4a edits; add the "delegated executor" role. | `docs/playbooks/conveyor-work.md`, new `docs/playbooks/conveyor-coordinate.md`, `.claude/skills/conveyor-coordinate/SKILL.md`, `cmd/conveyor/skills_assets/**`, `cmd/conveyor/skills.go`, `cmd/conveyor/skills_test.go` | req-agent-skills REQ-2, REQ-3 (AC-3.1–3.12); DEC-44; DEC-45 if standing delegation is adopted [unverified]. |
| 7 | M | Install to `.agents/skills` plus `.claude/skills`; drop other project roots; add OMP user root or rely on `~/.agents/skills`. | `cmd/conveyor/skills.go:152-159`, `cmd/conveyor/repo_init.go:380-383`, tests | req-agent-guidance-install AC-1.5, AC-1.6, REQ-2 [unverified]. |
| 8 | M | Advisory `roles` section in the local config plus `conveyor setup show --json`. | `internal/config/config.go`, `cmd/conveyor/named_execution_setup.go`, skills | DEC-44 revision; req-execution-configuration REQ-9; component-harness-execution [unverified]. |
| 9 | M | Retire server-pinned execution fields and worker-only harness probing (P1, P6); make enforcement labels declarative (P2, P3, P8–P10). | `internal/core/types.go`, `internal/store/**/work_orders*.go`, `internal/worker/service.go`, `internal/dispatch/dispatch.go`, `internal/httpapi/worker.go`, DB migration | req-worker AC-2.2, AC-2.3; DEC-56; component-work-orders [unverified]. |
| 10 | M | Open `report_continuation` to any claimant and add a harness field to claims (A11). | `internal/httpapi/mcp.go:145`, `:553`, `:919`; `internal/workorder/service.go:1286-1295` | component-mcp-protocol; req-260818-24dd3a AC-2.2 [unverified]. |
| 11 | M | `conveyor task status`, `conveyor inbox`, `task wait --until` (B1, G4, G5). | `cmd/conveyor/task_wait.go`, `cmd/conveyor/main.go`, new REST/MCP reads | req-agent-skills REQ-4 [unverified]; proposal B1 corpus notes. |
| 12 | L | CLI parity for the claim lifecycle with a local 0600 session file (G1, G2). | `cmd/conveyor/*`, `cmd/conveyor/kit_verify.go`, `cmd/conveyor/submit.go`, playbooks | component-cli-onboarding; component-mcp-protocol; req-delegated-execution [unverified]. |
| 13 | L | Self-claimed checkpoint recovery through `conveyor checkout` (P13, G8). | `cmd/conveyor/main.go:577-716`, `docs/playbooks/checkpoint-recovery.md` | component-attempt-checkpoints; component-git-delivery; DEC-10 [unverified]. |
| 14 | L | CLI or MCP document proposals (G3). | `cmd/conveyor/*`, `internal/httpapi/*`, `docs/playbooks/conveyor-planning.md` | component-document-corpus [unverified]. |
| 15 | L | Soft execution time and longer self-claim leases (A10, G9). | Per proposal A10 | DEC reversing the fixed-deadline rule; design-task-lifecycle; component-work-orders (per `proposal.md:120`). |

## 6. Open questions for the operator

1. Should a "coordinate this queue" request grant standing authority to approve plans, proposals, and merges (coordinate `:12-14`), which needs a DEC superseding DEC-45, or should the coordinator stay within DEC-45 and only offer and record on direct instruction?
2. May per-operator setup (host, harness/model/effort per role, concurrency) live in the local Conveyor config file, which needs a DEC-44 revision, or must it stay in each harness's memory?
3. Should `conveyor run` and `conveyor worker` stay in the product as optional adapters that skills never mention, or be retired so Conveyor ships no launcher at all?
4. Is CLI parity for the claim lifecycle (G1) a requirement, so harnesses without native MCP can comply, or is native MCP an acceptable hard prerequisite?
5. For install targets, is `.agents/skills` plus `.claude/skills` acceptable, given that Cursor and OMP would see both copies?
6. Should the Codex `conveyor-operator` plugin be removed, or kept as a pointer for marketplace discoverability?

## 7. Operator decisions (2026-10-10)

Answers given in the session; these settle questions 1–5 above and override the matching recommendations in §4 and §5.

1. **Judgment gates are soft gates that wait for a human; the channel does not matter.**
   Plan approval (and merge) is never automatic and never decided by an agent on its own judgment.
   It is recorded either in the web UI or by an agent acting on the operator's explicit statement in that agent's conversation ("I approve this plan, move forward"), and both count the same.
   What the gate protects is human judgment, not a particular interaction point; this matches DEC-45's direct-instruction rule, so no DEC supersession is needed.
   A coordinator decides only mechanical, non-judgment steps: scheduling, starting executor sessions, review admission, continuing a bounced implementer, retries.
   `conveyor-coordinate` lines 12–14 and its "routine merge within delegated scope" paragraph must be rewritten to that boundary; change list item 6 drops the DEC-45 supersession.
2. **No machine-wide setup file.**
   The operator switches harnesses, so a local config naming harness or model is wrong.
   Instead the CLI and MCP take harness, model, and user explicitly on every claim (proposal A11), and the skills tell each harness that it must identify itself and pass this data.
   Change list item 8 is replaced by: skills define the identity fields and when to send them; no `roles` config, no `conveyor setup show`.
3. **`conveyor run` and `conveyor worker` stay as optional adapters** that no skill mentions or requires.
4. **CLI parity.**
   The operator assumed MCP wraps the CLI.
   It does not: MCP is served by the API (`internal/httpapi/mcp.go`) and the CLI is a separate REST client, and the CLI lacks claim, renew, progress, contract, and verdict commands (§4d, G1).
   Operator ruling: CLI parity is preferred, but where it is missing, native MCP remains a required capability; change list item 12 stays L and optional.
5. **`conveyor-work` carries no harness launch steps.**
   The per-harness launch table, launcher-mode narrative, and MCP registration styles leave the normative playbook; at most they become a non-normative notes file (change list item 3, now decided).
6. **`conveyor-coordinate` is a developer's uncommitted skill.**
   The findings above apply when it is proposed for the repo; it should arrive already rewritten to decisions 1, 2, and 5.
7. **Skill installation is the operator's choice, not Conveyor's.**
   Today `repo init` writes the managed `AGENTS.md` section and copies every skill into `.claude/skills`, `.codex/skills`, `.cursor/skills`, and `.opencode/skills` (`cmd/conveyor/skills.go:154-159`), and it does nothing to keep those copies out of git.
   Operator direction: `repo init` writes the skills to one neutral folder (proposed `.ai/conveyor/skills/`) and prints how to install them into a harness; writing into tool folders becomes opt-in (for example `--into claude,cursor`).
   Whatever it writes must be git-ignored: check `.gitignore`, and if the path is not ignored add it to `.git/info/exclude` rather than editing the tracked `.gitignore`.
   Exception to resolve: this repository commits its own source wrappers under `.claude/skills/`, so ignoring must cover only the installed copies, not maintained sources.
   This replaces change list item 7 and question 5.
8. **Leases need a timer-based "still working" signal.**
   The claim lease defaults to 5 minutes (`internal/core/types.go:583`); a launched session is renewed every ten seconds by its launcher, but a self-claimed session is told to renew only "at each progress milestone" (`docs/playbooks/conveyor-work.md:175-185`), so a long test run or a stalled model turn lets the lease lapse.
   Most harnesses cannot run a timer inside the agent's turn, so the neutral fix belongs in the CLI: `conveyor order keepalive <order-id> --session <id> [--pid <agent pid>]`, a small background process that renews on a timer, stops when the order leaves `claimed` or the watched process exits, and is started by the skill right after claiming.
   With that in place a missed check-in really means the agent's process is gone or hung (proposal A10).
9. **Codex `conveyor-operator` plugin becomes a pointer** to the canonical skills, kept for marketplace discoverability (settles question 6).
