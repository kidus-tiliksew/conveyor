# Recovering an implementation checkpoint

Authority: `component-attempt-checkpoints`, in particular "Preservation
identity", "The exclusive local writer", "Launch and successor admission",
"Push and audit reconciliation", and "Recovering an existing checkpoint".
`component-git-delivery` owns checkout and repository identity, and
`component-work-orders` owns claims and release outcomes. A checkpoint
preserves unfinished work. It never establishes validation or review approval.

The examples use server `https://conveyor.kidus.sh` and workspace `demo`;
substitute your own.

## Procedure

1. Run matching Conveyor server and CLI versions.
2. Resolve only a gate that actually requires recovery, through its ordinary
   operator action. A checkpoint commit's `Termination-Reason` line that
   differs from the recorded release reason needs no second recovery:
   termination text is evidence, not an ownership key.
3. Resume through the launcher, not a standalone checkout:

   ```sh
   conveyor --server https://conveyor.kidus.sh --workspace demo run <task-id>
   ```

   or let a worker claim the order. The launcher obtains the live claim,
   acquires the task's writer lock, and asks the server to admit its session
   as a new writer generation. A successor may be claimed while the
   predecessor's launcher still holds the lock, but it starts no writing child
   until that lock is released.
4. Inside the launched session, resolve the worktree:

   ```sh
   conveyor --server https://conveyor.kidus.sh --workspace demo checkout <task-id>
   ```

   Keep the launcher-provided environment intact: `CONVEYOR_WRITER_PATH`,
   `CONVEYOR_WRITER_GENERATION`, `CONVEYOR_CURRENT_ATTEMPT_ID`, and, when a
   predecessor exists, `CONVEYOR_PREDECESSOR`, `CONVEYOR_PREVIOUS_ATTEMPT_ID`,
   and `CONVEYOR_PREVIOUS_WORK_ORDER_ID`. Checkout joins the inherited writer
   lock and validates the predecessor descriptor against the durable claim
   and release history, the repository identity, and the assigned branch.
   Dirty work additionally requires the local producer record to name the
   durable predecessor.
5. Checkout finishes any outstanding push or audit for an existing checkpoint
   before it returns the worktree. A local commit, a remote push, and an
   acknowledged audit are separate results; read each one. Reconciliation is
   idempotent and leaves the original commit and events unchanged.
6. Inspect the preserved diff, then run the repository's normal validation
   gates before submitting. Preserved work is unvalidated.

## What stops recovery

Recovery stops, names the missing evidence, and leaves the files in place
when:

- the producer of dirty or committed work cannot be established from the
  durable history and the local record, or the evidence conflicts;
- the task, order, attempt, session, repository, or branch does not match;
- the remote task branch has diverged from the local history;
- the writer generation is stale because a successor was admitted.

Do not relabel a producer to get past checkout. Files found in a worktree are
never attributed to the session that found them.

## The local record

The writer lock and its provenance record live under the repository's common
Git directory in `conveyor-writers/`. The record holds the writer, the
producer, the parent commit observed when the writer started, and any
checkpoint SHA whose push or audit may be outstanding. It is recovery
evidence, never authority. Do not delete it to bypass an ownership refusal; a
clean checkpoint without a durable audit needs its matching producer evidence
to recover the missing audit.

## What recovery never does

No recovery path resets or rebases a branch, force-pushes, stashes, amends
trailers, rewrites events, deletes writer records, strips predecessor
variables, or creates an empty commit (DEC-10). A standalone operator checkout
carries no predecessor or current-attempt evidence and is not proof that a
checkpoint can be recovered.
