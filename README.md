# Conveyor

An orchestration platform for automated software development — a
"software factory" that runs coding-agent pipelines (triage → spec →
implement → review → verify → merge → monitor) against Git
repositories in disposable containerized sandboxes.

Full design: [conveyor-spec.md](conveyor-spec.md) (v1.1, accepted).

## Status: Phase 2 complete (spec §19)

Phase 1's live GitHub issue → Codex → PR loop remains intact. Phase 2 adds the
validated durable multi-harness and human-gate substrate:

- [x] Postgres projections + append-only events through pgx/sqlc; transactional River enqueue and retry
- [x] Standalone `conveyor-runner` process; workspace-scoped durable queue and recovery
- [x] Claude Code 2.1.206 adapter alongside Codex, including stream-json, resume, budgets, and native permission mapping
- [x] Owner-scoped credential leasing, vendor-policy registry, rate-limit cooldown, and data-driven stage routing
- [x] Exact/common-encoding, credential-pattern, and entropy redaction at the shim boundary; safe redaction counts persisted with authoritative transcripts
- [x] Embedded Vite/React activity view with TanStack Router/Query, stage groups, costed timeline, SSE refresh, reason-coded review actions, and deep links
- [x] Durable GitHub `claiming` state and workspace isolation across tasks, jobs, events, transcripts, and interventions
- [x] Live Claude OAuth sandbox run through SOPS-backed delivery, including a real commit, native session-resumed handoff, redacted transcript, and `awaiting_human` projection

Phase 1 closure evidence remains in [docs/phase1-closure.md](docs/phase1-closure.md).
Phase 2 operation and credential setup are in [docs/phase2.md](docs/phase2.md),
with accepted trade-offs in [docs/known-limitations.md](docs/known-limitations.md).

## Running the loop

```sh
make build && make image                 # UI + four Go binaries + dual-harness image
cp conveyor.example.yaml conveyor.yaml   # configure Postgres, repo, credentials, policy
export CONVEYOR_DATABASE_URL='postgres://conveyor:...@localhost/conveyor?sslmode=disable'
export CONVEYOR_API_TOKEN="$(openssl rand -hex 32)"
bin/conveyord -config conveyor.yaml -poll-github 60s
# In another terminal / host with Docker and harness credentials:
bin/conveyor --config conveyor.yaml runner start --local
bin/conveyor task new "fix the typo in README" --repo api
```

Requires: Postgres, Docker on the runner, at least one configured harness
credential, and `gh`
authenticated for issue claiming and PR opening, and the same
`CONVEYOR_API_TOKEN` in the daemon and CLI environment. SOPS-backed secret
sets additionally require `sops` plus a configured age/KMS/PGP identity; see
[docs/secrets-and-policy.md](docs/secrets-and-policy.md).

## Layout

```
cmd/conveyor/         CLI (cobra) — task new/list/show, checkout, runner start, secrets set
cmd/conveyord/        control-plane daemon — API, GitHub ingestion, embedded activity UI
cmd/conveyor-runner/  standalone LocalDockerRunner River worker
cmd/conveyor-shim/    job shim — in-sandbox supervisor; stdlib-only static binary
cmd/conveyor-resume-experiment/  live Codex resume/cold calibration (§20.2)
internal/core/        shared domain types (tasks, jobs, stages, states)
internal/adapter/     harness adapter interface (§5.1) + codex/
internal/runner/      runner protocol (§3.2) + localdocker/
internal/gitx/        fetch-only bare mirrors + isolated task checkouts (§8, amendment §21.1)
internal/secrets/     secretref:// model + SOPS/plain local backend (§10)
internal/snapshot/    handoff snapshots — the job-to-job continuity contract (§8.3)
internal/store/       event-sourced Postgres store + explicit memory test/dev implementation
internal/httpapi/     REST/SSE activity + human review API and embedded SPA
internal/routing/     owner/policy/rate-limit-aware credential router
internal/redact/      shim-boundary exact, encoded, pattern, and entropy scrubber
internal/trigger/     github/ — issue label → task, branch → PR (§9)
images/base/          base Conveyor image every repo devcontainer extends (§6.1)
```

Deliberately deferred, matching the roadmap: K8sRunner, multi-repo worktree
sets, aggregate cost dashboard, and budget breakers (Phase 3); verification,
computer use, and runner-level domain egress enforcement (Phase 4).

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
