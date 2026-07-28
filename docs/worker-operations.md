# Durable Auto worker operation

`conveyor worker run` reuses the owner-only enrollment credential saved by its
first successful pairing. Restarting an enrolled worker normally needs no new
pairing token. Pair again only when the credential was revoked, removed, or is
reported invalid.

Install the enrolled worker under the host's user service manager so process
crashes, login, and control-plane restarts do not require operator
intervention:

```sh
# Pair and enroll once if this workspace has no saved credential yet.
bin/conveyor --workspace demo worker pair
bin/conveyor --workspace demo worker run --pairing-token <token> --once

# Install, inspect, and remove the workspace-specific user service.
bin/conveyor --workspace demo worker install
bin/conveyor --workspace demo worker status
bin/conveyor --workspace demo worker uninstall
```

`install` requires an existing saved enrollment, resolves the absolute
Conveyor executable, and writes one stable workspace-specific definition. It
uses a per-user launchd agent on macOS or a systemd user unit on Linux. The
definition contains only the explicit workspace, control-plane address, and
local paths needed by the existing `worker run` command. It never contains the
saved worker credential, an API token, or a pairing token. Unit, metadata, and
log files are created owner-only.

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

- Brief connection refusal, timeout, or retryable server failure produces a
  bounded reconnect delay; the worker stays alive and remains cancellable.
- Revoked or invalid credentials and invalid worker configuration terminate
  with an actionable error instead of retrying forever.
- The dashboard reports the last heartbeat/disconnection context and required
  harness health. It distinguishes work that never started from an interrupted
  attempt that needs explicit recovery.
- **Recover interrupted review round** requeues only interrupted incomplete
  seats in the latest round. Completed verdicts are retained.
