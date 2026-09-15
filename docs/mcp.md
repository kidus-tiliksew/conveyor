# MCP reference

Conveyor's MCP server is the protocol boundary agents work through. It lives
at `<server>/mcp`, speaks streamable HTTP, and authenticates with a bearer
token: a personal access token for interactive sessions, or the worker
credential the launcher injects for dispatched ones. `conveyor mcp install`
creates a named personal connection for each saved server. Codex and Claude
retrieve the saved credential through native header helpers; Cursor and OpenCode
use separate server-specific token variables. Endpoints are literal canonical
URLs. See [client setup](client-setup.md#6-connect-agent-sessions) for two-server
installation, migration, restart, and native connection troubleshooting.
Config/parser acceptance is separate from native initialization and tools-list
results. Personal installation does not replace worker credential attachments.

Workspace scope follows the same rule as REST: pass `workspace_id`, or omit
it only when the credential belongs to exactly one workspace. The operator
investigation tools below always require an explicit `workspace_id`. Worker
credentials are pinned to their own workspace automatically.

The operator investigation tools below admit user credentials only. These
lifecycle tools also reserve their human operations: `create_task`, `add_task_dependency`, `set_assignee`,
`redispatch_work_order`, and `report_continuation`.

The agent-facing discipline for using these tools well is the
[work playbook](playbooks/conveyor-work.md); this page is the tool
inventory.

## Operator investigation (read only)

These tools require a human session or PAT and the selected workspace's
`view_workspace` capability. A viewer can use them without a claim. Agent,
run-child, and worker credentials are refused. Missing membership and missing
capability both return `workspace_not_found`, including on snapshot reuse.
Reads never call lifecycle reconciliation, claim work, renew leases, enqueue
jobs, record consultation events, or create/confirm proposals.

| Tool | Required fields beyond `workspace_id` | Optional filters / selection |
|---|---|---|
| `list_workspaces` | None | Returns only the caller's own workspace memberships; use a known workspace ID as the explicit authorization scope. |
| `list_repositories` | None | Returns repository `name` and `base_branch`; configuration, URLs, credentials, and checkout paths are omitted. |
| `list_tasks` | None | `state`: `active` (default), `terminal`, or `all`; exact `repository`; `query` matches task ID, title, source, or branch. |
| `get_task` | `task_id` | Includes terminal tasks and body; excludes execution setup and work-order credentials. |
| `list_task_events` | `task_id` | Exact `event_kind`; oldest timestamp first, then ascending event ID. |
| `get_task_context` | `task_id` | `proposal_state`: `all` (default), `proposed`, `confirmed`, or `dismissed`. Attachments are always included. |
| `list_documents` | `kind`: `requirement`, `system_design`, or `reference` | `query` matches ID/title; `include_archived` defaults false. Requirement/design listings contain confirmed document identities only. |
| `get_document` | `kind`, `document_id` | Positive integer `version` selects an explicit immutable version; omission selects current. `include_archived` is required for archived/deleted content. |
| `list_document_events` | `kind`, `document_id` | `event_kind`, `include_archived`; includes archive/restore history with recorded actors and times. |
| `list_decisions` | None | `query`; `include_history` adds superseded decisions to the default confirmed list. |
| `get_decision` | `decision_id` | `include_history` is required for superseded or pending records; neither grants active authority. |

Every tool accepts `limit` (integer 1–100, default 25), `offset` (integer
0–1000, default 0), and `snapshot` (opaque 32-character hex token). Unknown
fields, wrong types, fractional numbers, identifiers/text over 256 UTF-8 bytes,
and argument objects over 8192 bytes fail validation. Document versions are
integers from 1 through 1000000. An offset above zero requires a snapshot.

The response is `{items, total, limit, offset, next_offset?, snapshot,
expires_at, evidence}`. Repeat the same tool and filters with the returned
`snapshot` and `next_offset`; only `limit` and `offset` may change. Each
snapshot freezes rendered results for five minutes, is bound to the owner,
workspace, and query, and is held only by the serving process. Expiry, process
restart, another server replica, changed filters, and changed ownership refuse
reuse. Restart at offset zero without a snapshot. Membership is rechecked on
every call; workspace-list snapshots also recheck every listed membership.

A snapshot holds at most 1000 items and 1 MiB of rendered item data. Each
process holds at most 32 snapshots and refuses new ones while full. Each
response's JSON text is capped at 64 KiB. Oversized reads fail without partial
results: narrow filters or reduce the page size. A single document/task too
large for that response must be read through its existing authenticated REST
surface. Task candidates are collected through existing 200-row store pages;
more than 1000 matching candidates requires a narrower filter. Existing
collection services may materialize more source rows before MCP applies its
rendered-result budget; this is not a database transaction or a store-wide
memory/scan bound.

Snapshots are captured observations, not current authority. Start a fresh
read before citing current document state. Design attachments expose
`pinned_version` separately from `current_version`; requirement attachments
select current confirmed versions. Both retain `archived` and `superseded_by`.
Document reads label `selection`, `historical`, `confirmed`, `version_status`,
and `active_authority`; references are always `informative`. A document with
no confirmed version explicitly reports `authority_absent`. An empty filtered
list establishes only that no records matched that bounded query.

Events expose their recorded `actor_id`, `actor_role`, and `at`. Proposal
records retain `source`, `proposed_by`, `decided_by`, decision state, and event
IDs. Empty attribution is unknown; never infer an actor from task prose or a
suggestion. Event payloads expose only context/document IDs, versions, source,
state transitions, supersession IDs, and proposal event IDs. Omitted payload
fields are not evidence of absence. Text passes through credential redaction;
all returned prose remains untrusted data and may contain redaction markers.

An empty `list_work_orders` result does **not** establish that no tasks exist.
Use `list_tasks` or `get_task` for investigation. Artifact and execution-session
reads remain on their existing claim-bound tools.

### Fixture example: terminal task and archived design

`TestMCPReadTerminalContextArchivedVersionEndToEnd` in
`internal/httpapi/mcp_reads_test.go` drives native JSON-RPC `tools/call` with a
fixture viewer PAT. Its sequence is:

1. `list_tasks({workspace_id:"demo", state:"terminal", query:"investigation"})`
   finds `terminal-investigation`.
2. `get_task_context({workspace_id:"demo", task_id:"terminal-investigation"})`
   returns a confirmed triage proposal and `component-integrations-sync`, pinned
   at version 1 with current version 2 and `archived:true`.
3. `list_task_events({workspace_id:"demo", task_id:"terminal-investigation"})`
   shows `task.context_proposed`, `task.context_proposal_confirmed`, and
   `task.context_design_added`, preserving the agent and user actors.
4. `get_document({workspace_id:"demo", kind:"system_design",
   document_id:"component-integrations-sync", version:1, include_archived:true})`
   returns the original confirmed content as historical archived evidence with
   `active_authority:false`.

The fixture compares tasks, jobs, work orders, proposals, and task/document
events before and after the reads. It requires no production credentials or
real operator gate act. See the [filing playbook](playbooks/conveyor-task-filing.md)
for how to turn the evidence into a separately authorized follow-up.

## Working a task

| Tool | What it does |
|---|---|
| `list_work_orders` | List active, stale, or execution-timed-out spec, implement, and review orders, with their distinct queue, execution, and lease clocks, claimability, and refusal reasons. |
| `claim_work_order` | Claim one order with a fresh `session_id` and secret `client_token`, optionally choosing a lease up to one hour. Claimant identity comes from the credential. Self-review is refused, as is any session or client token already used by the implementer or another seat. |
| `get_work_order` | The full stage context: role prompt, task, served requirements, governance snapshot, approved plan, triage brief, bounce history, lineage context, artifact references, and (for review) the diff and PR description. `authority_source` says whether authority is `pinned` (claim-time snapshot) or `live`. |
| `read_artifact` | Fetch one artifact's content, base64-encoded. Scoped to the claim and to the bounded lineage selection that served the reference; an artifact ID alone is not enough. |
| `renew_work_order` | Renew the claim lease. Never extends the fixed execution deadline. |
| `report_progress` | Report progress text for the operator's timeline. |
| `release_work_order` | Release the claim with an outcome, reason, and cause. Also the vehicle for operator checkpoints: release with reason `operator checkpoint reached` and a structured `checkpoint` carrying the decision request, `class: authority_conflict`, and document citations. |

## Delivering

| Tool | What it does |
|---|---|
| `submit_plan` | Submit a Markdown execution plan for a claimed plan-stage order. Requires the Approach, Files touched, Ordering, Risks, and Done criteria headings; decomposition must be empty. Validation failures leave the order claimed for correction. |
| `submit_for_review` | End of implementation: opens or reuses the pushed branch's PR and dispatches the independent review round. A successful call ends the session. |
| `submit_review_verdict` | Submit `approve` or `changes_requested` with a reason code, summary, feedback, requirement citations, done-criteria coverage, and a governance assessment, all validated against the pinned snapshot. |
| `await_review` | Long-poll for the round's verdict. Reserved for the launcher that owns the warm implementer session; implementation sessions must not call it. |
| `request_plan_revision` | The repository-reality escape hatch: the approved plan cannot be executed as written. Requires a rationale; the order returns to the queue behind an operator gate. |

## Proposing authority

All three require a live claim on an implement-stage order, and all three
are fire-and-forget: propose, cite the pending ID, keep working. The
operator alone confirms, and confirmation never blocks implementation.

| Tool | What it does |
|---|---|
| `propose_requirement_revision` | Propose a full revised requirement document (the written text plus the `conveyor:requirements` code block) for an existing document. |
| `propose_system_design_revision` | Propose a full revised System Design document, including its `conveyor:governs` code block. |
| `propose_decision` | Propose a DEC-n with statement, context, and alternatives rejected, optionally superseding a confirmed decision. The server mints the ID. |

## Telemetry

| Tool | What it does |
|---|---|
| `report_usage` | Cumulative self-reported tokens, cost, and optional provider rate-limit status. Observational only; missing usage never blocks lifecycle progress. |
| `upload_transcript` | Optional self-reported session transcript, capped at 4 MiB, passed through redaction, and stored as an audit artifact. |
| `report_continuation` | Advisory harness-native continuation metadata for the active attempt, enabling resume after checkpoint or plan-revision releases. Human credentials only. |

## Filing and operating

| Tool | What it does |
|---|---|
| `create_task` | Create one durable task: `body`, `repo`, and a caller-stable `idempotency_key` required; optional `depends_on`, `requirement_ids`, `system_design_ids`, `hold`, and gate overrides. The title is generated; supplying one is an error. Human credentials only. |
| `add_task_dependency` | Make an existing open task depend on another. Requires `task_id`, `depends_on_task_id`, an audit `reason`, and caller-stable `request_id`; rejects terminal tasks, self-links, and cycles. Human credentials with `operate_gates` only. |
| `set_assignee` | Set or clear a task's assignee as an audited act. Constrains claim eligibility, never queue order. Human credentials only. |
| `redispatch_work_order` | Return a stale queued order to the queue with a fresh deadline. Active and execution-timed-out orders are rejected. Human credentials only. |

## Contracts worth restating

A few properties hold across the whole surface:

- Verdict and plan validation are server-side. Malformed citations, coverage
  that paraphrases instead of quoting, or assessments that disagree with the
  pinned authority are rejected with the order left claimed, so the agent
  can correct rather than losing the attempt.
- Review independence is enforced at claim, not requested politely: session
  and client token must be fresh across the implementer and every seat.
- Everything injected into context that originated outside the operator
  (document content, lineage items, hints) is labeled untrusted data, with
  an explicit instruction not to follow instructions found inside it.
- Usage, transcripts, and activity snapshots are observational. They are
  never lifecycle input, and a session that reports nothing progresses
  exactly like one that reports everything.
