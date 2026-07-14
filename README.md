# Conveyor

Conveyor is a durable software-development orchestrator. It owns the task
pipeline, specifications, review gates, audit history, and requirements
corpus; operator-owned coding agents perform implementation and code review
through MCP.

The authoritative design is [conveyor-spec.md](conveyor-spec.md) v1.9. The
working pre-Beta breakdown is [docs/beta-plan.md](docs/beta-plan.md).

## Status

Phase 4.7's implementation is complete. The code now provides:

- in-process triage and spec stages through the OpenAI Responses API, with
  existing schema validators, redacted transcripts, observational usage
  metering, and stage timeouts;
- an authenticated MCP server with idempotent `create_task` intake plus
  implementation/review work orders, including leases, self-review
  prevention, progress/usage/transcript reporting, synchronous or MCP
  review, and the in-session `await_review` loop;
- content-addressed artifacts and a hierarchical requirements tree linking
  features, tasks, approved specs, pull requests, and events;
- the embedded activity/review UI, workspace creation/selection, requirements
  UI, workspace-scoped config, and MCP connection guidance; and
- Postgres projections plus River-backed durable dispatch.

The Phase 4.7 live dogfood exit and the five-task Beta criterion remain to be
run; implementation completion is not recorded as Beta evidence.

## Run locally

Requirements: Go 1.24, Node/npm, Postgres for durable operation, and an
authenticated `gh` CLI when GitHub issue intake or PR creation is enabled.

```sh
cp conveyor.example.yaml conveyor.yaml
export CONVEYOR_DATABASE_URL='postgres://conveyor:conveyor@localhost:5432/conveyor?sslmode=disable'
export CONVEYOR_API_TOKEN="$(openssl rand -hex 32)"
export CONVEYOR_API_KEY='your-deployment-openai-key'

make build
bin/conveyord -config conveyor.yaml -poll-github 60s
```

`CONVEYOR_API_KEY` is used only by server-owned in-process stages. MCP clients
authenticate to Conveyor with `CONVEYOR_API_TOKEN` and bring their own model
credentials or subscriptions.

Create and inspect work:

```sh
export CONVEYOR_ADDR='http://127.0.0.1:8080'
bin/conveyor --workspace demo task new 'fix the typo in README' --repo api --level L2
bin/conveyor --workspace demo task list
bin/conveyor --workspace demo config export > workspace.yaml
```

MCP clients can submit newly discovered work through `create_task`. Pass
`workspace_id` explicitly whenever more than one workspace exists. Required
arguments are `title`, `repo`, and a caller-stable `idempotency_key`; optional
arguments are `body`, `source`, `base_branch`, and `level` (default `L2`). The
tool durably creates and enqueues the normal task, returns immediately, and
reuses the original task when the same key and input are retried. Luna triage
then advances through the same audited pipeline used by UI, CLI, API, and
GitHub intake.

Open `http://127.0.0.1:8080/settings` for the MCP endpoint and client snippet.
Each review must use a fresh agent session and client token; Conveyor rejects
an implementer's attempt to claim the corresponding review work order.

### Branch ownership

Task intake records the canonical `conveyor/task-<id>` branch name and selected
base, but it does not create a local or remote Git ref. After claiming and
reading an implementation work order, the operator-owned agent runs
`conveyor checkout <task-id>` to create or reuse a clean, task-dedicated sibling
worktree. The helper safely creates or adopts the exact assigned branch,
preserves existing task commits across redispatches and review bounces, and
returns the resolved path for all edits, tests, commits, and pushes. The
primary checkout is never switched, stashed, reset, or edited. The agent pushes
before `submit_for_review`; Conveyor then opens or reuses the PR and never
resets or pushes the agent's branch.

The dashboard exposes the same checkout command before or after the first push.
If the task branch does not exist locally or remotely, the helper creates it
from a freshly fetched `origin/<base>`; existing local, remote, or registered
worktree history is preserved, and dirty, in-progress, or divergent states fail
closed. After the task merges or closes, `conveyor done <task-id>` removes only
a clean task worktree and retains the task branch.

### Codex plugin

The repository-owned plugin lives in `plugins/conveyor`, with repo-local
marketplace metadata in `.agents/plugins/marketplace.json`. It connects to the
local MCP endpoint using `CONVEYOR_API_TOKEN` from the Codex process environment
and never stores the credential. Installation, update, resume, and validation
instructions are in [plugins/conveyor/README.md](plugins/conveyor/README.md).

## Configuration ownership

`conveyor.yaml` bootstraps its named initial workspace on first Postgres start.
Authenticated operators can create and select additional workspaces in the
Workspace UI or through `POST /v1/workspaces`; creation atomically stores its
configuration and `workspace.created` audit event. Each workspace's routes,
bounce cap, repositories, config version, task intake keys, River queues, and
pipeline records remain independent. The
deployment file retains the database connection, prompt-pack path, and bare
repository cache path.

All workspace-scoped REST calls accept `workspace_id` or `X-Workspace-ID`.
The CLI accepts `--workspace` (or `CONVEYOR_WORKSPACE`) and MCP tools accept
`workspace_id`. Omission remains compatible only when exactly one workspace
exists; zero-workspace and multi-workspace ambiguity fail closed.

The Phase 4.7 document deliberately has no runner, image, credential pool,
secret reference, vendor policy, or tool policy fields. Operator agents own
their execution environment; repository CI is the mechanical verifier until
managed execution is explicitly activated in Phase 8.

Stage routes contain only model, timeout, and execution mode. Conveyor retains
token and USD usage as audit telemetry, but it has no allocation, remaining
balance, or usage-based execution gate (spec §21.6).

When upgrading from v1.5, remove `budget_usd` from the deployment bootstrap
file before restart. Startup canonicalizes an existing Postgres workspace
document, and migration 011 removes the obsolete job allocation column while
preserving cost/token telemetry and append-only audit events.

## Layout

```text
cmd/conveyor/             Cobra CLI: tasks, gates, config, checkout
cmd/conveyord/            control plane, River worker, API, MCP, embedded UI
internal/dispatch/        durable multi-stage orchestration
internal/inprocess/       direct server-owned Responses API stages
internal/workorder/       leased MCP lifecycle and protocol enforcement
internal/store/           memory test store and pgx/sqlc Postgres store
internal/httpapi/         REST, SSE, MCP, requirements/artifact APIs
internal/gitx/            bare repository cache and checkout support
internal/trigger/github/  issue intake, branch diff, and PR integration
internal/redact/          transcript redaction
pack/roles/               reviewable stage prompts
web/                      embedded React operator interface
```

## Development

```sh
make build
make test
make vet
make plugin-check
```
