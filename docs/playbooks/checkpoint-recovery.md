# Recovering a released implementation attempt

Authority: `component-attempt-checkpoints`, in particular "The exclusive local
writer" and "Recovering a released attempt". `component-git-delivery` owns
checkout and repository identity, and `component-work-orders` owns claims and
release outcomes. Recovery continues unfinished work. It never establishes
validation or review approval.

Conveyor launches no agent session, and checkpoint continuity is cold: a
recovered order is claimed by an agent session under a fresh session ID and
client token, and that session continues from the task branch and the order's
delivered context, including the operator's direction (DEC-94; DEC-97(1)). The
session may be the harness conversation that released the order. Conveyor
records no harness session ID and resumes no harness session.

The examples use server `https://conveyor.kidus.sh` and workspace `demo`;
substitute your own.

## Procedure

1. Run matching Conveyor server and CLI versions.
2. Resolve only a gate that actually requires recovery, through its ordinary
   operator action. Give the direction the next session needs; it arrives
   with the recovered order's delivered context.
3. Start an agent session in your own harness and ask it to work the task with
   the `conveyor-work` skill. It claims the recovered order over MCP under a
   fresh session ID and client token, and reads the order's contract and
   operator direction.
4. In that session, resolve the worktree:

   ```sh
   conveyor --server https://conveyor.kidus.sh --workspace demo checkout <task-id>
   ```

   Checkout reuses the task's registered worktree and branch. It takes the
   task's local writer lock for its own duration, so two checkouts of one task
   branch on a machine never resolve the worktree at the same time. It reads
   no `CONVEYOR_WRITER_*`, `CONVEYOR_PREVIOUS_ATTEMPT_*`, or
   `CONVEYOR_PREDECESSOR` variable and creates no checkpoint commit.
5. Checkout refuses a dirty registered task worktree, so leftover work from an
   earlier session is never adopted silently. The session that claimed the
   recovered order inspects that work in the task worktree, commits or
   discards it there, and runs checkout again.
6. Inspect the branch history, then run the repository's normal validation
   gates before submitting. A `wip(` checkpoint commit that an earlier
   launcher created stays on the branch as ordinary history and is never
   validation evidence.

## What stops recovery

Checkout stops and leaves the files in place when:

- the repository identity, task, or branch does not match;
- the registered task worktree is dirty;
- the remote task branch has diverged from the local history.

Do not relabel or move work to get past checkout. Files found in a worktree
are never attributed to the session that found them.

## The local writer lock

The writer lock lives under the repository's common Git directory at
`conveyor-writers/<sha256 of branch>`. Process death releases it. It is a
local exclusion, never authority. Do not delete it to bypass a wait.

## What recovery never does

No recovery path resets or rebases a branch, force-pushes, stashes, amends
trailers, rewrites events, or creates an empty commit (DEC-10).
