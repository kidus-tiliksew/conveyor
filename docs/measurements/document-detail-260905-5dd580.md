# Document detail payload measurements

Work order `260905-5dd580-implement-1`, workspace `demo`, measured on 2026-09-05.

The requirement detail now contains 50 newest-first lineage events, a total,
and an event snapshot id. Serving blueprints contain their task and optional
spec. Their event arrays and the inline lineage graph are absent. System Design
uses the same bounded history contract. Older events load from the document's
`/events` route; both explorers continue to load `/v1/lineage/{type}/{id}` on open.

## PostgreSQL fixture results

| Detail endpoint | Before bytes | After bytes | Before server time | After server time |
| --- | ---: | ---: | ---: | ---: |
| `/v1/requirements/req-delegated-execution` | 2,701,067 | 94,456 | 38.870250 ms | 19.637250 ms |
| `/v1/system-designs/design-260805-973cd4` | 184,276 | 16,400 | 4.107125 ms | 2.739459 ms |

The requirement meets the requested under-200-KB and under-one-second targets.
Its payload shrank 96.5%. System Design also meets both thresholds on this fixture.

These are uncompressed HTTP handler measurements on macOS ARM64 with the
worktree's isolated PostgreSQL 16 Alpine container. Timing surrounds
`handler.ServeHTTP` through an `httptest.ResponseRecorder`, including database
reads and JSON encoding. It excludes network transport and browser rendering.
Each value is one recorded request, not a latency percentile. These results do
not establish performance on the devbox database.

`SeedDocumentEventMeasurement` in `internal/store/storetest/document_events.go`
creates the two named documents with one confirmed version each. Two confirmed
serving blueprints each have a child task and 300 synthetic activity events with
2-KiB messages and identical timestamps. The System Design has 600 consultation
events. Normal creation, confirmation, and context events remain in the fixture.

The baseline used the original requirement and System Design handler files from
`a88c31e702c5ae9b8b2e95abeabb0332fb3cd530`. A Go overlay selected those two files
without changing the task checkout. Both measurements used the same fixture and
an empty test database. Workspace query values were isolated fixture ids:
`phase61-260905-1fa82d` before and `phase61-260905-a96814` after.

## Reproduction

Run `GOFLAGS=-v make test-integration` from the task checkout. The
`TestDocumentDetailMeasurementIntegration` output includes `MEASUREMENT postgres`
lines for both endpoints. `TestDocumentDetailMeasurement` supplies the memory
handler equivalent in the ordinary Go suite.

For the historical baseline, write the two handler files from the base commit
above into the task's disk-backed scratch directory using `git show`. Supply a
Go overlay JSON object with a `Replace` map from each absolute checkout filename
to its scratch copy, then set `GOFLAGS=-overlay=<absolute-overlay-file>`.
The baseline measurement used this temporary Make target alongside the repository
Makefile, so the test database followed the normal isolated lifecycle:

```make
measurement-db: compose-check test-db-up
	@trap '$(MAKE) test-db-down' EXIT; CONVEYOR_TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test ./internal/store/postgres -run TestDocumentDetailMeasurementIntegration -count=1 -v
```

## Contract and verification

The new page shape is `{events,total,limit,offset,snapshot_id}`. Limits are 1 to
200, defaulting to 50. Offset validation and `X-Conveyor-Total`,
`X-Conveyor-Limit`, and `X-Conveyor-Offset` match activity pagination. Supplying
`lineage_snapshot_id` as `snapshot_id` on later pages excludes subsequent event
appends, including backdated events. The snapshot bounds event ids; current
serving-context membership can still change, so refresh detail after a context
change.

The shared store conformance case compares the complete paged result with the
previous full reads on memory and PostgreSQL. It checks timestamp ties, append
isolation, totals, empty pages, bounds, and workspace isolation. HTTP tests check
field absence, the size target, page metadata and continuity, authentication,
and missing or foreign documents. A guard refuses full task or System Design
event reads from detail. Existing staleness tests cover the independent delivery
and acknowledgment projections. Playwright checks the older-page controls,
archive refresh, and graph requests made only when the explorer opens.

Verification follows confirmed `component-verification-strategy` v1 and
`component-document-corpus` v1. The change serves `req-260802-72fc68` v2 AC-5.1
and `req-document-operating-surfaces` v5 REQ-3. Confirm, dismiss, propose,
archive, staleness decisions, and event persistence keep their existing semantics.
There is no schema migration. The requirement list handler is unchanged.

The complete governing design revision was proposed through Conveyor as
`component-document-corpus` v2. It remains pending for the operator and is not
cited as confirmed authority. Submission proceeds without waiting for it.

Final local gates passed on the submitted sources:

- `make test`, including bundle freshness, Go tests, Biome, and all 241 Playwright cases.
- `make test-integration`, including the shared PostgreSQL paging conformance case and existing staleness tests.
- `make vet` and `make fmt-check`.
- `git diff --check`.

CI and deployment are separate from this local evidence; neither was awaited or
performed by the implementation session.
