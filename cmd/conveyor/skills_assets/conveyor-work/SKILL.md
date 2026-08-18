---
name: conveyor-work
description: Work an existing Conveyor task through its live claim, dedicated checkout, validation, delivery, and review-bounce lifecycle. Use when the operator asks a harness session to work on or implement a Conveyor task ID.
---

# Work a Conveyor task

Read and follow [docs/playbooks/conveyor-work.md](../../../docs/playbooks/conveyor-work.md)
— it is the canonical playbook for claim, contract, artifact, checkout, lease,
submission, release, exit, and review-bounce discipline.

Non-negotiables, restated: never edit or push for a task without holding its
live claim; never bypass a failed, declined, expired, or lost claim by working
bare. Fetch the delivered work-order contract before repository work, use
`conveyor checkout <task-id>` only for implementation and review orders, and
keep spec work read-only in its launched checkout. Renew throughout the
attempt, finish through the registered stage lifecycle tool or an explicit
truthful release, report, and exit; never poll `await_review` from a stage
session. Executor claims confer proposal capability only; operator
confirmations, gates, holds, drift resolution, and merge remain outside the
executor's authority.
