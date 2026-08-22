# Tasks

A task is the unit of intended change: one branch, one eventual pull
request, one thread of judgment from intent to merge. This page follows a
task through its whole life: creation, context, governance, execution,
review, and the links it leaves behind in the knowledge graph.

Four words are used precisely throughout:

- A **task** is the durable unit of intended change.
- A **job** is one execution of one pipeline stage for that task.
- A **work order** is the protocol boundary handed to an agent for one stage.
- An **attempt** is one successful claim of a work order.

## Creating tasks

Every creation route funnels into one intake path, so a task made from the
dashboard, the CLI, an agent's `create_task` call, the monitor, or a
staleness follow-up all behaves identically:
context is validated, the workspace execution setup is frozen onto the task,
gates are resolved, the title is generated, and triage is enqueued.

The routes:

- The dashboard's New task sheet: description, repository, base branch,
  attached requirements and designs, dependencies, hold, and the two gates.
- `conveyor task new` from the CLI.
- The `create_task` MCP tool, which requires `body`, `repo`, and a
  caller-stable `idempotency_key`. Reusing the key with an identical request
  returns the original task; reusing it with a different request is a
  conflict. The tool is reserved for human credentials; a dispatched agent
  cannot file tasks.
- The monitor and staleness follow-ups, which file ordinary gated tasks; see
  [Misalignment](misalignment.md).

A new task gets an ID like `260822-2bc3b1` (date plus random hex) and a
branch assignment `conveyor/task-<id>`. The branch is an assignment, not a
ref; nothing is created in Git until an agent checks out.

## Ordering, blocking, and eligibility

Three independent levers, none of which is priority:

`depends_on` names open tasks that must merge first. A task is blocked while any dependency is not `merged`. A
dependency that closes without merging is flagged unsatisfiable, a dead end
an operator resolves by unlinking it (with a reason) or closing the
dependent. Time spent blocked does not burn the work order's queue clock.

`hold` reserves a task from everyone's workers. A held task sits in the queue
until a person attaches an agent and claims it themselves, typically with
`conveyor run`. Hold changes nothing else about the task.

An assignee narrows claim eligibility to one person: assigned tasks are
claimable only by the assignee, unassigned tasks are first-come. Assignment
never affects queue order, and the assignee must be a workspace member whose
role can claim work. Otherwise, queued work is served oldest-first, with
review seats taking precedence.

## Triage

Every task passes first through triage, an in-process stage with a small
tool budget and read-only access to the corpus. It classifies the task (bug,
feature, chore), routes it `proceed` or `parked`, and writes an advisory
brief: open questions, affected areas, risks. It may suggest requirement and
design attachments, but only as proposals an operator confirms or dismisses;
triage never attaches context itself, and it never picks the next stage. The
gates frozen at intake decide that: `spec` if plan approval is on, otherwise
straight to `implement`. If triage exhausts its budget, the task proceeds
with a neutral verdict rather than parking, so a flaky model cannot wedge
intake.

## Work orders

A work order is what an agent actually claims: one stage of one task, with
explicit state (`queued`, `claimed`, `submitted`, `completed`, `cancelled`,
`stale`, `timed_out`) and three independent clocks:

- The **queue clock**: an unclaimed order goes stale after
  `work_order_queue_timeout` (default 24h) and needs an explicit redispatch.
- The **execution clock**: a fixed deadline starting at the first successful
  claim, from the stage's configured timeout. Nothing extends it.
- The **lease**: a claim holds a renewable lease (default 5 minutes, up to an
  hour). Renewal keeps the claim alive but never moves the execution
  deadline, so a wedged agent cannot hold a task forever.

Spec and implement stages get one order per dispatch. Review gets one order
per configured review seat, so a two-seat setup means two independent
reviews per round.

Nothing is pushed to agents. Two launchers pull: the durable worker
(`conveyor worker run`), which polls, claims eligible orders, and supervises
harness children from its local execution setup; and `conveyor run
<task-id>`, where a person claims and executes stages interactively. Both
launch the agent with its work-order identity and task assignment in the
environment and the Conveyor MCP endpoint configured.

## What a work order carries

`get_work_order` returns the assembled context for the stage, built in
layers:

- The stage's role prompt from the pack, plus any operator direction
  supplied at recovery.
- Served requirements: the REQ and AC statements of every confirmed
  requirement attached to the task, with the citation contract that tells
  implementation to cite IDs in code comments and tells review to classify
  its citations.
- The governance snapshot: full content of the confirmed System Design
  versions governing the task (pinned versions where an attachment pinned
  one), plus every confirmed and superseded decision, under a 64 KiB budget
  that reports anything it had to drop.
- The approved execution plan, whose `## Done criteria` become the
  implementation checklist and the review coverage contract.
- The triage brief, bounce history, and prior feedback.
- Bounded lineage context: neighboring nodes in the knowledge graph, each
  item tagged with the edge path and event that justified serving it, and
  artifact references readable through the scoped `read_artifact` tool.
- For review orders: the diff, the PR description, and task-owned
  verification evidence.

Two properties are worth knowing. For review orders, requirements and
governance are snapshotted at claim time and the verdict is validated against
exactly that snapshot, so authority cannot shift mid-review. And every layer
of lineage-derived content is labeled untrusted data with an explicit
instruction not to follow commands found inside it.

Repositories can ship advisory hints in `.conveyor/hints.yaml` (verification
commands, area ownership). Hints are always labeled advisory and never
override frozen workspace or plan authority.

## The execution loop

