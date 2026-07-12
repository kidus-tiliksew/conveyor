# Known limitations and resolution history

Deliberate gaps in the current implementation, each with its failure
mode, recovery procedure, and the phase that dissolves it. These are
accepted trade-offs, not bugs — the code sites carry pointers back to
this file.

## 1. Claimed issues are lost if conveyord dies before the PR

**Status: resolved in Phase 2.** GitHub tasks are first persisted as
`claiming`, which does not enqueue River. After the label transition succeeds,
the task moves transactionally to `queued`. Every poll reconciles durable
`claiming` tasks idempotently, covering a crash between those two systems.

**Phase 1 failure mode.** The GitHub poller durably claims an issue by moving
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

**Resolution.** `TaskClaiming` plus the Postgres/River transition described
above; no manual label repair is required.

## 2. A log-stream failure parks the task even if the job succeeded

**Status: resolved in Phase 2.** The redacted shim
`events-<job-id>.jsonl` is validated for a terminal event and persisted as the
authoritative transcript. Docker log
stream failures emit `job.log_stream_degraded` but do not fail a successful
job with a complete artifact.

**Phase 1 failure mode.** The dispatcher treats a runner-side log-stream error
(`LogEvent.Err`, e.g. `docker logs --follow` dying) as a job failure:
the task parks even though the container may have run to completion and
committed work (`internal/dispatch.runTask`).

**Why it's built this way.** Phase 1 is "logs only" — the transcript
*is* the product of a job, and silently accepting a job whose
transcript is incomplete would corrupt the audit record that later
phases (redaction, transcript mining) are built on. Failing closed is
the conservative choice.

**Recovery.** Nothing is lost: commits live on the task branch in the
persistent worktree, and the attempt-scoped events file in the task's `.conveyor/`
control dir usually has the full harness event stream even when the
docker log follower failed. Inspect, then re-dispatch; the re-run
resumes in the same worktree and sees the prior commits.

**Resolution.** Implemented in `internal/dispatch.inspectTranscript`; the live
stream is now an operator convenience rather than the audit source of truth.

## 3. Fresh handoff fallback is prompt-read-only, not mount-read-only

**Status: still accepted.** Phase 2 does not move fallback elicitation into a
second runner-owned container; the task worktree remains its blast-radius
boundary.

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

## 4. Injected secrets have no transcript redaction yet

**Status: resolved in Phase 2.** The shim scrubs injected exact values, common
encodings, known credential patterns/private keys, and high-entropy secret
assignments before stdout or the artifact. Only class counts persist.

**Phase 1 failure mode.** LocalDockerRunner resolves SOPS references and injects
single-line environment values at container creation. If an agent explicitly
prints one of those values, Phase 1 records it in `events.jsonl` and daemon
logs because the redaction engine is a Phase 2 deliverable.

**Why it's built this way.** Secrets injection is a Phase 1 core-loop
deliverable while redaction is explicitly Phase 2 in §19. The injection path
keeps values out of task state, API payloads, Docker argv, worktrees, and
persistent staging files; all output already passes through the shim choke
point where redaction attaches. Phase 1 can narrow exposure with non-production
sets, `local_eligible`, least-privilege values, command rules that forbid broad
environment dumps, and task prompts that never request values.

**Recovery.** Treat any printed value as compromised: remove the transcript
artifact from circulation, rotate the underlying secret, and re-encrypt the
set. Do not inject production credentials into Phase 1 jobs.

**Resolution.** `internal/redact` at the shim boundary plus persisted redaction
events and transcript metadata (spec §10.3).

## 5. The sandbox can write its own authoritative transcript file

**Status: still accepted.** Phase 2 scopes transcripts per attempt and validates
their JSONL structure, terminal event, redaction metadata, and provenance path,
but it does not provide a cryptographic or privilege boundary between the shim
and the sandbox that hosts it.

**Failure mode.** `events-<job-id>.jsonl` lives in the read-write control mount.
The shim is its intended writer, but a malicious or badly behaved process in
the same Tier A container can modify that file before exit. The dispatcher
would then validate and persist metadata for attacker-controlled transcript
bytes. Attempt scoping prevents a later run from truncating or being credited
with an earlier run's transcript, but it does not prove the bytes came only
from the shim.

**Why it remains.** The shim and harness currently share the container UID and
mount namespace. File modes cannot create an integrity boundary between them.
The trustworthy boundary is the runner/Docker daemon, so the durable fix is a
runner-owned transcript sink fed from the shim's dedicated output channel,
stored outside every sandbox mount, with a digest recorded alongside transcript
metadata.

**Recovery.** Treat a transcript as audit evidence, not tamper-proof evidence.
If its event sequence conflicts with commits, artifacts, or container logs,
park the task and retain all three for inspection.

**Resolved by.** A runner-owned, non-mounted transcript sink and persisted
content digest. This is security hardening of the existing Phase 2 boundary,
not permission to mine or trust sandbox-authored bytes silently.

## 6. A budget breaker skips handoff elicitation

**Status: accepted in Phase 3.** When the shim observes cumulative main-run
cost at or above the job budget, it cancels the harness promptly, emits the
terminal budget error, and does not start native-resume or fallback handoff
elicitation.

**Failure mode.** This is a deliberate exception to the "handoff snapshot —
always" continuity floor in spec §8.3. A budget-interrupted job can leave useful
partial work without the compact structured snapshot normally injected into
its successor. Starting another model turn to write that snapshot would spend
past the circuit breaker that just fired.

**Recovery.** The dispatcher persists partial token and cost totals from the
attempt transcript, halts the task at its exact recovery stage, and retains the
persistent worktree and partial transcript for review. An explicit resume can
continue from those artifacts, but may pay some rediscovery cost.

**Resolved by.** A runner-owned, non-model checkpoint assembled from already
captured transcript events and worktree metadata, or a separately reserved
handoff allowance that can be enforced without exceeding the main job budget.
