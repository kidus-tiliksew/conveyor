# Configuration

Conveyor has three configuration surfaces, and most confusion comes from
treating them as one. The deployment config describes a server. The local
execution config describes a machine that runs agents. The credentials file
remembers who you are per server. The setup guides keep the server and
client files separate, including on a solo machine. A combined
`conveyor.yaml` is supported, but select the intended file explicitly when a
host also runs agents. See [Server setup](server-setup.md) and
[Client setup](client-setup.md).

## Deployment config (server)

Read by `conveyord` (and written by `conveyor init`). The annotated
[conveyor.example.yaml](../conveyor.example.yaml) documents every field
inline; the shape:

```yaml
workspace: demo                  # optional bootstrap workspace
max_bounces: 10                  # review rounds before parking at a human gate
work_order_queue_timeout: 24h    # unclaimed orders go stale after this
# pack_dir: /path/to/pack        # strict override of the embedded role prompts
# planning_snapshot_max_bytes: 536870912  # cap compressed and extracted snapshot bytes

database:
  url: postgres://conveyor:conveyor@localhost:5432/conveyor?sslmode=disable

execution:
  spec_approval: true            # workspace default for the plan gate
  merge_approval: true           # workspace default for the merge gate
  implement_concurrency: 1
  review_concurrency: 1
  first_activity_timeout: 2m

repos:
  - name: api
    url: https://github.com/your-org/your-repo
    github: your-org/your-repo
    base: main

monitor:
  enabled: false
  repositories: [api]
  poll_interval: 1m
  startup_window: 24h
```

Plus `harnesses`, `execution_settings`, `review.seats`, and named `setups`,
which have the same shape as the local execution config below. The
`workspace:` and `repos:` entries are optional; leave them out to create the
workspace from the dashboard's first-run prompt instead.

### Database selection

`database.backend` accepts `postgres`, `singlestore`, and `memory`.
The daemon refuses `memory`, which is only for explicit test callers. Both
durable backends require `database.url` or `CONVEYOR_DATABASE_URL`.

| Backend | Connection form |
| --- | --- |
| PostgreSQL | `postgres://user:password@host:5432/conveyor?sslmode=require` |
| SingleStore | `singlestore://user:password@host:3306/conveyor?tls=true` |
| SingleStore | `mysql://user:password@host:3306/conveyor?tls=true` |
| SingleStore | `user:password@tcp(host:3306)/conveyor?tls=true` |

Keep credentials in the process environment. Percent-encode reserved characters
in URL passwords. An explicit backend wins over inference. When omitted,
SingleStore URL schemes and MySQL TCP/socket DSNs select SingleStore; other
connection strings retain PostgreSQL selection. `conveyor init` and
`conveyor user issue-link` apply the same inference to `CONVEYOR_DATABASE_URL`.
See [SingleStore operations](singlestore.md) before creating its database.

## Local execution config (executor machine)

Read by `conveyor run`, `conveyor worker`, and `conveyor checkout`. Created
by `conveyor config init-execution`, edited with `conveyor config set` and
`conveyor setup`. It describes the agent CLIs on this machine and how each
stage runs:

- `harnesses`: how each agent CLI is launched. The argv (executed directly,
  never through a shell), the MCP transport it supports, model and effort
  flag mappings, and a probe command. Built-in starting points exist for
  Codex, Claude, Grok, Cursor, and OpenCode.
- `execution_settings`: harness, model, effort, and timeout per stage
  (`spec`, `implementation`, `review`).
- `review.seats`: the ordered review panel; one durable review order is
  created per seat.
- `setups`: named bundles of the above, switchable per run with
  `conveyor run --setup <name>`.
- `worktree_root`: where task worktrees are created (default
  `~/.conveyor/worktrees`).

Setup contents never leave the machine. The server learns whether a
serviceable harness is present, not what it is.

The file is resolved in this order: explicit `--config`, `CONVEYOR_CONFIG`,
an existing `./conveyor.yaml`, then the per-user default at
`<user-config-dir>/conveyor/conveyor.yaml`. `conveyor config list` prints
the resolved path and which rule selected it.

## Credentials file

`<user-config-dir>/conveyor/credentials.json`, written by
`conveyor auth login` and `conveyor config set workspace`. One entry per
server: the personal access token and the default workspace. Plaintext by
design (the same trust model as `gh` and kubeconfig), file mode 0600,
directory 0700. Environment variables override it, which is the intended
mechanism for CI, workers, and MCP clients.

## Environment variables

Server (read by `conveyord`):

