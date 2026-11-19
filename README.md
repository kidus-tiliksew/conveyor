# Retained validation evidence: Conveyor task 261119-4e5308, pull request 1706

This record comes from `scripts/validation_evidence.py run`, as `docs/validation-evidence.md` describes. It is the
local-only read-egress measurement that approved plan v2 (step 4) names, taken in the task worktree at the head the pull
request submits for review round 2, `93888124be82fff949b073126f83f022a2e47ce6`. Commit `93888124` changes only the compile
guard test, so the marker SQL measured here is the one review round 1 judged at `cf6d149b`.

The run used `env -i` with only PATH, HOME, LANG, a fresh empty GOCACHE, and the SingleStore URL, under
`setpriv --inh-caps=-all --ambient-caps=-all`. The helper owned the fixture database: it created the database, took the
backend snapshots, ran the Make command once, and dropped the database. The SingleStore server was a disposable local
container that I started for this task,
`ghcr.io/singlestore-labs/singlestoredb-dev@sha256:2f181587a74b63a83e7dbd46b02e9dce031b8cacf1f5651bff297b2d8c87df37`
(`--cpus=4 --memory=6g`, SingleStore 10.9.0). The policy is `r1-policy.json`. The helper's HMAC key file is not published.

| Run | Purpose | Command | Head | Outcome | Owned fixture | Resource cleanup | Complete log |
| --- | --- | --- | --- | --- | --- | --- | --- |
| r1 | SingleStore read-egress measurement, every registered case including `activity-markers` and `activity-markers-v0.72.0` | `make measure-hot-read-egress-singlestore` | `93888124be82fff949b073126f83f022a2e47ce6` | success (exit 0) | `conveyor_markers_4f7fb5dc1dc2_test` on singlestore: teardown success | success | 67254 bytes, complete, sha256 `68a83ddfb0fc16fd3cda8ca019f00be4d3c51ae922e0329a80e2220bfa5254bc` |

Activity-marker results (5 trials per variant, medians; equal markers and horizon on both sides):

| Case | Baseline | Baseline bytes / rows | Fix bytes / rows |
| --- | --- | --- | --- |
| `activity-markers-v0.72.0` | `tag-v0.72.0@db84f10683f6b072e0a6b08e4c374d86d09ebae8` | 3,580,299 / 2,514 | 92,024 / 102 |
| `activity-markers` | `inline-queries@3062eb520fab76502cd86507fd0342a9dfb57c3c` | 3,577,335 / 2,514 | 91,970 / 102 |

Files: `r1-manifest.json` (the helper's manifest, which records the log digest, input fingerprints, backend probes, and
fixture phases), `r1-command.log` (the complete redacted log), `r1-policy.json`, `r1-fixture-ownership.json`,
`r1-fixture-phases.jsonl`, and `r1-invocation-inventory.json`.

To check: `sh ./verify.sh` runs `sha256sum -c SHA256SUMS`, checks the log against its manifest's digest, and prints the
command, head, outcome, fixture teardown, cleanup, and both measurements.
