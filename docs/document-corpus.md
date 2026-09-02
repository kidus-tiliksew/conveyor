# The document corpus

The corpus is the factory's design authority: the confirmed Requirements,
System Design documents, and decisions that tasks are implemented and
reviewed against. Everything else in Conveyor is machinery for getting work
to agree with these documents, or for noticing when it doesn't.

That makes the corpus the center of the factory in a concrete sense. Served
requirements become the acceptance criteria a reviewer must classify the
delivery against. Governing designs are pinned into review context and armed
as drift tripwires. Confirmed decisions are citable
authority. 

When the corpus is empty, reviews have nothing
to hold work to and the misalignment machinery has nothing to detect, and
Conveyor degrades into a task queue.

## The two tiers

Documents are either normative or informative.

Normative documents can be cited, gate reviews, and arm drift: Requirements,
System Design documents, and decisions.

Informative documents are reference material: product overviews, roadmaps,
anything uploaded as Markdown. They cannot be cited, on purpose. When a claim
in an overview should become enforceable, an operator promotes it into a
requirement, and Conveyor records a `derived_from` link back to the overview
when that requirement is confirmed. Informative documents never restate
acceptance criteria (DEC-34).

## Requirements

A requirement document has two parts: an explanation written for people, and
one `conveyor:requirements` code block that Conveyor parses. The explanation
can say whatever helps a reader; only the code block is validated:

A requirement is a black-box contract and each requirement document covers
one capability. It does not prescribe storage, services, queries, queues, or
algorithms unless that mechanism is itself a public contract (DEC-34).

````markdown
Operators need to recover a lost sign-in without database surgery.

```conveyor:requirements
- id: REQ-1
  statement: The system shall issue a fresh sign-in link for an existing account.
  user_story:            # optional, all three fields or none
    as_a: operator
    i_want: to reissue a sign-in link from the host
    so_that: a locked-out teammate can get back in
  acceptance_criteria:   # optional, IDs are AC-<parent>.<m>
    - id: AC-1.1
      statement: When the email has no account or invitation, the command fails without minting a token.
```
````

## System Design documents

A System Design document is Markdown describing a mechanism, with exactly one
`conveyor:governs` code block declaring the code it governs:

````markdown
```conveyor:governs
- repo: api
  paths:
    - internal/dispatch/**
    - internal/httpapi/mcp.go
```
````

Conveyor reads the governed paths from this code block and nowhere else;
there is no separate scope field to fill in. That means a document cannot
describe one scope in its text and register a different one. Paths are
relative to the repository root, and `**` matches across directories. Each
document also has a category; the operator names it at creation and it never
changes.

The governs block does double duty. It selects which designs are served as
pinned authority when a task touches those paths, and it arms
[drift detection](misalignment.md): a merge that changes governed paths
without the delivering task having consulted or proposed a revision to the
design raises a drift signal.

## Baselines and overlays

DEC-34 recommends evergreen System Design documents as component baselines and
a temporary feature overlay for delivery that changes several baselines. The
overlay opens by naming the exact baseline versions it changes, the
requirements it implements, its delivery state, and the absorbing document
for each lasting mechanism; it does not outrank a requirement or decision.

Planning agents recommend this pattern when a workspace creates its first
System Design document, when in-flight delivery spans several baselines, or
when an operator asks how to document a feature under development. A workspace
may decline and revise evergreen baselines directly, as Conveyor's own corpus
does, and the agent does not press the pattern again after that choice.

After delivery, the operator proposes the baseline revisions and then archives
the overlay naming those documents as successors. The archive step ships with
req-document-operating-surfaces REQ-5 AC-5.7; until then, the operator retires
the overlay by hand.

## Decisions

A decision (DEC-n) records a settled judgment: a statement, its context, and
the alternatives rejected. All three fields are required, which is the point;
a decision without recorded alternatives is just an opinion. Decisions are
workspace-wide and citable even where no System Design governs.

Decisions are never edited. A decision changes by supersession: propose a new
decision naming the confirmed one it supersedes, and when the new one is
confirmed, one transaction marks the predecessor `superseded` and the
successor `confirmed`. Two confirmed decisions can never supersede the same
predecessor. Superseded decisions stay visible and stay in review context,
labeled as superseded, so a reviewer can tell a stale citation from an
unknown one.

