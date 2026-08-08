# Conveyor

Conveyor is a software factory. You describe what a product should be
in versioned documents (requirements, system design, decisions), and
the factory turns the gap between those documents and the code into
tasks. Agents you run on your own machines claim the tasks over MCP,
then plan, implement, and review them. Humans confirm the documents,
approve the plans, and gate the merges. Every step is recorded as an
event, and the events build a knowledge graph that links each merged
change back through its task, plan, review, and evidence to the
requirement it serves.

Conveyor has been building itself since July 2026. Nearly every feature
in this repository entered as a document, went through the plan and
review gates, and merged with its lineage recorded, including the
machinery that replaced the factory's original spec format.

## The software factory

You can run a factory of agents dark, shipping code no human reads.
That feels fast for a few months, until nobody understands the system
anymore. Conveyor runs lit, with human judgment pushed as early in the
process as it will go:

- Work enters as requirements and design documents.
  Agents can draft and propose them; only an operator can confirm them.
- An optional plan gate has the agent submit a short written plan
  (approach, files, risks, done-criteria) for approval before it writes
  any code.
- Review runs in a separate agent session. The server rejects
  self-review at claim time, and a submission without test evidence is
  rejected outright.
- System Design documents declare which code they govern. A merge that
  touches governed code without a design revision raises a drift
  signal, and a monitor watches the default branch and files
  reconciliation tasks when changes land outside the pipeline.
- Every state transition appends to an event log. The knowledge graph
  is a projection of that log

## The document tiers

| Tier | Nature | IDs | Maintenance |
|---|---|---|---|
| Product overviews | Informative | — | Markdown uploads, versioned with diffs; enforceable claims promote into requirements with a section-anchored link |
| Requirements | Normative intent | `REQ-n` / `AC-n.m` | Drafted by agents or operators, versioned, operator-confirmed; user stories with nested acceptance criteria |
| System Design | Normative mechanism | — | Factory-resident markdown that declares the code it governs; ungoverned merges raise a drift signal |
| Decisions | Settled arguments | `DEC-n` | Extracted from real deliberation with the rejected alternatives on record; append-only with supersession |

The IDs are stable and citable in code comments. An implementing agent
cites the requirements and decisions its code serves, and the reviewer
checks those citations against the document versions pinned when the
work order was claimed, so the contract the agent saw is the contract
it is judged by.

## How a task moves

1. **Plan.** In the in-product planning agent or an operator-side
   session (see the [planning playbook](docs/playbooks/conveyor-planning.md)).
   Output: proposed requirement and design revisions plus a
   dependency-ordered set of tasks.
2. **Queue.** Tasks carry their context with them: the requirements
   they serve and the designs that govern them. There are no priority
   fields and no assignees. Blocked is derived from dependencies, and
   workers claim whatever is claimable.
3. **Plan gate** (optional). A stage-typed work order collects a
   versioned execution plan for approval or redirect before
   implementation dispatches.
4. **Implement.** An agent you own (Codex, Claude, anything that
   speaks MCP) claims the work order, checks out a dedicated worktree,
   and does every edit, test, commit, and push there. Conveyor never
   executes your code and never holds your model credentials.
5. **Review.** A fresh agent session judges the diff against the cited
   acceptance criteria and the plan's done-criteria, and files a
   structured verdict that the server validates.
6. **Merge and watch.** Gated or automatic merge, then the monitor
   keeps watching the branch for out-of-pipeline changes and
   post-merge failures.

## Architecture

```
   Operators (browser)                  Agents (Codex, Claude, ...)
          |                                       |
   React dashboard                      MCP work-order server
   Board / Tasks / Docs                 claim, plan, review, verdict
          |                                       |
          +----------------+----------------------+
                           |
                    conveyord (Go, one binary)
              REST API, SSE planning chat, event log
                           |
                      PostgreSQL
             events, documents, links, one queue
                           |
                 conveyor worker (your machine)
             supervises your agents, your credentials
                           |
              git worktrees -> your repos -> PRs
```

Conveyor is a coordination plane, not an execution sandbox. The worker
is a thin supervisor over agents you already run. Edits and tests
happen in worktrees on your hardware, and delivery lands as ordinary
pull requests.

## Running it

You need Go 1.24, Node/npm, PostgreSQL 15+, Docker with Compose, and
an authenticated `gh` CLI for GitHub-delivery repos.

```sh
cp conveyor.example.yaml conveyor.yaml
cp .env.example .env   # set CONVEYOR_API_KEY; regenerate the operator token: openssl rand -hex 32
make dev               # health-checked Postgres on :5432 + build + conveyord on :8080
```

Open `http://127.0.0.1:8080`. The Board and Tasks views are the
operating surfaces, and `http://127.0.0.1:8080/settings` has the MCP
endpoint with a paste-ready client snippet.

Start a worker on the machine where your agents run:

```sh
bin/conveyor --workspace demo worker pair                      # prints a single-use pairing token
bin/conveyor --workspace demo worker run --pairing-token <t>   # exchanges it for a revocable credential
```

File work from the CLI:

```sh
bin/conveyor --workspace demo task new --repo api --message 'fix the typo in README'
```

or over MCP with `create_task` (idempotent; `body`, `repo`, and a
caller-stable `idempotency_key` are required, and you can attach served
requirements and governing designs at intake). Any MCP-speaking agent
can claim work orders, submit plans, and file review verdicts with
`CONVEYOR_API_TOKEN`. `CONVEYOR_API_KEY` powers only the server-owned
triage and planning stages. Both binaries auto-load `.env`; process
environment wins.

## Status

Conveyor is in active development. The factory's own records carry the
full history, defects included.
