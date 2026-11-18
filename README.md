# Retained validation evidence: Conveyor task 261118-79d8d8, pull request 1689

These records come from `scripts/validation_evidence.py run`, as `docs/validation-evidence.md` describes. They are the
local-only read-egress measurement that approved plan item 7 names, taken in the task worktree. r16 and r17 are taken at the
submitted head `869c877c60ae54b20e4d27f7b56abbe128c83775`, which answers CodeRabbit's second review (PostgreSQL's
key-ambiguity guard also requires the lowered key list to be ASCII; SingleStore keeps the caller's `KeepAllClaims` per
chunk). r14 and r15 are at `3c10a248`, which merges `origin/main` (with the SingleStore event-write fix `d3e43ffbc`) into
`cefbaa1b`. Earlier heads of the same review round are kept as records: r1 to r3 at `86ba554b`, r4 and
r5 at `f689fed2` (a merge of `origin/main`), r6 to r8 at `04286be2` (the SingleStore key-list fix), r9 and r10 at `b09ba7b7`
(a merge of `origin/main`), and r11 to r13 at `cefbaa1b` (the answers to the CodeRabbit review: a claim read-back reruns the
page in one statement, and PostgreSQL binds open review orders as arrays). No merge changed the marker SQL. Each run used `env -i` with only PATH, HOME, LANG, a fresh
empty GOCACHE, and, for SingleStore, the backend URL, under `setpriv --inh-caps=-all --ambient-caps=-all`. The helper owned
each fixture database: it created the database, took the backend snapshots, ran the Make command once, and dropped the
database. The r1 PostgreSQL server was the helper's own managed container, which the helper removed after the run. The r4, r6, r9, r11, r14, and r17 PostgreSQL servers were further managed containers,
also removed. The r2, r3, r5, r7, r8, r10, r12, r13, r15, and r16 SingleStore server was a disposable local container,
`ghcr.io/singlestore-labs/singlestoredb-dev@sha256:2f181587a74b63a83e7dbd46b02e9dce031b8cacf1f5651bff297b2d8c87df37`
(`--cpus=4 --memory=6g`).

