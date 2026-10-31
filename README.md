# Retained local evidence for Conveyor task 261030-8cfc34 (PR #1474)

This orphan branch is never merged. It publishes the complete retained
bundles that `scripts/validation_evidence.py run` wrote for the task's
local-only exit gates at task head `e1d3aded23f0d8a8d5c25cfe39f7ad7dcff20cd0`.
The files are byte-identical copies of the originals under
`/home/user/.local/state/conveyor/261030-8cfc34/` on the implementing host.

| Directory | Record | Outcome |
| --- | --- | --- |
| `repeat20/` | Four flaky tests plus the two new settings tests, `--repeat-each=20` | `success`, exit 0; `check` eligible; bound to the pushed head |
| `negative-decisions/` | Decisions entry temporarily moved after Monitor | `failure`, exit 2 (expected) |
| `negative-inbox/` | Shared age line temporarily suppressed | `failure`, exit 2 (expected) |
| `inputs/` | Wrapper makefile and summary script named by every policy | n/a |

Each record directory holds its policy (`policy-<name>.json`) and its attempt
directory with `manifest.json`, `key`, and `command.log`. The repeat-20
attempt also holds `binding-e1d3aded….json`. The `key` is the helper's
32-byte integrity key for the manifest HMAC. It detects corruption, is not a
signature, and grants no access to anything. `SHA256SUMS` lists every file.

## Verify

```sh
git clone --depth 1 --single-branch --branch evidence/261030-8cfc34 \
  https://github.com/kidus-tiliksew/conveyor.git "$HOME/evidence-261030-8cfc34"
cd "$HOME/evidence-261030-8cfc34" && sha256sum -c SHA256SUMS
# From any checkout of the conveyor repository containing e1d3aded. The
# evidence directory must be outside that checkout and outside ~/.cache.
cd <conveyor checkout>
python3 -I "$HOME/evidence-261030-8cfc34/verify.py" --repo .
for d in "$HOME"/evidence-261030-8cfc34/*/attempt-*; do
  python3 scripts/validation_evidence.py inspect --output "$d"
done
```

`verify.py` uses the repository's own `read_record` to verify each
manifest's HMAC. It also matches each log's size and sha256 to its manifest,
matches each published policy to its manifest, compares the before and after
snapshots, and verifies the head binding. It also checks that each negative
control's input snapshot differs from the repeat-20 snapshot only in its
temporarily edited source file. `inspect` prints the helper's own
classification.

`check` additionally re-hashes the host's current inputs: the worktree,
the Node, Python, Chromium, shared-library, and font paths in the policy, and
the wrapper files at
`/home/user/.local/state/conveyor-inputs/261030-8cfc34/`. It also requires
the bundle at its original absolute path. It can therefore pass only on a host
with identical inputs at identical paths. On the implementing host it was run
again on 2026-10-31 at about 06:22 UTC:

```sh
# Inputs and policies at /home/user/.local/state/conveyor-inputs/261030-8cfc34/,
# attempt directories at /home/user/.local/state/conveyor/261030-8cfc34/.
cd /home/user/conveyor-worktrees/conveyor-task-261030-8cfc34   # task branch at e1d3aded
python3 scripts/validation_evidence.py check \
  --policy /home/user/.local/state/conveyor-inputs/261030-8cfc34/policy-repeat20.json \
  --output /home/user/.local/state/conveyor/261030-8cfc34/attempt-20261031T060809Z-96251-4f3cdd2bc37e
```

It printed `Eligible local evidence; no command was rerun.` for `repeat20`.
For the two negative controls it printed
`Evidence refused; run the affected validation again: failed/incomplete execution`,
which is the helper's designed answer for a record of a failing command.
