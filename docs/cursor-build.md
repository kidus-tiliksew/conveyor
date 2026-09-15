# Cursor CLI worker setup

Cursor CLI uses Conveyor's `environment` MCP transport because its headless
command has no per-run MCP-config flag. The worker injects the workspace-scoped
Conveyor address and credential into each child process. The durable harness
definition stores only the intended MCP server name.

## Harness definition

```yaml
harnesses:
  - name: cursor
    mcp_transport: environment
    mcp_attachment: conveyor
    command: [cursor-agent, -p, "{prompt}", --output-format, stream-json, --force, --trust, --add-dir, ..]
    resume_command: [--resume, "{session_id}"]
    model_args: [--model, "{model}"]
    probe_command: [cursor-agent, --version]
    probe_timeout: 30s
```

`--output-format stream-json` emits the init event before the model turn
finishes, which keeps Conveyor's first-activity check and native session resume
working. `--force --trust` grants the headless run permission to use tools.
Operator `permissions.deny` rules in `~/.cursor/cli-config.json` still apply.

## Usage

The worker observes Cursor's terminal stream-json `result` event as a
best-effort usage fallback when the agent does not call `report_usage`. It
records the event's non-negative integer `usage.inputTokens` and
`usage.outputTokens` with `worker_fallback` provenance and no estimated cost.
It does not include `cacheReadTokens` or `cacheWriteTokens`, and it ignores
ordinary stream events. An agent report always takes precedence over this
fallback. Cursor selection uses the `cursor-agent` command basename, so an
absolute command path still enables collection without changing the harness
arguments.

Attended runs summarize Cursor stream-json events; use `--raw` to print the original JSONL stream.

Cursor has no separate effort argument. Leave effort blank on Cursor routes
and review seats, and select the desired effort through the model slug. Cursor
adds a `Co-authored-by: Cursor` trailer to commits by default. Set
`attribution.attributeCommitsToAgent` to `false` in `cli-config.json` to disable
that behavior.

## Skills

`conveyor skills install --tool cursor` installs Conveyor's personal skills in
`~/.cursor/skills/<name>/SKILL.md`. Add `--project` to install them in
`<project>/.cursor/skills/<name>/SKILL.md` instead. Conveyor never writes
`~/.cursor/skills-cursor/`, which Cursor reserves for its built-in skills.

## Personal and worker registrations

For a personal client, run:

```sh
conveyor auth login --server https://factory.example.com
conveyor mcp install --server https://factory.example.com --name factory-conveyor --tool cursor
```

The installer writes a named global entry in `~/.cursor/mcp.json` with a literal
endpoint and `${env:CONVEYOR_MCP_TOKEN_<HASH>}` authorization reference. It prints
the exact per-server export using the saved-token bridge. Run that export in the
client's launch environment and restart Cursor. Repeat with another `--server`
to retain both personal connections. See [client setup](client-setup.md#6-connect-agent-sessions)
for naming, migration, rotation, and connection checks.

The worker harness above separately requires a `conveyor` attachment that uses
the child credential supplied by the launcher. Configure that attachment in the
worker account's global config:

```json
{
  "mcpServers": {
    "conveyor": {
      "url": "${env:CONVEYOR_ADDR}",
      "headers": {"Authorization": "Bearer ${env:CONVEYOR_API_TOKEN}"}
    }
  }
}
```

Personal installation does not replace this attachment. It will not migrate a
legacy shared-environment registration without explicit adoption and a matching
endpoint. Worker children receive the endpoint and attempt credential from their
launcher; never substitute the operator's saved personal token in that flow.

## Readiness and installation

Before every model turn, Conveyor runs
`cursor-agent mcp list-tools conveyor` in the child working directory and
environment. Readiness fails closed unless the command exits successfully and
lists Conveyor's claim, renewal, release, implementation-submission, and
review-verdict lifecycle tools. A readiness error means the global registration
is missing, invalid, or cannot complete the handshake. Check the worker attachment and its launcher-provided environment above, then
retry the work order. Personal `mcp install` does not repair worker readiness. Launch and readiness never create or repair Cursor
configuration; worker attachment changes are an explicit local setup act.

Install Cursor CLI so `cursor-agent` is on `PATH`. Use `cursor-agent`, never
the `agent` alias; the official installer replaces `~/.local/bin/agent`, which
other vendors may also claim. Authentication requires either `CURSOR_API_KEY`
in the operator environment or an interactive `cursor-agent login` completed
before starting Conveyor.