| Run | Purpose | Command | Head | Outcome | Time (UTC) | Owned fixture | Resource cleanup | Complete log |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| r1 | PostgreSQL read-egress measurement, all registered cases including `activity-markers` | `make measure-hot-read-egress-postgres` | `86ba554b9e5d6e29e96afffab8804bd4c967fd52` | success (exit 0) | 2026-11-18T14:39:54Z to 2026-11-18T14:40:58Z | `conveyor_markers_bbcfac2b2b26_test` on postgres: teardown success | success | 55137 bytes, complete, sha256 `63febabdd37d09f9e5bbd6093bf177a6eabf9176ee71e3044794d874e5399500` |
| r2 | SingleStore read-egress measurement, first attempt | `make measure-hot-read-egress-singlestore` | `86ba554b9e5d6e29e96afffab8804bd4c967fd52` | failure (exit 2): the store's schema migration 0020 hit the test's two-minute open deadline on a SingleStore container started minutes earlier; no case ran | 2026-11-18T14:41:16Z to 2026-11-18T14:43:58Z | `conveyor_markers_02b6231e13fa_test` on singlestore: teardown success | success | 574 bytes, complete, sha256 `f0371b0c8cf765e92a6c2e5ac429733705def14f5e4e27abbef04ae49b470522` |
| r3 | SingleStore read-egress measurement, same policy, rerun | `make measure-hot-read-egress-singlestore` | `86ba554b9e5d6e29e96afffab8804bd4c967fd52` | success (exit 0) | 2026-11-18T14:44:09Z to 2026-11-18T14:49:58Z | `conveyor_markers_1eafead397c7_test` on singlestore: teardown success | success | 55686 bytes, complete, sha256 `de176d6a694b455d8d25610ca0fa3b284b37ccaba26dea3191a0af89772bdfe4` |
| r4 | PostgreSQL read-egress measurement after the main merge | `make measure-hot-read-egress-postgres` | `f689fed21378c607635cf63381a92482d16052b4` | success (exit 0) | 2026-11-18T14:53:50Z to 2026-11-18T14:54:35Z | `conveyor_markers_d25017bec788_test` on postgres: teardown success | success | 55137 bytes, complete, sha256 `b8c6778114da8cb9131712d11ff6320482623719fc25f25536e56da41e8485c9` |
| r5 | SingleStore read-egress measurement after the main merge | `make measure-hot-read-egress-singlestore` | `f689fed21378c607635cf63381a92482d16052b4` | success (exit 0) | 2026-11-18T14:54:37Z to 2026-11-18T14:59:38Z | `conveyor_markers_a36926c08d47_test` on singlestore: teardown success | success | 55686 bytes, complete, sha256 `c1cf837a0d413e00172d6e9788c147cfeb0804de7e919a5760441637cd1feb6b` |
| r6 | PostgreSQL read-egress measurement at `04286be2` | `make measure-hot-read-egress-postgres` | `04286be2b0ef921bf97d39d9c6d5cb86f18240a7` | success (exit 0) | 2026-11-18T15:16:37Z to 2026-11-18T15:17:51Z | `conveyor_markers_ee7c243fd601_test` on postgres: teardown success | success | 55137 bytes, complete, sha256 `e010f4ce238b4ad51d274cd62bc984d0d358539439e86f2dd32735dba66c5888` |
| r7 | SingleStore read-egress measurement at `04286be2`, first attempt | `make measure-hot-read-egress-singlestore` | `04286be2b0ef921bf97d39d9c6d5cb86f18240a7` | failure (exit 2): schema migration 0040 hit the test's two-minute open deadline; no case ran | 2026-11-18T15:17:54Z to 2026-11-18T15:20:38Z | `conveyor_markers_ce7ad9fac2b8_test` on singlestore: teardown success | success | 760 bytes, complete, sha256 `80d64f18e584cc6a16a4eaa510bf18b90608c64be527e07be47192c3ac82341c` |
| r8 | SingleStore read-egress measurement at `04286be2`, same policy, rerun | `make measure-hot-read-egress-singlestore` | `04286be2b0ef921bf97d39d9c6d5cb86f18240a7` | success (exit 0) | 2026-11-18T15:20:51Z to 2026-11-18T15:27:24Z | `conveyor_markers_877ccd2c4a3c_test` on singlestore: teardown success | success | 55686 bytes, complete, sha256 `094c362721a20b2c411f6468b132e5f062afdbef1f21731a5218f4be96d4691c` |
| r9 | PostgreSQL read-egress measurement at `b09ba7b7` | `make measure-hot-read-egress-postgres` | `b09ba7b7c618113be60ffcc8e12d80071fb8edfe` | success (exit 0) | 2026-11-18T15:36:23Z to 2026-11-18T15:37:20Z | `conveyor_markers_35a49c18113d_test` on postgres: teardown success | success | 55137 bytes, complete, sha256 `a73497ed58dd45927008005026e89019c32a25d895af87e1ec6d18e64089377d` |
| r10 | SingleStore read-egress measurement at `b09ba7b7` | `make measure-hot-read-egress-singlestore` | `b09ba7b7c618113be60ffcc8e12d80071fb8edfe` | success (exit 0) | 2026-11-18T15:37:22Z to 2026-11-18T15:41:46Z | `conveyor_markers_bd38039c1672_test` on singlestore: teardown success | success | 55686 bytes, complete, sha256 `aa1d10b6804b060f148f31d825fafa704a56e272965e829cfb16917b515840c9` |
| r11 | PostgreSQL read-egress measurement at `cefbaa1b` | `make measure-hot-read-egress-postgres` | `cefbaa1b4b9ce3c5d515dc3bfa39b655bd5a8d42` | success (exit 0) | 2026-11-18T16:19:34Z to 2026-11-18T16:20:24Z | `conveyor_markers_1da1b3a4c3a0_test` on postgres: teardown success | success | 55137 bytes, complete, sha256 `e48b11563fdfbf286b7b1ce8ab4bfc1cf78d6303196f94edcc6cad64a8f53760` |
| r12 | SingleStore read-egress measurement at `cefbaa1b`, first attempt | `make measure-hot-read-egress-singlestore` | `cefbaa1b4b9ce3c5d515dc3bfa39b655bd5a8d42` | failure (exit 2): schema migration 0043 hit the test's two-minute open deadline; no case ran | 2026-11-18T16:20:26Z to 2026-11-18T16:23:06Z | `conveyor_markers_04e6d3fcec09_test` on singlestore: teardown success | success | 769 bytes, complete, sha256 `68bbbbd687e9092f557477c84954403deb242702ed83b3d1907c2e89fd41c629` |
| r13 | SingleStore read-egress measurement at `cefbaa1b`, same policy, rerun | `make measure-hot-read-egress-singlestore` | `cefbaa1b4b9ce3c5d515dc3bfa39b655bd5a8d42` | success (exit 0) | 2026-11-18T16:23:14Z to 2026-11-18T16:28:06Z | `conveyor_markers_a76b4a5f8277_test` on singlestore: teardown success | success | 55686 bytes, complete, sha256 `3b82fcaa3a2e5977b7b69bb82d62cb28509268d7ce7b0e58c03008557dd5e317` |
| r14 | PostgreSQL read-egress measurement at `3c10a248` | `make measure-hot-read-egress-postgres` | `3c10a248173ec0b5d5470271b58d31d75363fb4b` | success (exit 0) | 2026-11-18T16:49:31Z to 2026-11-18T16:50:20Z | `conveyor_markers_9a6bf0c99216_test` on postgres: teardown success | success | 55137 bytes, complete, sha256 `e683114af23c079db2aed4ed4554fb4331a6887df735cc02c88df972846aa2c1` |
| r15 | SingleStore read-egress measurement at `3c10a248` | `make measure-hot-read-egress-singlestore` | `3c10a248173ec0b5d5470271b58d31d75363fb4b` | success (exit 0) | 2026-11-18T16:50:22Z to 2026-11-18T16:53:22Z | `conveyor_markers_c6d14d18b935_test` on singlestore: teardown success | success | 55596 bytes, complete, sha256 `f54250db11627c11d21ab649580eeca5c01fa71f3e8f22bcc4793ddaae0f9323` |
| r16 | SingleStore read-egress measurement at the submitted head | `make measure-hot-read-egress-singlestore` | `869c877c60ae54b20e4d27f7b56abbe128c83775` | success (exit 0) | 2026-11-18T17:08:46Z to 2026-11-18T17:14:21Z | `conveyor_markers_53fc56b5e15e_test` on singlestore: teardown success | success | 55596 bytes, complete, sha256 `73ec7befea87f01fdec57f25d4a1fa4a2af16ef904304e8190b406c3f21b2768` |
| r17 | PostgreSQL read-egress measurement at the submitted head | `make measure-hot-read-egress-postgres` | `869c877c60ae54b20e4d27f7b56abbe128c83775` | success (exit 0) | 2026-11-18T17:14:30Z to 2026-11-18T17:15:39Z | `conveyor_markers_43852c446358_test` on postgres: teardown success | success | 55137 bytes, complete, sha256 `4f684b1c13532b6668826cc2816f2f1a965d1f754f9a295ab64f36ab38420526` |

