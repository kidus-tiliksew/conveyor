# Known limitations (Phase 1)

Deliberate gaps in the current implementation, each with its failure
mode, recovery procedure, and the phase that dissolves it. These are
accepted trade-offs, not bugs — the code sites carry pointers back to
this file.

## 1. Claimed issues are lost if conveyord dies before the PR

**Failure mode.** The GitHub poller durably claims an issue by moving
its label `conveyor:ready` → `conveyor:dispatched` *before* enqueueing
the task (`internal/dispatch.pollOnce`). Task state, however, lives in
the in-memory store. If conveyord crashes or restarts after the label
moved but before the task produced a PR, the restarted daemon has no
task record and the poller no longer sees the issue (no ready label):
the issue is claimed but orphaned.

**Why it's built this way.** The alternative — claiming after enqueue,
or not claiming at all — replays every in-flight issue on restart,
dispatching duplicate agent runs and burning tokens. Orphaning is
visible and cheap to fix; duplicate dispatch is silent and costs money.

**Recovery.** Re-add the `conveyor:ready` label to the issue. The next
poll re-claims it and creates a fresh task. Any worktree from the lost
attempt still exists under `jobs_dir` and is garbage-collected by the
staleness TTL; the new task gets its own branch and worktree.

**Resolved by.** Phase 2's Postgres store: task state survives
restarts, so the daemon resumes claimed-but-unfinished tasks instead of
forgetting them.

## 2. A log-stream failure parks the task even if the job succeeded

**Failure mode.** The dispatcher treats a runner-side log-stream error
(`LogEvent.Err`, e.g. `docker logs --follow` dying) as a job failure:
the task parks even though the container may have run to completion and
committed work (`internal/dispatch.runTask`).

**Why it's built this way.** Phase 1 is "logs only" — the transcript
*is* the product of a job, and silently accepting a job whose
transcript is incomplete would corrupt the audit record that later
phases (redaction, transcript mining) are built on. Failing closed is
the conservative choice.

**Recovery.** Nothing is lost: commits live on the task branch in the
persistent worktree, and `events.jsonl` in the task's `.conveyor/`
control dir usually has the full harness event stream even when the
docker log follower failed. Inspect, then re-dispatch; the re-run
resumes in the same worktree and sees the prior commits.

**Resolved by.** Phase 2, by making the shim's `events.jsonl` (uploaded
as an artifact) the authoritative transcript, with the docker log
stream demoted to a best-effort live view whose failure no longer
fails the job.

## 3. Fresh handoff fallback is prompt-read-only, not mount-read-only

**Failure mode.** When native session resume is unavailable or fails,
the shim starts a fresh harness run to reconstruct the handoff from the
task prompt and persistent worktree. The fallback prompt forbids writes,
but it runs inside the existing Tier A container whose task worktree is
mounted read-write. A misbehaving fallback agent could therefore modify
files or add a commit after the main implementation turn completed; the
dispatcher would see that state as part of the task.

**Why it's built this way.** A container mount cannot be made read-only
for one child process after the container has started. Codex's nested
sandbox cannot create its namespaces inside the unprivileged Tier A
container, which is why the main adapter uses container confinement.
Spinning up a second runner-owned container with a read-only worktree is
the correct enforcement boundary but is larger than the Phase 1 shim
fallback. The blast radius remains the task's own worktree. Fallback
events carry `phase: handoff_fallback`, including token usage, so the
resume-fidelity experiment can measure its extra cost separately.

**Recovery.** Inspect the task branch and worktree after a fallback run.
If the fallback changed state, revert only those post-implementation
changes and re-dispatch. The main implementation commit and prior
job-scoped handoff remain intact.

**Resolved by.** Move fallback elicitation into a runner-owned handoff
subjob with the worktree mounted read-only. At that point the prompt is
guidance and the mount is enforcement.
