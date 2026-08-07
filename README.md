# Conveyor

**A software factory: durable documents describe the desired state of a
product, and the factory reconciles code toward them — through planned,
reviewed, evidence-gated tasks, with every step on the record.**

Most agent tooling treats each change as a conversation that evaporates
when the terminal closes. Conveyor is built on the opposite bet: the
compounding value of AI-driven development is **what survives between
tasks** — confirmed intent, documented mechanism, recorded decisions,
and a knowledge graph connecting every merged line of code back to the
reason it exists. Task N should be cheaper than task 1, and the only
way to get there is to own the planning, the paper trail, and the
proof — not just the code generation.

Conveyor develops Conveyor. Since Beta (July 15, 2026), nearly every
feature in this repository was planned as a document in the factory,
delivered as a factory task through its own gates, reviewed by its own
adversarial panel against its own citation contracts, and merged with
its lineage recorded — including the machinery that retired the
factory's original spec format, and the surfaces you'd use to watch it
happen.

## The model

**Documents are truth; tasks are reconciliation.** Four durable tiers,
each with its own maintenance loop:

| Tier | Nature | IDs | Loop |
|---|---|---|---|
| Product overviews | Informative | — | Markdown uploads, versioned with diffs; enforceable claims **promote** into requirements with a section-anchored link |
| Requirements | Normative intent | `REQ-n` / `AC-n.m` | Drafted by agents or operators, versioned, operator-confirmed; user stories + nested acceptance criteria |
| System Design | Normative mechanism | — | Factory-resident markdown declaring the code it governs; merges that touch governed code without a design revision raise a **drift signal** |
| Decisions | Settled arguments | `DEC-n` | Extracted from real deliberation with alternatives-rejected on record; append-only with supersession |

Stable IDs are citable in code comments. Implementing agents cite the
requirements and decisions their code serves; reviewers validate those
citations against authority **pinned at claim time** — so the contract
an agent was shown is the contract it is judged by.

## The loop

1. **Plan** — in an operator-side agent session (the [planning
   playbook](docs/playbooks/conveyor-planning.md)) or the in-product
   planning agent. Output: requirement and design *proposals* plus a
   dependency-ordered task set. Agents propose; **operators confirm** —
   always, structurally.
2. **Task** — the sole transition object. Tasks carry attached context
   (the requirements they serve, the designs that govern them) and are
   claimable in dependency order from one queue. No priority fields, no
   assignees — blocked is a derived predicate, workers claim.
3. **Plan gate** — when enabled, a stage-typed work order collects a
   versioned markdown execution plan (approach, files, risks,
   done-criteria) for operator approval or redirect before any
   implementation dispatches.
4. **Implement** — an operator-owned agent (Codex, Claude, or anything
   speaking MCP) claims the work order, resolves a dedicated worktree,
   and does every edit, test, and push there. Conveyor never runs your
   code or holds your model credentials.
5. **Review** — a fresh agent session (self-review is rejected at claim
   time) judges the diff against cited acceptance criteria and plan
   done-criteria, with structured verdicts the server validates.
   Evidence-gated: no test output, no submission.
6. **Merge & watch** — gated or automatic merge; the monitor watches
   the default branch for out-of-pipeline changes and post-merge
   failures, and files reconciliation tasks instead of letting history
   drift silently.

Every transition appends an event. Every relationship in the knowledge
graph — session → document → task → work order → PR → evidence →
verdict — is a projection of those events, rebuildable from history,
never a free-standing claim. Agent token usage is telemetry on every
work order.

## Human authority

Two gates (plan approval, merge approval), document confirmation,
drift resolution, and task cancel/hold answer **only to human
credentials — structurally**, not by configurable permission. Agents
file tasks but cannot cancel them; they propose documents but cannot
confirm them; a persuaded agent hits a credential wall, not a policy
suggestion. The factory has paused mid-task to ask for a human
decision and refused to fabricate evidence to satisfy its own gates;
that behavior is the design, and it has survived live contact.

## Run locally

Requirements: Go 1.24, Node/npm, PostgreSQL 15+, Docker with Compose,
and an authenticated `gh` CLI for GitHub-delivery repos.

```sh
cp conveyor.example.yaml conveyor.yaml
cp .env.example .env   # set CONVEYOR_API_KEY; regenerate the operator token: openssl rand -hex 32
make dev               # health-checked Postgres on :5432 + build + conveyord on :8080
```

Open `http://127.0.0.1:8080` — the Board and Tasks views are the
operating surfaces. `http://127.0.0.1:8080/settings` has the MCP
endpoint and a paste-ready client snippet.

Start a worker on your hardware (it supervises your agents; it is not
an execution sandbox):

```sh
bin/conveyor --workspace demo worker pair                      # prints a single-use pairing token
bin/conveyor --workspace demo worker run --pairing-token <t>   # exchanges it for a revocable credential
```

File work from anywhere:

```sh
bin/conveyor --workspace demo task new --repo api --message 'fix the typo in README'
```

or over MCP with `create_task` (idempotent; `body`, `repo`, and a
caller-stable `idempotency_key` required; attach served requirements
and governing designs at intake). Any MCP-speaking agent can claim
work orders, submit plans, and file review verdicts using
`CONVEYOR_API_TOKEN` — model credentials stay yours.

`CONVEYOR_API_KEY` powers only the server-owned triage and planning
stages. `.env` is auto-loaded by both binaries; process environment
wins.

## Developing Conveyor

The authoritative design is [conveyor-spec.md](conveyor-spec.md) —
body §§1–20 normative, §21 the amendment record. **When code and spec
disagree, the spec wins**; changes go by version-bumped amendment,
never silent edits. Code cites the spec (`(spec §N)`) and confirmed
documents (`REQ-n`, `AC-n.m`, `DEC-n`) as its traceability layer.

```sh
make build            # binaries
make test             # Go + web typecheck + Biome + Playwright
make test-integration # disposable Postgres on :5433; the database-backed lane
make vet fmt-check
```

Planning and task filing from agent sessions follow the
[planning](docs/playbooks/conveyor-planning.md) and
[task-filing](docs/playbooks/conveyor-task-filing.md) playbooks
(`.claude/skills/` wraps them for Claude Code; `AGENTS.md` is a symlink
to the same guidance for Codex and friends). Every push is a proposal;
operators confirm.

## Status

Beta July 15, 2026. Phases through 8 (the desired-state document
model) are delivered and attested in the factory's own records; the
current work — including the UI it's judged by — is planned and built
through the loop above. [docs/phase8-plan.md](docs/phase8-plan.md) and
the spec's §21 record carry the history, defects and all: the
reviews, the incidents, and the amendments are part of the product.
