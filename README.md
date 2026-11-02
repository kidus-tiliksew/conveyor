# Retained local evidence: Conveyor task 261101-9c939d

Local-only exit gate for PR https://github.com/kidus-tiliksew/conveyor/pull/1499:
`tests/board-filters.spec.ts` passes 20 times in a row under Playwright's
`fullyParallel` setting. Each run went through `scripts/validation_evidence.py run`
(docs/validation-evidence.md) from the task worktree at the reviewed head
`3f2d7a504eebb66688d149f95a5836aee37bd956`. The worktree was clean.

Host: Linux, 4 CPUs. Tools: Node.js v24.22.1, Playwright 1.61.1 (Chromium build
1228 in the task cache), Python 3.12. Config: `fullyParallel: true` from
`web/playwright.config.ts`, no `--shard`, so each invocation runs every test.
Environment: `CONVEYOR_TASK_ID=261101-9c939d`; the task-cache variables come
from `prepare-cache` under `$HOME/.cache/conveyor/261101-9c939d`.

| Directory | Command | Result | Check and bind |
| --- | --- | --- | --- |
| `runs/attempt-20261102T041733Z-89938-d0f9bccd4e47` | `make test-web "PLAYWRIGHT_ARGS=tests/board-filters.spec.ts --repeat-each=20"` (default 2 workers, as in CI) | success, exit 0, 300 passed (3.7m) | `check` eligible, bound to the pushed head |
| `runs/attempt-20261102T041424Z-77911-4336e6b19e08` | `make test-web PLAYWRIGHT_WORKERS=4 "PLAYWRIGHT_ARGS=tests/board-filters.spec.ts --repeat-each=20"` | success, exit 0, 300 passed (2.5m) | `check` eligible, bound to the pushed head |

The table lists the two accepted runs. Every other attempt on this head is kept too:

- `runs/attempt-20261102T041016Z-67751-8e293b1c7ba1`: outcome `snapshot-failure` before the command started; the log is empty. The policy omitted the out-of-root target of `/usr/lib/python3.12/sitecustomize.py`. I added every out-of-root symlink target to `external_inputs` and reran.
- `runs/attempt-20261102T041048Z-67985-99d74ae5647a`: 2 workers, success, exit 0, 300 passed (2.9m). `check` refuses reuse ("changed files"). All 205 changed entries are under `web/node_modules/.vite*`, the dependency cache the Vite dev server rewrote during the run after `npm ci`. I reran the 2-worker gate (the 041733Z attempt) to get a record with unchanged inputs.

Each accepted directory holds the helper's `manifest.json` (HMAC envelope:
policy, before and after input fingerprints, Git head, timestamps, exit status,
and log digest), the complete unedited `command.log`, and the
`binding-<head>.json` that `bind` wrote. The private integrity keys stay in
durable local state at `~/.local/state/conveyor/261101-9c939d/<attempt>/key` on
the run host and are not published. The helper redacts inventoried environment
values from logs, and these files contain no credential or remote URL.
`policies/` holds the exact policies passed to `run`. `SHA256SUMS` covers every
other file, and `verify.py` rechecks all of it.

```sh
git fetch origin evidence/261101-9c939d
git worktree add --detach /tmp/evidence-9c939d FETCH_HEAD   # or: git archive FETCH_HEAD | tar -x -C <dir>
cd /tmp/evidence-9c939d
sha256sum -c SHA256SUMS
python3 -I verify.py
```
