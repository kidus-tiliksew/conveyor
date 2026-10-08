# Store conformance

`RunAll(t, Factory)` is the shared conformance entry point for every store
backend: memory, PostgreSQL, and SingleStore. Each `Factory.New` call returns a
fresh backend, workspace context, workspace ID, and configuration. `RunAll`
refuses a fixture whose context names a different workspace. The PostgreSQL
and SingleStore factories run only against an owned test database whose name
ends in `_test`, and each test binary drops its shared schema or database when
it exits.

| Backend | Entry point | Make target |
| --- | --- | --- |
| Memory | `internal/store/all_conformance_test.go` | `make test` |
| PostgreSQL | `internal/store/postgres/all_conformance_integration_test.go` | `make test-integration` (CI: `make test-integration-ci`) |
| SingleStore | `internal/store/singlestore/all_conformance_integration_test.go` | `make test-integration-singlestore-ci` |

Factory capability flags describe identity, membership, and token behavior.
A capability suite skips only when its factory does not report the capability.
A production-capable factory must report every capability; the PostgreSQL and
SingleStore factories are production-capable, and the memory factory reports
every capability without being production-capable. `Factory` has no named-suite
skip list, so every registered suite runs unless one of its capability flags is
absent (DEC-38, DEC-39). PostgreSQL is the reference implementation: a case
that exposes different behavior keeps PostgreSQL unchanged and fixes the
diverging backend.

`coverage.go` explicitly declares the methods exercised by each named suite and
its helpers. `RunAll` checks that declarations and registered runners match.
The reflection test checks all `store.Backend` methods, allowing only `IsDurable`
and `Close` as plumbing. Its negative tests add an undeclared method and remove
the declarations for an existing method. Declarations do not establish coverage
of every argument or failure branch; reviewers still assess the assertions.

Assertions use public store APIs. Backend-specific setup reaches a suite only
through fixture hooks, never through shared assertions on backend tables. The
event-log pack remains in `internal/eventlog/logtest` and is not invoked by
`RunAll`.

`component-verification-strategy` owns the harness, its orchestration, and the
method coverage check. Each domain component owns the conformance cases for its
own store methods.
