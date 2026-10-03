# Conveyor local planning

You are the headless planning twin: draft locally, push as **proposals**, and
let the operator confirm. Never look for a way to
create a confirmed version directly — none exists, by design, and the
proposal path is not a limitation to work around.

## Ground rules

- Auth: `Authorization: Bearer $CONVEYOR_API_TOKEN` (repo `.env`), and
  `?workspace_id=<ws>` on every call. Single workspace today: `demo`.
- **Propose→confirm**: every normative push lands unconfirmed. Confirmation
  is the operator's act alone: the operator confirms each layer in the web UI
  (Requirements / System Design surfaces). You never confirm — not on the
  operator's word, not through any endpoint.
- Draft in full before pushing: each version is a complete replacement,
  validated server-side. On a 400, fix the document and re-propose — the
  error names the specific rule violated.
- The factory's confirmed document corpus is the authority. Tier semantics
  live in the `design-document-corpus` System Design document and the
  confirmed requirement documents.

## Draft layout

The studio keeps one draft folder per feature and pushes it once, as proposals.
The operator's harness chooses the folder and keeps it out of git; Conveyor names no default path, and every command takes the draft directory as an argument.
`conveyor plan check <draft-dir>` fails when the folder is not git-ignored or holds a tracked file, so a draft can never be committed by accident.

| File | Holds | Pushed as |
|---|---|---|
| `brief.md` | Goal, users, current versus target behavior, diagrams, out of scope | Reference document (informative) |
| `requirements/<doc>.md` | One capability per file, in the exact `conveyor:requirements` format | Requirement document |
| `decisions.yml` | Local IDs `D1`, `D2`; statement, context, rejected alternatives | `DEC-n` proposals |
| `designs/<id>.md` | Mechanism, diagrams, one `conveyor:governs` fence | System Design document |
| `tasks.yml` | Title, body, dependencies, governing document IDs, docs paths to update or a `docs: none` reason | Tasks |
| `notes.yml` | Per-item plain-language headline, example, and why | Not pushed |
| `review.json` | Per-item content hash, verdict, comments, round | Not pushed |
| `round.yml` | The current grill round for `conveyor plan ask`: questions, options, recommended option | Not pushed |
| `answers.json` | The owner's answers to the last `conveyor plan ask` round, one per question | Not pushed |
| `hook.json` | Combined item hash of the last passing pre-push review | Not pushed |
| `push.json` | Server IDs minted for pushed items, so later layers resolve local citations and a re-run skips what was posted | Not pushed |

Normative files use the tier formats in this playbook, so what the owner approves is exactly what is pushed.
Readability lives in `notes.yml`, which is never pushed.
`brief.md` is pushed with the requirements layer as an informative reference document.

## Grill

Grill runs in the terminal: each question shows its options with the recommended one marked, and the owner picks or types.
A round with more than five questions, or one that needs a diagram, is served as one local browser form instead.
The agent writes the round to `round.yml`, runs `conveyor plan ask <draft-dir>`, and reads `answers.json` after the command exits; one submit answers the round.
Every answer is written to `decisions.yml` immediately as a `DEC-n`-shaped entry with statement, context, and rejected alternatives, so decisions need no rewriting later.
Facts are looked up, never asked; an answer that resolves a dependency unblocks the next question in the same round.

## Decompose and local checks

Decompose the settled idea into the corpus-shaped files above: one requirement document per capability, a decision for each settled posture, a design per mechanism, and tasks that name their governing documents.
`conveyor plan check <draft-dir>` runs the offline, deterministic checks, prints failures by item ID, and exits non-zero on any failure.
It checks that the draft folder is git-ignored and no draft file is tracked (`git check-ignore`, `git ls-files`); that every requirement carries a `conveyor:requirements` fence; that acceptance criteria read "When X, the system shall Y"; that `conveyor:governs` globs use the `*`, `?`, `**` dialect and match at least one tracked path; that every `REQ` has an `AC` and every `AC` is covered by a task; that every decision cites a requirement; that every task names its docs paths or a `docs: none` reason; and that no design cites a decision absent from the draft, preserving push order.
Only `conveyor plan push` contacts the server; the checks never do.

## Review page

`conveyor plan review <draft-dir> [--port N]` serves one page on `127.0.0.1` and blocks until the owner submits or closes it.
The page has an overview tab (goal, main diagram, "N of M approved"), one tab each for requirements, decisions, designs, and tasks, and a requirement-by-task traceability matrix.
Each item is a card with its ID, plain headline, exact normative text, example, related links, and Approve / Request change / Question controls with a comment box; "Approve all unchanged" exists per tab.
Approval is per item and bound to a content hash over the item's ID and normative fields only, normalized to LF line endings with trailing whitespace trimmed, and `conveyor plan review` recomputes hashes on every render.
Normative fields per kind: a requirement's statement, optional user story, and acceptance criteria; a decision's statement, context, and rejected alternatives; a design's body including its `conveyor:governs` fence; a task's title, body, dependencies, governing document IDs, and docs paths or `docs: none` reason.
`notes.yml` text is never hashed, so improving a headline or example never invalidates an approval.
A change does not cascade: an approved card whose linked item changed keeps its approval and shows a "linked item changed" badge.
The one "Send review" button writes `review.json` as `{round, items: [{id, hash, verdict, comment}]}` and returns; the agent reads `review.json` after the command exits.
Comments route back to the planner with the item ID and the file that holds the item, and the agent applies each to the right file.
A card whose hash changed since approval returns to pending and shows a before/after view while untouched approvals stay approved.
A comment that changes the design tree returns the item to Grill rather than being applied silently.

