# Local validation evidence for task 261113-f7a398

This branch holds only retained validation records for task 261113-f7a398 (PR #1638). It holds no source code. Each attempt directory holds the `manifest.json` and the complete `command.log` that `scripts/validation_evidence.py run` wrote (docs/validation-evidence.md), plus `SHA256SUMS` over all of them. The private integrity keys stay on the sandbox that ran the commands.

Every run executed in the dedicated task worktree at head `2b559b4436e25cfc1818ea7b76a9408ce88a1749` (`before.git.head` and `after.git.head` in each manifest). Before the runs, the task cache was prepared with `prepare-cache`, and `CONVEYOR_TASK_CACHE`, `GOCACHE`, `GOTMPDIR`, `TMPDIR`, `PLAYWRIGHT_BROWSERS_PATH`, and `npm_config_cache` were exported, as docs/validation-evidence.md requires.

The approved plan (v1) names these commands:

    make build
    make vet
    make fmt-check
    setpriv --inh-caps=-all --ambient-caps=-all make test-go MAKEOVERRIDES= RUN_GO_UNIT_TESTS='CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= $(VALIDATION_CHILD_ENV) go test ./internal/nextaction ./internal/store ./cmd/conveyor -count=1'
    setpriv --inh-caps=-all --ambient-caps=-all make test-ui MAKEOVERRIDES= PLAYWRIGHT_ARGS='tests/next-action-card.spec.ts'

Attempts 1 to 3 ran as `python3 scripts/validation_evidence.py run --timeout 500 --policy <policy>`. The helper's policy `command` must start with `make`, so for attempts 4 and 5 the `setpriv --inh-caps=-all --ambient-caps=-all` prefix wraps the helper: `setpriv --inh-caps=-all --ambient-caps=-all python3 scripts/validation_evidence.py run --timeout 580 --policy <policy>`. `make` and every child then run without inheritable or ambient capabilities. In this sandbox the user's `CapEff` is `0000000000000004` (CAP_DAC_READ_SEARCH) without the prefix. Each policy `command` is the plan's argv exactly.

| Attempt | Policy command | Exit | Outcome | Result in the log |
|---|---|---|---|---|
| `1-make-build` | `make build` | 0 | success | `vite build` finished; `bin/conveyor` and `bin/conveyord` linked |
| `2-make-vet` | `make vet` | 0 | success | `go vet ./...` reports nothing |
| `3-make-fmt-check` | `make fmt-check` | 0 | success | empty log: the recipe is silent and prints only unformatted files |
| `4-setpriv-make-test-go` | `make test-go MAKEOVERRIDES= RUN_GO_UNIT_TESTS=...` | 0 | success | `test-validation` suites `OK` (91, 20, and 97 tests, 8 skipped); `ok` for `internal/nextaction`, `internal/store`, and `cmd/conveyor`; `npm run lint` passes |
| `5-setpriv-make-test-ui` | `make test-ui MAKEOVERRIDES= PLAYWRIGHT_ARGS=tests/next-action-card.spec.ts` | 0 | success | `95 passed`, including the three new sealed checkpoint card tests |

The test-go log also prints `checksum verification failed` and `postgres before-snapshot failed ... connection refused`. Those lines are the expected output of negative cases inside the validation suites, which report `OK`.

The policies set `inputs_complete: false`, because the Python standard library, system libraries, the Docker daemon, and the Playwright browser download were not inventoried. The records are fresh-execution records and are not offered for reuse.

## Fetch and check

    git fetch https://github.com/kidus-tiliksew/conveyor.git evidence/261113-f7a398
    git worktree add --detach /tmp/evidence-261113-f7a398 FETCH_HEAD
    cd /tmp/evidence-261113-f7a398
    sha256sum -c SHA256SUMS
    python3 -c "import json,sys; [print(p, (r:=json.load(open(p))['record'])['outcome'], r['exit_status'], r['before']['git']['head'], r['after']['git']['head'], r['policy']['command']) for p in sys.argv[1:]]" */manifest.json
    grep -E '^(FAIL:|Ran |FAILED|OK|ok |--- FAIL)|passed|failed' */command.log

Each manifest's `record.log.sha256` equals the `SHA256SUMS` entry for its `command.log`.
