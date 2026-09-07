# SingleStore admission measurements

Task `260905-d3c03a`, workspace `demo`, measured 2026-09-07 in the dedicated
`conveyor/task-260905-d3c03a` worktree. This attempt preserved predecessor
checkpoint `2a163907` above base `4b8247c4` and independently validated its work.

The backend factory now admits SingleStore without an experimental option.
The shared conformance factory is production-capable, enables identity,
membership and token capabilities, and has no skip list. PostgreSQL remains
the behavioral reference; its implementation and pglog were not changed.
This report records local admission evidence, not a deployment or hosted CI
result. The operator still needs to set `SINGLESTORE_ROOT_PASSWORD` for GitHub CI.

## Environment and reproducible commands

The authorized local SQL target was Docker container `ff-infra-singlestore-1`,
exposing `127.0.0.1:25901`, image
`ghcr.io/singlestore-labs/singlestoredb-dev:latest`. SQL reported SingleStore
`9.1.1`; Docker reported a 3,879,731,200-byte memory limit. The root credential
was read from the container configuration into process memory. It is absent
from source, this report and the command examples below.

```sh
export CONVEYOR_TEST_SINGLESTORE_URL='root:<password>@tcp(127.0.0.1:25901)/conveyor_admission_test'
make test-integration-singlestore-ci
make test-integration
make test
make vet
make fmt-check
make build BIN="$CONVEYOR_TASK_CACHE/bin"
```

Tests used APFS disk-backed task scratch for `GOCACHE`, `GOTMPDIR`, `TMPDIR`,
`PLAYWRIGHT_BROWSERS_PATH` and `npm_config_cache`, with `GOFLAGS=-p=2`.
The SQL Make targets serialize packages with `-p=1`. The final SingleStore
rerun ran alone without concurrent SingleStore smoke or focused SQL tests.
PostgreSQL used the Make-managed disposable PostgreSQL 16 service on port
29990. Its target completed its own teardown.

Store fixtures create unique two-partition databases ending in `_test` and
remove their own databases on cleanup. Their connections use
`interpreter_mode=interpret` to bound compilation memory for short-lived
schemas on the dev image. Production `Open`, CLI/daemon integration, and the
smoke retain the default query mode. A one-partition database is unsupported
by this dev image. Interpretation is a fixture choice, not an application
configuration change.

## Results

| Check | Result | Measurement |
| --- | --- | --- |
| `make test-integration-singlestore-ci` | Passed on final source | 735.17s wall; SingleStore package 660.850s |
| Shared SingleStore RunAll | Production-capable run passed with no skips | 9m13.307721083s in the final serialized target |
| `make test-integration` | Passed, including PostgreSQL RunAll and durable startup/API creation | RunAll 40.297971417s; PostgreSQL package 73.251s |
| `make test` | Passed Go, installer, dashboard freshness, web typecheck, Biome and 241 Playwright cases | 167.13s wall; memory RunAll 500.641708ms |
| `make test-singlestore-unit` | Passed | 59.60s wall |
| `make build` with task-local `BIN` | Passed after the runtime fix | 13.55s wall |
| `make vet` | Passed | 1.75s wall |
| `make fmt-check` | Passed on final source | 0.33s wall |
| Default-mode CLI/daemon/API smoke | Passed | 5.798s inside smoke; 10.99s including database setup and Make |
| Missing SingleStore URL guard | Rejected, as required | Make exit 2 |
| Deliberate SQL regression probe | Failed, as required | Make exit 2; probe used a real fixture before `t.Fatal` |

SingleStore and PostgreSQL timings come from the same local validation
attempt. These are conformance execution times, not throughput benchmarks:
the backends have different fixture and schema setup costs. The complete
SingleStore target includes s2log event-log conformance, the storetest coverage
guard, all SingleStore store tests, CLI initialization/user tests and daemon
tests. Its twenty-minute package timeout and thirty-minute CI job timeout
allow the full pack to execute instead of truncating it at five minutes.
The coverage package also tests its own experimental-skip mechanism; those
synthetic skips are distinct from SingleStore RunAll, which has no skipped
suites. Backend-specific integration cases for the unconfigured PostgreSQL
URL are skipped in the SingleStore command and run in the PostgreSQL target.

## Defects found and coverage added

The former `EmptyProjections` and `WorkspaceControl` cases used empty
workspaces and could not detect five populated-workspace placeholders.
The shared `PopulatedProjections` suite now creates an assigned task,
a released work order, a GitHub lifecycle record, a blueprint parent/child,
and a consumed durable dispatch. It checks caller attention and pagination,
checkpoint candidates, repeatable publication reconciliation, exactly-once
blueprint closure, and missing-job repair without duplicating active jobs.
Both durable backends and the volatile backend execute these cases.

All residual declarations in `unimplemented.go` and `empty_projections.go`
were replaced with implementation. The work includes assignee validation,
submission governance, causal merge consultation, GitHub lifecycle and
review/issue publication behavior. Go-enforced ownership and uniqueness,
row locks, and transaction-bound event-log writes remain in place. Compile-time
Backend assertions cover both durable implementations.

Real SQL execution found an unsupported union inside a lineage `EXISTS`
query and JSON transcript separator differences. Existing shared cases now
pass after those compatibility fixes. Causal merge consultation also used
a task-event helper for a workspace event and rejected an absent workspace
before performing its scoped causal lookup. It now records the consultation
atomically through the workspace-event helper and returns no judgment when
causal history is absent. The existing SystemDesignDrift cases verify
consultation idempotence and cross-workspace isolation.

