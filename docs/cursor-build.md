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

Cursor has no separate effort argument. Leave effort blank on Cursor routes
and review seats, and select the desired effort through the model slug. Cursor
adds a `Co-authored-by: Cursor` trailer to commits by default. Set
`attribution.attributeCommitsToAgent` to `false` in `cli-config.json` to disable
that behavior.

## Global Conveyor registration

Register Conveyor in `~/.cursor/mcp.json` without literal credentials:

```json
{
  "mcpServers": {
    "conveyor": {
      "url": "${env:CONVEYOR_ADDR}",
      "headers": {
        "Authorization": "Bearer ${env:CONVEYOR_API_TOKEN}"
      }
    }
  }
}
```

Conveyor does not use project-level `.cursor/mcp.json` entries because Cursor
requires separate approval for them. The global registration loads under the
child environment without an approval prompt.

## Readiness and installation

Before every model turn, Conveyor runs
`cursor-agent mcp list-tools conveyor` in the child working directory and
environment. Readiness fails closed unless the command exits successfully and
lists Conveyor's claim, renewal, release, implementation-submission, and
review-verdict lifecycle tools. A readiness error means the global registration
is missing, invalid, or cannot complete the handshake. Repair
`~/.cursor/mcp.json` and retry the work order; Conveyor never writes Cursor's
configuration.

Install Cursor CLI so `cursor-agent` is on `PATH`. Use `cursor-agent`, never
the `agent` alias; the official installer replaces `~/.local/bin/agent`, which
other vendors may also claim. Authentication requires either `CURSOR_API_KEY`
in the operator environment or an interactive `cursor-agent login` completed
before starting Conveyor.
