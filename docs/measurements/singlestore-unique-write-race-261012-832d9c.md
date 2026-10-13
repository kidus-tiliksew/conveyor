# SingleStore unique-write race measurements

Task `261012-832d9c`, workspace `demo`, measured 2026-10-12 on GitHub-hosted
runners in the `conveyor/task-261012-832d9c` branch. The original failure was
CI run 38158581655, attempt 1, job 114526551936, head `66215157`.
`TestGoUniqueWriteRulesSerializeIntegration/deployment` reported
`deployment winners=2` there and passed on re-run.

Outcome: **reproduced, classified (b), fixed**. A commit spanning partitions
releases its `conveyor_locks` row before its guarded row on another partition
becomes visible. The next writer acquires the lock, counts zero, and also
commits. The live reference-name rule has the same race.

## Probe workflow

The temporary workflow `.github/workflows/singlestore-race-probe.yml` ran only
on pushes to the task branch. It was deleted in commit `f739e27b`, which stays
in the branch history. Each job ran a `singlestore` service copied from
`ci.yml`, with the same image, credential, port, `--cpus=4 --memory=4g`, and
health options. Isolated jobs built `go test -c` once and ran
`-test.run '^TestGoUniqueWriteRulesSerializeIntegration$' -test.count=10000 -test.v`
in one process with the warm shared pool and the 74-table reset before each
iteration. Package jobs ran `make test-integration-singlestore-ci` six times,
each with its own `CONVEYOR_FIXTURE_STATE`. Every job recorded
`docker image inspect` RepoDigests, `SELECT @@memsql_version`, and the
PASS/FAIL/SKIP/`(cached)` line counts and wall time, and uploaded the raw `-v` log.

Every job in every run reported image
`ghcr.io/singlestore-labs/singlestoredb-dev@sha256:23c0c5fd0381dae403234437f89b0b5454bd78d385d6201359acfb98d109291f`
and SingleStore `9.1.1`. That digest is the one in the failing CI job, and
`:latest` still resolved to it, so the two isolated image arms are the same image.

## Counts

Counts are `--- PASS`/`--- FAIL` lines for each subtest, summed from the
uploaded logs. Each isolated job's deployment PASS+FAIL equals its planned
10,000, with zero `--- SKIP` and zero `(cached)` lines.

| Run (attempt 1) | Head | Mode | Jobs | Deployment iterations | Deployment failures | Reference failures |
| --- | --- | --- | --- | ---: | ---: | ---: |
| 38237387228 | `ddad07fb` | isolated | 8 | 80,000 | 5 | 6 |
| 38237387228 | `ddad07fb` | package | 12 | 72 | 0 | 0 |
| 38242431554 | `f595399f` (instrumented) | isolated | 8 | 80,000 | 10 | 8 |
| 38244635729 | `2349f6d6` (fixed) | isolated | 4 | 40,000 | 0 | 0 |

Pre-fix deployment failures were 15 in 160,000 isolated iterations, about
1 in 10,700. Reference failures were 14 in 160,000. The 72 full package runs
saw none, which matches that rate. The fixed code ran 40,000 iterations of
each subtest with no failure. By the rule of three, the post-fix
per-iteration rate is below 3/40,000 at 95% confidence.

Package jobs report `skip=18`: three lines per run, from
`TestConveyordDurableStartupIntegration/postgres` and its two subtests, which
skip because the job sets no PostgreSQL URL. None is in
`internal/store/singlestore`. The job's zero-SKIP assertion therefore failed
on every package job even though all 72 runs exited 0 with the target passing.

### Jobs

| Run | Job | Name | Deployment pass/fail | Reference pass/fail | Wall s |
| --- | --- | --- | --- | --- | ---: |
| 38237387228 | 114771308574 | isolated failing-digest 1 | 10000/0 | 10000/0 | 286 |
| 38237387228 | 114771308680 | isolated failing-digest 2 | 10000/0 | 10000/0 | 202 |
| 38237387228 | 114771308688 | isolated failing-digest 3 | 10000/0 | 10000/0 | 415 |
| 38237387228 | 114771308411 | isolated failing-digest 4 | 9999/1 | 10000/0 | 201 |
| 38237387228 | 114771308616 | isolated latest 1 | 9998/2 | 9998/2 | 217 |
| 38237387228 | 114771308801 | isolated latest 2 | 10000/0 | 9997/3 | 426 |
| 38237387228 | 114771308619 | isolated latest 3 | 9999/1 | 9999/1 | 292 |
| 38237387228 | 114771308785 | isolated latest 4 | 9999/1 | 10000/0 | 424 |
| 38237387228 | 114771309015 | package 1 | 6/0 | 6/0 | 1377 |
| 38237387228 | 114771309136 | package 2 | 6/0 | 6/0 | 1917 |
| 38237387228 | 114771308723 | package 3 | 6/0 | 6/0 | 1769 |
| 38237387228 | 114771309090 | package 4 | 6/0 | 6/0 | 1658 |
| 38237387228 | 114771308997 | package 5 | 6/0 | 6/0 | 1326 |
| 38237387228 | 114771308788 | package 6 | 6/0 | 6/0 | 2055 |
| 38237387228 | 114771308979 | package 7 | 6/0 | 6/0 | 1676 |
| 38237387228 | 114771308951 | package 8 | 6/0 | 6/0 | 2014 |
| 38237387228 | 114771309160 | package 9 | 6/0 | 6/0 | 1960 |
| 38237387228 | 114771308856 | package 10 | 6/0 | 6/0 | 1706 |
| 38237387228 | 114771309108 | package 11 | 6/0 | 6/0 | 1732 |
| 38237387228 | 114771308999 | package 12 | 6/0 | 6/0 | 1967 |
| 38242431554 | 114788069446 | isolated latest 1 | 9998/2 | 9999/1 | 419 |
| 38242431554 | 114788069244 | isolated latest 2 | 10000/0 | 10000/0 | 438 |
| 38242431554 | 114788069405 | isolated latest 3 | 9999/1 | 9998/2 | 424 |
| 38242431554 | 114788069600 | isolated latest 4 | 9998/2 | 9999/1 | 230 |
| 38242431554 | 114788069490 | isolated latest 5 | 9999/1 | 10000/0 | 294 |
| 38242431554 | 114788069605 | isolated latest 6 | 9998/2 | 10000/0 | 287 |
| 38242431554 | 114788069677 | isolated latest 7 | 9998/2 | 9997/3 | 248 |
| 38242431554 | 114788069466 | isolated latest 8 | 10000/0 | 9999/1 | 423 |
| 38244635729 | 114795359750 | isolated latest 1 | 10000/0 | 10000/0 | 459 |
| 38244635729 | 114795359774 | isolated latest 2 | 10000/0 | 10000/0 | 456 |
| 38244635729 | 114795359815 | isolated latest 3 | 10000/0 | 10000/0 | 251 |
| 38244635729 | 114795359393 | isolated latest 4 | 10000/0 | 10000/0 | 266 |

