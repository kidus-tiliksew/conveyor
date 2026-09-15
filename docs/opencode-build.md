# OpenCode worker setup

OpenCode uses Conveyor's `environment` MCP transport because its headless command has no per-run MCP-config flag. The launcher injects the workspace-scoped Conveyor address and credentials into each child process; the durable harness definition stores only the intended MCP attachment name.

## Harness definition

```yaml
harnesses:
  - name: opencode
    mcp_transport: environment
    mcp_attachment: conveyor
    command: [opencode, run, "{prompt}", --format, json, --dangerously-skip-permissions]
    resume_command: [--session, "{session_id}"]
    model_args: [--model, "{model}"]
    effort_args:
      low: [--variant, low]
      medium: [--variant, medium]
      high: [--variant, high]
    probe_command: [opencode, --version]
    probe_timeout: 30s
```

On 2026-09-08, the implementation environment reported OpenCode 1.17.11 through `opencode --version`. Its `opencode run --help` accepts `--dangerously-skip-permissions`, described as auto-approving permissions that are not explicitly denied. The published documentation also uses the name `--auto`; Conveyor pins the spelling accepted by the probed binary. Operator `permission` deny rules in `~/.config/opencode/opencode.json` still apply.

`--format json` emits JSONL beginning with `step_start`, so stdout flows before the model turn finishes. Every event carries the top-level `sessionID`; Conveyor captures it for `--session` resume. OpenCode reads non-TTY standard input to EOF as a message, so a manually launched worker child must redirect stdin from `/dev/null`.

## Usage

The worker sums the non-negative integer `part.tokens.input` and
`part.tokens.output` counts from OpenCode's `step_finish` events across the
session. It records these totals as a best-effort `worker_fallback` when the
agent does not call `report_usage`, with no estimated cost. Reasoning and cache
counts are excluded. Malformed events and missing or negative counts are
ignored. An agent report always takes precedence over the fallback.

Selection uses the `opencode` command basename, so an absolute command path
still enables collection without changing the harness arguments.

Attended runs summarize OpenCode's JSON events; use `--raw` to print the
original JSONL stream.

OpenCode ends a run cleanly whenever a step finishes without a tool call,
including when the provider stream ends without a result. That step carries
`reason: "unknown"` and zero tokens, and the process exits 0 with nothing
submitted. The worker records the last `step_finish` reason and any `error`
event; when the child exits without a submission and the last reason was
neither `stop` nor `tool-calls`, or an error event appeared, the
`child_failed` reason ends with that diagnosis, for example `harness exited
before completing work order: OpenCode's last step ended with reason
"unknown" after 0 output tokens; the provider stream ended before the agent
finished`. Attended runs print the same abnormal step end as a warning line.
The attempt is retried by the ordinary release path.

## Environment-backed registration

Configure the global `~/.config/opencode/opencode.json` attachment with environment references, not literal credentials:

```json
{
  "mcp": {
    "conveyor": {
      "type": "remote",
      "url": "{env:CONVEYOR_ADDR}",
      "headers": {
        "Authorization": "Bearer {env:CONVEYOR_API_TOKEN}"
      }
    }
  }
}
```

OpenCode substitutes `{env:VAR}` in both fields; shell-style `${VAR}` is not substituted. An unknown top-level key in `opencode.json` makes every OpenCode start fail.

This environment attachment belongs to worker/run children. Personal MCP
installation uses a different registration and credential source.

## Global personal registration

```sh
conveyor auth login --server https://factory.example.com
conveyor mcp install --server https://factory.example.com --name factory-conveyor --tool opencode
```

The installer writes the global `$XDG_CONFIG_HOME/opencode/opencode.json`,
defaulting to `~/.config/opencode/opencode.json`. The selected name identifies a
`type: remote` entry with a literal endpoint, `oauth: false`, and
`Bearer {env:CONVEYOR_MCP_TOKEN_<HASH>}`. `<HASH>` is the full uppercase SHA-256
of the canonical base URL. Run the exact export printed by the installer to read
that server's saved credential, then launch or restart OpenCode from that
environment. A second server receives a separate name and token variable.

The ownership marker stays inside each entry because OpenCode rejects unknown
top-level keys. Files remain owner-only. Reinstall preserves other servers and
nested configuration. Adoption requires a matching endpoint; a shared worker
attachment additionally requires explicit `--adopt` and an unambiguous
`CONVEYOR_ADDR` binding. `--list` is read-only. Comments and unsafe symlink paths
are refused before replacement.

Changed configuration is checked with `opencode debug config` on an isolated
copy in the same directory, using placeholder environment references and a
30-second timeout. Output is withheld, and the client-modified copy is never
published. Parser failure preserves the original file. An explicitly selected
absent binary reports skipped parser validation. Parser acceptance does not
prove native initialization or tools-list success.

After rotating a credential, refresh its printed export and restart OpenCode.
See [client setup](client-setup.md#6-connect-agent-sessions) for complete naming,
migration, two-server installation, and native connection troubleshooting.

## Readiness and installation

Before every model turn, Conveyor runs `opencode mcp list` in the child working directory and environment. The command exits zero for connected and failed servers, so readiness requires a line naming `conveyor` as `connected` and rejects one naming it as `failed`. Conveyor then parses `opencode debug config` only in memory and verifies that the effective attachment is remote, points to the child's `CONVEYOR_ADDR`, and carries its bearer identity. Because debug output resolves the token, Conveyor never logs or includes it in diagnostics.

A readiness failure means the global registration is missing, invalid, disconnected, or resolves to a stale identity. Repair `~/.config/opencode/opencode.json`, verify the environment bridge above, and retry. Launch and readiness never create or repair OpenCode configuration.

Install OpenCode so `opencode` is on `PATH`. Effort uses `--variant`; variant names depend on the selected model. The OpenAI and Anthropic families accept `low`, `medium`, and `high`, while `glm-5.2` offers only `high` and `max`.

OpenCode does not add a co-author trailer to commits. It automatically loads personal skills from `~/.claude/skills`.
