---
name: conveyor-file-tasks
description: File implementation work on the Conveyor factory — single tasks or dependency-ordered task sets — via the MCP create_task tool, in the house style the factory's agents implement and review well. Use when the operator says to file a task, a fix wave, or a phase's work breakdown.
---

# Filing Conveyor tasks

Read and follow [docs/playbooks/conveyor-task-filing.md](../../../docs/playbooks/conveyor-task-filing.md)
— the canonical, tool-neutral playbook (body house style, boundaries,
exit criteria, dependency ordering).

Non-negotiables, restated: phase-sized work is never one task — file a
dependency-ordered set; siblings declare file ownership and migration-
number starts; no acceptance criterion may require an operator-only act
(use checkpoint phrasing instead). The confirmed factory document corpus is
the authority: cite confirmed REQ-n/AC-n.m, DEC-n, and governing System Design
document names or IDs. Every normative document change remains a proposal for
operator confirmation. For web-only work, allow the DEC-16 generated
`internal/httpapi/dashboard` bundle rather than banning `internal/**`; if
implementation discovers an approved-plan conflict, require the operator-gated
`request_plan_revision` path rather than an acceptance-criteria exception.
Write task bodies for people and agents who never saw the filing session:
attribute actions and observations to people by name, never personal host aliases,
SSH config names, home-directory paths, or IP addresses; carry evidence inline or
cite an uploaded artifact or a PR/task/event reference; never point to a file only
one machine can read or require preserving or relying on filer-only machine state
(DEC-28).
