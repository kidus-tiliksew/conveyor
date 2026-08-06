# Reconciliation: local planning skills

## Signal

- Task: `260806-9f5495`
- Out-of-pipeline commit: `ecc0674665ea6653af899d30b85db14d2ce81a4e`
- Source: <https://github.com/kidus-tiliksew/conveyor/commit/ecc0674665ea6653af899d30b85db14d2ce81a4e>
- Resolution: retained — accepted by amendment as spec §21.60 (v2.20, August 6, 2026)

The commit added `.claude/skills/conveyor-plan/SKILL.md` and
`.claude/skills/conveyor-file-tasks/SKILL.md`. It did not add or modify
`.agents/plugins/marketplace.json`; that manifest predates the signal and is
recorded here because it is part of the local plugin presentation through which
the guidance can be discovered. Later commit
`640f44e3e2925fa336f78a0077557e242034e772` moved the detailed instructions
into `docs/playbooks/conveyor-planning.md` and
`docs/playbooks/conveyor-task-filing.md`, leaving the skills as wrappers. That
relocation does not resolve the authority question below.

## Conflict with accepted authority

The local planning guidance describes an operator-side agent using the REST
API to propose requirements, System Design revisions, and decisions. The
accepted design does not currently grant that surface:

- `conveyor-spec.md` §9 makes planning sessions Conveyor-owned and in-process.
  It names MCP `create_task` plus `submit_spec` as the headless twin; it does
  not name a generic local REST document-planning workflow.
- §4.2 makes requirements versioned and operator-confirmed, requires drift
  reconciliation to propose rather than silently edit, and permits only
  machinery-created lineage.
- §21.58 defines the four document tiers and their propose→confirm lifecycle,
  but does not by itself define an operator-agent authority, authentication
  contract, REST route set, or lineage behavior for local planning.

The commit's proposal-only language preserves an important gate, but it does
not settle who may invoke the routes, which credential class authorizes each
operation, how local deliberation is audited, or how its products converge
with in-product planning and the MCP contract. The presence of either direct
push on `main` is not approval of those missing boundaries.

## Operator decision required

Until the operator selects one of the following outcomes, the local REST
planning workflow must not be relied on as an approved Conveyor planning
surface:

1. **Retain it through an accepted amendment.** The amendment must define the
   local surface's authority, operator-only confirmation boundary,
   authentication and credential class, exact route contract, lineage and
   transcript treatment, and convergence with in-product planning and MCP.
2. **Reject it as conflicting guidance.** A follow-on change should remove or
   revise only the local REST planning instructions. The MCP task-filing
   guidance and the pre-existing marketplace manifest should remain wherever
   they are independently consistent with the accepted design.

This record does not select either outcome, resolve the monitor drift, or
retroactively approve the direct push. It does not revise
`conveyor-spec.md`, any confirmed requirement, System Design document, or
approved specification. Any normative revision must be separately proposed
and operator-confirmed through the accepted workflow.
