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
acceptance criteria (DEC-109(3)).

## Requirements

A requirement document has two parts: an explanation written for people, and
one `conveyor:requirements` code block that Conveyor parses. The explanation
can say whatever helps a reader.
At proposal time, the first non-blank line must be a non-empty `# <title>` heading, and lines beginning with `REQ-n:` or `AC-n.m:` or a YAML `- id: REQ-`/`- id: AC-` item must stay inside the requirements fence; inline identifier citations remain legal.

A requirement is a black-box contract and each requirement document covers
one capability. It does not prescribe storage, services, queries, queues, or
algorithms unless that mechanism is itself a public contract (DEC-109(1)).

````markdown
# Sign-in recovery

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
[drift detection](misalignment.md): a merge whose task held the design records
a merge judgment with no drift, and any other change to the governed paths
records drift (`component-monitor-drift`).

A document that owns no code, such as a feature design (DEC-101(5)), declares
its repository with an empty path list:

````markdown
```conveyor:governs
- repo: api
  paths: []
```
````

The empty list is a scope that matches nothing, not a repository-wide wildcard
(`req-task-authority-boundary` REQ-5). Conveyor does not serve the document as
repository authority, no submitted diff attaches it, and no merge raises drift
against it. A task receives the document only through explicit attachment,
which pins a version as usual. The document still has exactly one code block,
and the list must be written out: a repository entry with an absent or null
`paths` is refused, so a misspelled key cannot empty a scope.

## The System Design standard

Conveyor's planning playbooks and skills teach every factory one System Design
standard: the content test of DEC-100 and the component and feature shapes of
DEC-101(2) to (8) (DEC-109(5)). Conveyor encodes neither shape as a document
kind or lifecycle state, and a category selects no behavior.

### The content test

A System Design document states what must stay true: boundaries, public
surfaces, invariants, and failure behavior. The code states how the system
works today (DEC-100(1)).

A sentence belongs in a System Design document only when a behavior-preserving
refactor would not force an edit to it, and either a caller or another component
would break if it became false or a reviewer should reject a pull request that
violates it (DEC-100(2)).

A component design changes only when a rule a reviewer should enforce changes, a
public surface is broken or removed or is added outside the component's
extension rules, a boundary or dependency between components changes, or behavior
a requirement demands changes (DEC-100(3)).

A System Design document holds no implementation walkthroughs, test inventories,
task identifiers, change plans, or narration of removed mechanisms (DEC-100(4)).

### Component designs

A component design has the category `Component design` and an ID
`component-<name>`. It describes one lasting capability or platform service: one
that outlives any single feature, whose contract fits in one paragraph, and that
owns code named by directory (DEC-101(2)).

A domain component owns one capability from storage through its user-interface
contract. A platform component sets rules other components follow and names no
specific feature (DEC-101(3)). The `conveyor` workspace's HTTP API, MCP protocol,
and web dashboard documents keep the platform label although they name specific
features (DEC-109(6)).

A component design holds its boundary, public surfaces, numbered invariants,
state and failure model, extension rules, verification by suite or gate, and
governs fence, and aims for about 2,500 words (DEC-101(4)).

### Feature designs

A feature design has the category `Feature design` and an ID `feature-<name>`.
It plans one change that spans several tasks or components, names the component
versions it changes, governs no path, and aims for about 2,000 words. A one-task
change has no feature design (DEC-101(5)).

The last task of a feature design closes it. Its surfaces and invariants move
into the components it changes, a new capability becomes a new component, and the
feature design is archived naming its successors (DEC-101(6);
req-document-archive REQ-1/AC-1.7). Operator-side planning authors those versions,
because a task authors none (DEC-102(1), (2)).

### Path ownership

