# Retained validation evidence: Conveyor task 261118-8c5bb4, pull request 1695

These records come from `scripts/validation_evidence.py run`, as `docs/validation-evidence.md` describes. They are the
local-only read-egress measurement that the task body (item 3) and the approved plan name for the requirement list page.
The registered read-egress cases run only under the local measurement Make targets, not in any `ci.yml` job. r3 and r4
are taken in the task worktree at the submitted head `c8498da42941cc7a6673017b5437a55ac8d63fd9`, which already contains
`origin/main` at `34e959bd02afc71f096cc0f8c973fbaa9cb74c0f`. r1 and r2 are kept as records of the earlier head
`4fa4a0cd30f590fc40b8df3ed17d99f897c65b76`. The later commit changes only conformance and route tests, not the measured
SQL, and the medians are the same.

Each run used `env -i` with only PATH, HOME, LANG, a fresh empty GOCACHE, and, for SingleStore, the backend URL, under
`setpriv --inh-caps=-all --ambient-caps=-all`. The helper owned each fixture database: it created the database, took the
backend snapshots, ran the Make command once, and dropped the database. The r1 and r3 PostgreSQL servers were the helper's own
managed containers, which the helper removed after each run. The r2 and r4 SingleStore server was a disposable local container,
`ghcr.io/singlestore-labs/singlestoredb-dev@sha256:2f181587a74b63a83e7dbd46b02e9dce031b8cacf1f5651bff297b2d8c87df37`
(`--cpus=4 --memory=6g`). The policies are the audited policies of task 261118-79d8d8 with this task's ID and the
database prefix `conveyor_reqlist`. The HMAC keys stay in local task state and are not published.

| Run | Purpose | Command | Head | Outcome | Time (UTC) | Owned fixture | Resource cleanup | Complete log |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| r1 | PostgreSQL read-egress measurement, all registered cases including `requirement-list-page` | `make measure-hot-read-egress-postgres` | `4fa4a0cd30f590fc40b8df3ed17d99f897c65b76` | success (exit 0) | 2026-11-18T18:59:40Z to 2026-11-18T19:00:48Z | `conveyor_reqlist_27ae1e8bb1f9_test` on postgres: teardown success | success | 63213 bytes, complete |
| r2 | SingleStore read-egress measurement, all registered cases including `requirement-list-page` | `make measure-hot-read-egress-singlestore` | `4fa4a0cd30f590fc40b8df3ed17d99f897c65b76` | success (exit 0) | 2026-11-18T19:00:57Z to 2026-11-18T19:03:11Z | `conveyor_reqlist_95758809146a_test` on singlestore: teardown success | success | 63697 bytes, complete |
| r3 | PostgreSQL read-egress measurement at the submitted head | `make measure-hot-read-egress-postgres` | `c8498da42941cc7a6673017b5437a55ac8d63fd9` | success (exit 0) | 2026-11-18T19:22:16Z to 2026-11-18T19:22:56Z | `conveyor_reqlist_1203ff54c9d6_test` on postgres: teardown success | success | 63213 bytes, complete |
| r4 | SingleStore read-egress measurement at the submitted head | `make measure-hot-read-egress-singlestore` | `c8498da42941cc7a6673017b5437a55ac8d63fd9` | success (exit 0) | 2026-11-18T19:22:57Z to 2026-11-18T19:24:46Z | `conveyor_reqlist_6747de435009_test` on singlestore: teardown success | success | 63697 bytes, complete |

The `requirement-list-page` case seeds its own workspace with 6 requirement roots, 4 shared blueprints, 14 delivery tasks
whose merge, round, and context events carry 16,384 bytes of unrelated payload, and 300 noise events on tasks the page
never reaches. The baseline runs the page's reads with the whole-payload delivery-event statement and the walk as it was
at `34e959bd` (`inline-queries@34e959bd02afc71f096cc0f8c973fbaa9cb74c0f`). The candidate runs the store methods at the
head. Both variants served equal work in every trial (one digest across all ten trials). Medians, baseline to candidate:

| Run | Backend | Returned bytes | Rows | Statements | Delivery-event read bytes |
| --- | --- | --- | --- | --- | --- |
| r1 | PostgreSQL | 1,049,296 to 32,194 | 118 to 118 | 8 to 8 | 1,028,631 to 11,529 |
| r2 | SingleStore | 1,048,667 to 32,458 | 118 to 118 | 8 to 8 | 1,027,033 to 10,824 |
| r3 | PostgreSQL | 1,049,296 to 32,194 | 118 to 118 | 8 to 8 | 1,028,631 to 11,529 |
| r4 | SingleStore | 1,048,667 to 32,458 | 118 to 118 | 8 to 8 | 1,027,033 to 10,824 |

The same runs also measure the read-egress cases this task moved off the removed aggregate methods. r1 and r3, and r2 and r4, report the same medians:

| Case | PostgreSQL bytes, baseline to candidate | SingleStore bytes, baseline to candidate |
| --- | --- | --- |
| `system-design-events-by-document` | 9,951,032 to 1,468 | 9,942,973 to 1,998 |
| `requirement-version-summaries` | 8,207,542 to 41,547 | 8,192,134 to 40,380 |
| `system-design-version-summaries` | 3,685,662 to 18,162 | 3,683,326 to 18,989 |

Check the bundle with `sh verify.sh` from a checkout of this branch.
