# Configuration

Conveyor has three configuration surfaces, and most confusion comes from
treating them as one. The deployment config describes a server. The local
execution config describes a machine that runs agents. The credentials file
remembers who you are per server. The
[setup playbook](playbooks/conveyor-install.md) keeps the server and client
files separate, including on a solo machine. A combined `conveyor.yaml` still
loads on both sides, because `conveyord` ignores its execution keys, but
select the intended file explicitly when a host also runs agents.

## Deployment config (server)

Read by `conveyord` (and written by `conveyor init`). It holds pipeline
policy and the settings of the stages that run inside `conveyord` (triage,
title generation, and planning), never harnesses or executor models (DEC-56).
The annotated [conveyor.example.yaml](../conveyor.example.yaml) documents
every field inline; the shape:

```yaml
workspace: demo                  # optional bootstrap workspace
max_bounces: 10                  # review rounds before parking at a human gate
work_order_queue_timeout: 24h    # unclaimed orders go stale after this
# pack_dir: /path/to/pack        # strict override of the embedded role prompts
# planning_snapshot_max_bytes: 536870912  # cap compressed and extracted snapshot bytes

database:
  url: postgres://conveyor:conveyor@localhost:5432/conveyor?sslmode=disable
  # max_conns: 32                # PostgreSQL pool size; see Database selection

execution_settings:
  control_plane:                 # in-process stages; the server configuring itself
    triage: {model: gpt-5.6-luna, effort: high, timeout: 20m}
    planning: {model: gpt-5.6-luna, effort: high, timeout: 20m}
  spec: {timeout: 30m}           # stage timeouts are pipeline policy
  implementation: {timeout: 4h}
  review: {timeout: 1h}

review:
  seats: [{}]                    # seat count only; seat models are client-local

execution:
  spec_approval: true            # workspace default for the plan gate
  merge_approval: true           # workspace default for the merge gate
  verify_stage: false            # workspace default for the kit verify stage
  implement_concurrency: 1
  verify_concurrency: 1
  review_concurrency: 1

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
  merge_deferral_bound: 10m
```

`planning_models` optionally lists the planning models the dashboard may
select. The `workspace:` and `repos:` entries are optional; leave them out to
create the workspace from the dashboard's first-run prompt instead.

Every workspace uses these control-plane settings, including a workspace whose
stored configuration is policy only: `conveyord` composes `triage`,
`planning`, and `planning_models` from this file into each workspace's runtime
configuration and never stores them in a workspace document. If planning
refuses with "planning control-plane settings are unavailable" or "planning
model is not configured", set `execution_settings.control_plane.planning.model`
(and `planning_models` for alternate models) in this file and restart
`conveyord`.

A deployment file written before DEC-56 may still carry `harnesses`,
`setups`, `default_setup`, `routing` models, stage `harness`, `model`,
`model_policy`, or `effort`, review fallbacks, or seat models. `conveyord`
and `conveyor init` ignore those keys, never validate them, and log one line
naming each ignored field without its value. When `default_setup` names one of
the file's `setups`, that setup's stage timeouts, review seat count, and
control-plane settings apply, as they did before, and its executor detail is
dropped. Other unknown keys still fail
the load. Move execution detail into a local execution config.

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

#### PostgreSQL pool size

`database.max_conns` sets the maximum number of connections in each
`conveyord` process's PostgreSQL pool, and `CONVEYOR_DATABASE_MAX_CONNS` is
its fallback. The pool serves API requests, the job queue, reconciliation,
the monitor, and long-held side-effect and planning locks. The size resolves
in this order:

1. An explicit `pool_max_conns` parameter in the connection string, for
   example `postgres://...?pool_max_conns=40`.
2. `database.max_conns` in the deployment file.
3. `CONVEYOR_DATABASE_MAX_CONNS`.
4. The default, 32.

Each value must be an integer from 1 to 2147483647. The file value and a
present variable are validated when the deployment file loads, even when a
higher-precedence value wins; an empty variable counts as unset. An
out-of-range file value and any invalid variable value are refused with an
error that names the setting, never its value. A file value that is not an
integer at all fails YAML decoding like any other typed key, and that parser
error quotes the offending scalar. Thirty-two is a starting point, not a measured
optimum: keep the total across every `conveyord` replica within your
database's connection limit. The setting changes nothing for SingleStore,
whose pool stays at 16 open and 4 idle connections.

Startup logs the effective size read from the opened pool
(`database pool max_conns=<n> (effective)`). Every 30 seconds, and once just
before shutdown closes the store, the daemon logs a pool snapshot:
`database pool stats (periodic|final): acquired_conns=… idle_conns=…
max_conns=… empty_acquire_count=… acquire_duration_ms=…`. The last two are
cumulative since startup.

