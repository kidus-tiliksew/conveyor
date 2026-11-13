# Local validation evidence for task 261113-f66367

This branch holds only retained validation records for task 261113-f66367 (PR #1637). It holds no source code. Each attempt directory holds the `manifest.json` and the complete `command.log` that `scripts/validation_evidence.py run` wrote (docs/validation-evidence.md), plus `SHA256SUMS` over all of them. The private integrity keys stay on the sandbox that ran the commands.

The branch holds two complete sets of the same five runs, each executed in the dedicated task worktree (`before.git.head` and `after.git.head` in each manifest):

- `head-229d534c/`: head `229d534c048f1b2d93fd781c77c82930042a4ad4`, the submitted head of PR #1637. It adds one test assertion fix and one CLI refusal message to the head below.
- The top-level attempt directories: head `3f8ec79e94b5954dc7d3dff6aa844597d443614f`, the merge of `origin/main` into the task branch, the first pushed head of PR #1637. Before the runs, the task cache was prepared with `prepare-cache`, and `CONVEYOR_TASK_CACHE`, `GOCACHE`, `GOTMPDIR`, `TMPDIR`, `PLAYWRIGHT_BROWSERS_PATH`, and `npm_config_cache` were exported, as docs/validation-evidence.md requires.

The approved plan's done criterion names these commands:

    make build
    make vet
    make fmt-check
    setpriv --inh-caps=-all --ambient-caps=-all make test-go MAKEOVERRIDES= RUN_GO_UNIT_TESTS='CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= $(VALIDATION_CHILD_ENV) go test ./internal/store ./internal/nextaction ./internal/httpapi ./internal/workorder ./cmd/conveyor'
    setpriv --inh-caps=-all --ambient-caps=-all make test-ui MAKEOVERRIDES= PLAYWRIGHT_ARGS='tests/next-action-card.spec.ts'

The helper's policy `command` must start with `make`, so for attempts 4 and 5 the `setpriv --inh-caps=-all --ambient-caps=-all` prefix wraps the helper: `setpriv --inh-caps=-all --ambient-caps=-all python3 scripts/validation_evidence.py run --timeout 580 --policy <policy>`. `make` and every child then run without inheritable or ambient capabilities. In that sandbox the user's `CapEff` is `0000000000000004` (CAP_DAC_READ_SEARCH) without the prefix and `0000000000000000` under it. Each policy `command` is the plan's argv exactly.

Both sets have the same outcome, shown here for the submitted head (`head-229d534c/`):

| Attempt | Policy command | Exit | Outcome | Result in the log |
|---|---|---|---|---|
| `1-make-build` | `make build` | 0 | success | `vite build` finished; `bin/conveyor` and `bin/conveyord` linked |
| `2-make-vet` | `make vet` | 0 | success | `go vet ./...` reports nothing |
| `3-make-fmt-check` | `make fmt-check` | 0 | success | empty log: the recipe is silent (`@`) and prints only unformatted files |
| `4-setpriv-make-test-go` | `make test-go MAKEOVERRIDES= RUN_GO_UNIT_TESTS=...` | 0 | success | `test-validation` suites `OK` (91, 20, and 97 tests, 8 skipped); `ok` for `internal/store`, `internal/nextaction` (cached result of an identical earlier build), `internal/httpapi`, `internal/workorder`, and `cmd/conveyor`; `npm run lint` passes |
| `5-setpriv-make-test-ui` | `make test-ui MAKEOVERRIDES= PLAYWRIGHT_ARGS=tests/next-action-card.spec.ts` | 0 | success | `92 passed`, including the five disposition card tests |

The policies set `inputs_complete: false`, because the Python standard library, system libraries, the Docker daemon, and the Playwright browser download were not inventoried. The records are fresh-execution records and are not offered for reuse.

## Fetch and check

    git fetch https://github.com/kidus-tiliksew/conveyor.git evidence/261113-f66367
    git worktree add --detach /tmp/evidence-261113-f66367 FETCH_HEAD
    cd /tmp/evidence-261113-f66367
    sha256sum -c SHA256SUMS
    python3 -c "import json,sys; [print(p, (r:=json.load(open(p))['record'])['outcome'], r['exit_status'], r['before']['git']['head'], r['after']['git']['head'], r['policy']['command']) for p in sys.argv[1:]]" */manifest.json
    python3 -c "import json,sys; [print(p, (r:=json.load(open(p))['record'])['outcome'], r['exit_status'], r['before']['git']['head'], r['after']['git']['head'], r['policy']['command']) for p in sys.argv[1:]]" head-229d534c/*/manifest.json
    grep -E '^(FAIL:|Ran |FAILED|OK|ok |--- FAIL)|passed|failed' */command.log head-229d534c/*/command.log

Each manifest's `log.sha256` equals the `SHA256SUMS` entry for its `command.log`.
