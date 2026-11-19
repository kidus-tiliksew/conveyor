# Retained validation evidence: Conveyor task 261119-6014c7, pull request 1700

These records come from `scripts/validation_evidence.py run`, as `docs/validation-evidence.md` describes. They are the
local-only runs that the task's Exit and approved plan v1 name:

- the new `ActivityMarkerReviewRounds` conformance case, run alone, at `4bf31f6bb8aac3c83b7e7c7978c9996f8147b68a`. That commit
  is `origin/main` `770f8e3be1ce35074ab6c8b1f2878a2d0bd8f661` plus the test only, so it shows the case failing on the
  unfixed marker reads (r1 PostgreSQL, r3 SingleStore);
- the activity-marker read-egress measurement before the fix (r4, r5, at `4bf31f6b`, whose store code equals `770f8e3be`)
  and after it (r6, r7, at `eea3db0e2d419e51c5e3f6401bc7336f5e1f06f9`, the pull request head when measured).

Each run used `env -i` with only PATH, HOME, LANG, a fresh empty GOCACHE, and, for SingleStore, the backend URL, under
`setpriv --inh-caps=-all --ambient-caps=-all`. The helper owned each fixture database: it created the database, took the
backend snapshots, ran the Make command once, and dropped the database. The PostgreSQL servers (r1, r4, r6) were the helper's
own managed containers, removed after each run. The SingleStore server (r2, r3, r5, r7) was a disposable local container of
`ghcr.io/singlestore-labs/singlestoredb-dev@sha256:2f181587a74b63a83e7dbd46b02e9dce031b8cacf1f5651bff297b2d8c87df37`
(`--cpus=4 --memory=4g`, as in `ci.yml`).

| Run | Purpose | Command | Head | Outcome | Time (UTC) | Owned fixture | Resource cleanup | Complete log |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| r1 | New case alone on PostgreSQL, unfixed reads | `make _test-integration-postgres POSTGRES_INTEGRATION_SHARD=store GO_INTEGRATION_FLAGS=-run=TestPostgresConformanceIntegration/ActivityMarkerReviewRounds` | `4bf31f6bb8aac3c83b7e7c7978c9996f8147b68a` | failure (exit 2), as expected: `AutomaticRetryTimedOutAgain`, `AutomaticRecoveryInterruptedAgain`, and `Page` fail; `SettledRoundControl` and `LiveReviewClaim` pass | 2026-11-19T08:47:28Z to 08:48:30Z | `conveyor_rounds_80b7cb15b4bd_test`: teardown success | success | 15197 bytes, complete, sha256 `b97ca69a7a6aae9d45a72c951c9ae4b22455394e9edf9b519853ad750b009e40` |
| r2 | New case alone on SingleStore, first attempt | `make _test-integration-singlestore SINGLESTORE_INTEGRATION_SHARD=conformance-2 GO_INTEGRATION_FLAGS=-run=TestSingleStoreConformanceIntegration/ActivityMarkerReviewRounds` | `4bf31f6b` (worktree) | fixture-failure: the helper refused a source URL whose database does not end in `_test`; nothing ran | 2026-11-19T08:48:39Z | none created | success | 0 bytes |
| r3 | New case alone on SingleStore, unfixed reads, same policy, rerun | same as r2 | `4bf31f6bb8aac3c83b7e7c7978c9996f8147b68a` | failure (exit 2), as expected: the same three subtests fail and the same two pass | 2026-11-19T08:48:48Z to 08:50:19Z | `conveyor_rounds_83b07ee936c6_test`: teardown success | success | 15730 bytes, complete, sha256 `0b38c32cc5be2572a55dbfd28633f28e9c119f56aef51570a30ac7d0b4523c9b` |
| r4 | PostgreSQL read-egress measurement before the fix | `make measure-hot-read-egress-postgres` | `4bf31f6bb8aac3c83b7e7c7978c9996f8147b68a` | success (exit 0) | 2026-11-19T08:50:43Z to 08:51:34Z | `conveyor_markers_3256f013a163_test`: teardown success | success | 63213 bytes, complete, sha256 `788f2cc202d2cfa1181dffa52ad6810e050fb8ff7c61bc0d7ec9191be4f80688` |
| r5 | SingleStore read-egress measurement before the fix | `make measure-hot-read-egress-singlestore` | `4bf31f6bb8aac3c83b7e7c7978c9996f8147b68a` | success (exit 0) | 2026-11-19T08:51:43Z to 08:53:57Z | `conveyor_markers_4f7747f5da5e_test`: teardown success | success | 63697 bytes, complete, sha256 `56a67d72a75f7a4768c48ea0590515c582f5c239a8a9870dfbb178a6c73b83c7` |
| r6 | PostgreSQL read-egress measurement after the fix | `make measure-hot-read-egress-postgres` | `eea3db0e2d419e51c5e3f6401bc7336f5e1f06f9` | success (exit 0) | 2026-11-19T09:02:48Z to 09:03:50Z | `conveyor_markers_ace0122895c5_test`: teardown success | success | 63213 bytes, complete, sha256 `b5e6a200b9143e5f5966a89e3e96eaef406502586dc3749484b1065d05336bd7` |
| r7 | SingleStore read-egress measurement after the fix | `make measure-hot-read-egress-singlestore` | `eea3db0e2d419e51c5e3f6401bc7336f5e1f06f9` | success (exit 0) | 2026-11-19T09:03:58Z to 09:06:00Z | `conveyor_markers_98cb0e2f1719_test`: teardown success | success | 63697 bytes, complete, sha256 `37cc1a44d34ba0eec23b512cc413f65116443dd2508e4897d93da47d4901364c` |

