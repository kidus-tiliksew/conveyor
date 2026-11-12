---
name: conveyor-work
description: Work an existing Conveyor task through its live claim, dedicated checkout, validation, delivery, and review-bounce lifecycle. Use when the operator asks a harness session to work on or implement a Conveyor task ID. A verification order follows conveyor-kit-verify in the same dedicated task worktree.
---

# Work a Conveyor task

Citation scope: the REQ, AC, DEC, and component IDs in this guidance, and its
other requirement, design, and reference IDs, cite the Conveyor product's own
design corpus, not the corpus of the factory you work in. Task work cites the
working factory's own confirmed corpus, as the delivered contract directs.

Read and follow [docs/playbooks/conveyor-work.md](../../../docs/playbooks/conveyor-work.md)
— it is the canonical playbook for session ownership, claim, contract,
artifact, checkout, lease, submission, release, exit, self-claimed delivery,
and review-bounce discipline.

Non-negotiables, restated: never edit or push for a task without holding its
live claim; never bypass a failed, declined, expired, or lost claim by working
bare. Fetch the delivered work-order contract before repository work, use
`conveyor checkout <task-id>` only for implementation, verification, and
review orders, follow `conveyor-kit-verify` for a verification order, and
keep spec work read-only in the checkout where it started. Work only in the path
`conveyor checkout` prints inside the configured worktree location; never work
from a checkout path outside it, and never create a separate clone to get
around a primary checkout that has uncommitted changes. Keep the lease alive for
the life of each claim and finish through the registered stage lifecycle tool
or an explicit truthful release. Executor claims confer no way to propose or revise
a document; operator confirmations, gates, assignment, drift resolution, and merge
remain outside the executor's authority. A self-claimed session records an
operator's gate or proposal decision only on the operator's direct instruction
in the same conversation, for its own task, with the operator's own credential
(DEC-84). Every claim names the harness in `agent` and the runtime's concrete model ID in
`model`, or the harness's reported value such as `auto` verbatim when the
harness does not expose one; never guess a model ID.

Every session claims its own order: a person started it in their own
harness, and Conveyor launches no agent session, so no launcher renews the
lease or owns a later stage (DEC-131(1)). Every verifier and reviewer reports
and exits after submitting its own result. The session that claimed the
task's order continues the playbook's self-claimed delivery loop after
implementation submission (DEC-131(2)): it starts a separate verifier or
reviewer agent per order, or one review primary agent for a round, awaits the verdict with `await_review`, and claims
each changes-requested successor under a fresh session ID and client token. At
a pending human gate it summarizes the decision with a dashboard link and
offers to record it, listing the replies as message text. It never offers them
through a question tool that blocks the turn until the operator answers in
chat (`AskUserQuestion`, `request_user_input`, `question`), because a blocked
turn cannot observe a gate resolved elsewhere. It never preselects or
recommends a gate reply, reads the gate again before recording a typed reply,
and goes straight into `conveyor task wait` after the offer. When its task
merges or closes, it runs `conveyor done <task-id>` from the primary checkout
and reports the result. After a merge it then fast-forwards a clean primary
checkout that is on the base branch, and otherwise reports why it skipped
(req-delegated-execution AC-3.8, AC-3.9). Each time it starts a planner,
verifier, reviewer, or review primary, it reads the person's stage preferences
with `get_stage_preferences` and starts that agent with the stored harness,
model, and effort. Review seat N takes seat entry N or else the review primary
entry. It plans in-session without asking when no planning preference is
stored. For an unset verification stage, an uncovered review seat, or two seats
on one model under a frozen true `review.distinct_models`, it asks the operator
and saves the direct answer with `set_stage_preference`. It reports a harness
or model that it cannot start and never replaces it. It changes a stage
preference only on the operator's direct instruction. Where agents run, how
many tasks run at once, and the review seat count rule stay in agent memory
(DEC-131(3); DEC-123(3)). A review primary only starts the seat agents: it
claims no seat, submits no verdict, and awaits no verdict. Delegated planners,
verifiers, reviewers, and review primaries never record gate or proposal
decisions and never write a stage preference.

For implementation delivery, commit after validation and run
`conveyor submit <task-id>` in the dedicated worktree with
`CONVEYOR_WORK_ORDER_ID` and `CONVEYOR_SESSION_ID` set to the claimed order and
session. It pushes the exact head, opens or reuses the pull request with the
executing machine's credential, and submits `head_sha`. Direct
`submit_for_review` requires that the PR already exists and that the call names
its pushed head SHA. Then continue the delivery loop above.
