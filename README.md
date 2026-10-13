# Conveyor

Coding agents can produce changes faster than a team can review them. The
harder question is whether those changes match what the team intended to build.

Conveyor gives that work a defined process. Humans confirm requirements,
system designs, and decisions. Agents use those documents to plan, implement,
and review tasks on your machines, with human approval where required.

Requirements traceability gives reviewers a basis for checking the work and
identifying where it has drifted from the agreed intent.

Conveyor has used this process to build itself since July 2026.

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

## The pipeline

```mermaid
flowchart LR
    triage["Triage"] --> plan["Plan<br/>(plan gate)"]
    plan --> implement["Implement"]
    implement --> verify["Verify<br/>(opt-in)"]
    verify --> review["Review"]
    review -->|merge gate| merge["Merge"]
    review -->|changes requested| implement
```

Every task moves through explicit stages, and each stage has its own contract.
A task freezes its policy when it is created, and that policy can turn the
plan stage, the verify stage, and the merge gate off.

- **Triage** classifies the task and writes an advisory brief for the
  stages that follow.
- **Plan** runs when the plan gate is on. An agent writes the execution plan,
  and an operator approves it before implementation starts.
- **Implement** has an agent change the task branch in a dedicated worktree
  and submit it. Conveyor opens the pull request.
- **Verify** runs the repository's [verification kits](docs/playbooks/conveyor-kit.md)
  and ordinary checks against the submitted head and seals typed evidence. It
  runs only when the task's frozen `verify_stage` policy turns it on.
- **Review** sends the pull request to one or more independent reviewer
  seats. Each seat judges it against the pinned requirements, designs, and
  decisions. Self-review is refused. A `changes_requested` verdict sends the
  work back to a fresh implementation order.
- **Merge** happens after a readiness check. With the merge gate on, the
  operator performs the merge from the dashboard inbox, `conveyor run`, or
  `conveyor task merge`.

Each task's next action is computed on the server. The dashboard, CLI, and
MCP all show the same next action, so it is always clear whether the task is
waiting on an agent, a worker, or you.

## Architecture

```mermaid
flowchart TB
    subgraph yours["Your machines"]
        operator["Operator in a browser"]
        session["Agent session<br/>Claude Code, Codex, Cursor,<br/>OpenCode, Grok Build"]
        runner["conveyor run / conveyor worker<br/>claims work orders, creates worktrees,<br/>launches your agent CLIs"]
    end

    subgraph daemon["conveyord, one Go binary"]
        api["REST API and SSE<br/>embedded React dashboard"]
        stages["In-process stages<br/>triage, planning chat"]
        dispatch["Dispatcher and GitHub monitor"]
        mcp["MCP server<br/>planning, work-order,<br/>and verification tools"]
    end

    db[("PostgreSQL or SingleStore<br/>event log, durable queue,<br/>documents, lineage")]
    llm["OpenAI Responses-compatible<br/>model endpoint"]
    github["GitHub<br/>repositories, PRs, issues, CI"]

    operator --> api
    session --> mcp
    runner --> mcp
    daemon --> db
    stages --> llm
    dispatch <--> github
    runner --> github
```

Three things run, in three places:

- **`conveyord`** is the control plane. It serves the REST API, the
  dashboard, and the MCP endpoint that agents use. It runs the small
  always-on stages itself: triage and the planning conversation, through the
  model endpoint you configure. Its dispatcher advances each task through the
  pipeline and leaves plan, implement, verify, and review as leased work
  orders for agents to claim. The GitHub monitor turns CI results and merges
  into ordinary task intake.
- **The database** is the only state. PostgreSQL or SingleStore holds an
  append-only, per-workspace event log; the durable queue runs on that log
  rather than on a separate broker. Documents, links, and the lineage
  projection are folded from the same events.
- **Your machines** do the work. `conveyor run` and `conveyor worker` claim
  work orders over HTTP, check out a Git worktree, and launch your agent
  CLIs with your credentials. Those CLIs report plans, progress,
  verification evidence, and review verdicts back over MCP. Delivery is an
  ordinary pull request. There is no hosted sandbox, and the server never
  holds your agent CLI credentials; the only model key it keeps is its own,
  for the in-process stages.

## Deployment database

Supply the connection as `database.url` in the server config, or as
`CONVEYOR_DATABASE_URL` in the process environment. Conveyor picks the
backend from the URL. Set `database.backend: postgres` or
`database.backend: singlestore` only if you want to state it explicitly.

