# Phase 3 operations and validation

> Historical closure record. Its pipeline and validators survive, but the
> sandbox execution mechanism described here was retired by spec §21.4. Use
> the repository README for current Phase 4.7 operation.

**Status: complete.** The automated test suite and the live dogfood run both
exercise triage → spec gate → implement → code-review redirect → implement
→ approval. The live run opened a GitHub PR from the persistent task
worktree without manual git operations.

## What Phase 3 adds

- Durable per-stage jobs and level-aware progression. Each completed gate
  persists its decided `next_stage` once; halted tasks retain a separate
  `recovery_stage` that only an explicit intervention can reactivate. L2 gates
  the exact spec version before implementation; L0/L1 can advance
  automatically; L3 surfaces after triage.
- `pack/roles/*.md` and `pack/policies/*.json` as the reviewable proto-pack.
- Schema-validated `conveyor:triage`, `conveyor:acceptance`,
  `conveyor:decomposition`, and `conveyor:review` blocks. Invalid output retries
  up to `max_bounces`, then requires human attention.
- Versioned Postgres `task_specs` artifacts. Approval attaches to the latest
  version and that exact content is injected into implementation and review.
- The activity detail renders the exact proposed spec above the L2 approval
  controls, so the pre-Phase 4 gate remains reviewable rather than sight-unseen.
- Independent review routing: the durable credential router excludes the
  implementation harness when claiming review capacity.
- Review `changes_requested` verdicts and human/GitHub review comments become
  structured redirect interventions against the persistent task worktree.
- Per-repo sandbox image overrides and per-stage wall-clock timeouts. Phase 3
  historically also shipped a spending circuit breaker; spec §21.6 removed it.

## Configure and run

Build the base and dogfood images:

```sh
make build
make image
make conveyor-dev-image
```

Set the Conveyor repository to `image: conveyor-dev:dev`, configure both Codex
and Claude credentials, and define `triage`, `spec`, `implement`, and `review`
routes. The `review` route needs capacity on a harness other than the one that
implemented the task. This is historical configuration guidance; the current
`conveyor.example.yaml` follows v1.6 and contains timeouts without spending
allocations.

Start the control and execution planes with GitHub polling enabled:

```sh
bin/conveyord -config conveyor.yaml -poll-github 60s
bin/conveyor --config conveyor.yaml runner start --local
```

Create an L2 task directly or label a GitHub issue `conveyor:ready`:

```sh
bin/conveyor task new "document one narrow behavior" \
  --repo conveyor --level L2 -m "Keep the change small and test-backed."
```

When the spec pauses, inspect `GET /v1/tasks/<id>/spec` (or
`GET /v1/tasks/<id>/activity`) and advance it through the CLI:

```sh
bin/conveyor task approve <id> --reason approved
```

The code-review agent either redirects automatically with a structured reason
or opens/reuses the task PR. New human review bodies and inline comments on the
PR are ingested on the GitHub poll interval as `github-review` redirects.

## Validation

Repository checks:

```sh
make build
make test
make vet
```

The live exit run must additionally demonstrate:

1. Triage, spec generation, and exact spec approval.
2. Implementation in `conveyor-dev:dev` running `make build`, `make test`, and
   `make vet` as appropriate to the task.
3. One code-review bounce followed by a corrected implementation.
4. A GitHub PR opened without manual git operations.

## Live exit evidence

Completed on 2026-07-11 with task `260711-5b4bca`, **Clarify local runner
credential staging documentation**:

- Triage completed with Claude Code. The exact third version of the generated
  spec was approved through the CLI after two spec-quality redirects.
- Codex implemented the approved contract in `conveyor-dev:dev` on branch
  `conveyor/task-260711-5b4bca`. After the image was repaired, implementation
  attempt 3 ran `make build`, `make test`, and `make vet` successfully in the
  sandbox without changing the already-correct commit.
- Independent Claude Code review returned `changes_requested`; Conveyor
  recorded `pipeline.bounced` with `review → implement`, bounce count 1, and
  routed the structured feedback into the second implementation attempt.
- The second review approved commit `8b3e247f3a684f17dba0b573d50194acc59bc483`.
  After the environment repair, review attempt 3 independently reran the same
  build, test, and vet checks and approved again. The final human gate was
  approved through the CLI and the task reached `approved`.
- Conveyor opened [GitHub PR #1](https://github.com/kidus-tiliksew/conveyor/pull/1)
  against `main`. It contains one documentation file and one commit; the
  CodeRabbit check completed successfully.

Stage jobs were `triage-1..3`, `spec-1..3`, `implement-1..3`, and
`review-1..3`, all prefixed by the task ID. The live run exposed four defects:
River's default one-minute job timeout, duplicate Claude final-output
collection, a missing `make` package in `conveyor-dev:dev`, and Debian login
shells hiding `/usr/local/go/bin`. Each was fixed during the run. The image now
asserts Go and Make availability through a login shell at build time, and the
successful pipeline completed on the same durable task.

This satisfies the Phase 3 exit criterion in spec §19. Phase 4 remains
inactive until explicitly activated.
