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
  live in the `design-document-corpus` System Design document and the
  confirmed requirement documents.

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

## Requirements (normative intent)

Prose + exactly one `conveyor:requirements` fence. Statement schema:

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
  Confirm: `POST /v1/requirements/{id}/versions/{version}/confirm`.
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
- Governed scope is load-bearing: merges touching those paths without a
  proposed revision raise the drift signal. Scope only what the document
  genuinely describes.
- Push: `POST /v1/system-designs` (create; id charset
  `[A-Za-z0-9][A-Za-z0-9._-]*`, no `/` or `:`),
  `POST /v1/system-designs/{id}/versions` (revision).
  Confirm: `POST /v1/system-designs/{id}/versions/{version}/confirm`
  (supports `If-Match`; confirming a later pending revision dismisses
  earlier ones).

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
- Push: `POST /v1/decisions`. Confirm/dismiss: the System Design UI, or
  `POST /v1/decisions/{id}/confirm`.
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
