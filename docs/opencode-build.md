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

For an operator session, bridge the REST server to its MCP endpoint before starting OpenCode:

```sh
export CONVEYOR_ADDR=https://factory.example.com/mcp
export CONVEYOR_API_TOKEN=$(conveyor auth token)
```

## Global Conveyor registration

Run `conveyor mcp install --tool opencode` after `conveyor auth login`.
The installer writes the global `~/.config/opencode/opencode.json`, or
`$XDG_CONFIG_HOME/opencode/opencode.json` when `XDG_CONFIG_HOME` is set to an
absolute path. It creates new files with mode 0600 and preserves other members.

```json
{
  "mcp": {
    "conveyor": {
      "type": "remote",
      "url": "{env:CONVEYOR_ADDR}",
      "headers": {
        "Authorization": "Bearer {env:CONVEYOR_API_TOKEN}"
      },
      "_conveyor_mcp_install": "owner=v1"
    }
  }
}
```

OpenCode substitutes `{env:VAR}` in the URL and header values. Shell-style
`${VAR}` does not work. The ownership marker belongs inside `mcp.conveyor`;
an unknown top-level key fails OpenCode config loading and can break every
OpenCode start on the machine.

The installer prints the missing bridge exports. Set them in the shell that
will launch OpenCode:

```sh
export CONVEYOR_ADDR=https://factory.example.com/mcp
export CONVEYOR_API_TOKEN=$(conveyor auth token)
```

The registration contains no token value and works with the selected server
through these environment references. An existing unmarked `conveyor` entry is
`skipped` unless `--adopt` is passed. Matching owned entries are `unchanged`.
Use `--list` to report without writing. Comment-bearing JSON and symlink paths
are refused with the destination path in the error.

Before publishing a changed file, the installer runs `opencode debug config`
on a staged copy with stdin from `/dev/null` and a 30-second timeout. The check
uses temporary config and data directories and placeholder bridge values.
Debug output is never printed. A nonzero exit, timeout, `Unrecognized key`, or
`ConfigInvalid` error leaves the original bytes intact. If OpenCode is absent,
explicit installation still writes the entry and reports that validation was
skipped. This check validates configuration; launch readiness still checks the
actual server connection.

## Readiness and installation

Before every model turn, Conveyor runs `opencode mcp list` in the child working directory and environment. The command exits zero for connected and failed servers, so readiness requires a line naming `conveyor` as `connected` and rejects one naming it as `failed`. Conveyor then parses `opencode debug config` only in memory and verifies that the effective attachment is remote, points to the child's `CONVEYOR_ADDR`, and carries its bearer identity. Because debug output resolves the token, Conveyor never logs or includes it in diagnostics.

A readiness failure means the global registration is missing, invalid, disconnected, or resolves to a stale identity. Repair `~/.config/opencode/opencode.json`, verify the environment bridge above, and retry. Launch and readiness never create or repair OpenCode configuration.

Install OpenCode so `opencode` is on `PATH`. Effort uses `--variant`; variant names depend on the selected model. The OpenAI and Anthropic families accept `low`, `medium`, and `high`, while `glm-5.2` offers only `high` and `max`.

OpenCode does not add a co-author trailer to commits. It automatically loads personal skills from `~/.claude/skills`.
