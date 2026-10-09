# Conveyor local planning

You are the headless planning twin: draft locally, push as **proposals**, and
let the operator confirm. Never look for a way to
create a confirmed version directly — none exists, by design, and the
proposal path is not a limitation to work around.

## Ground rules

- Auth: `Authorization: Bearer $CONVEYOR_API_TOKEN` (repo `.env`), and
  `?workspace_id=<ws>` on every call. Single workspace today: `demo`.
- **Propose→confirm**: every normative push lands unconfirmed. Confirmation
  is the operator's act — in the UI (Requirements / System Design surfaces)
  or, on their explicit word, via the confirm endpoints below. Never
  confirm without the operator's say-so in this conversation.
- Draft in full before pushing: each version is a complete replacement,
  validated server-side. On a 400, fix the document and re-propose — the
  error names the specific rule violated.
- The factory's confirmed document corpus is the authority. Tier semantics
  live in the `component-document-corpus` System Design document and the
  confirmed requirement documents.

## House style

Planning documents are executed, not merely read — agents act on them
alone. DEC-28 makes this discipline a review criterion at the
propose-confirm boundary; the incidents behind each rule are in the
"Documentation style and organization" reference document
(ref-260823-f4729f, current version; informative).

- One tier per job: requirements say what, System Design says what *is*,
  decisions say why, reference documents orient. Rationale never rides in
  a what-tier — extract a DEC-n and cite it.
- Every normative claim carries a citable ID; acceptance criteria are
  falsifiable "When <X>, the system shall <Y>" statements.
- Decidability test before any push: can an agent holding only this text
  plus a work order decide compliance? If not, rewrite.
- Dense normative core: every word in a governing document is a tax paid
  on every dispatch that attaches it. Push elaboration into decisions or
  reference documents.
- Governs scopes match what the prose actually describes, re-checked on
  every revision.
- Several tight design documents beat one broad one — pins attach whole
  documents by version. Aim for about 2,500 words per System Design
  document; 4,000 is the ceiling.
- Verify every mechanism claim against the code at the current head before
  writing it, and record each correction where a reviewer can see it.
- Point to the owning statement ("as req-X REQ-n specifies") instead of
  restating it; copies of one rule drift apart.
- Keep the workspace's orientation indexes (requirements by title, decisions
  by label, components by what they own, moved identifiers) current: update
  them in the same session as the confirmation that changes them.
- Requirements are black-box contracts with one capability per document; do
  not prescribe storage, services, queries, queues, or algorithms unless the
  mechanism is itself a public contract (DEC-57(1)).
- Confirmed-document precedence is requirements, then decisions, then System
  Design documents (DEC-57(2)).
- Reference documents orient and never restate acceptance criteria (DEC-57(3)).
- A proposal cannot cite a pending decision or design as authority; confirm
  requirements before decisions that cite them, then decisions before designs
  that cite both (DEC-57(4)).
- Recommend DEC-57(5)'s baseline-and-overlay pattern when
  `GET /v1/system-designs` returns an empty workspace list before a first
  design, delivery already spans several design baselines, or the operator
  asks how to document in-flight work. Explain evergreen component baselines
  and a temporary feature overlay, recommend the pattern as the default, and
  let the operator decline without blocking the draft; once absorbed, the
  archived overlay names its successors. The overlay never outranks a
  requirement or decision. Do not reintroduce the pattern when designs exist
  and the operator has not raised it.
- Name the mechanism, actor, and source. Replace generic praise with a fact,
  instruction, or number, and cite the REQ-n or DEC-n that holds each claim.
- Use one name per concept, especially for schema columns and API fields.
- Cut filler, hedging stacks, and ornamental adverbs; state the measurement or
  remove the claim.
- Prefer plain words, active voice, and one idea per sentence.
- Let structure carry content: do not restate bold labels, use "not just X,
  but Y", or force groups of three.
- Remove conversational residue and celebratory framing. The full sentence
  rules and their failure classes live in ref-260823-f4729f, current version
  (informative).

## Requirements (normative intent)

Prose + exactly one `conveyor:requirements` fence.
The first non-blank line must be a non-empty `# <title>` heading, and lines beginning with `REQ-n:` or `AC-n.m:` or a YAML `- id: REQ-`/`- id: AC-` item must stay inside that fence; inline identifier citations remain legal.

Statement schema:

```conveyor:requirements
- id: REQ-1
  statement: The system shall <verifiable intent>.
  user_story:            # optional, all three fields or none
    as_a: operator
    i_want: <capability>
    so_that: <outcome>
  acceptance_criteria:   # optional, nested AC-<parent>.<m>
    - id: AC-1.1
      statement: When <X>, the system shall <Y>.
```

