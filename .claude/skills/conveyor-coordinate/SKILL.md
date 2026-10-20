---
name: conveyor-coordinate
description: Coordinate several Conveyor tasks or a continuing delivery queue, running each stage order in its own agent thread, recording routine decisions within the scope the operator delegated, and escalating the rest. Use for requests such as "coordinate these tasks", "keep this queue moving", or "manage this fix wave". For working one assigned work order, use conveyor-work; for filing tasks, use conveyor-file-tasks.
---

# Coordinate Conveyor tasks

Read and follow
[docs/playbooks/conveyor-coordinate.md](../../../docs/playbooks/conveyor-coordinate.md)
— it is the canonical, tool-neutral playbook for setups, thread prompts,
supervision, delegated decisions, validation honesty, readable timeline text,
and the coordination record.

Non-negotiables, restated: the coordinator never claims a work order or edits
a task repository; each stage order runs in its own thread under
`conveyor-work`. Threads never start other threads, wait for verdicts, or
record a gate or proposal decision. The coordinator records only routine
decisions within the scope the operator delegated, with the operator's own
credential, and brings product, scope, risk, validation-waiver, and
operator-reserved decisions to the operator. The first sentence of every
progress message, decision reason, and review summary says in plain language
what happened and what happens next, with no internal identifiers.
