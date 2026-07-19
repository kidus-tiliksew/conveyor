# Grok Build worker setup

Grok Build uses Conveyor's `environment` MCP transport because its headless
command has no per-run MCP-config flag. The worker injects the workspace-scoped
Conveyor credential and address only into each child process. The durable
harness definition stores only the intended MCP server name.

## Harness definition

```yaml
harnesses:
  - name: grok
    mcp_transport: environment
    mcp_attachment: conveyor
    command: [grok, --single, "{prompt}", --permission-mode, bypassPermissions, --no-plan]
    model_args: [--model, "{model}"]
    effort_args:
      low: [--reasoning-effort, low]
      medium: [--reasoning-effort, medium]
      high: [--reasoning-effort, high]
    probe_command: [grok, --version]
    probe_timeout: 30s
```

The command must contain exactly one whole-element `{prompt}` and no
`{mcp_config}`. `mcp_attachment` is a non-secret Grok server identity, not a
URL, header, token, command fragment, or environment template.

## Non-secret Grok registration

Author the registration directly in `~/.grok/config.toml` (or the applicable
project `.grok/config.toml`):

```toml
[mcp_servers.conveyor]
url = "${CONVEYOR_ADDR}"
headers = { "Authorization" = "Bearer ${CONVEYOR_API_TOKEN}" }
```

Do not use `grok mcp add` for this registration. Grok 0.2.103 expands these
variables when it connects, but `mcp add` rejects a variable-based URL while
writing configuration. Never replace either reference with a literal Conveyor
address containing credentials or an API token.

Grok also discovers compatible registrations in `~/.claude.json`, project
`.mcp.json`, and other supported sources. A successful connection from one of
those sources does not satisfy Conveyor readiness. The effective server named
by `mcp_attachment` must come from Grok `config.toml`, retain the exact URL and
authorization references above, and complete `grok mcp doctor <name> --json`
under the child environment before a model turn begins.

## Readiness failures

A launch fails closed when the registration is missing, shadowed, malformed,
ambiguous, literal/token-bearing, points at another server, or cannot complete
the MCP handshake. Repair the operator-owned Grok configuration and restart or
retry the work order; Conveyor never writes or deletes Grok configuration. Do
not copy a Conveyor credential into a configuration file while repairing it.

The ordinary `probe_command` confirms only that the harness binary is
available. It does not prove MCP attachment. Conveyor performs the real
inspection and doctor handshake separately for every claimed implementation
and independent-review launch.
