# Conveyor

Conveyor is a software factory for agent-written code. It queues work from
confirmed requirements, System Design documents, and decisions. Agents on your
machines plan, implement, and review that work. Operators confirm the
documents, approve plans when required, and control the merge policy.

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

Conveyor has used this process to build itself since July 2026.

## Why a software factory

Generating more code is easy. Checking that the code matches product intent is
the hard part.

A queue of unsupervised agents can ship code that nobody has read. Conveyor
puts human judgment at the points where mistakes change the product or its
architecture:

- Agents and operators draft documents. Operators confirm them.
- A separate agent reviews each change. The server rejects self-review and
  missing test evidence.
- Governed code changes without a design revision raise a drift signal.
- The monitor files reconciliation tasks for work outside the pipeline and
  failures found after merge.

Conveyor coordinates the work. It does not run your code in a hosted sandbox
or hold your model credentials. Agents edit and test in Git worktrees on your
hardware. Delivery uses ordinary pull requests.

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

The event log is the source of the graph. PostgreSQL stores a projection for
queries. `conveyor lineage rebuild` rebuilds it from the workspace's events.

## How work moves

Planning produces confirmed documents and dependency-ordered tasks. An agent
claims a task in a dedicated worktree, submits a plan when required, then
implements and tests the change. A different agent reviews it against the
pinned documents and test evidence. Conveyor applies the merge gate and
monitors the default branch afterward.

## Documents are the authority

Conveyor keeps four document types:

- **Product overviews.** These provide background. Conveyor versions the Markdown
  uploads with diffs, but they do not bind implementation. Only a requirement
  can bind implementation.
- **Requirements.** These state what the product must do. Each requirement has
  a stable `REQ-n` ID and each acceptance criterion has a stable `AC-n.m` ID.
- **System Design documents.** These state how the system works and which
  repository paths they govern.
- **Decisions.** These record a settled choice, its context, and the rejected
  options. Conveyor never edits a confirmed `DEC-n`. A later decision may
  supersede it.

Agents and operators can draft and propose new versions. Conveyor never edits a
written version. Only an operator can confirm a proposal.

An implementation cites the requirements and decisions it serves. Review checks
the document versions captured when the agent claimed the work order. A
revision made mid-task cannot change what the reviewer checks.

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
                  REST API, planning chat, event log
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
lineage projection, and River queue. The worker launches agent CLIs with your
local credentials.

## Before you install

A deployed factory needs:

- PostgreSQL 15 or newer
- Git
- an authenticated `gh` CLI, or `GH_TOKEN` for headless use
- an API key for the configured OpenAI-compatible model endpoint
- the agent CLIs you plan to run

The release installer needs `curl`, `tar`, and a SHA-256 tool. It does not need
`sudo`.

Source development also needs Go 1.24, Node with npm, and Docker with Compose.

## Getting started

The steps below stand up a factory end to end on one machine. A team server
works the same way; repeat step 5 on each contributor machine.

### 1. Install the release

Install the latest `conveyor` and `conveyord` binaries in `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/main/install.sh | sh
```

To install a reviewed version, pin the installer and the release:

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/v1.2.3/install.sh | sh -s -- v1.2.3
```

Set `CONVEYOR_INSTALL_DIR` to choose another destination. The installer supports
Linux and macOS on amd64 and arm64. It downloads the selected GitHub release and
checks the published SHA-256 digest before replacing either binary.

### 2. Stand up PostgreSQL and the environment

Start PostgreSQL 15 or newer, generate the operator API token, and export the
three required variables:

```sh
openssl rand -hex 32
```

```sh
export CONVEYOR_DATABASE_URL='postgres://conveyor:conveyor@localhost:5432/conveyor?sslmode=disable'
export CONVEYOR_API_TOKEN='<generated token>'
export CONVEYOR_LLM_API_KEY='<provider API key>'
```

Authenticate the host with `gh auth login`. On a team server, use a dedicated
forge machine account: GitHub records every factory action under the host's
account, and a personal account makes those actions look like one person's
work.

### 3. Start the server

`conveyord` reads a deployment config. Start from the annotated example:

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/main/conveyor.example.yaml -o conveyor.yaml
```

Adjust the harness and review-seat entries for the agent CLIs on the machine.
Canonical role prompts are embedded in the release; set `pack_dir` only to use
a strict on-disk custom-pack override. The `workspace:` and `repos:` entries
are optional — leave them out to create the workspace in the dashboard in the
next step.

```sh
conveyord -config ./conveyor.yaml
```

To install a user service instead, run `conveyord install --config
./conveyor.yaml` and inspect it with `conveyord status`.

Startup applies pending embedded database migrations, then creates the
organization and first operator and binds `CONVEYOR_API_TOKEN` as that
operator's token. A binary refuses to start if a newer release created the
store.

### 4. Set up the workspace in the dashboard

Open `http://127.0.0.1:8080` and paste the `CONVEYOR_API_TOKEN` value on the
Settings page — the token stays in session storage and is forgotten when the
tab closes. Create the workspace from the board's first-run prompt, then add
the repository (name, URL, GitHub slug, base branch) on the Workspace page.
Settings also mints personal access tokens for contributors and shows the MCP
client configuration.

