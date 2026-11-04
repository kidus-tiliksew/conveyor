---
name: conveyor-file-tasks
description: File implementation work on the Conveyor factory — single tasks or dependency-ordered task sets — via the MCP create_task tool, in the house style the factory's agents implement and review well. Use when the operator says to file a task, a fix wave, or a phase's work breakdown.
---

# Filing Conveyor tasks

Citation scope: the REQ, AC, DEC, and component IDs in this guidance, and its
other requirement, design, and reference IDs, cite the Conveyor product's own
design corpus, not the corpus of the factory you work in. Task work cites the
working factory's own confirmed corpus, as the delivered contract directs.

Read and follow [docs/playbooks/conveyor-task-filing.md](../../../docs/playbooks/conveyor-task-filing.md)
— the canonical, tool-neutral playbook (body house style, boundaries,
exit criteria, dependency ordering).

Non-negotiables, restated: phase-sized work is never one task — file a
dependency-ordered set; siblings declare file ownership and migration-
number starts; no acceptance criterion may require an operator-only act
(use checkpoint phrasing instead). Before filing, search active and recent
terminal tasks with `list_tasks` repository and text filters, and when a match
exists report its ID and state to the operator and file only on the operator's
direction. Immediately before `create_task`, re-read every cited DEC and
document version, replace or drop superseded or archived citations, and when a
confirmed requirement forbids the requested behavior file no implementation
task and offer the operator a requirement revision with `conveyor-plan`.
The confirmed factory document corpus is
the authority: cite confirmed REQ-n/AC-n.m, DEC-n, and governing System Design
document names or IDs, and never direct a task to propose or revise a document;
a document change goes through `conveyor-plan` and is confirmed first. Follow the target repository's own filing and
generated-artifact guidance, and never ban a directory whose generated output
the change must regenerate; if implementation discovers an approved-plan
conflict, require the operator-gated `request_plan_revision` path rather than
an acceptance-criteria exception.
Write task bodies for people and agents who never saw the filing session:
attribute actions and observations to people by name, never personal host aliases,
SSH config names, home-directory paths, or IP addresses; carry evidence inline or
cite an uploaded artifact or a PR/task/event reference; never point to a file only
one machine can read or require preserving or relying on filer-only machine state
(DEC-28).