## Classification

T1 is the attempt whose lock returned first. (a) means both held the lock at
once. (b) means T2 acquired the lock during or after T1's commit and still
counted zero. (c) means another row or test interfered.

**Run 38237387228, deployment, from the #1213 diagnostic lines.** Every failure
listed exactly two `deployment_credential=TRUE` rows, `token-0` and `token-1`,
with no interleaved output, which rules out (c). T2's lock returned inside T1's
lock-to-commit interval, 0.55 to 1.74 ms before T1's commit returned:

| Job | Diagnostic line (`locks_integration_test.go:272`) | T2 lock before T1 commit return |
| --- | --- | ---: |
| 114771308411 | `token-0 lock …55.628971427Z commit …55.630312998Z; token-1 lock …55.627006424Z commit …55.62951812Z` | 0.547 ms |
| 114771308616 | `token-0 lock …22.58496936Z commit …22.588195963Z; token-1 lock …22.587545323Z commit …22.589124132Z` | 0.651 ms |
| 114771308616 | `token-0 lock …23.55547049Z commit …23.557779107Z; token-1 lock …23.552688416Z commit …23.557214105Z` | 1.744 ms |
| 114771308619 | `token-0 lock …30.42225096Z commit …30.429780534Z; token-1 lock …30.428142267Z commit …30.429910894Z` | 1.639 ms |
| 114771308785 | `token-0 lock …15.852963189Z commit …15.855524423Z; token-1 lock …15.846530699Z commit …15.85387167Z` | 0.909 ms |

Each margin falls within the commit round trips measured in the same runs
(1.6 to 6.5 ms, below), so these timestamps fit both (a) and (b). The #1213
block covers only the deployment subtest, so the six reference failures in this
run carry no timestamps. Under the plan's ambiguity rule, run 38242431554 added
temporary test-only instrumentation, since reverted. It logged each attempt's
connection, lock return, count read and its time, commit start (callback
return), and commit return, for both subtests.

**Run 38242431554, all 18 failures (10 deployment, 8 reference): (b).** In
every failure both attempts read count 0. T2's lock returned 0.73 to 3.52 ms
after T1's commit started, never before it. T2's count was read 1.31 to
6.86 ms after T1's commit started, while T1's commit took 1.60 to 6.45 ms
to return. The lock excluded T2 until T1 committed. T1's commit then
released the lock row before T1's guarded row on the other partition became
visible. Local checks against SingleStore 9.1.1 agree: a held lock blocked a
second `lockKey` in 400 of 400 trials, with fresh and pre-existing lock rows.

## Fix and regression evidence

`claimKey` locks the same `conveyor_locks` row and reads its new `holder`
column (migration 0022). `setClaim` writes it, so the claim commits on the
lock row atomically with the release. `writeRow` claims
`one-deployment-credential` or the workspace's `LOWER(name)`. It counts a
claim held by another token or document as a conflict alongside the committed
rows, then records the writer as holder. Credential retirement and reference
soft delete release the claim. Hard deletes of `user_tokens` and
`reference_documents` are refused. The design change is proposed as
`component-persistence` v45 and DEC-67.

`TestUniqueClaimsOutliveUnseenRowsIntegration` commits a winner, deletes its
row so that only the lock row records it (the state T2 saw), and expects the
next writer to be refused. It also checks that retirement and soft delete
release their claims. Local SingleStore 9.1.1:

| Code under test | Command | Result |
| --- | --- | --- |
| Fixed branch | `go test ./internal/store/singlestore -run 'TestUniqueClaimsOutliveUnseenRowsIntegration\|TestGoUniqueWriteRulesSerializeIntegration\|TestUniqueRuleInputsFailBeforeSQL\|TestMigration' -count=1 -v` | all pass |
| `write_rules.go`, `locks.go`, `reference_documents.go` from `origin/main` | same test, `-run TestUniqueClaimsOutliveUnseenRowsIntegration` | `deployment`, `deployment_release`, `reference` fail (second writer accepted) |
| Mutant: drop the deployment claim conflict | same | `deployment`, `deployment_release` fail |
| Mutant: drop deployment claim release | same | `deployment_release` fails |
| Mutant: drop the reference claim conflict | same | `reference` fails |
| Mutant: drop reference claim release | same | `reference_release` fails |

The #1213 diagnostics in `TestGoUniqueWriteRulesSerializeIntegration` are
unchanged.
