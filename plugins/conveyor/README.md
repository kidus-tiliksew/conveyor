# Conveyor Codex plugin

This plugin provides the `conveyor-operator` skill for task intake,
implementation, and independent review. It registers no default MCP server.

## Install locally

Install the plugin and a named native connection:

```sh
codex plugin marketplace add .
codex plugin add conveyor@conveyor-local
conveyor auth login --server https://factory.example.com
conveyor mcp install --server https://factory.example.com --name factory-conveyor --tool codex
```

Codex uses its native header helper to read the selected saved credential from
Conveyor at connection time. The generated configuration includes an absolute
Conveyor executable path and credential-file reference, with no token value.
Restart Codex after installation. Installation and parser acceptance do not
prove a native MCP connection: check the named server's native status and tools.
See [client setup](../../docs/client-setup.md#6-connect-agent-sessions) for
multiple servers, other clients, rotation, migration, and troubleshooting.

If an older plugin registered `conveyor-plugin` at localhost, remove or disable
that plugin connection and reinstall this connection-neutral plugin. The native
installer does not edit plugin caches or treat localhost as your remote server.

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

Start a new Codex task after reinstalling so updated skills are loaded.

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
