# Durable worker operation

A worker is a client that polls the queue and runs work without you
attached. Before pairing one, finish [Client setup](client-setup.md) and
confirm on this machine:

- The server and workspace are selected and `conveyor auth status` succeeds.
- The execution config has a verified `repos[].checkout` mapping, and you
  pass its absolute path with `--config` when installing the service.
- Git credentials and the agent CLI login work in the service account's
  environment, not only in your interactive shell. Service launches do not
  inherit shell exports.

Installing a worker does not install `conveyord`.

For headless Git credentials, supply `CONVEYOR_GIT_TOKEN` through the startup
environment of `conveyor run` or `conveyor worker run`. Conveyor passes it to
child processes through askpass only; it is never saved and is not accepted
as a command-line argument. Arrange credentials for the initial clone
separately. Before claiming, Conveyor runs a bounded
`git ls-remote --heads <repository URL> <base branch>` with terminal prompting
disabled; a failure leaves the order queued and names both credential paths.
The result is cached for the invocation, so restart the worker after changing
credentials. Captured output is scrubbed before display and upload. Claims
do not depend on a token stored in account settings; the workspace GitHub
App covers control-plane operations. Pull requests are opened on the
executing machine by `conveyor submit`. Upgrade the worker binary and the
daemon together.

`conveyor worker run` reuses the owner-only enrollment credential saved by its
first successful pairing. Restarting an enrolled worker normally needs no new
pairing token. Pair again only when the credential was revoked, removed, or is
reported invalid.

Every enabled worker also requires the client-local execution setup used by
`conveyor run`. Create it on the worker host before starting or upgrading the
worker:

```sh
conveyor config init-execution
conveyor --workspace demo worker run
```

The worker uses the same client-side resolution chain as `conveyor run`: an
explicit `--config` path, `CONVEYOR_CONFIG`, an existing `./conveyor.yaml`, then
the per-user default under the platform user-config directory. Use an explicit
absolute path for a deliberately non-default service configuration. Worker
startup loads and validates the complete selected file before enrollment,
heartbeat, work listing, or claiming; a missing or invalid file exits with the
exact path and the `config init-execution` remediation command. Harness probes
and child launches come from this local file. Execution fields still returned
by an older server are logged and ignored during the transition, while
assignment, hold, claim, lease, heartbeat, and recovery remain server-owned.

New implicit task checkouts use `~/.conveyor/worktrees` on the worker host.
Set the top-level client-local field `worktree_root` to an absolute path (or a
`~/...` path) to choose a different root for both worker and `conveyor run`
children. An explicit `conveyor checkout --path` still takes precedence.
Changing this setting does not move or remove existing registered worktrees;
Git registration continues to locate and clean those at their original paths.

Install the enrolled worker under the host's user service manager so process
crashes, login, and control-plane restarts do not require operator
intervention:

```sh
# Pair and enroll once if this workspace has no saved credential yet.
conveyor --workspace demo worker pair
conveyor --workspace demo worker run --pairing-token <token> --once

# Install, inspect, and remove the workspace-specific user service.
conveyor --workspace demo worker install
conveyor --workspace demo worker status
conveyor --workspace demo worker uninstall
```

`install` requires an existing saved enrollment and a valid local execution
setup, resolves both the selected configuration path and Conveyor executable
to absolute paths, and writes one stable workspace-specific definition. Pass
`--config /absolute/path/to/conveyor.yaml` when installing from a deliberately
non-default location. It
uses a per-user launchd agent on macOS or a systemd user unit on Linux. The
definition contains only the explicit workspace, control-plane address, and
local paths needed by the existing `worker run` command, including its explicit
`--config` path. It never contains the saved worker credential, an API token,
or a pairing token. Unit, metadata, and log files are created owner-only.

Repeated installation converges on the same unit. Conveyor refuses to
overwrite or remove an unrecognized or different-workspace definition at the
resolved path. `uninstall` is safe to repeat and preserves the enrollment;
use the separate `worker revoke <worker-id>` flow when revocation is intended.

`status` deliberately reports two different facts:

- `local_service` is the installed/running/stopped/failed state reported by
  launchd or systemd;
- `remote_worker` is the control plane's distinct live/stale/revoked state,
  last heartbeat, and harness probes for the saved enrollment.

