# Local validation evidence for task 261113-d85611

This branch holds only retained validation records for task 261113-d85611 (PR #1635). It holds no source code. Each attempt directory holds the `manifest.json` and the complete `command.log` that `scripts/validation_evidence.py run` wrote (docs/validation-evidence.md), plus `SHA256SUMS` over all of them. The private integrity keys stay on the sandbox that ran the commands.

Every run executed in the task worktree at the unchanged reviewed head `af8cce34409e0ad12b8e39c9ab5859ab1709e3b5` (`before.git.head` and `after.git.head` in each manifest). Scripts, the Makefile, and tests were not changed. The plan's command is:

    make test-go RUN_GO_UNIT_TESTS='$(VALIDATION_CHILD_ENV) env CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= go test ./internal/store ./cmd/conveyor -count=1'

| Attempt | How it ran | Exit | Outcome |
|---|---|---|---|
| `A-with-capability-plan-command` | plan command; sandbox user holds ambient CAP_DAC_READ_SEARCH (CapEff 0x4) | 2 | failure: `test-validation` 7 failures |
| `B-setpriv-plan-command` | plan command under `setpriv --inh-caps=-all --ambient-caps=-all` | 2 | failure: `test-validation` 6 failures |
| `C-setpriv-plan-command-makeoverrides` | same, with `MAKEOVERRIDES=` added before the override | 0 | success |

Causes:

- A, 1 of 7: `CacheCleanupFilterTests.test_uninspectable_proc_refuses_with_owner_isolation` expects a refusal for a mode-0 directory. The sandbox user's ambient CAP_DAC_READ_SEARCH can read that directory. B and C run without the capability, and that test passes.
- A and B, 6 of 7: `MakeGraphTests.test_composite_once_serial_and_parallel_preserves_all_checks` (4 subtests) and `test_failure_propagates_and_drift_refused` (2 subtests). These tests start a nested `make validate` with `dict(os.environ, ...)`, so the nested make inherits `MAKEFLAGS`. `MAKEFLAGS` carries the plan command's command-line `RUN_GO_UNIT_TESTS=...` override. The nested graph then runs the focused `go test` instead of `go test ./...`, and the tests' `'go test ./...' not found` assertion fails. This occurs on any host, with or without the capability.
- C: `MAKEOVERRIDES=` is GNU make's documented way to stop passing command-line variable definitions to sub-makes. The outer make still applies the override and runs every `test-go` prerequisite (compose-check, dashboard-fresh, test-release, test-validation, test-validation-environment, vk10-runtime). Then it runs `go test ./internal/store ./cmd/conveyor -count=1` and `npm run lint`. The log shows the three `test-validation` suites `OK` (91, 20, and 97 tests), `ok github.com/kidus-tiliksew/conveyor/internal/store`, and `ok github.com/kidus-tiliksew/conveyor/cmd/conveyor`.
- Without the override and without the capability, plain `make test-validation` passes on this sandbox.

The policies set `inputs_complete: false`, because the Python standard library, system libraries, the Docker daemon, and the Playwright browser download were not inventoried. The records are fresh-execution records and are not offered for reuse.

## Fetch and check

    git fetch https://github.com/kidus-tiliksew/conveyor.git evidence/261113-d85611
    git worktree add --detach /tmp/evidence-261113-d85611 FETCH_HEAD
    cd /tmp/evidence-261113-d85611
    sha256sum -c SHA256SUMS
    python3 -c "import json,sys; [print(p, (r:=json.load(open(p))['record'])['outcome'], r['exit_status'], r['before']['git']['head'], r['after']['git']['head'], r['policy']['command']) for p in sys.argv[1:]]" */manifest.json
    grep -E '^(FAIL:|Ran |FAILED|OK$|ok )' */command.log

Each manifest's `log.sha256` equals the `SHA256SUMS` entry for its `command.log`.
