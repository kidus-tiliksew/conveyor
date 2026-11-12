---
name: conveyor-coordinate
description: Coordinate several Conveyor tasks or a continuing delivery queue, running each stage order in its own agent thread, recording routine decisions within the scope the operator delegated, and escalating the rest. Use for requests such as "coordinate these tasks", "keep this queue moving", or "manage this fix wave". For working one assigned work order, use conveyor-work; for filing tasks, use conveyor-file-tasks.
---

# Coordinate Conveyor tasks

Citation scope: the REQ, AC, DEC, and component IDs in this guidance, and its
other requirement, design, and reference IDs, cite the Conveyor product's own
design corpus, not the corpus of the factory you work in. Task work cites the
working factory's own confirmed corpus, as the delivered contract directs.

Read and follow
[docs/playbooks/conveyor-coordinate.md](../../../docs/playbooks/conveyor-coordinate.md)
— it is the canonical, tool-neutral playbook for setups, thread prompts,
supervision, delegated decisions, bounded-queue policy and outcomes,
validation honesty, readable timeline text, and the coordination record.

Non-negotiables, restated: the coordinator never claims a work order or edits
a task repository; each stage order runs in its own thread. Verification
threads follow `conveyor-kit-verify`, and plan, implementation, and review
threads follow `conveyor-work`. Threads never start other threads, wait for
verdicts, or record a gate or proposal decision, and an implementation thread
never proposes or revises a document. The one exception: when the review
primary entry is set, one review primary thread per round starts the seat
threads, claims no seat, and submits no verdict. Before each thread start, the
coordinator reads the person's stage preferences with `get_stage_preferences`
for that thread's harness, model, and effort. Review seat N takes seat entry N
or else the review primary entry. The host and the number of tasks run at once
come from the coordinator's own agent memory. It asks about every uncovered
choice in one message with a recommendation for each, saves stage answers with
`set_stage_preference`, and saves host and concurrency answers in agent memory.
It changes a saved choice only on the operator's direct instruction, and it
reports an unavailable host, harness, or model as a blocker. Threads never write
a stage preference. The coordinator records only
routine decisions within the scope the operator delegated, with the operator's
own credential, and brings product, scope, risk, validation-waiver, and
operator-reserved decisions to the operator. The first sentence of every
progress message, decision reason, and review summary says in plain language
what happened and what happens next, with no internal identifiers.
