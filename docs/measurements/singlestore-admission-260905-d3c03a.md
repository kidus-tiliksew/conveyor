# SingleStore admission measurements

Task `260905-d3c03a`, workspace `demo`, measured 2026-09-06 from the dedicated
`conveyor/task-260905-d3c03a` worktree based on `4b8247c4`.

Status: validation is incomplete. `ProductionCapable` remains false and the
backend factory still requires the experimental test option. The complete
SingleStore pack must pass before the admission switch changes. Deployment
and operational documentation in this change is a draft for that admission.

## Environment and commands

The authorized local target is Docker container `ff-infra-singlestore-1`,
exposing `127.0.0.1:25901`. Its image is
`ghcr.io/singlestore-labs/singlestoredb-dev:latest`; SQL reported SingleStore
`9.1.1`. Docker reported a 3,879,731,200-byte memory limit. The root password
was read from the container configuration in process memory and is omitted
from this report, source, and command examples.

Use the approved root credential only in the process environment:

```sh
export CONVEYOR_TEST_SINGLESTORE_URL='root:<password>@tcp(127.0.0.1:25901)/conveyor_admission_test'
make test-integration-singlestore-ci
make test-integration
make test
make vet
make fmt-check
```

`conveyor_admission_test` is the connection database. Store fixtures create
unique `conveyor_<timestamp>_test` databases and drop their own databases at
cleanup. Builds and tests use disk-backed task directories for `GOCACHE`,
`GOTMPDIR`, `TMPDIR`, `PLAYWRIGHT_BROWSERS_PATH`, and `npm_config_cache`, with
`GOFLAGS=-p=2`. The Make SQL targets serialize packages with `-p=1`.

The Mac ran out of disk space during a Playwright browser download. Subsequent
web checks reused the already installed Chromium revision 1228 and FFmpeg
revision 1011 through links in the task browser directory. No unrelated cache
was removed. Docker's socket later became unavailable; neither Docker nor its
SingleStore container was restarted by this session.

## Results so far

| Check | Result | Time |
| --- | --- | --- |
| `make build` with task-local `BIN` | Passed before final admission changes | 66.19 s wall |
| `make test-integration` | Passed; PostgreSQL implementation unchanged | PostgreSQL package 86.533 s, CLI 55.990 s, dispatch 2.637 s |
| `make test-singlestore-unit` | Passed after the SQL compatibility fixes | CLI 48.130 s, daemon 0.525 s |
| `make test` | Go, installer, dashboard freshness, typecheck, Biome and 241 Playwright cases passed | 160.24 s wall |
| SingleStore event-log conformance | Passed against the real local engine | package 0.906 s |
| `make test-web` | Typecheck, Biome and 241 Playwright cases passed | 106.75 s wall, Playwright 1.6 min |
| `make vet` | Passed | 10.82 s wall |
| `make fmt-check` | Passed | 0.73 s wall |
| Full SingleStore store conformance | Incomplete, failures described below | No admission timing claimed |
| Fresh database CLI/daemon/API smoke | Not run while factory remains gated | Pending |

The first complete SingleStore attempt reached the former five-minute package
timeout. A second attempt with a twenty-minute limit ran RunAll for 7m18.739s,
but leaf unavailability and connection resets produced failures. Docker's
container state reported `OOMKilled=true`. That package took 546.020s and the
Make command took 550.07s. Those timings describe a failed run.

A one-partition fixture experiment failed immediately because this engine
edition rejects `PARTITIONS 1`. Fixtures therefore keep two partitions and
set `interpreter_mode=interpret` only on fixture connections. The application
connection default is unchanged. Interpreting avoids compiling query plans
for short-lived schemas; the separate daemon smoke must use the default mode.

The interpreted attempt exposed two local SQL compatibility defects: the
lineage-node existence query used a union inside `EXISTS`, and SingleStore
returned compact planning-message JSON where the shared transcript contract
expects stable separators. Both have local fixes covered by existing shared
cases, but they still require real SQL revalidation. Later in that attempt,
connections failed and Docker's socket disappeared. The failed store package
took 336.634s. No skipped-suite or production-capability claim is made from
these runs.

## Added coverage and smoke procedure

The new shared `PopulatedProjections` suite creates a claimed and released
work order, an assigned task, a GitHub lifecycle row, a blueprint parent and
child, and a consumed durable dispatch. It checks checkpoint candidates,
caller attention and pagination, repeatable publication reconciliation,
exactly-once blueprint closure, and missing-job repair without duplicating an
active job. PostgreSQL passed these cases without implementation changes.
The prior `EmptyProjections` and `WorkspaceControl` cases used only empty
workspaces and could not detect the five populated-workspace placeholders.

The new CLI and daemon integration cases exercise durable initialization,
sign-in link rotation, reopening, bootstrap identity, workspace persistence,
readiness and orderly shutdown. Real SingleStore runs remain pending.

After creating a fresh database on the authorized instance, run:

```sh
export CONVEYOR_DATABASE_URL='root:<password>@tcp(127.0.0.1:25901)/conveyor_admission_smoke'
make build BIN="$CONVEYOR_TASK_CACHE/bin"
make smoke-singlestore BIN="$CONVEYOR_TASK_CACHE/bin" SMOKE_OUTPUT="$CONVEYOR_TASK_CACHE/smoke"
```

`scripts/smoke-singlestore-admission.py` requires a new output directory and
uses the two release binaries. It initializes the deployment, starts the
daemon, creates a workspace and repository through the API, confirms a
requirement, files a held task, confirms revision two, and reads task, requirement, lineage and
activity JSON models. A local deterministic title provider handles LLM
requests; no real model or forge service is called. The JSON results file
contains HTTP statuses and elapsed times. Configuration and daemon logs stay
in the private task directory. This procedure has not yet completed and is
not browser proof.

## Remaining delivery gates

The `SINGLESTORE_ROOT_PASSWORD` repository secret is unset according to the
operator's work-order direction. The existing CI job refuses missing
credentials, rejects an unset integration URL, and injects a deliberate
failing test before running the full target. Setting that secret is a pending
operator action; local evidence may be submitted without a green hosted
SingleStore job once the local admission checks pass.

Complete `component-persistence` and `component-runtime` revisions are still
pending implementation evidence. Agents may propose those revisions, while
confirmation remains an operator action. No proposal confirmation or
production deployment has occurred in this task.

## Environment checkpoint

The final local check still found no Docker socket. Available host disk space
had recovered to about 1.2 GiB after test scratch cleanup, but no permission
to restart Docker Desktop or remove external caches had arrived. The attempt
therefore preserves the uncommitted task worktree and leaves admission gated.
The remaining work is real SQL revalidation, factory admission and its tests,
adding CLI/daemon packages to the SingleStore target, the default-mode smoke,
final evidence and design proposals, then commit, push and submission.