The server mints DEC-n IDs from a per-workspace sequence; like requirement
IDs, they are never recycled.

## Precedence and confirmation order

When confirmed documents disagree, read requirements first, decisions second,
and System Design documents third (DEC-34). Requirements hold the public
contract, decisions constrain the chosen direction, and designs describe the
mechanism within those constraints.

Confirm requirements before the decisions that cite them, then confirm
decisions before the designs that cite both (DEC-34). A pending document has no
authority, so this order prevents a proposal from depending on an unconfirmed
premise.

## Propose, then confirm

No document changes directly. Every write is a proposed version, and a
proposal confers no authority until an operator confirms it. This one rule is
load-bearing enough that it shows up in four different places:

- Agents mid-implementation propose through the MCP tools
  (`propose_requirement_revision`, `propose_system_design_revision`,
  `propose_decision`), which require a live claim on an implement-stage work
  order. The tools are explicitly fire-and-forget: the agent proposes, cites
  the pending ID, and keeps working. Waiting for confirmation is forbidden.
- Planning sessions push proposals over REST: an operator-side agent session
  drafts the document and submits it, following the
  [planning playbook](playbooks/conveyor-planning.md).
- The monitor drafts amendment proposals when an operator resolves drift as
  intentional.

Every version records its origin (the task, drift record, or operator
session that authored it) as provenance. Origin is never authority; no
origin confirms itself.

Confirmation is operator-only (`confirm_documents`) and moves forward only.
Confirming version N retires every earlier unconfirmed version (for
requirements) or dismisses it (for designs); the content survives, it just
becomes permanently unactionable. Attempting to confirm a version older than
the current one returns a conflict. Versions are immutable and strictly
monotonic per document.

The same capability may dismiss one pending requirement or System Design
version directly. The version keeps its immutable content, statement IDs,
dismissal actor, and dismissal time in history, but leaves pending and
attention projections and can never be confirmed later. Direct dismissal does
not archive or delete the document, and confirmation still resolves earlier
pending versions as described above.

The Pending proposals page collects every undecided proposal, with its age,
its origin, and confirm and dismiss actions, so an operator can see at a glance how
much authority the factory is waiting on. A pending task-authored
proposal also blocks that task's review from being claimed until it is
decided either way, which is the one place a proposal touches the pipeline;
see [Misalignment](misalignment.md#pending-proposals).

## How documents reach a task

Requirements and designs attach to tasks, at intake (`requirement_ids` /
`system_design_ids` on task creation) or later from the task page. The two
attachment kinds age differently, on purpose:

- A requirement attachment stays live: the task is always served the
  document's current confirmed version.
- A design attachment pins the confirmed version current at attach time.
  Reviews judge against the pinned version even if a newer one is confirmed
  later. The pin only moves forward at a recorded step when the task next
  enters implementation, never silently.

Triage may suggest attachments but never makes them; its suggestions wait as
proposals for an operator to confirm or dismiss.

When an agent claims a work order, Conveyor renders the served requirements
(with their REQ and AC statements) and the governance snapshot (governing
design content, confirmed and superseded decisions) into the agent's
context. Review orders get a snapshot frozen at claim time, and reviewers
must return structured assessments classifying their citations against
exactly that authority. The mechanics live in [Tasks](tasks.md#what-a-work-order-carries).

## Planning from an agent session

You author documents from an agent session in your project checkout,
following the [planning playbook](playbooks/conveyor-planning.md); the
skills installed by `conveyor skills install` wrap it. The session explores the repository and
the existing corpus, drafts the document in full, and pushes it over REST as
a proposed version. Every push is a proposal; you confirm it from the
pending proposals queue. The same discipline covers new documents,
revisions, decisions, and promotions from reference material.

## Traceability

Code written by the factory cites the authority behind it: confirmed REQ-n
and AC-n.m IDs, DEC-n decisions, and governing System Design document IDs in
code comments where an implementation decision needs explanation. Reviewers
classify those citations into disjoint lists (cited, unknown, unserved or
ungoverned, superseded) in every verdict, and the server checks the lists
against the pinned authority. The result is that `git blame` on a governed file leads to
a task, the task leads to the requirement version it served, and the
requirement leads back to every delivery that served it. That chain is the
[knowledge graph](concepts.md#the-knowledge-graph).
