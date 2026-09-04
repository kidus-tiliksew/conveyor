# Conveyor

Generating code is easier than ever. Checking that the code
matches product intent is now the bottleneck, and it is a bottleneck that
gets worse as agents get faster: a queue of unsupervised agents can ship
more unread code per day than a team can read.

A software factory is the answer to this shape of problem. You do
not inspect every screw; you fix the process so that inspection happens at
the points where mistakes can happen, and you make every unit traceable
so that when something is wrong you know what else is affected.

Conveyor is a software factory for agent-written code. 

It queues work from Requirements, System Design documents, and Decisions. Human operators confirm the documents, and approve plans when required. Agents on your machines plan, implement, and review that work.

Conveyor has used this process to build itself since July 2026. 

Contributions go through the factory too. To start contributing, request access first by opening an issue, and you will get a workspace account to plan and pick up work from.

<table>
  <tr>
    <td colspan="2">
      <a href="docs/assets/board.png">
        <img src="docs/assets/board.png" width="100%" alt="The Conveyor board with tasks in planning, implementation, review, and verification">
      </a>
      <br><sub>Task board</sub>
    </td>
  </tr>
  <tr>
    <td width="50%">
      <a href="docs/assets/screenshots/system-design.png">
        <img width="100%" src="docs/assets/screenshots/system-design.png" alt="A confirmed System Design document with its architecture diagram">
      </a>
      <br><sub>Confirmed System Design</sub>
    </td>
    <td width="50%">
      <a href="docs/assets/screenshots/requirement-drift.png">
        <img width="100%" src="docs/assets/screenshots/requirement-drift.png" alt="A requirement with a delivery drift signal awaiting operator judgment">
      </a>
      <br><sub>Requirement drift</sub>
    </td>
  </tr>
  <tr>
    <td width="50%">
      <a href="docs/assets/screenshots/task-lineage.png">
        <img width="100%" src="docs/assets/screenshots/task-lineage.png" alt="A merged task with its plan, pull request, and lineage">
      </a>
      <br><sub>Task plan and lineage</sub>
    </td>
    <td width="50%">
      <a href="docs/assets/screenshots/review-loop.png">
        <img width="100%" src="docs/assets/screenshots/review-loop.png" alt="A task timeline with changes requested and the following implementation round">
      </a>
      <br><sub>Review feedback returning to implementation</sub>
    </td>
  </tr>
  <tr>
    <td colspan="2">
      <a href="docs/assets/screenshots/mcp-setup.png">
        <img width="100%" src="docs/assets/screenshots/mcp-setup.png" alt="The MCP setup dialog for coding clients">
      </a>
      <br><sub>MCP client setup</sub>
    </td>
  </tr>
</table>

## The knowledge graph

Conveyor links each change to the documents, task, review, and test evidence
behind it.

```mermaid
flowchart LR
    intent["Confirmed requirements<br/>and designs"] --> task["Task"]
    task --> delivery["Delivered change"]

    intent -.-> check{"Misalignment checks"}
    delivery -.-> check
    repository["Observed repository"] -.-> check

    check -->|mismatch found| signal["Signal"]
    signal --> followup["Judgment or gated follow-up"]
    followup -->|re-enters the factory| task
```

Conveyor checks each delivery against confirmed requirements and governing
designs. When a delivery and its confirmed intent disagree, Conveyor raises a
signal. Repository drift and post-merge failures raise signals too. Conveyor
never rewrites code or documents on its own. An operator can acknowledge the
signal or send follow-up work through the normal gates.

## Architecture

```text
   Operators in a browser              Agents such as Codex or Claude
             |                                      |
      React dashboard                       MCP work-order server
      board, tasks, docs                    claim, plan, review
             |                                      |
             +------------------+-------------------+
                                |
                         conveyord in Go
                  REST API, dashboard, event log
                                |
                           PostgreSQL
                  events, documents, links, queue
                                |
                    conveyor worker on your machine
                       supervises your agent CLIs
                                |
                    Git worktrees, repositories, PRs
```

`conveyord` is one Go binary. PostgreSQL stores the event log, documents,
lineage projection, and the log-backed queue. The worker launches agent CLIs with your
local credentials.

## Installation

A factory host needs PostgreSQL 15 or newer, Git, an authenticated `gh` CLI,
an API key for an OpenAI-compatible model endpoint, and the agent CLIs you
plan to run.

Install the latest `conveyor` and `conveyord` binaries into `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/main/install.sh | sh
```

The installer verifies the release checksum before replacing either binary
and does not need `sudo`. Pinning a reviewed version, building from source,
and upgrades are covered in [Installation](docs/installation.md).

From there, [Getting started (solo)](docs/getting-started-solo.md) stands up
a factory end to end on one machine, and
[Getting started (multiplayer)](docs/getting-started-multiplayer.md) covers
a shared team server.

## Documentation

Full docs live in [docs/](docs/README.md). There's no docs site yet.

**Getting started**

- [Installation](docs/installation.md): release installer, source builds, prerequisites
- [Getting started (solo)](docs/getting-started-solo.md): one person, one machine, end to end
- [Getting started (multiplayer)](docs/getting-started-multiplayer.md): a shared team server

**Guides**

- [CLI reference](docs/cli.md): every `conveyor` and `conveyord` command
- [Authentication](docs/auth.md): sign-in, tokens, roles, GitHub identity
- [Configuration](docs/configuration.md): the three config surfaces and every environment variable
- [MCP reference](docs/mcp.md): the tools an agent uses to work a task

**The factory**

- [Concepts](docs/concepts.md): the software factory, the knowledge graph, light and dark factory patterns
- [The document corpus](docs/document-corpus.md): requirements, System Designs, decisions, and the propose-confirm cycle
- [Tasks](docs/tasks.md): how work is created, given context, executed, reviewed, and linked
- [Misalignment](docs/misalignment.md): drift, staleness, and pending proposals

**Operations**

- [Worker operations](docs/worker-operations.md): durable worker enrollment, service install, recovery
- [GitHub lifecycle](docs/github-lifecycle.md): how issues, PRs, and review statuses are projected onto GitHub
- [Known limitations](docs/known-limitations.md): accepted boundaries of the current implementation

**Playbooks** (installable as agent skills with `conveyor skills install`)

- [Planning](docs/playbooks/conveyor-planning.md): draft and push documents from a local agent session
- [Task filing](docs/playbooks/conveyor-task-filing.md): file well-formed tasks and dependency chains
- [Working a task](docs/playbooks/conveyor-work.md): the claim, checkout, submit, review lifecycle

## Status

Conveyor is under active development. Its event log records defects and
reconciliation work alongside successful merges.

## License

Conveyor is available under the [MIT License](LICENSE).