The JSON output also gives the exact unit and stdout/stderr log paths. Remote
status requires the normal operator API credential; when it is unavailable,
the local result is still returned with a remote error instead of treating
local process state as proof of liveness.

On Linux, installation enables the unit in the systemd user manager's
`default.target`. Conveyor does not create a root service or change the host's
lingering policy. Enable user lingering separately only when the worker must
run without a logged-in user and that matches the machine's security policy.
On macOS, the LaunchAgent starts when the user logs in and restarts the worker
after an unsuccessful exit.

## Manual service-manager troubleshooting

The supported commands above own installation and removal. For diagnosis, use
the exact unit and log paths printed by `worker status`.

On Linux:

```sh
systemctl --user status <unit-name>
systemctl --user show <unit-name> --property=ActiveState,SubState,Result
```

On macOS:

```sh
launchctl print gui/$(id -u)/<launchd-label>
plutil -lint <unit-path>
```

Do not hand-edit a managed definition. If inspection shows an ownership
conflict, move the unrelated file deliberately or select the correct workspace
before running `worker install` again.

## Reboot/login exit demonstration

Phase 5.5 is not complete merely because deterministic unit tests pass. On a
supported host, install the service, log out/in or reboot, and verify that
`worker status` reports a server-visible heartbeat within one liveness lease
without manually starting the process. Then verify an install/uninstall
round-trip and retain the output as the completion evidence.

## Sleep and wake

Reconnect and lease reconciliation are the correctness mechanisms. Conveyor
does not keep the host awake by default. If an operator intentionally wants a
Mac to resist idle sleep for a particular foreground run, this optional wrapper
is available:

```sh
caffeinate -i /absolute/path/to/conveyor worker run
```

`caffeinate` is not a substitute for reconnect logic, lease expiry, stale-child
rejection, or service-manager restart policy. After a long sleep or outage, the
worker stops any child whose authority cannot be proven and reconciles against
the server before accepting further results.

## What operators should expect

Activity snapshots show the latest 4 KiB of redacted event summaries for
supported harnesses (OpenCode, Claude, Codex, and Cursor), with bounded raw
text for other output. Failure details keep the latest 2 KiB of the same
stdout summaries plus raw stderr diagnostics. Both tails discard oldest
content first. Renewal retains the last rendered lines when an event has no
display summary. The transcript remains a separate redacted raw session
capture, and `--raw` console output is unchanged.

- Brief connection refusal, timeout, or retryable server failure produces a
  bounded reconnect delay; the worker stays alive and remains cancellable.
- Revoked or invalid credentials and invalid worker configuration terminate
  with an actionable error instead of retrying forever.
- The dashboard reports the last heartbeat/disconnection context and required
  harness health. It distinguishes work that never started from an interrupted
  attempt that needs explicit recovery.
- **Recover interrupted review round** requeues only interrupted incomplete
  seats in the latest round. Completed verdicts are retained.

## Submit implementation work

After validation and committing in the task worktree, the claimed session runs
`conveyor submit <task-id>`. The session's `CONVEYOR_WORK_ORDER_ID` and
`CONVEYOR_SESSION_ID` identify the live order. The command pushes the exact
commit, fetches the task's pull-request template, opens or reuses the pull
request against the assigned base, and submits the head SHA for review.

Git and the GitHub API use credentials resolved on this machine:
`CONVEYOR_GIT_TOKEN`, including its child-only askpass handoff, or the host's
credential helper through `git credential fill` for `https://github.com`.
SSH access alone does not supply a GitHub API bearer. Credentials are never
command arguments or server payloads, and captured errors redact their exact
and encoded forms. A failure preserves any pushed branch and existing pull
request; retrying reuses them.

The server reads the existing pull request with the workspace's GitHub App,
checks its head SHA and base branch, and records it. It does not open pull
requests. Missing or mismatched pull requests are refused. Review inputs and
governance paths come from GitHub's comparison of the recorded base and head;
missing or malformed file data and comparisons with 300 or more files are
refused because completeness cannot be established. Direct MCP
`submit_for_review` remains available for an already-open pull request and
requires `head_sha` along with the work order and session. After submission
succeeds, report the handoff and exit without polling `await_review`.