- IDs are permanent: never reuse or renumber a REQ or AC that ever existed
  in any version, even deleted ones (high-water rule; the server rejects
  recycling). Revisions may add IDs above the high-water mark only.
- Statement-only entries remain legal; don't force user stories onto
  requirements that aren't user-facing.
- When splitting a document, keep the most-cited capability in place so its
  identifiers and their citations stay valid; moved statements get fresh
  identifiers in their new document.
- A moved identifier is retired in its source and never reused there.
  Record an old-to-new table in the workspace's orientation document the
  same day.
- When two documents contradict each other, read the current code to decide
  which side is right, then leave one owning statement and make the others
  cite it. Code never changes confirmed intent on its own: record the
  disagreement, file code defects as tasks, and take product choices to the
  operator (DEC-57(2)).
- Push: `POST /v1/requirements` (new document) and
  `POST /v1/requirements/{id}/versions` (revision).
  Confirm: `POST /v1/requirements/{id}/versions/{version}/confirm`.
  Dismiss one pending version:
  `POST /v1/requirements/{id}/versions/{version}/dismiss`.
- **Promotion**: when a claim originates in an uploaded overview, carry
  `derived_from: {document_id, version, section_anchor, target_id}` on the
  proposal — anchor must be a real heading slug in that document version;
  the provenance edge mints at confirmation.

## System Design (normative mechanism)

Markdown with exactly one `conveyor:governs` fence declaring governed
paths:

```conveyor:governs
- repo: conveyor
  paths:
    - internal/workorder/**
    - internal/httpapi/mcp.go
```

- Glob dialect is `*`, `?`, `**` only — no `[..]` character classes (the
  server rejects them). Paths are repo-relative, case-sensitive.
- Document what the system IS, not a change plan. An evergreen baseline
  carries no history, change-plan, or absorption narration; a removed
  mechanism is deleted from the text, not described as retired. Category is
  operator-named (Architecture, Database design, API contracts, …) and
  immutable after creation.
- Cite code as `<repo>:path#Symbol` (for example
  `conveyor:internal/core/system_design.go#ContainsDecisionToken`), never by
  line range: line ranges drift onto unrelated code within weeks.
- When the operator takes the baseline-and-overlay recommendation, draft the
  temporary overlay to open with the exact baseline versions it changes, the
  requirements it implements, its delivery state, and the absorbing owner for
  each lasting mechanism (DEC-57(5)).
- Governed scope is load-bearing: merges touching those paths without a
  proposed revision raise the drift signal. Scope only what the document
  genuinely describes.
- Every source, test, build, configuration, and mechanism-documentation path
  is governed by exactly one document. Only historical evidence
  (point-in-time measurements, incident reconciliations, retained
  screenshots) and narrative guides that describe no mechanism stay
  ungoverned, and the ownership map says so explicitly. A code file holding
  several components' mechanisms defeats single ownership; split the file,
  not the rule.
- Push: `POST /v1/system-designs` (create; id charset
  `[A-Za-z0-9][A-Za-z0-9._-]*`, no `/` or `:`),
  `POST /v1/system-designs/{id}/versions` (revision).
  Confirm: `POST /v1/system-designs/{id}/versions/{version}/confirm`
  (supports `If-Match`; confirming a later pending revision dismisses
  earlier ones).
  Dismiss one pending version:
  `POST /v1/system-designs/{id}/versions/{version}/dismiss`.

Direct dismissal is an operator-only `confirm_documents` act. The UI requires
confirmation before sending it. The dismissed version stays in history with
its actor and time and cannot be confirmed later; it does not archive or
delete the document.

A component baseline, the evergreen half of the baseline-and-overlay
pattern, takes this shape. The demo workspace adopts it as its convention
(DEC-58); elsewhere it is a recommendation the operator may decline.

````markdown
# <Component title>

Lifecycle: evergreen component baseline.

Implements: `req-<capability>` REQ-1, REQ-2 (AC-2.1, AC-2.3); `req-<other>` REQ-4

This component owns <its concerns>. `component-<neighbour>` owns <the
adjacent concern>. `component-<other>` owns <the next adjacent concern>.

## <Mechanism>

<Current state only: what the code does today, cited as
`<repo>:path/to/file.go#Symbol`.>

## Verification

- <Behavior>: `<repo>:path/to/file_test.go#TestName`, `#TestOtherName`.

