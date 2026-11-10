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
context is validated, the workspace pipeline policy is frozen onto the task,
the plan state and merge gate are resolved, the title is generated, and triage
is enqueued.

The routes:

- The dashboard's New task sheet: description, repository, base branch,
  attached requirements and designs, dependencies, a Plan state picker, an
  optional execution plan with its base commit and an Approve this plan
  checkbox for members with `operate_gates`, and the merge approval gate.
- `conveyor task new` from the CLI.
- The `create_task` MCP tool, which requires `body`, `repo`, and a
  caller-stable `idempotency_key`. Reusing the key with an identical request
  returns the original task; reusing it with a different request is a
  conflict. An agent session files as the person whose credential it holds,
  and only when that person's role grants `create_tasks`; the task records
  MCP as its source and the person as its creator (DEC-116(5)). The session
  may state either a `plan_state` or a plan, never both, and may state
  `merge_approval`, including `false`. It never approves a plan: a filed plan
  waits at the plan approval gate for a person (DEC-125(6)).
- The monitor and staleness follow-ups, which file ordinary gated tasks; see
  [Misalignment](misalignment.md).

A new task gets an ID like `260822-2bc3b1` (date plus random hex) and a
branch assignment `conveyor/task-<id>`. The branch is an assignment, not a
ref; nothing is created in Git until an agent checks out.

## Plan state

Every task carries exactly one plan state. It decides whether the task needs
an execution plan before implementation, and implementation starts only from
no plan required or planning completed (`req-task-plan-state` REQ-1;
DEC-125(5)).

| Plan state | Wire value | Meaning |
|---|---|---|
| No plan required | `not_required` | The task body is the statement of done, and triage sends the task to implementation. |
| Planning needed | `needed` | A plan work order is open or will open; an agent writes the execution plan. |
| Plan awaiting approval | `awaiting_approval` | A plan version waits at the plan approval gate for a person. |
| Planning completed | `completed` | A person approved the current plan version; implementation works from it. |

**At intake.** A request states a plan state of `needed` or `not_required`
only; a task reaches plan awaiting approval or planning completed at intake
only by filing a plan. When the request states no plan state and files no
plan, the task takes the workspace's `execution.default_plan_state` (AC-2.1).
The legacy `spec_approval` request field and workspace key are aliases: `true`
reads as `needed` and `false` as `not_required`. The workspace key supplies
the default only when `default_plan_state` is absent, and the default is
`needed` when neither key is set. A request that sends both `plan_state` and
a `spec_approval` with a different meaning is refused; send only
`plan_state`. Either one wins over a legacy `level`, where L0 and L1 read as
`not_required` and L2 and L3 as `needed`. The workspace default applies only
when the request states none of these. See [Configuration](configuration.md)
for the workspace default.

A task can also be filed with an execution plan. The plan must pass the same
content rules as a plan a plan work order submits, and an invalid plan is
refused with nothing recorded. A valid plan becomes plan version 1 and the
task starts at plan awaiting approval (AC-2.2). A request that files a plan
states no plan state, because the plan sets it, and a `spec_approval` of
`false` with a plan is refused (AC-2.6). The request may name
the commit the plan was read against, which must be a full commit ID (AC-2.5).
Only a REST request can approve the plan it files: when it states the plan
approved, Conveyor records a plan approval under the filer's credential, which
needs `operate_gates`, and the task starts at planning completed (AC-2.3). MCP
`create_task` never approves a plan, so a plan an agent session files waits
for a person (AC-2.4; DEC-125(6)).

**After intake.** Only these acts move a task's plan state (REQ-3):

- A plan work order submits a plan. It becomes the next plan version, and the
  task moves to plan awaiting approval (AC-1.2).
- A person approves the plan at the plan approval gate, which moves the task
  to planning completed. A redirect at the gate moves the task back to
  planning needed and delivers the direction to the next plan work order
  (AC-1.3).
