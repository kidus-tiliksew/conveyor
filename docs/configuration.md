# Configuration

Conveyor has three configuration surfaces, and most confusion comes from
treating them as one. The deployment config describes a server. The local
configuration file holds a machine's worktree location and verification
records. The credentials file remembers who you are per server. The
[setup playbook](playbooks/conveyor-install.md) keeps the server and client
files separate, including on a solo machine, so select the intended file
explicitly when a host also runs agent sessions.

## Deployment config (server)

Read by `conveyord` (and written by `conveyor init`). It holds pipeline
policy and the settings of the stages that run inside `conveyord` (triage,
title generation, and planning). It holds no agent harness, model, or effort
for any stage (DEC-132). The person who starts an agent session chooses its
harness and model.
The annotated [conveyor.example.yaml](../conveyor.example.yaml) documents
every field inline; the shape:

```yaml
workspace: demo                  # optional bootstrap workspace
max_bounces: 10                  # review rounds before parking at a human gate
work_order_queue_timeout: 24h    # unclaimed orders go stale after this
# pack_dir: /path/to/pack        # strict override of the embedded role prompts
# planning_snapshot_max_bytes: 536870912  # cap compressed and extracted snapshot bytes

database:
  backend: postgres              # the URL comes from CONVEYOR_DATABASE_URL
  # max_conns: 32                # PostgreSQL pool size; see Database selection
  # singlestore_compress: true   # SingleStore wire compression; see Database selection

execution_settings:
  control_plane:                 # in-process stages; the server configuring itself
    triage: {model: gpt-5.6-luna, effort: high, timeout: 20m}
    planning: {model: gpt-5.6-luna, effort: high, timeout: 20m}
  spec: {timeout: 30m}           # stage timeouts are pipeline policy
  implementation: {timeout: 4h}
  review: {timeout: 1h}

review:
  seats: [{}]                    # seat count only; a seat holds no harness, model, or effort
  distinct_models: false         # refuse a seat claim that repeats a round's model name

execution:
  default_plan_state: needed     # plan state of a task filed with none: needed or not_required
                                 # spec_approval supplies the default only when this is absent;
                                 # both keys with different meanings are rejected
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

A deployment file written before execution setups were retired may still
carry `harnesses`, `setups`, `default_setup`, `routing`, a stage `harness`,
`model`, `model_policy`, or `effort`, review fallbacks, or seat models.
`conveyord` and `conveyor init` ignore those keys, never validate them, and
log one line naming each ignored field without its value. A `routing` stage
keeps only its timeout, so a stage that `execution_settings` leaves without a
timeout still takes it. The `model`, `model_tier`, and `effort` of
`routing.stages.triage` still set the triage model and effort where
`execution_settings.control_plane.triage` states none. When `default_setup`
names one of the file's `setups`, that setup's stage timeouts, review seat
count, and control-plane settings apply, as they did before, and its executor
detail is dropped. Other unknown keys still fail the load. Conveyor keeps
execution detail nowhere, so remove those keys when convenient.

A workspace configuration that an earlier release stored may hold the same
keys. Every read drops them before it decodes the rest, logs each removed
field by name once per process, and never rewrites the stored row or changes
its version. The row keeps the stage timeouts and review seat count it
resolved to, and triage and planning settings always come from the
deployment file.

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

A SingleStore connection requests protocol compression by default. For
`conveyord` the setting resolves in this order: `database.singlestore_compress`
in the deployment file, then `CONVEYOR_SINGLESTORE_COMPRESS`, then the
connection string's `compress` parameter, then `true`. `conveyor init` and
`conveyor user` read only the `compress` parameter, then `true`. The variable
must be a boolean even when the file setting wins, and an invalid `compress`
parameter stops the open. Neither refusal echoes the value. When the server
does not negotiate compression, the connection runs uncompressed with no error.
`conveyord` logs one line after the store opens:
`SingleStore protocol compression requested=<true|false> negotiation=<state>`,
where the state is `negotiated`, `not_negotiated`, or `unknown` when no
connection handshake was observed yet. PostgreSQL ignores both settings.

`conveyor init` writes `database.backend` and no URL, because a URL can hold a
password. `conveyord` then needs `CONVEYOR_DATABASE_URL`. The installed service
reads it from the owner-only `.env` beside the config or the file `--env-file`
names. A foreground `conveyord` reads it from the file `CONVEYOR_ENV_FILE`
names, or from `.env` in its working directory. The Linux service unit sets
`CONVEYOR_ENV_FILE` to the file it names, as the macOS definition does, so the
service never loads a second `.env`. `conveyord install` writes the unit, so a
service installed by an earlier release keeps its old unit until
`conveyord install` runs again. Every command trims whitespace around the URL
when it opens the database, and changes nothing else in it. A blank value
refuses to open.

A file from an earlier version can hold `database.url`. It still loads, and its
URL wins over the variable. Rerunning `conveyor init` over it requires the same
backend and the same URL as `CONVEYOR_DATABASE_URL`, leaves the file unchanged,
and prints a notice that the file holds a URL, with no part of the URL. To
retire the URL, move it into the variable and delete the key.

#### Several daemons on one database

`runtime.peers: true` declares that this `conveyord` runs beside other
daemons on the same database. The default is `false`: a single daemon learns
of new work from its own commits and sends no statement to the database
while idle.

```yaml
runtime:
  peers: true
