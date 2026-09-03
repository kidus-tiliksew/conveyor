# Configuration

Conveyor has three configuration surfaces, and most confusion comes from
treating them as one. The deployment config describes a server. The local
execution config describes a machine that runs agents. The credentials file
remembers who you are per server. On the factory host the first two
intentionally share one `conveyor.yaml`; everywhere else they are separate
files with separate owners.

## Deployment config (server)

Read by `conveyord` (and written by `conveyor init`). The annotated
[conveyor.example.yaml](../conveyor.example.yaml) documents every field
inline; the shape:

```yaml
workspace: demo                  # optional bootstrap workspace
max_bounces: 10                  # review rounds before parking at a human gate
work_order_queue_timeout: 24h    # unclaimed orders go stale after this
# pack_dir: /path/to/pack        # strict override of the embedded role prompts
# cache_dir: ~/.conveyor/cache   # bare repo cache for checkout/diff flows

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

## Local execution config (executor machine)

Read by `conveyor run`, `conveyor worker`, and `conveyor checkout`. Created
by `conveyor config init-execution`, edited with `conveyor config set` and
`conveyor setup`. It describes the agent CLIs on this machine and how each
stage runs:

- `harnesses`: how each agent CLI is launched. The argv (executed directly,
  never through a shell), the MCP transport it supports, model and effort
  flag mappings, and a probe command. Built-in starting points exist for
  Codex, Claude, Grok, and Cursor.
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
| `CONVEYOR_DATABASE_URL` | Postgres connection string. Required. |
| `CONVEYOR_API_TOKEN` | Bound as the first operator's token at bootstrap. Required. |
| `CONVEYOR_LLM_API_KEY` | Key for in-process triage and spec stages. Required. |
| `CONVEYOR_LLM_BASE_URL` | OpenAI-compatible endpoint override. |
| `CONVEYOR_PUBLIC_URL` | External dashboard URL; used for sign-in links and origin checks. |
| `CONVEYOR_LISTEN_ADDR` | Daemon listen address as `host:port`; used when `-addr` is not explicitly set. |
| `PORT` | Daemon listen port; resolves to `0.0.0.0:<PORT>` when neither `-addr` nor `CONVEYOR_LISTEN_ADDR` is set. |
| `CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY` | Base64 of exactly 32 bytes; encrypts per-user GitHub tokens. Required before anyone can store one. |
| `CONVEYOR_SMTP_HOST` / `_PORT` / `_USERNAME` / `_PASSWORD` / `_FROM` | Invitation email delivery. Configured only when host and from are both set; otherwise links are surfaced for manual delivery. |
| `CONVEYOR_ORGANIZATION_NAME`, `CONVEYOR_FIRST_OPERATOR_EMAIL`, `CONVEYOR_FIRST_OPERATOR_DISPLAY_NAME` | First-operator identity at bootstrap. |
| `CONVEYOR_CONTROL_PLANE_MODEL`, `CONVEYOR_TRIAGE_MODEL`, `CONVEYOR_PLANNING_MODEL` | Process-level model overrides for in-process stages; never change stored config. |

Client (read by `conveyor`):

| Variable | Purpose |
|---|---|
| `CONVEYOR_ADDR` | Server URL (default `http://localhost:8080`). |
| `CONVEYOR_API_TOKEN` | Bearer token; overrides the stored credential. |
| `CONVEYOR_WORKSPACE` | Workspace id; overrides the stored default. |
| `CONVEYOR_CONFIG` | Local execution config path. |
| `CONVEYOR_WORKTREE_ROOT` | Overrides the worktree root for `checkout`. |
| `CONVEYOR_WORKER_TOKEN` | Pre-supplied worker enrollment credential (bypasses the saved file). |
| `CONVEYOR_ENV_FILE` | Dotenv path (default `./.env`). |

Both binaries load the dotenv file first, and real environment values always
win over file values. `CONVEYOR_API_KEY` and `CONVEYOR_API_BASE_URL` are
deprecated fallbacks for the `_LLM_` pair and remain only for existing
installations.

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
