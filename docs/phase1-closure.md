# Phase 1 closure evidence

Phase 1 closed on July 10, 2026 against specification v1.1. The final
validation exercised the previously open paths together, not as isolated
unit fixtures.

## Live issue-to-PR canary

- Scratch repository: `kidus-tiliksew/conveyor-phase1-e2e-20260710`
- Source issue: [#3 Validate Phase 1 secret and policy delivery](https://github.com/kidus-tiliksew/conveyor-phase1-e2e-20260710/issues/3)
- Conveyor task: `260710-c2d1ec`
- Agent commit: `bdb3b268a148f43f610b655cc40da5136bd277a7`
- Pull request: [#4](https://github.com/kidus-tiliksew/conveyor-phase1-e2e-20260710/pull/4)

The task was claimed by moving `conveyor:ready` to
`conveyor:dispatched`. The commit was authored as
`Conveyor <agent@conveyor.local>`, pushed on
`conveyor/task-260710-c2d1ec`, and the PR body preserved task and issue
provenance.

## Secret delivery

The canary was created through `conveyor secrets set --from-stdin` using
real SOPS 3.13.2 and an ephemeral age identity. The encrypted set was mode
0600. LocalDockerRunner decrypted it at boot, supplied it through a
short-lived Docker env file, and removed the staging directory immediately
after container creation.

Inside the agent run, this command succeeded with no output:

```sh
test -n "$CONVEYOR_PHASE1_CANARY"
```

The agent never printed, hashed, or persisted the value. A post-run
exact-value scan confirmed that the decrypted canary did not occur anywhere
under the job control directory, including `events.jsonl`, `job.log`, the
handoff, and Codex session artifacts.

## Tool policy and confinement

The live job generated a job-scoped Codex execpolicy file. Checking the
staged rule inside the running container returned `decision: forbidden` for
`printenv`, with the Conveyor policy rule as the match.

A second no-secret container then asked the real Codex 0.142.0 CLI to run
exactly `printenv` with that generated policy. The command tool rejected
`/bin/bash -lc printenv` before process creation with
`Blocked by Conveyor's per-job tool policy`; Codex reported the rejection
without attempting an alternative command.

Docker inspection showed exactly three mounts:

1. the isolated task checkout at
   `/conveyor/jobs/task-260710-c2d1ec/fixture` (read-write);
2. the job control directory at `/conveyor/control` (read-write);
3. the auth-only credential stage at `/conveyor/creds/codex` (read-only).

The shared bare cache was not mounted. The agent's file-change event also
reported the deterministic task path, proving that the host temp path did
not leak into the sandbox namespace.

## Automated verification

The closure change is covered by tests for:

- real and fake SOPS encryption/decryption, stdout/stderr payload separation,
  and atomic mode-0600 writes;
- secret-ref validation, `local_eligible` rejection, short-lived env staging,
  and absence of values from Docker argv;
- policy propagation and generated Codex `allow`/`forbidden` prefix rules;
- unattended approval and container-confinement flags;
- isolated task checkout persistence, serialized bare-cache mutation, and
  deterministic sandbox paths;
- authenticated redispatch, CLI checkout/done lifecycle, safe human-branch
  fast-forwarding, and reuse of an existing open PR;
- strict configuration parsing, path-safe repository names, and rejection of
  unenforceable Phase 1 network policy;
- the pre-existing dispatcher, handoff, runner, API, GitHub, and resume suites.

Repository-wide commands are `make build`, `make test`, `make vet`, and
`go test -race ./...`. The base image is rebuilt with `make image` before
live validation.

## Accepted Phase 1 limitations

The recovery procedures in [known limitations](known-limitations.md) remain
part of the Phase 1 contract. In particular, secrets are now injected but
automatic transcript redaction begins in Phase 2; production secrets must
not be used during Phase 1. Domain-level container egress policy and job-log
SSE are also explicit follow-ups rather than silently claimed features.