The full agent-facing discipline is the
[work playbook](playbooks/conveyor-work.md); this is the shape of it.

An implement agent claims the order with a fresh session, reads the context,
and resolves its dedicated worktree:

```sh
conveyor checkout <task-id>
```

That yields a sibling worktree on the assigned branch (default under
`~/.conveyor/worktrees`), created and reused with hard safety checks and
never by rewriting history. Every edit, test, commit, and push happens there;
the primary checkout is never touched. Spec-stage orders are the exception:
they run read-only and never check out.

If the task has a plan gate, the spec stage comes first: the agent submits a
Markdown plan with required headings (Approach, Files touched, Ordering,
Risks, Done criteria). With the gate on, an operator approves or rejects the
plan; approval also publishes a GitHub issue carrying the approved spec.

Implementation works the plan, validates through the repository's Make
targets, walks the done criteria and acceptance criteria, commits, pushes the
exact assigned branch, and calls `submit_for_review`. Conveyor then opens or
reuses the pull request (as the executing user's GitHub identity) and
dispatches the review round. Agents never open PRs themselves, and a
successful submit ends the session.

Each review seat claims with a session and client token that must be
independent of the implementer and of every other seat; self-review is
refused at claim. A verdict is not a vibe: it must carry a reason code, a
summary, requirement citations classified against the pinned snapshot, done
criteria coverage quoting the plan's criteria verbatim, and a governance
assessment naming which designs and decisions were applicable, cited,
unknown, or superseded. Malformed assessments are rejected and the order
stays claimed for correction.

An approved round moves the task to the merge gate (or straight to merge if
the gate is off). `changes_requested` bounces: the feedback is recorded, and
a new implementation order is created in a fresh session with that feedback
delivered as context. The bounced order is never revived. Bounces are counted
since the last human intervention, and hitting `max_bounces` (default 10)
parks the task at a human gate rather than failing it.

Merged and closed are terminal. After either, `conveyor done <task-id>`
removes the local worktree, keeping the branch.

## Checkpoints, plan revision, recovery

Two escape hatches let an agent stop without failing:

An **operator checkpoint** is for authority conflicts: the plan collides with
confirmed corpus authority, or an approved criterion explicitly requires an
operator decision. The agent first authors the relevant proposals
(requirement, design, or decision revisions), then releases the order with a
structured decision request citing the documents and the pending proposal
IDs. The release is a successful handoff, and the enriched citations show the
operator exactly which document versions are involved and what is already
pending.

A **plan revision request** is for repository reality: the approved plan
cannot be executed as written. The agent releases with a rationale and the
task waits at an operator gate for a revised plan.

Around those, the lifecycle is built to survive failure. Attempts that die
leave their work: a successor adopting a dirty predecessor worktree commits
it as a `wip(<attempt-id>)` checkpoint and pushes before continuing, so no
work is silently lost, and checkpoint commits are marked as preservation,
never as validation evidence. Failed attempts retry on a bounded schedule,
with connectivity failures backed off separately and identical consecutive
failures suppressed. Stale queued orders need an explicit redispatch;
interrupted review rounds can be recovered seat by seat, keeping completed
verdicts; merge conflicts get a dedicated fix dispatch with merge-not-rebase
instructions and a refresh review afterward.

## The knowledge graph

Every step above leaves edges. Lineage is projected from the append-only
event log, never asserted directly: a `task.created` event, a
`pull_request.opened` event, a `review.completed` event each project their
edges, and the whole graph can be rebuilt from events at any time
(`conveyor lineage rebuild`).

The node types span the factory: requirements and their versions, System
Designs and their versions, decisions, reference documents, repository
paths, tasks, work orders, pull requests, commit ranges, evidence, and
verdicts. The edges say what happened:

```
requirement       --serves-->          task
system_design_version --governs-->     task, repository_path
task              --depends_on-->      task
task              --dispatches-->      work_order
work_order        --produced_verdict--> verdict  <--supports-- evidence
task              --submitted_as-->    pull_request
task              --merged_range-->    commit_range
system_design_version / decision --proposed_by--> task
```

So a merged change answers, from the graph alone: which requirement versions
it served, which designs governed it and whether it consulted them, and who
reviewed it and what evidence supported the verdict. The same graph feeds
forward: it is the
source of the bounded lineage context served into future work orders, so
related work arrives already knowing its neighbors.

Traversal is always budgeted (depth, nodes, links) and reports what it left
out, because an unbounded graph walk is how context windows and API latencies
die. The dashboard's knowledge explorer is the same bounded traversal,
rendered.

## Observability

Every claim mints an attempt ID, and the order tracks the current and last
attempt, outcome, failure category and bounded detail, retry counts, and
pacing. Live activity from a running attempt is surfaced on the task
timeline.

Transcripts arrive on three channels: agents may self-report one through
`upload_transcript` (redacted, size-capped, stored as an audit artifact);
launchers capture a bounded termination transcript at attempt death; and
in-process stages persist their full model transcript content-addressed.
Redaction counts are recorded alongside.

Agents self-report token and cost usage through `report_usage`. Usage is
observational telemetry only: it never gates claims, progress, or
submissions, and a reported zero is distinguishable from a session that
never reported. In-process stages record exact token counts and priced cost.
The task timeline rolls all of it up per attempt and per task.

The substrate under all of this is the append-only event ledger, on the
order of 130 event kinds, streamable per task over SSE. The task activity
endpoint returns the whole picture in one read: jobs, events, work orders
with checkpoints and transcripts, interventions, review diagnostics, merge
readiness, and attention state.