`make test` runs these.

```conveyor:governs
- repo: <repo>
  paths:
    - path/to/package/**
```
````

The `Implements:` line lists the document-qualified REQ/AC identifiers that
are current at proposal time, and the ownership paragraph names the owner of
every neighbouring concern the mechanism sections touch.

## Decisions (DEC-n)

Propose when deliberation settles an enforceable posture with real
rejected alternatives — extraction, not description. Shape:

```json
{"statement": "<the posture, one enforceable sentence>",
 "context": "<what was deliberated and why this holds>",
 "alternatives_rejected": "<what was considered and why not>",
 "supersedes": ""}
```

- Leave `id` empty (server mints the next DEC-n); `supersedes` only
  against a currently **confirmed** decision — list first
  (`GET /v1/decisions`).
- Number the clauses of a multi-part decision, "(1) … (2) …", so a document
  can cite one clause as DEC-n(k). Word every clause to match what the code
  does; fixing a misstated clause takes a successor decision.
- `supersedes` names exactly one decision, and only one confirmed decision
  can supersede a given one. Consolidating two decisions therefore takes one
  successor for each.
- Before proposing a successor, count the current documents that cite the
  decision it supersedes: confirmation flags each one for revision. Hold a
  wording-only successor until those documents are being revised anyway.
- Point pending drafts that cite the superseded decision at the successor
  before confirming it, and keep them pending until it is confirmed
  (DEC-57(4)).
- The supersession check: confirming a successor scans the current confirmed
  version of every live requirement and System Design document, and the
  current version of every undeleted reference document, for the superseded
  ID as a whole token (`DEC-4` never matches `DEC-40`; examples and quotes
  count). Each match opens a flag. A flag auto-clears when a later
  confirmed revision or reference upload drops the token, and reopens if the
  token returns; a pending proposal clears nothing, and only the operator
  dismisses a flag (`component-document-corpus`).
- Push: `POST /v1/decisions`. Confirm/dismiss: the System Design UI, or
  `POST /v1/decisions/{id}/confirm`.
- Confirmed DEC-n are citable in code comments and task bodies alongside
  REQ-n/AC-n.m and governing System Design document IDs.

## Product overviews (informative)

Markdown only, 2 MiB cap, `.md`/`.markdown` extension authoritative.
`POST /v1/reference-documents` (multipart `name` + `file`); re-upload via
`POST /v1/reference-documents/{id}/versions` supersedes with full history
retained. Never cited, never gates — promote enforceable claims into
requirements instead. An upload or re-upload publishes immediately, with no
proposal or confirmation step, so treat it as outward-facing.

## Restructuring a corpus

A restructuring changes many documents at once; order and checks matter
more than any single document. The elaboration is the "Restructuring a
corpus" section of ref-260823-f4729f, current version (informative).

- Establish facts first: check each affected claim against the code at the
  current head and record every correction.
- Then draft in tier order: decisions, then requirements, then designs.
  Confirmation still follows DEC-57(4): a document is confirmed only after
  every document it cites, so no confirmed document cites unconfirmed
  authority.
- When content moves, propose and have the operator confirm the receiving
  documents before revising the donors.
- Check path ownership after every confirmation: a path with no governing
  document between two confirmations escapes drift.
- Give parallel drafters one shared brief and one ownership map as the
  single source of truth for governed paths.
- Before every push, run automated checks on each draft: its governs scope
  equals the agreed map, no superseded decision is cited (whole tokens
  against `GET /v1/decisions`), no code citation uses a line range, no label
  from a retired document remains, and every named document exists.
- Record each code-versus-document disagreement instead of writing around
  it, then split the record into code tasks (filed through
  `conveyor-task-filing.md`) and decisions for the operator.
- Verify a reviewer's claim against the code or the corpus before acting on
  it.
- Update the orientation index and its old-to-new tables in the session of
  each confirmation that moves something.
- Write each reference upload from a temporary file and check that the file
  is non-empty before sending it; the upload publishes at once.
- Agents propose every normative change; the operator confirms each one.

## What this surface cannot do (and must not fake)

- Confirm anything without the operator.
- Record drift resolutions, approve gates, cancel/hold tasks — operator
  acts; surface them to the operator instead.
- Create lineage that didn't happen: no fabricated origins, no session
  edges for work that had no session. Local planning deliberation that is
  worth keeping should be distilled into the documents themselves or a
  DEC — that is the promotion doctrine established by DEC-9.

For filing the resulting work, read `conveyor-task-filing.md` in this directory.
