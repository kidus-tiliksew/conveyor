# Test pipeline benchmark for 260811-0fa80f

Measured on 2026-08-11 on the same macOS host and repository worktree. Times
are wall-clock seconds from `/usr/bin/time -p`. Process peaks are one-second
samples of host-wide `node` and `chrome-headless-shell` counts, so unrelated
Conveyor activity can only make the reported peaks more conservative.

## Ordinary run

The untouched baseline was commit `cfc9827`. Its `make test` completed in
117.31 seconds, discovered and passed 169 tests in 16 files, and Playwright
selected its machine-dependent default of 6 workers. The optimized run on the
same base completed in 79.32 seconds, discovered and passed the same 169 tests,
and used the new default of 2 workers.

| Measurement | Baseline | Optimized |
| --- | ---: | ---: |
| Aggregate `make test` | 117.31s | 79.32s |
| Peak host Node processes | 57 | 46 |
| Peak host Chromium processes | 28 | 8 |
| Playwright workers | 6 | 2 |
| Discovered/passed tests | 169/169 | 169/169 |

The phase replay below records the commands independently, so cache state and
background host load are explicit rather than pretending that the rows add up
to the aggregate measurement. The baseline replay used a warm Go build cache;
the initial aggregate run's slowest Go package took 28.263 seconds. The current
optimized replay followed the merge of current `main` and therefore includes
the additional upstream browser test described below.

| Phase | Baseline replay | Optimized replay | Command/notes |
| --- | ---: | ---: | --- |
| Dependency installation | 3.44s, then repeated by `test-web` | 7.33s once | `cd web && npm ci` |
| Dashboard build and TypeScript validation | 4.10s | 8.25s | `cd web && npm run build`; includes `tsc --noEmit` |
| Compose isolation validation | 1.18s | 2.15s | `python3 scripts/validate_compose_isolation.py` |
| Go tests | 2.24s warm | 35.99s cold/current-base | `CONVEYOR_TEST_DATABASE_URL= go test ./...` |
| Chromium installation check | 0.97s | 1.04s | `npx playwright install chromium` |
| Standalone duplicate TypeScript validation | 0.45s | removed from aggregate | `npm run typecheck` remains in standalone `make test-web` |
| Biome lint/format check | 1.93s | 1.79s | `npm run lint` |
| Browser startup and E2E | 1.2m reported | 64.74s | 6 versus 2 workers; 169 versus current 170 tests |

`make -n test` confirms the optimized aggregate contains exactly one `npm ci`
and one TypeScript-validating command. Standalone `make test-web
PLAYWRIGHT_ARGS=--list` still performs a clean install and typecheck, passes,
and discovers the complete inventory. A dry run with
`PLAYWRIGHT_INSTALL_ARGS=--with-deps` preserves the CI Chromium installation
command.

## Controlled concurrent runs

Two Playwright invocations ran simultaneously from the same unchanged test
tree with separate dynamically selected ports. `PLAYWRIGHT_WORKERS=6`
reproduced the former machine default; `PLAYWRIGHT_WORKERS=2` exercised the new
default. All four invocations discovered and passed 169 tests.

| Workers per invocation | Run wall times | User CPU per run | Peak Node | Peak Chromium |
| ---: | --- | --- | ---: | ---: |
| 6 | 82.74s, 83.34s | 179.30s, 179.61s | 47 | 52 |
| 2 | 82.00s, 82.11s | 151.72s, 151.33s | 44 | 40 |

The two-worker setting did not worsen elapsed time and reduced user CPU by
about 15% plus peak Chromium count by about 23%. This supports retaining the
bounded default. Splitting `web/tests/task-full.spec.ts` was not retained:
the worker cap already improved both aggregate time and controlled-contention
resource use, while a split would add fixture, naming, and discovery risk.

## Inventory reconciliation

The implementation changes no file under `web/tests`. The original-base before
and after runs both discovered exactly 169 tests. While this branch was being
validated, current `main` advanced through `14f6353`; upstream commit `0992238`
added `requirement staleness can file one linked follow-up and be dismissed in
place` to `web/tests/requirements-planning.spec.ts`. After merging that base,
the branch discovers and passes 170 tests in 16 files. No original test was
deleted, skipped, renamed, weakened, or replaced.

Post-merge validation passed with 170/170 tests: `make build`, `make vet`,
`make fmt-check`, `make test` (105.36s), and `git diff --check`.
