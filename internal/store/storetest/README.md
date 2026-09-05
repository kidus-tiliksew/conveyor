# Store conformance

`RunAll(t, Factory)` is the shared entry point for the memory and PostgreSQL
backends. Each `Factory.New` call returns a fresh backend and workspace. The
PostgreSQL factory also isolates deployment identity in a fresh schema through
the existing guarded integration helper. Its database name must end in `_test`.

Memory invokes `RunAll` once in `internal/store/all_conformance_test.go` under
`make test`. PostgreSQL invokes it once in
`internal/store/postgres/all_conformance_integration_test.go` under
`make test-integration`. CI already runs the same PostgreSQL package through
`make test-integration-ci`; no workflow or gate changes are needed.

Factory capability flags describe identity, membership, and token behavior.
Capability suites skip only when their flags are absent. A production-capable
factory must report every capability. Both current factories report all three.

`coverage.go` explicitly declares the methods exercised by each named suite and
its helpers. `RunAll` checks that declarations and registered runners match.
The reflection test checks all `store.Backend` methods, allowing only `IsDurable`
and `Close` as plumbing. Its negative tests add an undeclared method and remove
the declarations for an existing method. Declarations do not establish coverage
of every argument or failure branch; reviewers still assess the assertions.

Assertions use public store APIs. PostgreSQL-specific legacy seeding stays in
the integration fixture. The event-log pack remains in
`internal/eventlog/logtest` and is not invoked by `RunAll`.

## Implementation verification, 2026-09-05

Work order `260905-a67325-implement-1`, workspace `demo`:

| Run | Wall-clock time |
| --- | ---: |
| Memory `RunAll` | 0.535 seconds |
| PostgreSQL `RunAll` | 49.550 seconds |

These are local macOS arm64 measurements, including fixture setup and cleanup,
not performance limits. Set `GOFLAGS=-v` on the corresponding Make target to
include the `RunAll elapsed` log. PostgreSQL was also measured with
`GOFLAGS='-run=TestPostgresConformanceIntegration -v' make test-integration`.

Validation passed: `make fmt-check`, `make build`, `make vet`, `make test`, and
`make test-integration`. The web run passed all 241 Playwright tests. An additional
Make-driven `go build ./...` passed. A signature comparison preserved all 193
original `Store` methods without caller edits. The 37 registered suites declare
coverage for the 253-method `Backend`, with the two plumbing exemptions.

The suite exposed these memory differences from PostgreSQL:

- `SetTaskHold` committed a hold when its audit actor contained a NUL.
  PostgreSQL updates the row, rejects the event text, and rolls the transaction
  back. Memory now validates representable audit text before changing the hold.
  The shared test verifies that failed event insertion leaves both the hold and
  event count unchanged.
- `ListRequirementDeliveryEventsForTasks` returned empty map entries and ignored
  workspace context. PostgreSQL returns only matching event rows in the current
  workspace. Memory now filters task ownership and omits empty entries. Shared
  cases check empty results, a populated batch, and foreign-workspace exclusion.

PostgreSQL implementation, schema, migrations, and the event-log pack are
unchanged. Complete design revisions were proposed through the claimed MCP
session as `component-verification-strategy` v2 and `component-persistence` v4,
using the served confirmed v1 baselines. These proposals remain unconfirmed.
Confirmed DEC-34 and DEC-38 establish PostgreSQL reference precedence over the
older verification design's memory-reference sentence.