Every `/v1` response from a store that measures it carries a
`Server-Timing` header with that request's database time:
`db;dur=<ms>` (summed query time), `db_queries;desc="<count>"`, and, on
PostgreSQL, `db_acquire;dur=<ms>` (summed time spent acquiring pool
connections). SingleStore omits `db_acquire`. The header holds only durations
and counts, never SQL, identifiers, or workspace data. A streamed response
reports the database activity before its first byte.

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
- `worktree_location`: where new task worktrees are created. Required on a
  machine that runs `conveyor checkout`, `conveyor run`, or the worker:
  `mode: repository` places them at `<primary checkout>/<dir>` (`dir`
  defaults to `_worktrees`, which checkout adds to `.git/info/exclude`);
  `mode: machine` places them under `root` (default `~/.conveyor/worktrees`).
  Repository placement keeps worktrees beside the code and visible to IDE
  worktree pickers; tools that ignore Git excludes, such as Docker build
  contexts, also see them. Machine placement keeps every repository's
  worktrees in one place outside the project folders.
  Record it with `conveyor config set worktree_location <value>` (`repository`,
  `repository:<dir>`, `machine`, or `machine:<root>`) or the
  `conveyor config init-execution` step. The legacy `worktree_root: <path>`
  still reads as `mode: machine`.

  Upgrading: earlier releases silently used `~/.conveyor/worktrees` when
  neither key was set. Such a file is now unconfigured, so `conveyor run` and
  the worker refuse to start and checkout creates nothing until you choose.
  `conveyor config set worktree_location machine` keeps the old placement.
  Existing worktrees are never moved.

- `repos`: one entry per registered repository the machine works on, with
  the registered `name`, the `url`, the absolute `checkout` path of a clone
  whose remote matches, and `base`. Running from inside the repository also
  permits discovery, but the explicit `checkout` mapping lets a worker find
  the clone when it starts elsewhere. `worktree_location` does not provide
  the primary clone.

Setup contents never leave the machine. The server learns whether a
serviceable harness is present, not what it is.

### Example

For a machine with Claude Code installed, `conveyor config init-execution`
writes a file like this:

```yaml
execution_settings:
  spec:
    harness: claude
    model: claude-opus-5
    effort: high
    timeout: 30m
  implementation:
    harness: claude
    effort: high
    timeout: 4h
  review:
    timeout: 1h

harnesses:
  - name: claude
    mcp_transport: json_file
    command:
      [claude, -p, "{prompt}", --mcp-config, "{mcp_config}", --strict-mcp-config,
       --allowedTools, "mcp__conveyor__*", --output-format, stream-json,
       --verbose, --permission-mode, bypassPermissions, --add-dir, ..]
    model_args: [--model, "{model}"]
    effort_args:
      high: [--effort, high]
    probe_command: [claude, --version]

review:
  seats:
    - {model: claude-opus-5, harness: claude}
```

Each stage names the harness that runs it, the model, and a wall-clock
timeout. The harness entry is the launch recipe: the exact argv with
placeholders for the prompt and the generated MCP config, and a probe command
that Conveyor runs to verify the CLI is present before it claims anything.
The probe only checks that the CLI launches; it does not prove account or
model access. The review seats are the review panel: one independent review
order per seat, in order, so a second seat with a different model gives a
second opinion on every delivery.

### Harness MCP transports

