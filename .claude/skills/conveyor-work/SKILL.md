---
name: conveyor-work
description: Work an existing Conveyor task through its live claim, dedicated checkout, validation, delivery, and review-bounce lifecycle. Use when the operator asks a harness session to work on or implement a Conveyor task ID.
---

# Work a Conveyor task

Read and follow [docs/playbooks/conveyor-work.md](../../../docs/playbooks/conveyor-work.md)
— it is the canonical playbook for claim, contract, artifact, checkout, lease,
submission, release, and review-bounce discipline.

Non-negotiables, restated: never edit or push for a task without holding its
live claim; never bypass a failed, declined, expired, or lost claim by working
bare. Fetch the delivered work-order contract before repository work, perform
all work in the path returned by `conveyor checkout <task-id>`, renew throughout
the attempt, and finish through the registered stage lifecycle tool or an
explicit truthful release. Executor claims confer proposal capability only;
operator confirmations, gates, holds, drift resolution, and merge remain
outside the executor's authority.
