# Conveyor

An orchestration platform for automated software development — a
"software factory" that runs coding-agent pipelines (triage → spec →
implement → review → verify → merge → monitor) against Git
repositories in disposable containerized sandboxes.

Full design: [conveyor-spec.md](conveyor-spec.md) (v1.0, accepted).

## Status: Phase 1 (spec §19)

Phase 1 proves the core loop — GitHub issue → agent run → PR, logs only:

- [ ] Codex adapter with ChatGPT subscription auth — `internal/adapter/codex`
- [ ] LocalDockerRunner — `internal/runner/localdocker`
- [x] Bare-clone + worktree manager — `internal/gitx`
- [x] Secrets injection model (refs in control plane, values at edge) — `internal/secrets`
- [ ] Base image — `images/base`
- [x] Handoff snapshot schema — `internal/snapshot`
- [ ] Resume-fidelity experiment (spec §20.2)
- [ ] GitHub issue → PR trigger — `internal/trigger/github`
- [ ] Job shim log streaming — `cmd/conveyor-shim`

(Checked = scaffolded with working core; unchecked = stubbed, `TODO(phase1)` markers inline.)

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

Run the control plane: `bin/conveyord -addr :8080`, then
`curl -s localhost:8080/v1/tasks`.