```

- **PostgreSQL.** Each notice-raising transaction also sends
  `pg_notify('conveyor_changes', ...)`, and each daemon holds one direct,
  unpooled connection that runs `LISTEN conveyor_changes`. When the database
  closes that connection, for example when a scale-to-zero database suspends,
  the daemon reconnects on its next request and then catches up every
  workspace once. Use a direct connection string, not a transaction pooler:
  `LISTEN` needs a session.
- **SingleStore.** There is no change notification, so each daemon runs one
  peer check every 30 seconds: a single statement that reads every
  workspace's job log head and task-event position, however many workspaces,
  open dashboard streams, and pending `await_review` calls the daemon serves.
  A successful check publishes a job notice for each workspace whose job log
  head moved and a task notice for each workspace whose task-event position
  moved since the last successful check. A position moves only when the check
  can read every task event it counts, so a task event that is not yet visible
  to the check moves no position and publishes no notice until a later
  successful check reads it. Work another daemon commits starts, and an open
  stream or `await_review` sees another daemon's change, after a successful
  check that observes the moved position. A failed check publishes nothing and
  keeps the previous positions, so a later successful check still finds every
  change.

With `runtime.peers` off, the default, no peer check runs on any backend, so a
daemon does not see work that another daemon commits.

The daemon logs its peer mode at startup: `runtime peers: off`,
`runtime peers: PostgreSQL change notification`, or
`runtime peers: one peer check every 30s for the whole daemon`.

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

## Local configuration (agent machine)

Read by `conveyor checkout`, `conveyor config`, and `conveyor kit verify` on a
machine where you start agent sessions. It holds no harness, model, effort, or
execution setup: you choose the harness and model when you start an agent
session, and the session claims its work order itself over MCP (DEC-104(2),
DEC-97(1)). The file holds:

- `worktree_location`: where new task worktrees are created. Required on a
  machine that runs `conveyor checkout`:
  `mode: repository` places them at `<primary checkout>/<dir>` (`dir`
  defaults to `_worktrees`, which checkout adds to `.git/info/exclude`);
  `mode: machine` places them under `root` (default `~/.conveyor/worktrees`).
  Repository placement keeps worktrees beside the code and visible to IDE
  worktree pickers; tools that ignore Git excludes, such as Docker build
  contexts, also see them. Machine placement keeps every repository's
  worktrees in one place outside the project folders.
  Record it with `conveyor config set worktree_location <value>` (`repository`,
  `repository:<dir>`, `machine`, or `machine:<root>`). The legacy
  `worktree_root: <path>` still reads as `mode: machine`.

  Upgrading: earlier releases silently used `~/.conveyor/worktrees` when
  neither key was set. Such a file is now unconfigured, so checkout creates
  nothing until you choose. `conveyor config set worktree_location machine`
  keeps the old placement. Existing worktrees are never moved.

- `kit_permissions` and `verification_toolchains`: the operator's local
  verification records that `conveyor kit verify` intersects with server
  grants, described in the `conveyor-kit-verify` playbook.

A file that an earlier release wrote may still carry `harnesses`,
`execution_settings`, `execution`, `review`, `routing`, `setups`,
`default_setup`, or `planning_models`. The CLI ignores those keys, never
validates them, and logs one line naming them without their values.

## Credentials file

`<user-config-dir>/conveyor/credentials.json`, written by
`conveyor auth login` and `conveyor config set workspace`. One entry per
server: the personal access token and the default workspace. Plaintext by
design (the same trust model as `gh` and kubeconfig), file mode 0600,
directory 0700. Environment variables override it, which is the intended
mechanism for CI and MCP clients.

## Files on a machine

| File | Path |
|---|---|
| Binaries | `~/.local/bin` (or `CONVEYOR_INSTALL_DIR`) |
| CLI credentials and per-server defaults | `<user-config-dir>/conveyor/credentials.json` |
| Default per-user local configuration | `<user-config-dir>/conveyor/conveyor.yaml` |
| Task worktrees | The `worktree_location`: `<primary checkout>/_worktrees` (repository) or `~/.conveyor/worktrees` (machine); no default |

`<user-config-dir>` is `~/Library/Application Support` on macOS and
`~/.config` on Linux. Credential files are created with mode 0600 in 0700
directories. The setup playbook places the server's `.env` and deployment
`conveyor.yaml` in `~/.conveyor/server` and the local configuration in
`~/.conveyor/client/conveyor.yaml`, selected with `CONVEYOR_CONFIG`.

## Environment variables

Server (read by `conveyord`):

| Variable | Purpose |
|---|---|
| `CONVEYOR_DATABASE_URL` | PostgreSQL URL or SingleStore URL/DSN. Required unless `database.url` is set. |
| `CONVEYOR_DATABASE_MAX_CONNS` | PostgreSQL pool size when `database.max_conns` is not set (default 32); an explicit `pool_max_conns` in the URL wins. See [PostgreSQL pool size](#postgresql-pool-size). |
| `CONVEYOR_SINGLESTORE_COMPRESS` | Request SingleStore wire compression for `conveyord`. Defaults to `true`; set to `false` to disable it. `database.singlestore_compress` in the deployment file takes precedence. Direct `conveyor` CLI database connections do not read this setting; use `compress=false` in the SingleStore DSN or URL to opt them out. |
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

The database read statistics count wire bytes. When compression is negotiated,
the reported bytes are compressed and are not directly comparable with
statistics collected before compression was enabled.

Client (read by `conveyor`):

| Variable | Purpose |
|---|---|
| `CONVEYOR_ADDR` | Server URL (default `http://localhost:8080`). |
| `CONVEYOR_API_TOKEN` | Bearer token for the environment server (normalized `CONVEYOR_ADDR`, else `http://localhost:8080`); ignored for any other resolved server, which uses its stored credential. |
| `CONVEYOR_WORKSPACE` | Workspace id; overrides the stored default. |
| `CONVEYOR_CONFIG` | Local configuration file path. |
| `CONVEYOR_WORK_ORDER_ID`, `CONVEYOR_SESSION_ID` | The claimed order and the claim's session ID, which a self-claimed agent session sets on `conveyor submit`. |
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