In r1 and r3 the failing comparison is the one the task body names. For the automatic retry history the durable read
reports `"automatic_retry_exhausted":false` in `review_recovery`, and the folds over the task's complete event list report
`true`. For the automatic in-place recovery history the same holds for `interrupted_review_recovery`.

The `activity-markers` egress case compares the read at `inline-queries@3062eb520fab76502cd86507fd0342a9dfb57c3c` (baseline)
with the public `ReadActivityInputs` at each run's head (candidate) on 12 tasks, 40 history steps, 2,460 events, 1,024-byte
padding, and 6 tasks with an open review order. Both variants served equal work in every trial. The candidate figures are
the before and after figures of this change:

| Run | Backend | Head | Candidate returned bytes (median) | Candidate rows (median) | Candidate statements (median) | Baseline returned bytes (median) |
| --- | --- | --- | --- | --- | --- | --- |
| r4 | PostgreSQL | `4bf31f6b` (before) | 56,824 | 90 | 4 | 3,645,385 |
| r6 | PostgreSQL | `eea3db0e` (after) | 56,824 | 90 | 4 | 3,645,385 |
| r5 | SingleStore | `4bf31f6b` (before) | 55,931 | 102 | 5 | 3,580,299 |
| r7 | SingleStore | `eea3db0e` (after) | 55,931 | 102 | 5 | 3,580,299 |

The egress fixture writes none of the three review-round kinds, so selecting them adds no row to it, and no bound changed.

## Files for each run

- `rN-manifest.json`: the helper's manifest, with the policy, before and after input and backend snapshots, outcome, exit
  status, log digest, fixture phases, and resource cleanup. Its HMAC key stays on the run machine. The key only detects
  accidental corruption, and the log's sha256 above checks the published log.
- `rN-command.log`: the complete command output.
- `rN-policy.json`: the audited input policy, copied from the manifest.
- `rN-fixture-ownership.json` (not for r2, which created no database) and `rN-fixture-phases.jsonl`: the owned database
  record and its prepare, snapshot, gate, and teardown phases.
- `rN-invocation-inventory.json`: the invocation's owned resources and their removal.

No retained file holds a database URL or password. The helper refuses to reuse backend evidence, so these records are fresh
executions and are not candidates for `check` or `bind`.

Run `./verify.sh` to check `SHA256SUMS`, check each log against the digest its manifest recorded, and print each run's
command, head, outcome, fixture teardown, resource cleanup, test results, and `activity-markers` measurement.
