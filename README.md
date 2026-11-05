# Evidence for task 261105-b62258

SingleStore runs for the distinct review seat model visibility fix, at task
branch commit ee9de4aedcc6238a157ab6e85bb9c25fb8b35b40, against the image CI uses
(ghcr.io/singlestore-labs/singlestoredb-dev:latest), through
`make test-integration-singlestore-ci` with the fixture lifecycle.

| File | Variant | Expected | Make exit |
| --- | --- | --- | --- |
| positive-with-fix.log | The fix as committed | Pass | 0 |
| negative-control-1-fix-removed.log | work_orders.go at base c6881d42 (patch alongside) | The forced-gap tests fail: seat 2 claims the repeated name | 2 |
| negative-control-2-row-wait-removed.log | Event wait kept, order row wait removed (patch alongside) | The row-unseen cases fail | 2 |
| conformance-review-distinct-models.log | ReviewDistinctModels conformance cases, with the fix | Pass | 0 |

Each log's first line records the head and command, and its last line records
the make exit. The database password is redacted. To reproduce a control, check
out the commit, `git apply` its patch, and run the command on the first line.
