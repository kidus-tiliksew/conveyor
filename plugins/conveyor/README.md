# Conveyor Codex plugin

This repository is the source of truth for Conveyor's Codex plugin. The plugin
connects Codex to the local Conveyor MCP server and provides the
`conveyor-operator` skill for task intake, implementation, and independent
review.

## Install locally

Requirements:

- Conveyor is running at `http://127.0.0.1:8080`.
- `CONVEYOR_API_TOKEN` is exported in the environment that launches Codex.
- The `codex` CLI is available.

From the repository root:

```sh
read -r -s CONVEYOR_API_TOKEN
export CONVEYOR_API_TOKEN
codex plugin marketplace add .
codex plugin add conveyor@conveyor-local
```

The MCP configuration reads the token from `CONVEYOR_API_TOKEN` at runtime. It
contains no token, home-directory path, or machine-specific configuration.

After installation, start a new Codex task so the plugin skill and MCP tools are
loaded. If the desktop app was already open before the environment variable was
set, restart it from an environment that supplies the token.

## Start or resume Conveyor work

In a new Codex task, mention the Conveyor plugin and ask it to create a durable
task, implement a specific task ID, or review the next work order. Task creation
uses Conveyor's normal triage and spec gates; it does not bypass them.

For an existing implementation, keep using the current Codex task while its
claim is live. After a Codex restart, start a new task and ask Conveyor to resume
the task ID. The new session lists work orders and claims the queued or expired
order with fresh session credentials. It never reuses a client token or resets
the assigned branch.

If an unclaimed order has exceeded the configured queue-retention timeout,
`list_work_orders` reports it as non-claimable `stale`. Use
`redispatch_work_order` to reset that queue clock through the audited service
path; do not edit the database. Redispatch does not revive an execution-timed-
out or actively claimed order.

The task's branch field is an assigned canonical name, not proof that a Git ref
already exists. After claiming and reading the work order, the implementation
agent runs `conveyor checkout <task-id>` and uses the returned dedicated
worktree path for every edit, test, commit, and push. The helper safely reuses
existing task history or creates the missing branch from the freshly fetched
base without switching the primary checkout. The same path is reused across
review bounces. Humans use the same helper, and `conveyor done <task-id>` only
removes a clean worktree after the task merges or closes.

## Update

Pull the latest repository changes, then reinstall the plugin from the local
marketplace snapshot:

```sh
git pull --ff-only
codex plugin add conveyor@conveyor-local
```

Start a new Codex task after reinstalling so updated skills and tools are loaded.

## Validate

The repository-owned check validates the marketplace, plugin manifest, MCP
configuration, skill discovery metadata, and secret/path hygiene:

```sh
make plugin-check
```

When Codex's system skill validators are installed, also run the canonical
validators:

```sh
python3 "${CODEX_HOME:-$HOME/.codex}/skills/.system/plugin-creator/scripts/validate_plugin.py" plugins/conveyor
python3 "${CODEX_HOME:-$HOME/.codex}/skills/.system/skill-creator/scripts/quick_validate.py" plugins/conveyor/skills/conveyor-operator
```

Run the repository's full gates before publishing an update:

```sh
make build
make test
make vet
git diff --check
```
