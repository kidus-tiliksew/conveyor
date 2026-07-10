# Conveyor

An orchestration platform for automated software development — a
"software factory" that runs coding-agent pipelines (triage → spec →
implement → review → verify → merge → monitor) against Git
repositories in disposable containerized sandboxes.

Full design: [conveyor-spec.md](conveyor-spec.md) (v1.1, accepted).

## Status: Phase 1 complete (spec §19, amendment §21.1)

Phase 1 proves the core loop — GitHub issue → agent run → PR, logs only:

- [x] Codex adapter with ChatGPT subscription auth — `internal/adapter/codex`
- [x] LocalDockerRunner (Tier A mounts, log streaming, artifact collection) — `internal/runner/localdocker`
- [x] Bare cache + isolated task checkouts at deterministic sandbox paths; shared cache never mounted — `internal/gitx`
- [x] SOPS/local secret management, `local_eligible` enforcement, and job-scoped environment injection — `internal/secrets`, LocalDockerRunner
- [x] Per-repo command policy propagated into job-scoped Codex execpolicy rules — adapter, shim
- [x] Base image — `images/base` (`make image`)
- [x] Handoff snapshots: native-resume elicitation with a fresh-run fallback, injected on re-dispatch — `internal/snapshot`, shim
- [x] Resume-fidelity experiment (spec §20.2) — live Codex 0.142.0 → 0.143.0 matrix using a local host-equivalent restore boundary; calibrated default: snapshot-briefed cold start — `experiments/resume-fidelity`
- [x] GitHub issue polling → task, commits → PR — `internal/trigger/github`, `internal/dispatch` (validated live)
- [x] Job shim: harness supervision, event streaming, session capture — `cmd/conveyor-shim`
- [x] Phase 1 CLI: checkout/done/redispatch, secrets set, combined local runner start — `cmd/conveyor`
- [x] Phase 1 closure canary: restrictive policy + SOPS-injected non-production secret through live issue → PR — `docs/phase1-closure.md`

All Phase 1 blocker markers are resolved. Follow-up markers identify accepted,
non-blocking limitations: job-log SSE and domain-level container egress
enforcement. The v1.1 amendment (§21.1) keeps the volatile Phase 1 control
plane and local runner co-process; the durable standalone runner claim/lease
boundary lands with Postgres + River in Phase 2.
Accepted trade-offs with failure modes and recovery procedures are in
[docs/known-limitations.md](docs/known-limitations.md).

## Running the loop

```sh
make build && make image                 # binaries + conveyor-base:dev
cp conveyor.example.yaml conveyor.yaml   # edit: your repo + github slug
export CONVEYOR_API_TOKEN="$(openssl rand -hex 32)"
bin/conveyor --config conveyor.yaml runner start --local --poll-github 60s
bin/conveyor task new "fix the typo in README" --repo api
bin/conveyor task list && bin/conveyor task show <id>
```

Requires: Docker running, `codex` logged in on the host (only a staged
copy of `~/.codex/auth.json` is mounted into each sandbox), `gh`
authenticated for issue claiming and PR opening, and the same
`CONVEYOR_API_TOKEN` in the daemon and CLI environment. SOPS-backed secret
sets additionally require `sops` plus a configured age/KMS/PGP identity; see
[docs/secrets-and-policy.md](docs/secrets-and-policy.md).

## Layout

```
cmd/conveyor/         CLI (cobra) — task new/list/show, checkout, runner start, secrets set
cmd/conveyord/        control-plane daemon — HTTP API (chi), in-memory store for Phase 1
cmd/conveyor-shim/    job shim — in-sandbox supervisor; stdlib-only static binary
cmd/conveyor-resume-experiment/  live Codex resume/cold calibration (§20.2)
internal/core/        shared domain types (tasks, jobs, stages, states)
internal/adapter/     harness adapter interface (§5.1) + codex/
internal/runner/      runner protocol (§3.2) + localdocker/
internal/gitx/        fetch-only bare mirrors + isolated task checkouts (§8, amendment §21.1)
internal/secrets/     secretref:// model + SOPS/plain local backend (§10)
internal/snapshot/    handoff snapshots — the job-to-job continuity contract (§8.3)
internal/store/       task/job store interface; in-memory now, Postgres in Phase 2
internal/httpapi/     Phase 1 REST surface; job-log SSE is a follow-up (§17.3)
internal/trigger/     github/ — issue label → task, branch → PR (§9)
images/base/          base Conveyor image every repo devcontainer extends (§6.1)
```

Deliberately deferred, matching the roadmap: Postgres/pgx/sqlc/River and
the events table (Phase 2), the React dashboard (Phase 2), K8sRunner
(Phase 3), redaction enforcement (Phase 2 — but all logs already flow
through the shim choke point), and domain-level container egress enforcement.

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