The first smoke exposed workspace creation returning HTTP 500: queue preflight
loaded configuration for a workspace that had not yet been persisted.
Creation now passes the validated candidate configuration to the callback,
which checks the rescue threshold and registers the runtime. Reconciliation
continues reading persisted configuration. HTTP tests verify preflight sees
the candidate before persistence and that callback failure prevents a write.
Parameterized daemon tests create a workspace through the running API and
reopen it on both durable backends. Existing dispatch tests retain threshold
refusal coverage. This runtime fix is needed for the approved API smoke;
PostgreSQL storage code is unchanged.

SingleStore CLI integration verifies repeatable init, identity/workspace
persistence, and sign-in token rotation. Its public URL fixture matches the
existing PostgreSQL fixture so the common helper can parse the sign-in link.

## Fresh database smoke

A task-local helper created a fresh two-partition database on the authorized
instance, named `conveyor_admission_smoke_1788760759877960000_test`.
The two release binaries were then exercised with:

```sh
export CONVEYOR_DATABASE_URL='root:<password>@tcp(127.0.0.1:25901)/conveyor_admission_smoke_1788760759877960000_test'
make smoke-singlestore BIN="$CONVEYOR_TASK_CACHE/bin" SMOKE_OUTPUT="$CONVEYOR_TASK_CACHE/smoke"
```

`scripts/smoke-singlestore-admission.py` requires a new output directory,
runs `conveyor init`, starts `conveyord`, and uses a local deterministic title
provider. It issues a sign-in link with `conveyor user issue-link` and shuts
down the daemon and provider. No real model or forge service is called.

Init took 1.609s and readiness took 1.209s. The smoke completed thirteen HTTP
requests: workspace creation (201), repository configuration read/write
(200/200), requirement creation and first confirmation (201/200), held task
creation (201), requirement revision and confirmation (201/200), and task,
requirement, lineage, workspace activity and task activity reads (all 200).
The resulting workspace was `admission`, task `260907-ab3f96`, and requirement
`req-admission` confirmed at version 2. Total smoke time was 5.798s.
This is process and API evidence, not browser evidence.

The init configuration contains the database URL and stays in owner-only
local scratch. The JSON smoke results contain statuses and timings without
credentials. The two smoke databases (one failed pre-fix attempt and the
successful attempt) remain on the authorized local instance; no shared
container was restarted or global database setting changed.

## Failed attempts and evidence limits

Earlier complete runs found the SQL defects above. After those fixes, a full
pre-admission target passed (677.17s wall; RunAll 9m16.754458959s). Only then
was normal factory admission enabled and the conformance factory marked
production-capable.

A subsequent production-capable RunAll passed in 9m23.084047958s without
skips, but the leaf connection reset during later backend-specific fixtures.
Docker reported `OOMKilled=true`, and subsequent database creation failed
with unavailable-leaf errors. The engine recovered without intervention.
That attempt also found the CLI public-URL fixture omission and an incorrect
new test expectation about legacy routing fields; both test issues were
corrected. The final serialized rerun passed the entire target in 735.17s, including all
38 SingleStore RunAll suites without skips, backend-specific fixtures and
CLI/daemon tests. The earlier memory failure did not recur.

An overlapping `make vet` and `make test` initially raced with npm replacing
`node_modules`; vet failed while enumerating a disappearing directory.
After web setup completed, vet passed. The earlier PostgreSQL attempt also
rejected the incorrect legacy-routing test expectation; the corrected full
PostgreSQL target passed. Failed runs are not counted as passing evidence. One post-compaction wrapper
invocation used the primary checkout; its result is excluded entirely. The
final command explicitly selected the dedicated task worktree.

## Authority and delivery checklist

This change follows DEC-38 and DEC-39 and was checked against confirmed
`component-persistence` v9, `component-runtime` v3,
`component-verification-strategy` v4 and `component-http-api` v3.
Shared conformance exercises the served lineage contract
`req-260802-72fc68` v2, identity/membership contract
`req-accounts-and-membership` v1, and lifecycle/queue contract
`req-task-lifecycle-and-queue` v6. Durable init, queue startup and the smoke
exercise `req-deployment-and-releases` v1. Existing denial, rollback,
idempotence and workspace-isolation cases remain enabled. No requirement,
operator capability, queue semantics or PostgreSQL implementation was relaxed.

All nine approved-plan done criteria are represented here: residual methods
and assertions; production-capable conformance; normal factory/configuration
selection; CLI/daemon parity; CI refusal/probe/full-target wiring; fresh
smoke and comparison timing; operational documentation; complete design
proposals; and repository validation with failed-attempt causes disclosed.

Complete immutable proposals were submitted through MCP: `component-persistence`
v10 and `component-runtime` v4. Both remain pending operator confirmation.
They document normal SingleStore admission, constraints and startup behavior,
plus the queue-creation preflight change. Confirmation is not a condition of
implementation submission under the work-order direction.

The CI service is configured with the SingleStore image and credential
requirement. The local unset-URL and deliberate-regression checks failed as
intended. According to explicit operator direction, the repository secret
`SINGLESTORE_ROOT_PASSWORD` remains unset. The operator must set it and rerun
the hosted job; this task may be submitted on local evidence. No hosted
SingleStore success or production deployment is claimed.