## Push

When every item is approved, push one layer at a time with `conveyor plan push <draft-dir> --layer requirements|decisions|designs|tasks [--dry-run]`, in the order reference document, requirements, decisions, designs, tasks.
The command reads live confirmation state from the server and refuses a layer whose prerequisites are not yet confirmed: a decision citing an unconfirmed requirement, a design citing an unconfirmed decision, or a task citing an unconfirmed document.
`--dry-run` prints the proposal payloads without posting.
The push never confirms: `CapabilityConfirmDocuments` is operator-only, so the owner confirms each layer in the web UI (Requirements / System Design surfaces) before the next layer runs.
Only `plan push` contacts the server; `plan check` stays offline.
After the last layer is confirmed, the operator or their harness deletes or archives the draft; Conveyor never deletes it.
Every later change is a Conveyor revision.

## Pre-push review hook

The optional pre-push review hook lives in `.conveyor/planning.yaml`, a repo-tracked sibling of `.conveyor/docs.yaml`, not in `docs.yaml`.
`docs.yaml` is read and pinned by the server for the review gate, so a planning-only concern stays out of a server-enforced contract; `.conveyor/planning.yaml` is read only by the local studio and is never pinned.
An absent file or absent `pre_push_review` key means the studio skips the hook.
The hook is an argv list executed without a shell, with a timeout and a `blocking` flag.
It runs once over the whole approved draft before the requirements layer; its pass is bound to the hash of every normative item, so it runs again only when an item changed since that pass.
The last pass is recorded in `hook.json` in the draft folder.

Example `.conveyor/planning.yaml`:

```yaml
schema_version: 1
pre_push_review:
  command: ["your-review-command"]
  timeout_seconds: 900
  blocking: true
```

## House style

Planning documents are executed, not merely read — agents act on them
alone. DEC-28 makes this discipline a review criterion at the
propose-confirm boundary; the incidents behind each rule are in the
"Documentation style and organization" reference document
(ref-260823-f4729f, informative).

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
  documents by version.
- Requirements are black-box contracts with one capability per document; do
  not prescribe storage, services, queries, queues, or algorithms unless the
  mechanism is itself a public contract (DEC-34).
- Confirmed-document precedence is requirements, then decisions, then System
  Design documents (DEC-34).
- Reference documents orient and never restate acceptance criteria (DEC-34).
- A proposal cannot cite a pending decision or design as authority; confirm
  requirements before decisions that cite them, then decisions before designs
  that cite both (DEC-34).
- Recommend DEC-34's baseline-and-overlay pattern when
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
  rules and their failure classes live in ref-260823-f4729f v2 (informative).

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
- Push: `POST /v1/requirements` (new document) and
  `POST /v1/requirements/{id}/versions` (revision).
  The operator confirms the proposed version in the Requirements UI.
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
- Document what the system IS, not a change plan. Cite code as
  `conveyor:path:line-range` evidence. Category is operator-named
  (Architecture, Database design, API contracts, …) and immutable after
  creation.
- When the operator takes the baseline-and-overlay recommendation, draft the
  temporary overlay to open with the exact baseline versions it changes, the
  requirements it implements, its delivery state, and the absorbing owner for
  each lasting mechanism (DEC-34).
- Governed scope is load-bearing: merges touching those paths without a
  proposed revision raise the drift signal. Scope only what the document
  genuinely describes.
- Push: `POST /v1/system-designs` (create; id charset
  `[A-Za-z0-9][A-Za-z0-9._-]*`, no `/` or `:`),
  `POST /v1/system-designs/{id}/versions` (revision).
  The operator confirms the proposed version in the System Design UI;
  confirming a later pending revision dismisses earlier ones.
  Dismiss one pending version:
  `POST /v1/system-designs/{id}/versions/{version}/dismiss`.

Direct dismissal is an operator-only `confirm_documents` act. The UI requires
confirmation before sending it. The dismissed version stays in history with
its actor and time and cannot be confirmed later; it does not archive or
delete the document.

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
- Push: `POST /v1/decisions`. Confirm/dismiss: the operator does it in the
  System Design UI.
- Confirmed DEC-n are citable in code comments and task bodies alongside
  REQ-n/AC-n.m and governing System Design document IDs.

## Product overviews (informative)

Markdown only, 2 MiB cap, `.md`/`.markdown` extension authoritative.
`POST /v1/reference-documents` (multipart `name` + `file`); re-upload via
`POST /v1/reference-documents/{id}/versions` supersedes with full history
retained. Never cited, never gates — promote enforceable claims into
requirements instead.

## What this surface cannot do (and must not fake)

- Confirm anything without the operator.
- Record drift resolutions, approve gates, cancel/hold tasks — operator
  acts; surface them to the operator instead.
- Create lineage that didn't happen: no fabricated origins, no session
  edges for work that had no session. Local planning deliberation that is
  worth keeping should be distilled into the documents themselves or a
  DEC — that is the promotion doctrine established by DEC-9.

For filing the resulting work, read `conveyor-task-filing.md` in this directory.