Every source, test, build, configuration, and mechanism-documentation path is
governed by exactly one component design (DEC-101(7)). Historical evidence
(point-in-time measurements, incident reconciliations, retained screenshots) and
narrative guides that restate no mechanism are governed by no document
(DEC-101(8)).

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
and System Design documents third (DEC-109(2)). Requirements hold the public
contract, decisions constrain the chosen direction, and designs describe the
mechanism within those constraints.

Confirm requirements before the decisions that cite them, then confirm
decisions before the designs that cite both (DEC-109(4)). A pending document has no
authority, so this order prevents a proposal from depending on an unconfirmed
premise.

## Propose, then confirm

No document changes directly. Every write is a proposed version, and a
proposal confers no authority until an operator confirms it. This one rule is
load-bearing enough that it shows up in several places:

- A task never proposes. A session holding a work order implements confirmed
  documents and authors no version (DEC-102); when its work would require
  changing one, it stops at an authority-conflict checkpoint and describes the
  conflict. Versions a task authored earlier keep that origin in history.
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

A requirement or System Design document's listed title follows the heading of
the version an operator confirms. When the confirmed version's first non-blank
line is a non-empty `# <title>` heading that differs from the listed title,
the confirmation updates the title in the same transaction and records a
`requirement.title_changed` or `system_design.title_changed` event naming the
previous title, the new title, the confirmed version, and the confirming
actor. Proposing a version never changes the title, and the document ID and
slug never change, so citations, URLs, and lineage keep working
(`req-document-operating-surfaces` AC-6.1).

The same capability may dismiss one pending requirement or System Design
version directly. The version keeps its immutable content, statement IDs,
dismissal actor, and dismissal time in history, but leaves pending and
attention projections and can never be confirmed later. Direct dismissal does
not archive or delete the document, and confirmation still resolves earlier
pending versions as described above. The Dismiss and Revise dialogs accept an optional
operator note, shown beside the dismissal record in version history.

Operators holding both `propose_documents` and `confirm_documents` can use
Revise on a pending requirement or System Design proposal in the queue or its
document attention surface. The dialog loads the full proposal, names its origin
and version, and compares edits with the confirmed version. Submit proposes a
new operator-origin version, then confirms it with the reviewed current version
in `If-Match`; confirmation dismisses earlier pending versions. Validation errors
keep the edits open. If confirmation fails, the replacement remains pending and
the dialog directs the operator to review it again. Decisions have no Revise action.

The Pending proposals page collects every undecided proposal, with its age,
its origin, and confirm and dismiss actions, so an operator can see at a glance how
much authority is undecided. A pending proposal holds no task's work, including
one a task authored before DEC-102; see
[Misalignment](misalignment.md#pending-proposals).

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

A work order carries the session's instructions, plan, done criteria, and served
requirement statements with their REQ and AC IDs. It names the governing System
Design documents and decisions without carrying their bodies (DEC-103(1)). A
session finds and reads design text over MCP by search, outline, and section
read, each of which accepts a version (DEC-103(3)). Review orders get a snapshot
frozen at claim time, and reviewers must return structured assessments
classifying their citations against exactly that authority. The layers are listed
in [What a work order carries](tasks.md#what-a-work-order-carries).

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

### Operator dismissal notes

The requirement and System Design confirm and dismiss REST routes accept an
optional `note` string under the existing `confirm_documents` capability.
The server trims surrounding whitespace and accepts at most 2000 Unicode
characters; a longer note returns HTTP 400 before any write. Empty or absent
notes preserve the existing response and event shapes. MCP mutations do not
accept notes.

Direct dismissal records the note on that version. Confirmation records it on
every earlier pending version dismissed by the transaction. History exposes
`dismissal_note` beside the existing actor and timestamp, without changing
content or statement identifiers. The existing dismissal events add `note`
only when present; requirement supersession keeps `requirement.version_retired`.
A note stays on the dismissed version's history. It is not document authority,
and no task or work-order context carries it (req-260810-23b69f REQ-5, AC-5.1,
AC-5.3, AC-5.4).