- **PostgreSQL** uses
  `postgres://user:password@host:5432/conveyor?sslmode=require`.
- **SingleStore** uses `singlestore://user:password@host:3306/conveyor?tls=true`
  or `mysql://` with the same URL shape. It also takes a MySQL DSN such as
  `user:password@tcp(host:3306)/conveyor?tls=true`.

URL passwords must be percent-encoded. SingleStore needs an empty database
created before `conveyor init`; [SingleStore operations](docs/singlestore.md)
covers the rest.

## Installation

The fastest route is [Getting started (solo)](docs/getting-started-solo.md),
a single command list that stands up a factory on one machine. A server host
needs a database, a model API key, and a GitHub App for each workspace. The
App's private key is encrypted with `CONVEYOR_GITHUB_APP_KEY_ENCRYPTION_KEY`.
Each executor machine needs Git credentials and an authenticated agent CLI.
[Server setup](docs/server-setup.md) and [Client setup](docs/client-setup.md)
cover each role in full.

Install the latest `conveyor` and `conveyord` binaries into `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/main/install.sh | sh
```

The installer verifies the release checksum before replacing either binary
and does not need `sudo`. Pinning a reviewed version, the container image,
building from source, and upgrades are covered in
[Server setup](docs/server-setup.md).
[Getting started (multiplayer)](docs/getting-started-multiplayer.md) covers
a shared team server.

To prepare a repository for factory work, run `conveyor repo init` from its
checkout. It adds a Conveyor section to `AGENTS.md` and `CLAUDE.md` and
installs project-scoped skills for Claude Code, Codex, Cursor, and OpenCode.
Deliver the generated files through a task. `conveyor mcp install` registers
the Conveyor MCP endpoint with your local agent CLIs.

## Documentation

Full docs live in [docs/](docs/README.md). There's no docs site yet.

**Getting started**

- [Server setup](docs/server-setup.md): install the binaries, start a database, run the daemon, invite clients
- [Client setup](docs/client-setup.md): sign in, Git and GitHub access, execution setup, first task
- [Getting started (solo)](docs/getting-started-solo.md): the quick start, one person, one machine, end to end
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
- [SingleStore operations](docs/singlestore.md): running the factory on SingleStore instead of PostgreSQL
- [The log core](docs/log-core.md): the event log and the durable queue that runs on it
- [Known limitations](docs/known-limitations.md): accepted boundaries of the current implementation

**Agent harnesses**

- [Cursor CLI](docs/cursor-build.md), [Grok Build](docs/grok-build.md), and [OpenCode](docs/opencode-build.md): worker setup for harnesses that receive MCP through the environment

**Playbooks** (installable as agent skills with `conveyor repo init` or `conveyor skills install`)

- [Planning](docs/playbooks/conveyor-planning.md): draft and push documents from a local agent session
- [Task filing](docs/playbooks/conveyor-task-filing.md): file well-formed tasks and dependency chains
- [Working a task](docs/playbooks/conveyor-work.md): the claim, checkout, submit, review lifecycle
- [Testing strategy](docs/playbooks/conveyor-testing-doc.md): write the testing-strategy System Design document that reviewers judge verification against
- [Authoring verification kits](docs/playbooks/conveyor-kit.md): declare repository kits in `.conveyor/kits/manifest.yaml`
- [Executing verification kits](docs/playbooks/conveyor-kit-verify.md): run kits and ordinary checks under a verify claim and submit typed evidence

[Checkpoint recovery](docs/playbooks/checkpoint-recovery.md) is an operator
procedure for recovering preserved implementation work. It is not installed
as a skill.

## Contributing

Contributions go through the factory too. To start contributing, request
access by opening an issue. You will get a workspace account to plan and
pick up work. Agents edit only under a live claim, in the task worktree that
`conveyor checkout <task-id>` resolves.

The backend is Go and the dashboard is React. `make build`, `make vet`,
`make fmt-check`, and `make test` are the local gate. `make test` covers Go
tests, the web typecheck, Biome, and Playwright. `make test-integration` runs
against PostgreSQL. [Local validation evidence](docs/validation-evidence.md)
describes scratch space and retained evidence for work on this repository.

## Acknowledgements

Supported by the awesome [FunnelFlux](https://funnelflux.com) team ❤️

## License

Conveyor is available under the [MIT License](LICENSE).
