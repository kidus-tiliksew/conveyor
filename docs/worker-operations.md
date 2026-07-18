# Durable Auto worker operation

`conveyor worker run` reuses the owner-only enrollment credential saved by its
first successful pairing. Restarting an enrolled worker normally needs no new
pairing token. Pair again only when the credential was revoked, removed, or is
reported invalid.

Run the worker under the host's normal service manager so process crashes,
logout/login, and control-plane restarts do not require operator intervention.
The service should execute the same foreground command and preserve the user's
normal environment, including `CONVEYOR_ADDR` and `CONVEYOR_WORKSPACE`.

On Linux, a user-level systemd unit can use:

```ini
[Unit]
Description=Conveyor Auto worker
After=network-online.target

[Service]
ExecStart=/absolute/path/to/conveyor worker run
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
```

Install it as `~/.config/systemd/user/conveyor-worker.service`, then run
`systemctl --user daemon-reload`, `systemctl --user enable --now
conveyor-worker`, and inspect it with `journalctl --user -u conveyor-worker`.
Enable user lingering only when the worker must run while the user is logged
out and that matches the machine's security policy.

On macOS, use a per-user launchd agent with `ProgramArguments` containing the
absolute Conveyor binary path followed by `worker` and `run`, plus
`RunAtLoad` and `KeepAlive` set to true. Load it with `launchctl bootstrap
gui/$(id -u) ~/Library/LaunchAgents/<label>.plist` and inspect its configured
standard-output/error log paths. Keep the credential file owner-readable only.

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