| Variable | Purpose |
|---|---|
| `CONVEYOR_DATABASE_URL` | PostgreSQL URL or SingleStore URL/DSN. Required unless `database.url` is set. |
| `CONVEYOR_API_TOKEN` | Bound as the first operator's token at bootstrap. Required. |
| `CONVEYOR_LLM_API_KEY` | Key for in-process triage and spec stages. Required. |
| `CONVEYOR_LLM_BASE_URL` | OpenAI-compatible endpoint override. |
| `CONVEYOR_PUBLIC_URL` | External dashboard URL; used for sign-in links and origin checks. |
| `CONVEYOR_LISTEN_ADDR` | Daemon listen address as `host:port`; used when `-addr` is not explicitly set. |
| `PORT` | Daemon listen port; resolves to `0.0.0.0:<PORT>` when neither `-addr` nor `CONVEYOR_LISTEN_ADDR` is set. |
| `CONVEYOR_SHUTDOWN_TIMEOUT` | Total daemon shutdown budget (default `25s`); used when `-shutdown-timeout` is not explicitly set. Must be positive. |
| `CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY` | Base64 of exactly 32 bytes; encrypts workspace GitHub App private keys and legacy forge tokens. Required before connecting an app. |
| `CONVEYOR_SMTP_HOST` / `_PORT` / `_USERNAME` / `_PASSWORD` / `_FROM` | Invitation email delivery. Configured only when host and from are both set; otherwise links are surfaced for manual delivery. |
| `CONVEYOR_ORGANIZATION_NAME`, `CONVEYOR_FIRST_OPERATOR_EMAIL`, `CONVEYOR_FIRST_OPERATOR_DISPLAY_NAME` | First-operator identity at bootstrap. |
| `CONVEYOR_CONTROL_PLANE_MODEL`, `CONVEYOR_TRIAGE_MODEL`, `CONVEYOR_PLANNING_MODEL` | Process-level model overrides for in-process stages; never change stored config. |

Client (read by `conveyor`):

| Variable | Purpose |
|---|---|
| `CONVEYOR_ADDR` | Server URL (default `http://localhost:8080`). |
| `CONVEYOR_API_TOKEN` | Bearer token for the environment server (normalized `CONVEYOR_ADDR`, else `http://localhost:8080`); ignored for any other resolved server, which uses its stored credential. |
| `CONVEYOR_WORKSPACE` | Workspace id; overrides the stored default. |
| `CONVEYOR_CONFIG` | Local execution config path. |
| `CONVEYOR_WORKTREE_ROOT` | Overrides the worktree root for `checkout`. |
| `CONVEYOR_WORKER_TOKEN` | Pre-supplied worker enrollment credential (bypasses the saved file). |
| `CONVEYOR_ENV_FILE` | Dotenv path (default `./.env`). |

Both binaries load the dotenv file first, and real environment values always
win over file values. `CONVEYOR_API_KEY` and `CONVEYOR_API_BASE_URL` are
deprecated fallbacks for the `_LLM_` pair and remain only for existing
installations.

On SIGINT or SIGTERM, `conveyord` stops the queue from fetching work, cancels HTTP
request base contexts, drains HTTP and active jobs, then cancels remaining jobs
and closes the selected database within the shutdown budget. The hard-stop phase reserves up
to five seconds, or half of a shorter budget. Queue crash recovery uses a
stuck-job threshold equal to the largest effective triage or spec route timeout
across the startup workspaces plus a five-minute safety margin. Startup logs
both effective values and refuses an invalid route/threshold relationship.

Dispatched agent sessions additionally receive their assignment in the
environment (`CONVEYOR_TASK_ID`, `CONVEYOR_TASK_BRANCH`,
`CONVEYOR_TASK_BASE_BRANCH`, `CONVEYOR_TASK_REPO`, `CONVEYOR_TASK_REPO_URL`,
`CONVEYOR_WORK_ORDER_ID`, `CONVEYOR_SESSION_ID`, and attempt identifiers).
These are set by the launcher; you never set them yourself.

## Workspace config over the API

The server-side workspace configuration (repos, harness catalog entries,
review seats, setups, execution defaults, monitor) is also editable at
runtime: through the Workspace page, or round-tripped as YAML with
`conveyor config export` and `conveyor config import`, which uses optimistic
concurrency and rejects unknown keys.

## Workspace GitHub App connection

Connect the app through Workspace settings in an authenticated dashboard session.
GitHub creates it from Conveyor's manifest, then asks you to install it and select
repositories. The settings status lists coverage for the workspace's registered
GitHub repository slugs. A missing, revoked, suspended, or uncovered installation
returns a permission failure directing you to workspace settings.

Set the public URL used for dashboard sign-in before connecting the app. The
manifest callback state is bound to that session, expires after ten minutes, and
can be consumed once. Installation returns use separate single-use state. The
normal session cookie stays Strict; a ten-minute cookie scoped to the app
endpoints permits the GitHub return navigation. Operator status responses include
an `installation_url` to resume installation or update access. The server stores only the encrypted private key and app
metadata. Installation tokens expire within one hour and remain in a process-local
cache until five minutes before expiry. App replacement and disconnect invalidate
the cache. No GitHub credential belongs in `conveyor.yaml`.

The dispatcher, work-order reads, and monitor resolve the workspace app without
host credentials. Legacy forge-token APIs and claim presence checks remain for
compatibility until their separate retirement release.
