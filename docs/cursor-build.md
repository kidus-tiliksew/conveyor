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

## Global Conveyor registration

Run `conveyor mcp install --tool cursor` to register Conveyor globally in
`~/.cursor/mcp.json` without literal credentials. The command preserves other
servers and manages only its ownership-marked `mcpServers.conveyor` entry:

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

Cursor reads the address and credential from its environment. For operator
sessions, export the MCP endpoint and stored-credential bridge before starting
Cursor:

```sh
export CONVEYOR_ADDR=https://factory.example.com/mcp
export CONVEYOR_API_TOKEN=$(conveyor auth token)
```

Conveyor commands accept the same `CONVEYOR_ADDR` value and remove one trailing
`/mcp` segment when resolving the REST server base. Worker children receive the
matching MCP endpoint from their launcher.

## Readiness and installation

Before every model turn, Conveyor runs
`cursor-agent mcp list-tools conveyor` in the child working directory and
environment. Readiness fails closed unless the command exits successfully and
lists Conveyor's claim, renewal, release, implementation-submission, and
review-verdict lifecycle tools. A readiness error means the global registration
is missing, invalid, or cannot complete the handshake. Run
`conveyor mcp install --tool cursor`, verify the environment bridge above, and
retry the work order. Launch and readiness never create or repair Cursor
configuration; only the explicit install command writes the owned global entry.

Install Cursor CLI so `cursor-agent` is on `PATH`. Use `cursor-agent`, never
the `agent` alias; the official installer replaces `~/.local/bin/agent`, which
other vendors may also claim. Authentication requires either `CURSOR_API_KEY`
in the operator environment or an interactive `cursor-agent login` completed
before starting Conveyor.