- An approved plan revision request returns the task to planning needed
  (AC-3.4). A request that planning start on a task with no plan required
  does the same when an operator approves it (AC-3.3); see
  [Checkpoints, plan revision, recovery](#checkpoints-plan-revision-recovery).
- An operator sets the task to planning needed or no plan required, with a
  required reason. Conveyor records the old and new state, opens or cancels
  the task's plan work order to match, and keeps every earlier plan version
  readable (AC-3.1).
- An operator attaches an execution plan to a task at no plan required or
  planning needed. It becomes the next plan version, the task moves to plan
  awaiting approval, and any open plan work order is cancelled (AC-3.2).

Both operator acts need `operate_gates` and are refused while an
implementation work order of the task is claimed. A change to the workspace
default never moves an existing task's plan state (AC-3.5; DEC-125(7)).

**Plan versions.** Each plan version records its origin: `plan_order` for a
plan a plan work order submitted, `intake` for a plan filed with the task, and
`operator` for a plan an operator attached. A version also records the commit
the plan was read against when its author supplied one. The implementation
work order of a task at planning completed carries the approved plan version,
its recorded commit or a statement that none was recorded, and the time of
the approval (REQ-4, AC-4.1).

The CLI commands are `conveyor task new` with `--plan-state`, `--plan-file`,
`--plan-base`, and `--approve-plan`; `conveyor task plan-state`; and
`conveyor task attach-plan`. Their flags are in the [CLI reference](cli.md#task).
On the dashboard, the plan section of task detail offers the same two
operator acts, Change plan state and Attach plan, to members with
`operate_gates`.

## Ordering, blocking, and eligibility

Three independent levers, none of which is priority:

`depends_on` names open tasks that must merge first. A task is blocked while any dependency is not `merged`. A
dependency that closes without merging is flagged unsatisfiable, a dead end
an operator resolves by unlinking it (with a reason) or closing the
dependent. Time spent blocked does not burn the work order's queue clock.

An operator can add the same ordering constraint after both tasks exist with
`POST /v1/tasks/{id}/dependencies`, `conveyor task link`, or the MCP
`add_task_dependency` tool. The operation requires the dependency task ID, an
audit reason, and a caller-stable request ID. Both tasks must be open in the
same workspace; self-links and cycles are rejected. A retry with the same
request is idempotent, and an already-present dependency returns success
without another audit event. A late dependency pauses queued implementation
orders in place and affects future claims, but never interrupts claimed work.

A task carries no hold or other reservation (DEC-125(3)): dependencies and
assignment are the only narrowing of who may claim its work. Intake refuses a
new `hold: true` with `task_hold_retired`, and `PUT /v1/tasks/{id}/hold`
answers 410. Hold values and `task.hold.*` events recorded before the
retirement stay readable as history and affect nothing. A request-changes
bounce re-queues the implementation order like any other: the agent session
that implemented it claims the successor order, or any eligible member's
session can claim it.

An assignee narrows claim eligibility to one person: assigned tasks are
claimable only by the assignee, unassigned tasks are first-come. Assignment
never affects queue order, and the assignee must be a workspace member whose
role can claim work. Otherwise, queued work is served oldest-first, with
review seats taking precedence.

The dashboard's **Start over** action is available on non-terminal tasks to
members with `operate_gates`. Its dialog requires a reason (up to 200
characters), accepts an optional note for the next session (up to 2000), and
previews work-order cancellation and the known pull request to close. Starting
over decides no document proposal: a pending version stays pending and stays
decidable by an operator with `confirm_documents`. A retry reuses the request ID and input so it returns the same successor. The
new task starts on a new branch with the same body and pins, carries the
operator's reason and note, and links back to the retired task; both have a
`restarted` marker. The successor has no plan version. Its plan state is no
plan required when the retired task's was, and planning needed otherwise; a
plan the retired task had approved comes along as reference context only,
never as an approval. The retired task links forward and shows the recorded PR
close outcome. Forge closure can retry or fail without undoing the restart.

## Triage

Every task passes first through triage, an in-process stage with a small
tool budget and read-only access to the corpus. It classifies the task (bug,
feature, chore), routes it `proceed` or `parked`, and writes an advisory
brief: open questions, affected areas, risks. It may suggest requirement and
design attachments, but only as proposals an operator confirms or dismisses;
triage never attaches context itself, and it never picks the next stage. The
task's [plan state](#plan-state) decides that when triage proceeds
(`req-task-plan-state` AC-1.1). A task at no plan required or planning
completed goes to implementation. A task at planning needed gets a plan work
order. A task at plan awaiting approval waits at the plan approval gate. If
triage exhausts its budget, the task proceeds
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

Spec, implement, and verify stages get one order per dispatch. Review gets one order
per review seat in the frozen policy, so a two-seat policy means two independent
reviews per round.

Nothing is pushed to agents, and Conveyor launches none (DEC-97). You start
an agent session in your own harness, such as Claude Code, Codex, Cursor, or
OpenCode, with the Conveyor MCP connection and skills installed, and ask it to
work a task. Following the `conveyor-work` skill, the session claims the
task's current order over MCP, resolves the task worktree with
`conveyor checkout`, does the stage's work, and submits. After an
implementation submission it starts an isolated verifier or reviewer agent for
each verification or review order, awaits the verdict, claims any
changes-requested successor, and offers each human gate to you in the
conversation. You choose the harness and model when you start the session;
Conveyor stores no execution setup (DEC-104). To keep several tasks moving, ask
a session to coordinate them with the `conveyor-coordinate` skill.

## What a work order carries

`get_work_order` returns the assembled context for the stage, built in
layers:

- The stage's role prompt from the pack, plus any operator direction
  supplied at recovery.
- Served requirements: the REQ and AC statements of every confirmed
  requirement attached to the task, with the citation contract that tells
  implementation to cite IDs in code comments and tells review to classify
  its citations.
- The governance lists: every governing System Design by ID, title, and
  version (pinned versions where an attachment holds one and, for a review or
  verify order, the versions pinned at its claim), attached documents first,
  and every confirmed decision by ID with a one-line
  summary. Bodies stay on the server; sessions read them through
  `search_documents` and `get_document` (see
  [Work order delivery](mcp.md#work-order-delivery)).
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

If the task's plan state is planning needed, the plan stage comes first: the
agent submits a Markdown plan with required headings (Approach, Files touched,
Ordering, Risks, Done criteria). No submission approves a plan. The task moves
to plan awaiting approval, and a person approves the plan or redirects it at
the plan approval gate. When the workspace's `issue_publication.enabled`
setting is on, approval also publishes a GitHub issue carrying the approved
plan.

A task implements confirmed documents and proposes none of its own. Requirement
and design proposals a task authored before that rule wait for an operator on
the dashboard's pending proposals page, and the working session offers them to
you in the conversation. Review waits until those proposals are confirmed or
dismissed; a reviewer whose claim a proposal blocks is refused and claims
again later. A credential without confirmation capability sees who can act.

Implementation works the plan, validates through the repository's Make
targets, walks the done criteria and acceptance criteria, commits, pushes the
exact assigned branch, and calls `submit_for_review`. Conveyor then opens or
reuses the pull request (as the executing user's GitHub identity) and
dispatches the review round. Agents never open PRs themselves. A successful
submit ends the implementation claim, and the session continues the delivery
loop in the `conveyor-work` playbook.

When the task's frozen `verify_stage` policy is on, a verify order sits
between submission and review. A verifier claims it in a fresh session,
prepares a verification context bound to the exact submitted head, runs the
repository's selected [verification kits](playbooks/conveyor-kit.md) and any
ordinary checks, and submits typed evidence that is sealed for review. Actions
that need operator authority wait for a claim-bound grant
(`conveyor verification permissions`). Review is not dispatched until a
completed verify order exists for the submitted head. A passing kit is
evidence only, never approval. With the policy off, submission goes straight
to review. The [verification playbook](playbooks/conveyor-kit-verify.md) has
the full protocol.

Each review seat claims with a session and client token that must be
independent of the implementer and of every other seat; self-review is
refused at claim. A verdict is not a vibe: it must carry a reason code, a
summary, requirement citations classified against the pinned snapshot, done
criteria coverage quoting the plan's criteria verbatim, and a governance
assessment naming which designs and decisions were applicable, cited,
unknown, or superseded. Malformed assessments are rejected and the order
stays claimed for correction.

An approved round moves the task to the merge gate (or straight to merge if
the gate is off). At the gate, the operator's merge is a single act: Conveyor
checks the pull request's readiness and merges it, whether the act comes
from the dashboard inbox or `conveyor task merge`. `changes_requested` bounces: the feedback is recorded, and
a new implementation order is created in a fresh session with that feedback
delivered as context. The bounced order is never revived. Bounces are counted
since the last human intervention, and hitting `max_bounces` (default 10)
parks the task at a human gate rather than failing it.

Merged and closed are terminal. After either, `conveyor done <task-id>`
removes the local worktree, keeping the branch.

## Checkpoints, plan revision, recovery

An operator can start an open task over with `POST /v1/tasks/{id}/restart`,
providing `request_id`, a required `reason` of at most 200 characters, and an
optional `note` of at most 2000 characters. The request cancels the task and its
non-terminal work orders and creates one linked successor carrying its
body, repository, base branch, open dependencies, attached context, frozen
policy, and previous approved plan as reference context. The successor has
no plan version and starts at no plan required when the retired task did,
otherwise at planning needed. The successor starts
at triage on a new assigned branch and receives the reason and note as operator
direction on its first work order. The API requires `operate_gates`. Pending
document proposals stay pending for an operator with `confirm_documents` to
decide. An identical request ID replays the original result; terminal tasks otherwise require
`POST /v1/tasks`. This API never resets or changes the retired branch or
worktree and does not close its pull request.

Two escape hatches let an agent stop without failing:

An **operator checkpoint** is for authority conflicts: the work would require
changing a confirmed document, or an approved criterion explicitly requires an
operator decision. The agent authors no document version. It releases the order
with a structured decision request that describes the conflict and citations of
the confirmed clauses in conflict. The release is a successful handoff, and the
enriched citations show the operator exactly which document versions are
involved and whether a revision is already pending. Once a confirmed revision
answers every citation, the factory resumes the order.

A **plan revision request** is for repository reality: the approved plan
cannot be executed as written while confirmed documents stand. The agent releases with a rationale and the
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
event log, never asserted directly: the writer of each event, such as
`task.created`, `pull_request.opened`, or `review.completed`, projects that
event's edges in the same transaction. `conveyor lineage rebuild` replays the
log to regenerate every edge kind the projector owns. Rebuild keeps rows it
cannot regenerate and reports them as `preserved_unregenerable`, and it leaves
rows of other kinds, such as retained historical feature assignments, in
place.

The node types span the factory: planning sessions and bundles, requirements
and their versions, System Designs and their versions, reference documents
and their versions, decisions, repository paths, blueprints and their plan
versions, tasks, work orders, pull requests, commit ranges, evidence, and
verdicts. The edges say what happened:

```
requirement           --serves-->           task, blueprint
requirement_version   --serves-->           task
requirement           --versions-->         requirement_version
system_design         --versions-->         system_design_version
reference_document    --versions-->         reference_document_version
requirement_version / system_design_version / reference_document_version / decision
                      --supersedes-->       its predecessor
requirement_version   --derived_from-->     reference_document_version
system_design_version --governs-->          task, repository_path
system_design_version / decision --proposed_by--> task, planning_session
planning_session      --produced_requirement / produced_design / produced_bundle / produced_blueprint--> what it produced
planning_bundle       --proposes-->         requirement_version, system_design_version, decision
planning_bundle       --creates-->          task
blueprint             --plans-->            task
blueprint             --versions-->         blueprint_version
blueprint_version     --materializes-->     task
task                  --depends_on-->       task
task                  --succeeded_by-->     task
task                  --dispatches-->       work_order
work_order / task / planning_session --consulted--> system_design_version
planning_session      --consulted-->        reference_document_version
work_order            --produced_verdict--> verdict  <--supports-- evidence
verdict / requirement_version / system_design_version --cites--> decision
verdict               --flags_superseded--> decision
task                  --submitted_as-->     pull_request
task                  --submitted_range / merged_range--> commit_range
```

A requirement attachment records the confirmed version current when it is
attached, so `requirement_version --serves--> task` names the version a
delivery served. Attachments recorded before that field existed carry only
`requirement --serves--> task`. `blueprint --plans--> task` joins a task's
plan side (its planning session and plan versions) to its delivery side (its
work orders, pull requests, and ranges). `task --succeeded_by--> task` links a
task that was started over to its replacement. A verdict cites the confirmed
decisions its governance assessment names and flags the superseded ones it
found, and a confirmed requirement or System Design version cites the
decisions its content names, so a decision reaches the documents and reviewed
deliveries that rely on it (DEC-90).

So a merged change answers, from the graph alone: which requirement versions
it served (for attachments that recorded one), which designs governed it and
whether it consulted them, which planning session produced it, and who
reviewed it and what evidence supported the verdict. The same graph feeds
forward: it is the
source of the bounded lineage context served into future work orders, so
related work arrives already knowing its neighbors.

Traversal is always budgeted (depth, nodes, links, and per-node fan-out) and
reports what it left out, axis by axis, because an unbounded graph walk is how
context windows and API latencies die. A popular node, such as a requirement
that serves many tasks, contributes at most its fan-out, so a task's own work
orders, verdicts, pull requests, and ranges arrive ahead of the siblings
reached through it. Each store reads only the links of the nodes the walk
reaches and returns at most a fixed number per node, so a walk's cost does
not grow with unrelated parts of the workspace. A review or verify order
shows each served requirement at the version its claim pinned. The
dashboard's knowledge explorer is the same bounded traversal, rendered.

Operators query the graph through three read-only surfaces, all bounded:

- `conveyor lineage show <type> <id>` (`GET /v1/lineage/{type}/{id}`, MCP
  `get_lineage`) walks from one node. `--direction out|in|both`, `--kind`, and
  `--node-type` narrow the walk; a link they refuse still counts against its
  node's fan-out, so a filter never widens store reads. Each node carries its
  depth, its `edge_path`, and its status: task or work-order state, version
  status (`current`, `superseded`, `proposed`, `dismissed`), or verdict outcome,
  round, and seat. This answers "what delivered this requirement, who reviewed
  it, against which design version?"
- `conveyor lineage impact <requirement|system_design> <id>`
  (`GET /v1/lineage/{type}/{id}/impact`) lists every task each version served
  or governed, with task state, version status, and merged commit ranges.
  `--outdated` keeps merged tasks built on a superseded version. This answers
  "if I change this document, which merged work was built on the old
  version?" A task whose attachment recorded no version is listed as
  `unrecorded` and never counted as outdated. The read covers at most 1000
  version-to-task links and pages them.

Walk defaults are the agent context budget. The operator read budget
(`execution_settings.control_plane.planning.context.operator_read_depth` and
`operator_read_nodes`, at most 16 and 1024) raises only the caps an operator
read may ask for; agent context assembly keeps its own budget.

## Observability

Every claim mints an attempt ID, and the order tracks the current and last
attempt, outcome, failure category and bounded detail, retry counts, and
pacing. Live activity from a running attempt is surfaced on the task
timeline.

Transcripts arrive on two channels: agents may self-report one through
`upload_transcript` (redacted, size-capped, stored as an audit artifact), and
in-process stages persist their full model transcript content-addressed. The
control plane captures no harness output of its own (DEC-98).
Redaction counts are recorded alongside.

Agents self-report cumulative token usage through `report_usage`. Usage is
observational telemetry only: it never gates claims, progress, or
submissions, and a reported zero is distinguishable from a session that
never reported. Conveyor collects no cost; a legacy `cost_usd` argument is
accepted and ignored. In-process stages record exact token counts.
The task timeline rolls all of it up per attempt and per task.

The substrate under all of this is the append-only event ledger, on the
order of 130 event kinds, streamable per task over SSE. The task activity
endpoint returns the whole picture in one read: jobs, events, work orders
with checkpoints and transcripts, interventions, review diagnostics, merge
readiness, and attention state.

`GET /v1/tasks/{id}/next-action` returns the full next action, including `label`.
`GET /v1/next-actions` returns the inbox: `decisions`, `blocked`, and `automatic`
bands, paging headers, `document_proposal` items, and an `acts` log. Each
`document_proposal` item carries `proposal.age_seconds`, the whole seconds since
`proposed_at` at read time (zero for a future time, omitted without one), and
the server's `label`, `question`, and `if_ignored` text. `if_ignored` reads
"The proposal stays pending.", because a pending proposal holds no task's work.
Each task item's `pull_request` is the task's recorded pull request (`url`,
`number`, `repository` from its latest `pull_request.opened`), omitted when
none is recorded; the task's GitHub issue stays on `task.github`. The bounded
task detail's `summaries.park_reason` states what triage recorded when it
parked the task, and `summaries.dispatch_failure` the latest dispatch failure.
Pending proposals expose `attention.task_count` and
`attention.operator_decisions` from that same projection.
`task_count` counts the tasks in the `decisions` band, and `operator_decisions`
adds the pending document proposals, so it equals the `decisions` total. A task
with `actor=operator` and `judgment=false`, such as a stalled one, is listed
under `automatic` and left out of both. It still has `needs_attention` and
appears in `GET /v1/attention/tasks` for its assignee.

## Repository documentation closure

A repository may declare `.conveyor/docs.yaml` with `schema_version: 1`,
`docs` entries containing `path`, a required nonempty `governs` list of
repository-relative source globs, and `description`, plus a `rule` containing
`none_statement` (default `docs: none`), `reason_required`, and `text`.
`conveyor docs validate [repository]` validates the closed YAML schema and
reports each entry whose document or governing source glob matches no tracked
file. It exits nonzero on malformed declarations or unmatched entries.
An absent declaration exits zero and reports that the gate is off.

At submission the declaration is read at the PR base SHA, not pinned at claim.
For each document governing a changed source path, the PR must update that
document or include a qualifying docs-none line in the agent-authored PR body.
The server refuses before submission side effects if any affected document
remains untouched without that line, or declaration discovery is unavailable or
malformed. Refusal writes no evidence or events. No declaration (or no GitHub
slug) preserves existing submission behavior and records nothing.

Exact-task/head admission evidence requires a reviewer
`documentation_assessment`; without evidence the field must be omitted.
`updated_paths` must equal the evidence, and approvals require empty
`unresolved` and `conflicts` lists. Review completion activity includes the
assessment only when supplied (REQ-9/AC-9.1–9.6; DEC-71).
