# Retained local evidence for Conveyor task 261031-66d10d (PR #1475)

This orphan branch is never merged. It publishes the complete retained
bundles that `scripts/validation_evidence.py run` wrote for the task's
local-only gates, byte-identical to the originals under
`/home/user/.local/state/conveyor/261031-66d10d/` on the implementing host.
Every attempt is included, failed ones too.

| Directory | Record | Head | Result |
| --- | --- | --- | --- |
| `built-gate/` | AC-3.2 test `web/tests/board-map.spec.ts:777`, `--repeat-each=20`, `BOARD_MAP_CPU_THROTTLE=4`, built-dashboard project (round 2) | `61f566bb` | outcome `success`, exit 0; 20/20 at or below 1,200 ms (313–497 ms, median 368.5) |
| `validation-docker/` | `make test-validation-docker` (Playwright server lifecycle changed) | `61f566bb` | two `snapshot-failure` attempts before execution (external inputs held symlinks outside the inventoried roots), then `success`, exit 0 |
| `round1-dev-server/` | The same AC-3.2 gate on the Vite dev server (round 1, superseded) | `9f5015fa`, `cf518502` | `9f5015fa`: 20/20 passed the test's 2,000 ms assertion (list reporter, no per-run times), then 848–1,156 ms. `cf518502`: **1 of 20 over 1,200 ms** (max 1,298), then **6 of 20 over** (max 1,359). |

Each attempt directory holds `manifest.json`, `key`, and `command.log`.
`key` is the helper's integrity key for the manifest HMAC: it detects
corruption, is not a signature, and grants no access to anything. Each
directory's `policy-*.json` is the policy file as last used; every manifest
embeds the exact policy of its own attempt. The `validation-docker` attempts
ran with `external_inputs` of `/usr/lib/python3`, then `/usr/lib/python3.12`,
then none.

`ac32-gate-report*.json` are Playwright JSON reports that the gate wrote
through `PLAYWRIGHT_JSON_OUTPUT_NAME` to a file outside the worktree. They
carry each run's `groups, tasks, and edges after read (ms)` annotation in run
order. The manifests do not hash them. The `command.log` lists each run's pass
and duration, and the log redacts the inventoried value `4` of
`BOARD_MAP_CPU_THROTTLE` wherever that digit appears.

The `built-gate` record and two round-1 records show one changed file between
the before and after snapshots: `web/node_modules/.vite/deps/_metadata.json`.
That is Vite's dependency-optimizer cache, which the dev server Playwright
also starts rewrites. The records are therefore fresh executions at their
recorded head with a clean worktree, and are not bound for reuse.

## Verify

```sh
git clone --depth 1 --single-branch --branch evidence/261031-66d10d \
  https://github.com/kidus-tiliksew/conveyor.git "$HOME/evidence-261031-66d10d"
cd "$HOME/evidence-261031-66d10d" && sha256sum -c SHA256SUMS
# From any checkout of the conveyor repository containing 61f566bb. The
# evidence directory must be outside that checkout and outside ~/.cache.
cd <conveyor checkout>
python3 -I "$HOME/evidence-261031-66d10d/verify.py" --repo .
for d in "$HOME"/evidence-261031-66d10d/*/attempt-*; do
  python3 scripts/validation_evidence.py inspect --output "$d"
done
```

`verify.py` uses the repository's own `read_record` to verify each
manifest's HMAC, matches each log's size and sha256 to its manifest, and
prints each record's outcome, exit status, head, snapshot error, and
before/after drift. It also prints each JSON report's per-run elapsed times.
It reruns nothing and needs no network.
