# CLI reference

Two binaries: `conveyor` (the user CLI) and `conveyord` (the server daemon).
Both load a `.env` file from the working directory before anything else
(`CONVEYOR_ENV_FILE` selects a different file), and process environment values
always win over file values.

## Global flags and resolution

Every `conveyor` command accepts:

- `--server`: the Conveyor server URL
- `--workspace`: workspace id, required when the server has more than one
  workspace

Values resolve in a fixed order, and `conveyor config list` reports which
source won for each:

| Value | Order |
|---|---|
| server | explicit `--server`, then `CONVEYOR_ADDR`, then `http://localhost:8080` |
| token | `CONVEYOR_API_TOKEN`, then the stored credential file |
| workspace | explicit `--workspace`, then `CONVEYOR_WORKSPACE`, then the stored file, then the singleton fallback |

An explicitly typed flag beats the environment; an untouched flag default does
not. Requests carry `Authorization: Bearer <token>` and `X-Workspace-ID`.

Executor-side commands (`run`, `worker`, `checkout`, `config`, `setup`) also
resolve a local execution config file, in this order: explicit `--config`,
`CONVEYOR_CONFIG`, an existing `./conveyor.yaml`, then the per-user default at
`<user-config-dir>/conveyor/conveyor.yaml`. See
[Configuration](configuration.md) for what goes in it.

## auth

Manage local login credentials, stored per server in
`<user-config-dir>/conveyor/credentials.json` (plaintext by design, mode 0600,
same trust model as `gh` and kubeconfig).

| Command | What it does |
|---|---|
| `conveyor auth login` | Prompt for a personal access token with hidden input, verify it against `/v1/me`, and store it. Refuses piped input; the token never appears in process arguments. |
| `conveyor auth status` | Show the effective server, your identity, and the token's label. |
| `conveyor auth token` | Print the stored token, for `export CONVEYOR_API_TOKEN=$(conveyor auth token)`. |
| `conveyor auth logout [--revoke]` | Remove the local entry; `--revoke` also revokes the token on the server. |

## init

`conveyor init` initializes an organization and first workspace interactively:
organization name, first operator identity, workspace id, and the target
repository. It requires `CONVEYOR_DATABASE_URL` and `CONVEYOR_API_TOKEN` in
the environment plus `CONVEYOR_LLM_API_KEY` when the generated setup uses
in-process triage or planning. It registers the repository by name, URL, and
default branch without requiring a local clone or forge tool, writes
`conveyor.yaml` (mode 0600), and prints the first operator's sign-in link.
`--config` picks a different output path.

## task

Create and inspect tasks. Titles are always generated from the body.

| Command | What it does |
|---|---|
| `conveyor task new` | Create a task. `--repo` (required), `-m/--message` for the body, `--base` (default `main`), `--depends-on <id>` (repeatable), `--hold`, `--setup <name>`, `--spec-approval` and `--merge-approval` (`default`, `on`, or `off`). |
| `conveyor task list` | List tasks: ID, state, repo, source, title. |
| `conveyor task show <id>` | Show a task and its jobs as JSON. |
| `conveyor task close <id>` | Cancel a non-terminal task. `--reason` is required. |
| `conveyor task link <task> <dependency>` | Make an existing open task depend on another open task. `--reason` and `--request-id` are required; cycles are rejected. |
| `conveyor task unlink <task> <dependency>` | Remove one blocking dependency edge. `--reason` and `--request-id` required. |
| `conveyor task setup <id>` | Change a task's frozen execution setup for future work only. Exactly one of `--setup <name>` or `--apply-latest`, plus `--reason` and `--request-id`. |
| `conveyor task approve <id>` | Approve at a human gate. `--reason` defaults to `approved`; `-m` adds a comment. |
| `conveyor task request-changes <id>` | Bounce work at the merge gate. `-f/--feedback` is required and goes verbatim to the next implementation order. |
| `conveyor task reject <id>` | Reject at a human gate. `--reason` required. |
| `conveyor task redirect <id>` | Redirect at a human gate. `--reason` and `--message` required. |

Note the naming split: `conveyor task setup` changes a task's frozen
workspace setup; `conveyor config init-execution` creates your local
execution settings. They are different objects.

## run

```sh
conveyor run <task-id>
```

Explicitly claim and execute one task on this machine, stage by stage. In a
terminal it runs a full-screen view with the stage output, and surfaces
operator gates and pending document proposals inline so you can approve,
request changes, or confirm without leaving the run.

- `--auto`: run every claimable stage without per-stage confirmation.
  Operator gates still apply.
- `--setup <name>`: use a named local execution setup for this run only. The
  harness is probed before anything is claimed.
- `--raw`: print the raw harness event stream instead of the TUI.
- `--config`: select the local execution config.

