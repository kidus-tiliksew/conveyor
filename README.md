# Retained validation evidence: Conveyor task 261118-79d8d8, pull request 1689

These records come from `scripts/validation_evidence.py run`, as `docs/validation-evidence.md` describes. They are the
local-only read-egress measurement that approved plan item 7 names, taken in the task worktree. r4 and r5 are taken at the
submitted head `f689fed21378c607635cf63381a92482d16052b4`, which merges `origin/main` into
`86ba554b9e5d6e29e96afffab8804bd4c967fd52`. r1 to r3 are the same measurement at `86ba554b`, before that merge. The merge
changed no marker SQL. Each run used `env -i` with only PATH, HOME, LANG, a fresh
empty GOCACHE, and, for SingleStore, the backend URL, under `setpriv --inh-caps=-all --ambient-caps=-all`. The helper owned
each fixture database: it created the database, took the backend snapshots, ran the Make command once, and dropped the
database. The r1 PostgreSQL server was the helper's own managed container, which the helper removed after the run. The r4 PostgreSQL server was a second managed container, also removed. The r2, r3,
and r5 SingleStore server was a disposable local container,
`ghcr.io/singlestore-labs/singlestoredb-dev@sha256:2f181587a74b63a83e7dbd46b02e9dce031b8cacf1f5651bff297b2d8c87df37`
(`--cpus=4 --memory=6g`).

| Run | Purpose | Command | Head | Outcome | Time (UTC) | Owned fixture | Resource cleanup | Complete log |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| r1 | PostgreSQL read-egress measurement, all registered cases including `activity-markers` | `make measure-hot-read-egress-postgres` | `86ba554b9e5d6e29e96afffab8804bd4c967fd52` | success (exit 0) | 2026-11-18T14:39:54Z to 2026-11-18T14:40:58Z | `conveyor_markers_bbcfac2b2b26_test` on postgres: teardown success | success | 55137 bytes, complete, sha256 `63febabdd37d09f9e5bbd6093bf177a6eabf9176ee71e3044794d874e5399500` |
| r2 | SingleStore read-egress measurement, first attempt | `make measure-hot-read-egress-singlestore` | `86ba554b9e5d6e29e96afffab8804bd4c967fd52` | failure (exit 2): the store's schema migration 0020 hit the test's two-minute open deadline on a SingleStore container started minutes earlier; no case ran | 2026-11-18T14:41:16Z to 2026-11-18T14:43:58Z | `conveyor_markers_02b6231e13fa_test` on singlestore: teardown success | success | 574 bytes, complete, sha256 `f0371b0c8cf765e92a6c2e5ac429733705def14f5e4e27abbef04ae49b470522` |
| r3 | SingleStore read-egress measurement, same policy, rerun | `make measure-hot-read-egress-singlestore` | `86ba554b9e5d6e29e96afffab8804bd4c967fd52` | success (exit 0) | 2026-11-18T14:44:09Z to 2026-11-18T14:49:58Z | `conveyor_markers_1eafead397c7_test` on singlestore: teardown success | success | 55686 bytes, complete, sha256 `de176d6a694b455d8d25610ca0fa3b284b37ccaba26dea3191a0af89772bdfe4` |
| r4 | PostgreSQL read-egress measurement at the submitted head | `make measure-hot-read-egress-postgres` | `f689fed21378c607635cf63381a92482d16052b4` | success (exit 0) | 2026-11-18T14:53:50Z to 2026-11-18T14:54:35Z | `conveyor_markers_d25017bec788_test` on postgres: teardown success | success | 55137 bytes, complete, sha256 `b8c6778114da8cb9131712d11ff6320482623719fc25f25536e56da41e8485c9` |
| r5 | SingleStore read-egress measurement at the submitted head | `make measure-hot-read-egress-singlestore` | `f689fed21378c607635cf63381a92482d16052b4` | success (exit 0) | 2026-11-18T14:54:37Z to 2026-11-18T14:59:38Z | `conveyor_markers_a36926c08d47_test` on singlestore: teardown success | success | 55686 bytes, complete, sha256 `c1cf837a0d413e00172d6e9788c147cfeb0804de7e919a5760441637cd1feb6b` |

The `activity-markers` case compares the read at `inline-queries@3062eb520fab76502cd86507fd0342a9dfb57c3c` (baseline) with
the public `ReadActivityInputs` at each run's head (candidate) on 12 tasks, 40 history steps, 2,460 events, 1,024-byte padding,
and 6 tasks with an open review order. Both variants served equal work in every trial:

| Run | Backend | Returned bytes (median) | Rows (median) | Statements (median) |
| --- | --- | --- | --- | --- |
| r1 | PostgreSQL | 3,642,421 to 56,770 | 2,502 to 90 | 4 to 4 |
| r3 | SingleStore | 3,580,299 to 55,931 | 2,514 to 102 | 5 to 5 |
| r4 | PostgreSQL | 3,645,385 to 56,824 | 2,502 to 90 | 4 to 4 |
| r5 | SingleStore | 3,574,371 to 55,823 | 2,514 to 102 | 5 to 5 |

Byte totals differ by a few kilobytes between runs because each run's fixture uses fresh task IDs and timestamps.

## Files for each run

- `rN-manifest.json`: the helper's manifest, with the policy, before and after input and backend snapshots, outcome, exit
  status, log digest, fixture phases, and resource cleanup. Its HMAC key stays on the run machine. The key only detects
  accidental corruption, and the log's sha256 above checks the published log.
- `rN-command.log`: the complete command output, including every `READ_EGRESS_MEASUREMENT` line.
- `rN-policy.json`: the audited input policy (r2, r3, and r5 use the same policy; r1 and r4 use the same policy).
- `rN-fixture-ownership.json` and `rN-fixture-phases.jsonl`: the owned database record and its prepare, snapshot, gate,
  and teardown phases.
- `rN-invocation-inventory.json`: the invocation's owned resources and their removal.

No retained file holds a database URL or password. The helper refuses to reuse backend evidence, so these records are fresh
executions and are not candidates for `check` or `bind`.

Run `./verify.sh` to check `SHA256SUMS`, check each log against the digest its manifest recorded, and print each run's
command, head, outcome, fixture teardown, resource cleanup, test result, and `activity-markers` measurement.
