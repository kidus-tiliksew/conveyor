---
name: conveyor-work
description: Work an existing Conveyor task through its live claim, dedicated checkout, validation, delivery, and review-bounce lifecycle. Use when the operator asks a harness session to work on or implement a Conveyor task ID.
---

# Work a Conveyor task

Read and follow [docs/playbooks/conveyor-work.md](../../../docs/playbooks/conveyor-work.md)
— it is the canonical playbook for session mode, claim, contract, artifact,
checkout, lease, submission, release, exit, self-claimed delivery, and
review-bounce discipline.

Non-negotiables, restated: never edit or push for a task without holding its
live claim; never bypass a failed, declined, expired, or lost claim by working
bare. Fetch the delivered work-order contract before repository work, use
`conveyor checkout <task-id>` only for implementation and review orders, and
keep spec work read-only in its launched checkout. Keep the lease alive for
the life of each claim and finish through the registered stage lifecycle tool
or an explicit truthful release. Executor claims confer proposal capability
only; operator confirmations, gates, holds, drift resolution, and merge remain
outside the executor's authority. A self-claimed session records an operator's
gate or proposal decision only on the operator's direct instruction in the same
conversation, for its own task, with the operator's own credential (DEC-45).
Every claim names the harness in `agent` and the runtime's concrete model ID in
`model`, or the harness's reported value such as `auto` verbatim when the
harness does not expose one; never guess a model ID.

Session mode decides what follows a stage submission. A session that
`conveyor run` or a worker launched reports and exits, and never polls
`await_review`; the launcher owns verdicts and successor orders. Every verifier
and reviewer reports and exits after submitting its own result. A session that
called `claim_work_order` itself continues the playbook's self-claimed
delivery loop after implementation submission: it starts a separate verifier or
reviewer agent per order, awaits the verdict with `await_review`, and claims
each changes-requested successor under a fresh session ID and client token. At
a pending human gate it summarizes the decision with a dashboard link, offers
to record it, and otherwise waits with `conveyor task wait`. When its task
merges or closes, it tells the operator to run `conveyor done <task-id>` from
the primary checkout and never runs that command itself. It plans
in-session unless the operator's planning preference names another agent.
Delegated planners, verifiers, and reviewers never record gate or proposal
decisions.

For implementation delivery, commit after validation and run
`conveyor submit <task-id>` in the dedicated worktree with
`CONVEYOR_WORK_ORDER_ID` and `CONVEYOR_SESSION_ID` set to the claimed order and
session. It pushes the exact head, opens or reuses the pull request with the
executing machine's credential, and submits `head_sha`. Direct
`submit_for_review` requires that the PR already exists and that the call names
its pushed head SHA. Then follow the session-mode rule above.