`conveyor checkout` and `conveyor submit` read the task's repository, base,
and branch from the task with your credential. Checkout ignores every
`CONVEYOR_TASK_*`, `CONVEYOR_WRITER_*`, `CONVEYOR_PREVIOUS_ATTEMPT_*`, and
`CONVEYOR_WORKTREE_ROOT` variable that earlier launchers set.

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
count, review scopes, gates, bounce limits, monitor, issue publication) is also
editable at runtime: through the Workspace page, or round-tripped as YAML with
`conveyor config export` and `conveyor config import`, which uses optimistic
concurrency and rejects unknown keys.

`review.distinct_models` is off when the document never set it. When it is true
at a task's intake, the task's frozen policy holds it, and a review seat claim
is refused with `review_seat_model_not_distinct` when its reported model name is
blank or repeats, ignoring letter case and surrounding whitespace, the name
another seat of the round reports. A released, expired, or cancelled seat and an
earlier round do not count. Conveyor compares reported names only: it verifies
no name, chooses no model, and keeps no list of models. A later workspace edit
changes no existing task. To change one task, an operator runs
`conveyor task set-distinct-models <task-id> --off|--on --reason <text>`. The
server refuses that change while a seat of the task is claimed or holds a
submitted verdict.

`later_round_review` sets what a review round compares after a bounce. Its
values are `full` and `delta`, and `full` applies when the document never set
it. `full` reviews the whole change. `delta` reviews only the changes since the
last complete review round.

