# Conveyor

Conveyor is a durable software-development orchestrator. It owns the task
pipeline, specifications, review gates, audit history, and requirements
corpus; operator-owned coding agents perform implementation and code review
through MCP.

The authoritative design is [conveyor-spec.md](conveyor-spec.md) v1.6. The
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
- the embedded activity/review UI, requirements UI, slim workspace config,
  and MCP connection guidance; and
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
export CONVEYOR_URL='http://127.0.0.1:8080'
bin/conveyor task new 'fix the typo in README' --repo api --level L2
bin/conveyor task list
bin/conveyor config export > workspace.yaml
```

MCP clients can submit newly discovered work through `create_task`. Required
arguments are `title`, `repo`, and a caller-stable `idempotency_key`; optional
arguments are `body`, `source`, `base_branch`, and `level` (default `L2`). The
tool durably creates and enqueues the normal task, returns immediately, and
reuses the original task when the same key and input are retried. Luna triage
then advances through the same audited pipeline used by UI, CLI, API, and
GitHub intake.

Open `http://127.0.0.1:8080/settings` for the MCP endpoint and client snippet.
Each review must use a fresh agent session and client token; Conveyor rejects
an implementer's attempt to claim the corresponding review work order.

## Configuration ownership

`conveyor.yaml` bootstraps a workspace on first Postgres start. After that,
the workspace name, routes, bounce cap, and repositories are versioned in
Postgres and editable through the UI/API or `conveyor config import`. The
deployment file retains the database connection, prompt-pack path, and bare
repository cache path.

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
```
