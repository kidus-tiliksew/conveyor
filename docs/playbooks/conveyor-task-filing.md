# Filing Conveyor tasks

Tasks are the factory's only transition object. The body you write is the
contract the spec, implement, and review agents are held to — write it
for them, not for the operator.

## Mechanics

- File via MCP `create_task`: `body`, `repo`, `idempotency_key`
  (required — kebab slug + date, e.g. `phase8-3-plan-stage-20260807`),
  optional `depends_on: [task-ids]`, `hold`, `spec_approval`,
  `merge_approval`. Defaults are correct for ordinary work (spec gate on,
  merge gate off).
- The title is generated from the body; don't write one.
- **Phase-sized work is never one task.** File a dependency-ordered set —
  each task one coherent slice with its own exit criterion, `depends_on`
  expressing the real order. (Lesson: 8.2 shipped as a single 11-item
  task → one 4-hour session, whole-phase review, migration collision.)
- Parallel tasks collide on the shared namespaces nobody owns. State it
  explicitly in each body: which files each sibling owns ("Do NOT touch X
  — sibling task NNN owns it") and where migration numbering starts
  ("migrations start at NNN; the duplicate-version guard enforces this").

## Body house style

Structure that has survived contact with the agents:

1. `# <imperative summary>` then one paragraph: what was observed or
   decided, with evidence (task/PR/review references) — the why.
2. Numbered contract items, most important first. Each: the defect or
   goal, `file.go:line` anchors, a concrete failure scenario or behavior
   spec, and the fix **shape** (constrain the approach only where it
   matters; leave the rest to the spec stage).
3. `## Boundaries` — what must NOT change. Always include the standing
   ones that apply: both gates untouched; no priority/assignee/
   phase fields; events append-only; propose→confirm authority unchanged;
   drift is a signal never a gate; conformance suites extend, never
   shrink; new edge kinds ship with projector + two-store conformance +
   vocabulary lockstep + rebuild proof on day one.
4. `## Exit` — observable, agent-satisfiable criteria ending with the
   gates: `make test` (and `make test-integration` when stores/migrations
   are touched) green.

## Authority boundary in acceptance criteria

Never write a criterion requiring an operator-only act (gate approval,
drift resolution, confirming decisions/designs/requirements, cancel/
hold). The implementing agent can neither perform nor verify those. If
the work genuinely needs one mid-flight, phrase it as an operator
checkpoint — "pause and report until the operator has done X; the pause
is the success condition" — or scope it out. (Lesson: task 260805-b8b13c
looped through three doomed recoveries on an AC only an operator could
satisfy.)

If implementation reveals that the approved execution plan conflicts with
repository reality, direct the implementer to call the operator-gated
`request_plan_revision` surface. Never turn the conflict into an acceptance-
criteria exception or authorize a prose-only departure from the approved plan.

## Source boundaries

For web-only work, state that regenerating `internal/httpapi/dashboard` is
expected build output of the web change under DEC-16. Never impose a blanket
"no changes under `internal/**`" boundary; exclude unrelated internal source
explicitly without excluding the generated dashboard bundle.

## Citations and context

- The confirmed factory document corpus is the authority for new work. Cite
  confirmed REQ-n/AC-n.m, DEC-n, and the governing System Design document name
  or ID. Name the System Design
  document governing the task's paths and say whether the change alters
  the documented mechanism (if yes, the agent should propose the design
  revision in-session; if no, say no revision is warranted — this steers
  the drift/suppression outcome).
- Attach the requirements a task serves directly at intake with
  `requirement_ids` (or a planning-bundle task's `context.requirement_ids`).

## After filing

Tell the operator which gates will come to them and in what order. Don't
watch the queue unless asked — the factory notifies through its own
surfaces.
