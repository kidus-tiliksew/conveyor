# Retained validation evidence: Conveyor task 261115-661c39, pull request 1662

These records come from `scripts/validation_evidence.py run`, as `docs/validation-evidence.md` describes.
The helper ran in the task worktree at head `6d942ad98879ccf2e40b50c4510d673114272978`.
Each run used `env -i` with only PATH, HOME, LANG, a fresh empty GOCACHE, and the backend URL, under
`setpriv --inh-caps=-all --ambient-caps=-all`. The helper owned each fixture database: it created the database,
took the backend snapshots, ran the Make command once, and dropped the database. The SingleStore server was a
disposable local container, `ghcr.io/singlestore-labs/singlestoredb-dev@sha256:2f181587a74b63a83e7dbd46b02e9dce031b8cacf1f5651bff297b2d8c87df37`
(SingleStore 10.9.0, `--cpus=4 --memory=6g`). The r4 PostgreSQL server was the helper's own managed
`postgres:16-alpine` container, which the helper removed after the run.

| Run | Purpose | Command | Head | Outcome | Time (UTC) | Owned fixture | Resource cleanup | Complete log |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| r1 | Results and plan checks, measurement off (done criterion 7) | `make test-document-event-plans-singlestore DOCUMENT_EVENT_MEASUREMENT=0` | `6d942ad98879ccf2e40b50c4510d673114272978` | success (exit 0) | 2026-11-15T16:08:18Z to 2026-11-15T16:09:55Z | `conveyor_docplans_8a1f3d5bad1c_test` on singlestore: prepare success, before and after snapshots success, teardown success | success | 96858 bytes, complete, sha256 `5647b7aaae8495a844ac6ee7adfeada6042ea19e13fc8271c51bc08452789d06` |
| r2 | Measurement: 200,000 unrelated events with 4 KiB payloads (done criterion 6) | `make test-document-event-plans-singlestore DOCUMENT_EVENT_MEASUREMENT=200000` | `6d942ad98879ccf2e40b50c4510d673114272978` | success (exit 0) | 2026-11-15T16:10:08Z to 2026-11-15T16:17:23Z | `conveyor_docplans_1026f3bd0841_test` on singlestore: prepare success, before and after snapshots success, teardown success | success | 234499 bytes, complete, sha256 `f415f847864583dab6fbfda2083cc26a79d37d96156221409d30a330510df654` |
| r3 | Focused document event checks with a SingleStore fixture (done criterion 7) | `make test-document-events` | `6d942ad98879ccf2e40b50c4510d673114272978` | success (exit 0) | 2026-11-15T16:17:31Z to 2026-11-15T16:19:19Z | `conveyor_docplans_d54c3fc05cb0_test` on singlestore: prepare success, before and after snapshots success, teardown success | success | 112366 bytes, complete, sha256 `ff0ec111947c9dc4a5f241e376c24c72fc89f28ee37256707038c3f8ee2619fa` |
| r4 | Focused document event checks with a managed PostgreSQL fixture (done criterion 7) | `make test-document-events` | `6d942ad98879ccf2e40b50c4510d673114272978` | success (exit 0) | 2026-11-15T16:19:23Z to 2026-11-15T16:20:27Z | `conveyor_docplans_7873e03515ff_test` on postgres: prepare success, before and after snapshots success, teardown success | success | 17952 bytes, complete, sha256 `ec4196ec2c6f905755ec4f78b2a1bf87b3c24d34bccf560cd7b5c7827ecba4a0` |

## Files for each run

- `rN-manifest.json`: the helper's manifest, with the policy, before and after input and backend snapshots,
  outcome, exit status, log digest, fixture phases, and resource cleanup. Its HMAC key stays on the run machine.
  The key only detects accidental corruption, and the log's sha256 above checks the published log.
- `rN-command.log`: the complete command output. The helper replaced every non-public inventoried
  environment value, including database URLs, with `[REDACTED]`.
- `rN-policy.json`: the audited input policy.
- `rN-fixture-ownership.json` and `rN-fixture-phases.jsonl`: the owned database record and its
  prepare, snapshot, gate, and teardown phases.
- `rN-invocation-inventory.json`: the invocation's owned resources and their removal.

The helper refuses to reuse backend evidence, so these records are fresh executions. They are not candidates for
`check` or `bind`. CI evidence for the same head is CI run 43732005122, attempt 1.

Run `./verify.sh` to check `SHA256SUMS`, check each log against the digest its manifest recorded, and print each
run's command, head, outcome, fixture teardown, resource cleanup, test results, and measured medians.