The `activity-markers` case compares the read at `inline-queries@3062eb520fab76502cd86507fd0342a9dfb57c3c` (baseline) with
the public `ReadActivityInputs` at each run's head (candidate) on 12 tasks, 40 history steps, 2,460 events, 1,024-byte padding,
and 6 tasks with an open review order. Both variants served equal work in every trial:

| Run | Backend | Returned bytes (median) | Rows (median) | Statements (median) |
| --- | --- | --- | --- | --- |
| r1 | PostgreSQL | 3,642,421 to 56,770 | 2,502 to 90 | 4 to 4 |
| r3 | SingleStore | 3,580,299 to 55,931 | 2,514 to 102 | 5 to 5 |
| r4 | PostgreSQL | 3,645,385 to 56,824 | 2,502 to 90 | 4 to 4 |
| r5 | SingleStore | 3,574,371 to 55,823 | 2,514 to 102 | 5 to 5 |
| r6 | PostgreSQL | 3,645,385 to 56,824 | 2,502 to 90 | 4 to 4 |
| r8 | SingleStore | 3,577,335 to 55,877 | 2,514 to 102 | 5 to 5 |
| r9 | PostgreSQL | 3,642,421 to 56,770 | 2,502 to 90 | 4 to 4 |
| r10 | SingleStore | 3,580,299 to 55,931 | 2,514 to 102 | 5 to 5 |
| r11 | PostgreSQL | 3,645,385 to 56,824 | 2,502 to 90 | 4 to 4 |
| r13 | SingleStore | 3,580,299 to 55,931 | 2,514 to 102 | 5 to 5 |
| r14 | PostgreSQL | 3,645,385 to 56,824 | 2,502 to 90 | 4 to 4 |
| r15 | SingleStore | 3,580,299 to 55,931 | 2,514 to 102 | 5 to 5 |
| r16 | SingleStore | 3,580,299 to 55,931 | 2,514 to 102 | 5 to 5 |
| r17 | PostgreSQL | 3,645,385 to 56,824 | 2,502 to 90 | 4 to 4 |

Byte totals differ by a few kilobytes between runs because each run's fixture uses fresh task IDs and timestamps.

## Files for each run

- `rN-manifest.json`: the helper's manifest, with the policy, before and after input and backend snapshots, outcome, exit
  status, log digest, fixture phases, and resource cleanup. Its HMAC key stays on the run machine. The key only detects
  accidental corruption, and the log's sha256 above checks the published log.
- `rN-command.log`: the complete command output, including every `READ_EGRESS_MEASUREMENT` line.
- `rN-policy.json`: the audited input policy (r2, r3, r5, r7, r8, r10, r12, r13, r15, and r16 use the same policy; r1, r4, r6, r9, r11, r14, and r17 use the same policy).
- `rN-fixture-ownership.json` and `rN-fixture-phases.jsonl`: the owned database record and its prepare, snapshot, gate,
  and teardown phases.
- `rN-invocation-inventory.json`: the invocation's owned resources and their removal.

No retained file holds a database URL or password. The helper refuses to reuse backend evidence, so these records are fresh
executions and are not candidates for `check` or `bind`.

Run `./verify.sh` to check `SHA256SUMS`, check each log against the digest its manifest recorded, and print each run's
command, head, outcome, fixture teardown, resource cleanup, test result, and `activity-markers` measurement.