The generated MCP config for a `json_file` harness holds no credential. It
names one `conveyor` HTTP server whose header is the reference
`Authorization: Bearer ${CONVEYOR_API_TOKEN}`, and the launcher supplies the
scoped token only in the child's environment. Claude Code expands `${VAR}` in
remote MCP headers loaded through `--mcp-config`
([Claude Code MCP](https://code.claude.com/docs/en/mcp#environment-variable-expansion-in-mcp-json)).
Before trusting the child, the launcher requires Claude's stream-json
initialization event to report that the `conveyor` server is `connected`
with the stage's lifecycle tools, within the harness `probe_timeout`. A
missing or failed receipt stops the child and releases the claim; it never
falls back to a literal token or another configuration source.

| Harness definition | `json_file` support |
| --- | --- |
| Built-in `claude` template, the example above, and the harness in `conveyor.example.yaml` | Supported |
| A renamed definition whose command runs `claude` with `-p`, one `--mcp-config "{mcp_config}"`, `--output-format stream-json`, and `--verbose` | Supported; `--strict-mcp-config` is added at launch when missing |
| Any definition that omits `mcp_transport` | Treated as `json_file` and checked the same way |
| A wrapper script or another agent CLI on `json_file`, including Codex, Grok, Cursor, or OpenCode | Refused before claiming, naming the harness |
| Built-in Codex (`toml_override`), Grok, Cursor, and OpenCode (`environment`) | Not `json_file`; unchanged |

A refused harness is recreated from the built-in `claude` template with
`conveyor config init-execution`, or its stage moves to the Codex
`toml_override` setup or a Grok, Cursor, or OpenCode environment setup. A
token never belongs in a harness definition or in the harness's own
configuration. The environment transports are described on the
[Cursor](cursor-build.md), [Grok](grok-build.md), and
[OpenCode](opencode-build.md) pages.

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

## Files on a machine

| File | Path |
|---|---|
| Binaries | `~/.local/bin` (or `CONVEYOR_INSTALL_DIR`) |
| CLI credentials and per-server defaults | `<user-config-dir>/conveyor/credentials.json` |
| Default per-user execution config | `<user-config-dir>/conveyor/conveyor.yaml` |
| Worker enrollment credentials | `<user-config-dir>/conveyor/workers/<workspace>.json` |
| Task worktrees | The `worktree_location`: `<primary checkout>/_worktrees` (repository) or `~/.conveyor/worktrees` (machine); no default |

`<user-config-dir>` is `~/Library/Application Support` on macOS and
`~/.config` on Linux. Credential files are created with mode 0600 in 0700
directories. The setup playbook places the server's `.env` and deployment
`conveyor.yaml` in `~/.conveyor/server` and the local execution config in
`~/.conveyor/client/conveyor.yaml`, selected with `CONVEYOR_CONFIG`.

## Environment variables

Server (read by `conveyord`):

| Variable | Purpose |
|---|---|
| `CONVEYOR_DATABASE_URL` | PostgreSQL URL or SingleStore URL/DSN. Required unless `database.url` is set. |
| `CONVEYOR_DATABASE_MAX_CONNS` | PostgreSQL pool size when `database.max_conns` is not set (default 32); an explicit `pool_max_conns` in the URL wins. See [PostgreSQL pool size](#postgresql-pool-size). |
| `CONVEYOR_API_TOKEN` | Bound as the first operator's token at bootstrap. Required. |
| `CONVEYOR_LLM_API_KEY` | Key for the in-process triage and planning stages. Required. |
| `CONVEYOR_LLM_BASE_URL` | OpenAI-compatible endpoint override. |
| `CONVEYOR_PUBLIC_URL` | External dashboard URL; used for sign-in links and origin checks. |
| `CONVEYOR_LISTEN_ADDR` | Daemon listen address as `host:port`; used when `-addr` is not explicitly set. |
| `PORT` | Daemon listen port; resolves to `0.0.0.0:<PORT>` when neither `-addr` nor `CONVEYOR_LISTEN_ADDR` is set. |
| `CONVEYOR_SHUTDOWN_TIMEOUT` | Total daemon shutdown budget (default `25s`); used when `-shutdown-timeout` is not explicitly set. Must be positive. |
| `CONVEYOR_DEBUG_ADDR` | Opt-in `net/http/pprof` listener as `host:port` (default off); used when `-debug-addr` is not explicitly set. The host must be a loopback IP address such as `127.0.0.1` or `[::1]` unless `-debug-addr-allow-remote` is passed; any other address stops startup. Never mounted on the API port. |
| `GOMEMLIMIT` | Go runtime soft memory limit, read by the Go runtime rather than by Conveyor. Set it to about 75–80% of the container memory limit; [Container memory limit](#container-memory-limit) explains why, and [Heap profiles](#heap-profiles) describes the profile listener. |
| `CONVEYOR_GITHUB_APP_KEY_ENCRYPTION_KEY` | Base64 of exactly 32 bytes; encrypts workspace GitHub App private keys. Required before connecting an app. `CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY` is its deprecated alias: set alone, it supplies the key and `conveyord` logs one startup warning; set to a different value beside the new name, `conveyord` refuses to start. When renaming, move the existing value; a new value cannot decrypt stored App keys. |
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
| `CONVEYOR_WORKTREE_ROOT` | Overrides the worktree container for `checkout`; run and the worker set it for their children to the resolved container. |
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

On Kubernetes, set `terminationGracePeriodSeconds` to at least the selected
budget plus five seconds (30 seconds for the default). Cloud Run has a fixed
ten-second termination window, so set `-shutdown-timeout` or
`CONVEYOR_SHUTDOWN_TIMEOUT` below ten seconds, such as `8s`. An explicit flag
takes precedence over the environment value.

Dispatched agent sessions additionally receive their assignment in the
environment (`CONVEYOR_TASK_ID`, `CONVEYOR_TASK_BRANCH`,
`CONVEYOR_TASK_BASE_BRANCH`, `CONVEYOR_TASK_REPO`, `CONVEYOR_TASK_REPO_URL`,
`CONVEYOR_WORK_ORDER_ID`, `CONVEYOR_SESSION_ID`, and attempt identifiers).
These are set by the launcher; you never set them yourself.

## Container image

Release tags are published as `ghcr.io/kidus-tiliksew/conveyor:<version>`.
Stable releases also move `ghcr.io/kidus-tiliksew/conveyor:latest`;
prereleases do not. The image contains both Conveyor binaries and the `git`
runtime tool, and no credentials or configuration. It ships no GitHub CLI:
the control plane reaches GitHub through the workspace GitHub App over REST
(DEC-59). Its entrypoint is `conveyord`, which needs the deployment file at
the path `-config` names and a container-reachable listen address such as
`-addr 0.0.0.0:8080`. `CONVEYOR_API_TOKEN`, `CONVEYOR_DATABASE_URL`,
`CONVEYOR_LLM_API_KEY`, and `CONVEYOR_GITHUB_APP_KEY_ENCRYPTION_KEY` come
from the platform's secret facility, never from the image or `conveyor.yaml`.
`/healthz` answers on the published port once the process starts. An image
upgrade replaces the image and restarts it; startup applies embedded database
migrations, and there is no separate migration command.

### Container memory limit

The Go runtime in `conveyord` does not read the container memory limit.
Without a soft limit, the garbage collector lets the heap grow to about twice
its live size before it collects, so a burst of short-lived allocations can
cross the container limit and the kernel kills the process. With `GOMEMLIMIT`
at about 75–80% of the container memory limit, for example
`GOMEMLIMIT=800MiB` under a 1 GiB limit or `GOMEMLIMIT=3200MiB` under a 4 GiB
limit, the collector works harder as the heap nears that value, and the
remaining headroom covers memory the Go heap does not count. The image sets
no `GOMEMLIMIT`, because the right value depends on the limit each deployment
sets.

Measurement, not a guarantee: on 2026-10-27, a deployment running v0.50.0
with a workspace holding a long task history kept a live heap under 100 MiB,
while single list requests peaked above 1 GiB of allocation. Under a 1 GiB
limit without `GOMEMLIMIT`, that deployment was killed for exceeding its memory
limit repeatedly until the limit was raised to 4 GiB. These figures predate
the list-read fixes; a heap profile of your own deployment is the basis for
choosing a limit.

### Heap profiles

`conveyord` can serve Go runtime profiles (`net/http/pprof`) on a second
listener. It is off by default and is never mounted on the API port.
`-debug-addr 127.0.0.1:6060`, or `CONVEYOR_DEBUG_ADDR=127.0.0.1:6060`, turns
it on; an explicit flag wins. The address must be a loopback IP address;
`conveyord` refuses to start with any other address unless
`-debug-addr-allow-remote` is also set. The listener has no authentication,
so it stays on loopback. Startup logs the bound address.

On Kubernetes, `kubectl port-forward` reaches a loopback listener inside the
pod, so no remote opt-in is needed:

```sh
kubectl port-forward pod/<conveyord-pod> 6060:6060
go tool pprof -top http://127.0.0.1:6060/debug/pprof/heap
curl -o allocs.pb.gz http://127.0.0.1:6060/debug/pprof/allocs
curl -o goroutines.txt 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1'
```

`heap` shows the live set at the last collection, and `allocs` shows a
sampling of cumulative allocations since the process started, which finds the
requests that create garbage. `/debug/pprof/profile?seconds=30` records a CPU
profile and `/debug/pprof/trace?seconds=5` records an execution trace.
Profiles carry stack traces, symbol names, and allocation sizes, not request
data. Shutdown closes the debug listener at once, so an in-flight profile is
cut off.

## Workspace config over the API

The server-side workspace configuration (repos, stage timeouts, review seat
count, gates, bounce limits, monitor, issue publication) is also editable at
runtime: through the Workspace page, or round-tripped as YAML with
`conveyor config export` and `conveyor config import`, which uses optimistic
concurrency and rejects unknown keys.

`issue_publication.enabled` decides whether plan approval publishes a GitHub
issue for the task. It is off when the document never set it, including every
workspace created before the setting existed. Turning it on publishes only for
plans approved afterward; turning it off creates no further issues and leaves
published ones unchanged. The server records `issue_publication.enabled_at`,
the time the setting was last turned on, and ignores a value you send.

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
an `installation_url` to resume installation or update access. The app requests
metadata read access and contents, pull requests, issues, and commit statuses
write access. The server stores only the private key, encrypted with AES-256
under `CONVEYOR_GITHUB_APP_KEY_ENCRYPTION_KEY`, and app metadata. Installation tokens expire within one hour and remain in a process-local
cache until five minutes before expiry. App replacement and disconnect invalidate
the cache. No GitHub credential belongs in `conveyor.yaml`.

The dispatcher, work-order reads, and monitor resolve the workspace app without
host credentials. Stored personal and workspace forge tokens are retired; the
server keeps no GitHub credential other than the encrypted App private key.