Requires `CONVEYOR_API_TOKEN` and a stored GitHub token (see
[Authentication](auth.md#github-forge-tokens)).

## checkout and done

```sh
conveyor checkout <task-id>
```

Create or reuse the task's dedicated worktree and print its absolute path
(designed for `cd "$(conveyor checkout <id>)"`). The destination is
`<worktree_root>/<repo>-task-<id>`; the root resolves from an explicit
`--path`, then the `worktree_root` config key, then `~/.conveyor/worktrees`.

Checkout is deliberately conservative. It must run inside the target
repository, verifies the repository's identity before fetching, refuses a
dirty or mid-operation primary checkout, refuses a diverged task branch, and
never resets, rebases, or force-recreates anything. If the branch already has
a registered worktree it is reused; if the remote is strictly ahead the
worktree is fast-forwarded.

```sh
conveyor done <task-id>
```

Remove the task's worktree after the task is `merged` or `closed`. It must
run in the repository's primary checkout, and it keeps the branch, so
unmerged history is never deleted.

## worker

Enroll and run the durable worker that polls the queue and supervises agent
harnesses. Day-to-day operation is covered in
[Worker operations](worker-operations.md).

| Command | What it does |
|---|---|
| `conveyor worker pair` | Issue a short-lived single-use pairing token (`--ttl`, default 10m). |
| `conveyor worker run` | Heartbeat, claim queued work, and supervise harnesses. `--pairing-token` for first enrollment, `--name`, `--once` to drain and exit, `--config`. |
| `conveyor worker list` | List enrolled workers and their health. |
| `conveyor worker revoke <worker-id>` | Revoke an enrolled worker. |
| `conveyor worker install` / `uninstall` / `status` | Manage the workspace-specific user service (launchd on macOS, systemd user unit on Linux). |

A worker validates its complete local execution config before it enrolls,
heartbeats, or claims, so it never advertises work it cannot run. Enrollment
credentials are saved at `<user-config-dir>/conveyor/workers/<workspace>.json`
and reused across restarts.

## config

| Command | What it does |
|---|---|
| `conveyor config init-execution` | Interactive wizard: detect installed agent CLIs, probe them, write the local execution config. |
| `conveyor config set execution.<stage>.<field> <value>` | Set one field without the wizard. Stage is `spec`, `implement`, or `review`; field is `harness`, `model`, `effort`, or `timeout`. Harnesses are probed before being written. |
| `conveyor config set workspace <id>` | Set the default workspace for the effective server. |
| `conveyor config list` | Show every effective setting and which source supplied it. |
| `conveyor config export` | Write the server-side workspace config as YAML. |
| `conveyor config import <path\|->` | Replace the workspace config from YAML, with optimistic concurrency via the current version. Unknown keys are rejected. |

## setup

Manage named local execution setups. Setup contents never leave your machine.

| Command | What it does |
|---|---|
| `conveyor setup create <name>` / `edit <name>` | Interactively create or edit a named setup. |
| `conveyor setup list` | List every named setup. |
| `conveyor setup delete <name>` | Delete a non-default setup. |
| `conveyor setup default <name>` | Designate the default setup. |
| `conveyor setup seat add <setup>` | Append a review seat at lowest priority. `--harness`, `--model`, `--effort` all required. |
| `conveyor setup seat remove <setup> <position>` | Remove a seat by one-based position. The last seat cannot be removed. |
| `conveyor setup seat move <setup> <from> <to>` | Reorder seats. |

One durable review order is created per configured seat, in order, so seat
position is review priority.

## mcp and skills

| Command | What it does |
|---|---|
| `conveyor mcp install` | Register the Conveyor MCP server (`<server>/mcp`) with detected Claude Code, Codex, and Cursor clients. `--tool claude\|codex\|cursor`, `--list` to report without writing, `--adopt` to take over an unmarked existing registration. Cursor is reported as unsupported until its native MCP target is available. |
| `conveyor skills install` | Install the embedded agent skills (`conveyor-work`, `conveyor-plan`, `conveyor-file-tasks`) into `~/.claude/skills`, `~/.codex/skills`, and `~/.cursor/skills`. `--project` targets the project directory instead, `--tool` narrows the detected client, and `--force` allows downgrading skills a newer release installed. |

MCP registration writes an environment-backed token reference
(`CONVEYOR_API_TOKEN`), never a token value. Installs refresh files Conveyor
owns and refuse unrelated collisions.

## monitor

| Command | What it does |
|---|---|
| `conveyor monitor status` | Workspace monitor health and unresolved drift, as JSON. |
| `conveyor monitor resolve <drift-id> --outcome <outcome>` | Record an audited drift reconciliation. Outcomes: `requirements_amended`, `design_document_updated`, `conflict_resolved`, `change_reverted`. |

See [Misalignment](misalignment.md) for what drift is and when to use each
outcome.

## lineage

```sh
conveyor lineage rebuild --workspace <id> --reason '<why>' --request-id <key>
```

Replay every workspace event and repair the lineage projection in one
transaction. Rows that cannot be regenerated from events are preserved and
reported, never deleted.

## user

```sh
conveyor user issue-link <email>
```

Issue a fresh sign-in link for an existing account or pending invitation.
Host-local: it needs `CONVEYOR_DATABASE_URL` and talks to Postgres directly,
which is what makes it work as the lockout-recovery path. With
`CONVEYOR_PUBLIC_URL` set it prints a complete link; otherwise it prints the
raw token for you to deliver.

## conveyord

The daemon takes single-dash flags:

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | Listen address |
| `-config` | `conveyor.yaml` | Deployment config path |
| `-poll-github` | `0` | Poll interval for `conveyor:ready` issues; `0` disables |
| `-worker-retry-delay` / `-worker-retry-max` | `1s` / `4s` | Supervised-child retry backoff |

Service verbs run before flag parsing:

| Command | What it does |
|---|---|
| `conveyord install --config <path> [--env-file <path>]` | Install and start the daemon as a user service. The env file (default `.env` beside the config) must be owner-readable, mode 0600 or stricter. |
| `conveyord uninstall` / `conveyord status` | Remove the service, or report `{installed, state, unit_path, stdout_log, stderr_log}` as JSON. |
| `conveyord version` | Print the release version. |

Startup requires `CONVEYOR_DATABASE_URL`, `CONVEYOR_API_TOKEN`, and
`CONVEYOR_LLM_API_KEY`, applies pending migrations, and creates the
organization and first operator on a fresh database.
