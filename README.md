# Conveyor

An orchestration platform for automated software development — a
"software factory" that runs coding-agent pipelines (triage → spec →
implement → review → verify → merge → monitor) against Git
repositories in disposable containerized sandboxes.

Full design: [conveyor-spec.md](conveyor-spec.md) (v1.0, accepted).

## Status: Phase 1 (spec §19)

Phase 1 proves the core loop — GitHub issue → agent run → PR, logs only:

- [x] Codex adapter with ChatGPT subscription auth — `internal/adapter/codex`
- [x] LocalDockerRunner (Tier A mounts, log streaming, artifact collection) — `internal/runner/localdocker`
- [x] Bare-clone + worktree manager — `internal/gitx`
- [x] Secrets reference model — `internal/secrets` (runner-side resolution/injection remains in scope)
- [x] Base image — `images/base` (`make image`)
- [x] Handoff snapshots: elicited at job end via session resume, injected on re-dispatch — `internal/snapshot`, shim
- [ ] Resume-fidelity experiment (spec §20.2)
- [x] GitHub issue polling → task, commits → PR — `internal/trigger/github`, `internal/dispatch` (validated live)
- [x] Job shim: harness supervision, event streaming, session capture — `cmd/conveyor-shim`

Remaining gaps are marked `TODO(phase1)` (in-scope) and
`TODO(phase1-followup)` (loop runs without them) in the code. In-scope
items include secret delivery/SOPS, tool-policy mapping, the standalone
local-runner poll loop, and the resume-fidelity experiment.
Accepted trade-offs with failure modes and recovery procedures are in
[docs/known-limitations.md](docs/known-limitations.md).

## Running the loop

```sh
make build && make image          # binaries + conveyor-base:dev
cp conveyor.example.yaml conveyor.yaml   # edit: your repo + github slug
export CONVEYOR_API_TOKEN="$(openssl rand -hex 32)"
bin/conveyord -config conveyor.yaml -poll-github 60s
bin/conveyor task new "fix the typo in README" --repo api
bin/conveyor task list && bin/conveyor task show <id>
```

Requires: Docker running, `codex` logged in on the host (only a staged
copy of `~/.codex/auth.json` is mounted into each sandbox), `gh`
authenticated for issue claiming and PR opening, and the same
`CONVEYOR_API_TOKEN` in the daemon and CLI environment.

## Layout

```
cmd/conveyor/         CLI (cobra) — task new/list/show, checkout, runner start, secrets set
cmd/conveyord/        control-plane daemon — HTTP API (chi), in-memory store for Phase 1
cmd/conveyor-shim/    job shim — in-sandbox supervisor; stdlib-only static binary
internal/core/        shared domain types (tasks, jobs, stages, states)
internal/adapter/     harness adapter interface (§5.1) + codex/
internal/runner/      runner protocol (§3.2) + localdocker/
internal/gitx/        bare mirrors + per-task worktrees, serialized fetches (§8)
internal/secrets/     secretref:// model + local-file backend (§10)
internal/snapshot/    handoff snapshots — the job-to-job continuity contract (§8.3)
internal/store/       task/job store interface; in-memory now, Postgres in Phase 2
internal/httpapi/     REST + SSE surface (§17.3)
internal/trigger/     github/ — issue label → task, branch → PR (§9)
images/base/          base Conveyor image every repo devcontainer extends (§6.1)
```

Deliberately deferred, matching the roadmap: Postgres/pgx/sqlc/River and
the events table (Phase 2), the React dashboard (Phase 2), K8sRunner
(Phase 3), redaction enforcement (Phase 2 — but all logs already flow
through the shim choke point).

## Development

```sh
make build   # bin/conveyor, bin/conveyord
make test
make vet
make shim    # linux static shim binaries for the base image
make image   # build conveyor-base:dev with the shim baked in
```

Run the control plane: `bin/conveyord -addr 127.0.0.1:8080`, then
`curl -s localhost:8080/v1/tasks`.