### 5. Connect the CLI, skills, and MCP clients

Authenticate the CLI with a personal access token from Settings, then install
Conveyor's agent skills and native MCP registrations:

```sh
conveyor --server https://factory.example auth login
conveyor skills install
conveyor --server https://factory.example mcp install
```

Both install commands configure detected Codex and Claude Code clients; pass
`--tool` to select one, or `--list` to report state without writing. An
install refreshes files Conveyor owns and refuses unrelated collisions. MCP
registration writes an environment-backed token reference, never a token
value. If the token bridge is missing, the command prints this setup line to
add to the shell environment yourself:

```sh
export CONVEYOR_API_TOKEN=$(conveyor auth token)
```

Repeat this step on each contributor machine.

### 6. Create an execution setup

A machine that runs tasks needs a local execution configuration describing its
agent harnesses. The factory host's `conveyor.yaml` from step 3 already
qualifies; on other machines, generate one:

```sh
conveyor config init-execution
```

The wizard detects installed agent CLIs, probes them, and writes the harness,
stage, and review-seat configuration.

### 7. Build the document corpus

Confirmed documents are what the factory implements and reviews against, so
write them before filing work. Open an agent session in your project — the
installed skills wrap the [planning playbook](docs/playbooks/conveyor-planning.md)
— and draft requirements, System Design documents, and decisions. Every push
is a proposal; confirm each one in the dashboard. The in-product planning chat
is the same flow without the local session.

### 8. Create and run tasks

File dependency-ordered tasks from the same agent session using the
[task-filing playbook](docs/playbooks/conveyor-task-filing.md), or from the
CLI (see [File work](#file-work)). Then run one by ID:

```sh
conveyor run <task-id>
```

Client-side execution commands resolve one local file in this order: an
explicit `--config` path, `CONVEYOR_CONFIG`, an existing `./conveyor.yaml`,
then the per-user default at the platform user-config directory under
`conveyor/conveyor.yaml`. The working-directory file remains compatible with
factory hosts where deployment and execution settings intentionally share one
file. Use `conveyor config list` to see the selected path and source.

Conveyor shows each claimable stage before it claims the work. Pass `--auto` to
run claimable stages without those prompts. Operator gates still apply.

Use a durable worker when a machine should poll the queue and run available
work without an attached operator. The
[worker operations guide](docs/worker-operations.md) covers enrollment, service
installation, and recovery.

## File work

File a task from the CLI:

```sh
conveyor --workspace demo task new \
  --repo api \
  --message 'fix the typo in README'
```

MCP clients can use `create_task`. The call requires `body`, `repo`, and a
caller-stable `idempotency_key`. It may also attach served requirements and
governing designs.

Agents authenticate with `CONVEYOR_API_TOKEN` to claim work orders, submit
plans, and file review verdicts. The server-owned LLM provider credential is
`CONVEYOR_LLM_API_KEY`; its optional endpoint is `CONVEYOR_LLM_BASE_URL`. Both
binaries load `.env`, and process environment values take precedence over
stored CLI defaults. The old `CONVEYOR_API_KEY` and `CONVEYOR_API_BASE_URL`
names remain deprecated fallbacks for existing installations.

## Develop from source

Clone the repository and run:

```sh
cp conveyor.example.yaml conveyor.yaml
cp .env.example .env
make dev
```

Set `CONVEYOR_LLM_API_KEY` in `.env` and generate the initial operator token with
`openssl rand -hex 32`. `make dev` starts a health-checked PostgreSQL instance
on port 5432, builds the project, and starts `conveyord` on port 8080.

Open `http://127.0.0.1:8080`. The Settings page at
`http://127.0.0.1:8080/settings` provides the MCP endpoint and client
configuration.

Connect the local CLI with a personal access token from Settings:

```sh
bin/conveyor --server http://127.0.0.1:8080 auth login
bin/conveyor config set workspace demo
bin/conveyor auth status
```

The CLI reads the token through hidden terminal input and verifies it before
saving. It never passes the token in process arguments.

Run `make build` when you only need the binaries. The build writes them to
`bin/`.

## Credentials and upgrades

Conveyor stores CLI credentials and workspace defaults per server. The local
JSON file is plaintext, like `gh` and kubeconfig. Conveyor creates its parent
directory with mode 0700 and the file with mode 0600.

Use `conveyor auth logout --revoke` to remove the local entry and request remote
revocation. `conveyor config list` reports the effective server and workspace,
including whether each value came from a flag, the environment, the stored
file, or the singleton fallback. It also reports the resolved client execution
configuration path and whether a flag, `CONVEYOR_CONFIG`, the working-directory
file, or the user default selected it.

For CI, workers, and MCP clients, set environment variables. They override
stored values:

```sh
export CONVEYOR_ADDR=https://factory.example.com
export CONVEYOR_API_TOKEN="$(conveyor --server "$CONVEYOR_ADDR" auth token)"
export CONVEYOR_WORKSPACE=demo
```

To upgrade, rerun the installer with the new version and restart `conveyord`.
Startup applies pending migrations.

## Status

Conveyor is under active development. Its event log records defects and
reconciliation work alongside successful merges.

## License

Conveyor is available under the [MIT License](LICENSE).