A submission fixes the scope of its head once. The scope is `delta` only when
the last complete round's commit is an ancestor of the new head. In every
other case the round reviews the whole change. This includes a round that
cancelled a seat, seats that reviewed different commits, and a failed GitHub
read. A retried submission of the same head keeps its first scope. While an
approval is stale, `refresh_review` alone sets the scope.

`refresh_review` sets what a refresh review compares after the head moves past
an approved commit. Its values are `delta` and `full`, and `delta` applies when
the document never set it. `delta` reviews the changes since the approved
commit. `full` reviews the whole change. A save with any other value, including
`none`, is refused, and the refusal names the member.

A task freezes both values at intake, and a later workspace edit changes no
existing task. A task frozen before `later_round_review` existed reads as
`full`. A task frozen with `refresh_review: none` keeps that value. To change
one task's later-round scope, an operator runs
`conveyor task set-later-round-review <task-id> --full|--delta --reason <text>`.
The new value applies to the next implementation submission. The server
refuses that change only for a merged or closed task. No command changes a
task's frozen `refresh_review`.

`issue_publication.enabled` decides whether plan approval publishes a GitHub
issue for the task. It is off when the document never set it, including every
workspace created before the setting existed. Turning it on publishes only for
plans approved afterward; turning it off creates no further issues and leaves
published ones unchanged. The server records `issue_publication.enabled_at`,
the time the setting was last turned on, and ignores a value you send.

`execution.progress_report_interval` sets how often an agent session that
holds a claim reports progress. The value is duration text. It is `10m` when
the document never set it, including a document with no `execution` block.
`0` means that no interval applies. Any other value must be from `1m` to
`60m`, both included. A save with another value, a negative value, or text
that is not a duration is refused with 422 `validation_failed`, and the refusal
names `execution.progress_report_interval`. A deployment file with such a
value fails to load. Every configuration read and `conveyor config export`
serve the effective value, so a workspace that never set it reads `10m`.

No task freezes this setting. Each `get_work_order` read states the current
value as `progress_report_interval_seconds`, so a change applies at the next
read. Report timing gates no claim, lease renewal, submission, review, or
merge (`req-delegated-execution` REQ-6; `component-runtime` invariant 42).

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
metadata and checks read access and contents, pull requests, issues, and commit
statuses write access. The server stores the private key and the webhook
secret, each encrypted with AES-256 under
`CONVEYOR_GITHUB_APP_KEY_ENCRYPTION_KEY`, and app metadata. Installation tokens expire within one hour and remain in a process-local
cache until five minutes before expiry. App replacement and disconnect invalidate
the cache. No GitHub credential belongs in `conveyor.yaml`.

### Webhook delivery

GitHub tells Conveyor about pull requests, checks, commit statuses, pushes, and
`conveyor:ready` labels through the app's signed webhook at
`<public URL>/v1/github/webhook/<workspace>`. An app created from the manifest
subscribes `pull_request`, `check_suite`, `check_run`, `status`, `push`, and
`issues` and gets its webhook secret from GitHub, so it needs no setup. For an
app connected before webhook delivery existed, generate a secret in Workspace
settings (`POST /v1/workspaces/{id}/github-app/webhook-secret`, shown once) and
enter it, the receiver URL, those six events, and checks read permission in the
app's GitHub settings. Workspace settings show the receiver URL, the events,
whether a secret is set, and when the last delivery was accepted; a workspace
has webhook delivery from its first accepted delivery.

A workspace without webhook delivery keeps a timed GitHub check, at most once a
minute, only while one of its tasks waits on GitHub or its monitor is enabled.
A workspace with webhook delivery makes no timed GitHub call; each daemon start
still reads every task that waits on GitHub once and polls each enabled
monitor once. Timed `conveyor:ready` issue intake runs only when `conveyord`
is started with `-poll-github`.

The dispatcher, work-order reads, and monitor resolve the workspace app without
host credentials. Stored personal and workspace forge tokens are retired; the
server keeps no GitHub credential other than the encrypted App private key.
