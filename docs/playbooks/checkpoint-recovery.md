# Recovering an implementation checkpoint

Authority: component-git-delivery v9 CP-1 through CP-5 and
component-work-orders v15 HO-1 through HO-4. Checkpoints preserve unfinished
work. They do not establish validation or review approval.

1. Install the matching Conveyor server and CLI versions. Resolve any existing
   task gate through its normal operator action. Do not request another recovery
   solely because a checkpoint trailer and a release event use different words.
2. Resume with `conveyor --workspace demo run <task-id>` or the worker. The
   launcher obtains the exact claim, acquires the task writer lock, and asks the
   server to admit that session and writer generation. A surviving child keeps
   the inherited lock even if its launcher dies. The successor waits for local
   ownership before starting its child.
3. Inside that session, run `conveyor --workspace demo checkout <task-id>`.
   Keep the launcher-provided predecessor, current attempt, session, and writer
   environment. Checkout compares the predecessor descriptor against durable
   claim and release records, repository identity, branch, and any checkpoint
   audit. Dirty work additionally requires the original local producer record.
4. Checkout resumes an interrupted push or audit before returning the worktree.
   Inspect the preserved diff and run the normal validation gates before
   submission. A local commit, a remote push, and an acknowledged audit are
   separate results.

## Existing checkpoints

For task `260915-e190ff`, inspect checkpoint `52e9a323` through its assigned
branch; for `260915-e26dd8`, inspect `6328315f`. The session resolves the full
commit SHA and compares its immutable attempt/order trailers with the task's
claim and checkpoint history. Matching identity with different termination text
can be reconciled without modifying that commit. The reconciliation event keeps
the original reason, observed release reason, producer, recovering writer, and
claim/release/checkpoint event references.

The twenty-file checkpoint is not proof that the attempt that discovered those
files authored them. If its producing identity cannot be established from the
history and local recovery record, retain the files and report the missing or
conflicting evidence. Do not relabel the producer to get past checkout.

The local record lives under the repository common Git directory in
`conveyor-writers/`, alongside its OS lock. It retains the producer and the
observed parent plus any checkpoint SHA whose push or audit may be outstanding.
Do not delete it to bypass an ownership refusal. A clean checkpoint with no
audit needs this matching producer evidence to recover a missing audit.

No recovery path amends trailers, rewrites events, resets or rebases a branch,
force-pushes, stashes dirty content, strips predecessor variables, or creates an
empty commit. Divergent history and unverifiable ownership stop recovery.
Standalone operator checkout lacks the execution session's evidence and is not
a proof that resumed execution can recover a checkpoint.
