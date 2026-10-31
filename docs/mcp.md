# MCP reference

Conveyor's MCP server is the protocol boundary agents work through. It lives
at `<server>/mcp`, speaks streamable HTTP, and authenticates with a bearer
token: a personal access token for interactive sessions, or the worker
credential the launcher injects for dispatched ones. `conveyor mcp install`
creates a named personal connection for each saved server. Codex and Claude
retrieve the saved credential through native header helpers; Cursor and OpenCode
use separate server-specific token variables. Endpoints are literal canonical
URLs. [Personal connections](#personal-connections) describes naming,
two-server installation, migration, restart, and native connection
troubleshooting. Config/parser acceptance is separate from native
initialization and tools-list results. Personal installation does not replace
worker credential attachments.

Workspace scope follows the same rule as REST: pass `workspace_id`, or omit
it only when the credential belongs to exactly one workspace. The operator
investigation tools below always require an explicit `workspace_id`. Worker
credentials are pinned to their own workspace automatically.

Every tool names one capability, checked against the live role binding of the
credential's owning user on every call. A refusal answers `workspace_not_found`,
exactly like an unknown workspace.

An agent credential may use only tools whose capability every contributor
holds, plus `create_tasks`: today `view_workspace`, `claim_work`,
`request_changes`, `propose_documents`, and `create_tasks`. The rule is derived
from the role bundles, not from a list of tool names, so a tool added under a
higher capability is refused for agents automatically. That refuses
`add_task_dependency` and `attach_task_branch` (`operate_gates`), `set_assignee`
(`set_assignee`), and `redispatch_work_order` (`recover_work`) with
`<tool> requires an operator-scoped user credential`. Two separate rules refuse
agents the same way: the operator investigation tools below admit user
credentials only, and `report_continuation` belongs to the launching client
(`conveyor run` or a worker). `create_task` is the one maintainer act an agent
credential may perform, under the rules in its row below (DEC-60).

The agent-facing discipline for using these tools well is the
[work playbook](playbooks/conveyor-work.md); this page is the tool
inventory.

## Personal connections

`conveyor mcp install` registers a personal connection for sessions that plan
documents or operate Conveyor through MCP. The
[setup playbook](playbooks/conveyor-install.md) runs it after
`conveyor auth login` and `conveyor skills install`. `--tool
codex|claude|cursor|opencode` selects one client. `--list` reports planned
registration changes without writing files, running helpers, or opening
connections. OpenCode can be selected before its binary is installed; the
output then reports parser validation as skipped. Skills and MCP registrations
are separate installations.

### Two servers and stable names

Each saved server gets its own connection and credential:

```sh
conveyor mcp install --server https://second.example.com/team --name team-conveyor
conveyor mcp install --server https://factory.example.com
```

Reinstalling without `--name` reuses the sole owned name for that endpoint.
Otherwise the default is `<host-slug>-<12-character-URL-hash>-conveyor`: the
slug uses at most 30 lowercase ASCII letters, digits, or hyphens, and the hash
is the first 12 hex characters of SHA-256 over the canonical URL. Scheme,
explicit port, and base path participate; `https://host`, `https://host:443`,
and `https://host/team` are distinct. Canonicalization follows login:
lowercase scheme and host, remove trailing slashes and one terminal `/mcp`,
and reject credentials, queries, and fragments.

`--name` trims whitespace, lowercases ASCII letters, and accepts 1–63 letters,
digits, underscores, or hyphens, starting with a letter or digit. An occupied
name with a different endpoint is refused, including an installer-owned name.
A new name for one existing owned endpoint renames that registration and
retains its policies. Multiple owned matches require an explicit existing
name.

### Credential retrieval and client configuration

| Client | Personal config | Credential reference |
|---|---|---|
| Codex | `~/.codex/config.toml` | Native `http_headers_helper` command |
| Claude Code | `~/.claude.json` | Native `headersHelper` command |
| Cursor | `~/.cursor/mcp.json` | `Bearer ${env:CONVEYOR_MCP_TOKEN_<HASH>}` |
| OpenCode | `$XDG_CONFIG_HOME/opencode/opencode.json`, default `~/.config/opencode/opencode.json` | `Bearer {env:CONVEYOR_MCP_TOKEN_<HASH>}` with `oauth: false` |

All registrations use a literal canonical endpoint. `<HASH>` in the table is
the full uppercase SHA-256 of the canonical server URL; the installer prints
the actual variable and export. Cursor and OpenCode require their individual
variables in the environment that launches the client. A desktop application
launched elsewhere may not inherit those exports.

Codex and Claude run the installed Conveyor executable with
`auth token --server <base> --credentials-file <absolute-path> --format http-headers`.
The command prints a JSON Authorization header only into the client's private
pipe. It reads the saved credential afresh and never substitutes the ambient
`CONVEYOR_API_TOKEN`. The config contains only a quoted command and paths;
there is no helper script or token copy. The explicit credential-file
reference also supports a nondefault configuration directory on desktop
launches.

The ordinary `auth token` form remains explicit raw-token output for command
substitution. Neither output form belongs in a config, log, or transcript.
The CLI's normal server-bound environment precedence and worker child
credentials are unchanged. Personal installation does not set up a worker's
environment attachment; the harness pages describe that registration.

### Migration and connection checks

Only installer-marked registrations are migrated automatically. A matching
legacy `conveyor` registration moves to the selected name while nested tool
policies, timeouts, and enablement survive. Manual registrations remain
intact; `--name <existing-name> --adopt` permits adoption only when the
endpoint matches and no conflicting credential source is configured.
Unsupported TOML layouts, duplicate members, ambiguous endpoints, and name
collisions are refused before publication. Codex adoption needs conventional
`[mcp_servers.name]` tables.

Legacy Cursor and OpenCode shared-environment registrations may serve a
worker. Migration requires `--adopt` and a matching `CONVEYOR_ADDR` endpoint;
otherwise they remain intact. The installer never repairs a worker attachment
during a launch. Files are written atomically with owner-only permissions, and
failed validation preserves the prior file. OpenCode keeps its owner marker
inside each entry and refuses comments it cannot preserve.

Installation reports the name, endpoint, credential source, file result,
parser result, restart requirement, and native connection result separately.
A client loads a new registration after a restart. Codex and Claude parser
checks and OpenCode's staged `debug config` check do not initialize MCP. The
installer reports native initialize and tools-list as **not run**; the named
connection in the client shows the native result. The controlled repository
checks exercise actual native initialization and `tools/list` without
production credentials. Unsupported clients require an upgrade, not a
shared-token fallback.

After rotation, log in again for that server. Codex caches helper headers for
a connection and can refresh once after an authentication rejection; Claude
reads its helper at connection and reconnection. Cursor and OpenCode need
their individual exports refreshed and the client restarted. A missing saved
credential produces a non-secret login instruction. A revoked credential is
rejected by the server on use; a native 401 or 403 means the selected server's
saved credential or export needs checking, without switching tokens.

Client references: [Codex HTTP helpers](https://learn.chatgpt.com/docs/extend/mcp?surface=cli),
[Claude dynamic headers](https://code.claude.com/docs/en/mcp#use-dynamic-headers-for-custom-authentication),
[Cursor MCP](https://cursor.com/docs/mcp), and
[OpenCode remote MCP](https://opencode.ai/docs/mcp-servers/).

When native MCP receives an external denial, the actual client error and
endpoint are an operator checkpoint. A generic HTTP probe is not native MCP
connection evidence, and bypassing TLS or changing Cloudflare policy to hide a
failure is not a fix. The plugin provides connection-neutral guidance. An old
plugin-provided localhost connection is removed or disabled separately from
native registration migration.

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
| `get_task` | `task_id` | Includes terminal tasks and body; excludes execution setup and work-order credentials. `next_action` carries the server-stated `label`, `actor`, `kind`, `judgment`, `summary`, `deadline`, and `blocked_by`; `get_task_status` returns the full statement. |
| `list_operator_inbox` | None | The inbox `GET /v1/next-actions` serves, in the same server order: decisions first, then tasks blocked by other tasks, then acts needing no judgment, with pending document proposals among the decisions. `band`: `decisions`, `blocked`, or `automatic`; exact `state`, `repository`, `serves_requirement`, `governing_design`, and `assignee`; `query` matches as the REST `q` filter does. |
| `list_task_events` | `task_id` | Exact `event_kind`; oldest timestamp first, then ascending event ID. Long histories arrive in bounded windows; see [Task event windows](#task-event-windows). |
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
snapshot freezes rendered results for five minutes, is bound to the presenting
credential (another credential of the same user cannot use it), workspace, and
query, and is held only by the serving process. A first `get_task`,
`get_document`, or `get_decision` read that returns its complete single item
retains nothing: its response has no `snapshot`, `expires_at`, or
`next_offset`, and a client that needs the item again restarts the read. Expiry, process
restart, another server replica, changed filters, and changed ownership refuse
reuse. Restart at offset zero without a snapshot. Membership is rechecked on
every call; workspace-list snapshots also recheck every listed membership.

A snapshot holds at most 1000 items and 1 MiB of rendered item data. Each
credential retains at most 8 snapshots (event windows included) and 16 event
cursors across all of its workspaces; a new read at that cap evicts the same
credential's least-recently-used snapshot or traversal, never another
caller's. Each process holds at most 32 snapshots and 64 cursors. At that
ceiling a new read is refused with `snapshot capacity reached: process limit
32 retained snapshots; retry at <time> after expiry (within 5 minutes)` (or
the `event cursor capacity reached: process limit 64 retained cursors` form),
naming the earliest expiry; pages of existing snapshots still work. Each
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

### Task event windows

`list_task_events` reads a task history of any length as a sequence of bounded
immutable windows (`component-mcp-investigation-reads`; `component-mcp-protocol`
owns the transport). The first request uses
the ordinary `workspace_id`, `task_id`, optional exact `event_kind`, and
`limit`; it needs no cursor and succeeds for histories over 1000 events.

- **Capture.** The first request records, from one consistent backend read, the
  task's highest visible event ID and the number of matching events at or below
  it. Every later window of that traversal is limited to that ID ceiling.
  Events appended afterwards, including events with backdated timestamps,
  appear only in a fresh traversal.
- **Window fields.** Inside a window, `snapshot`, `offset`, `next_offset`,
  `limit`, `expires_at`, and `evidence` keep their usual meaning: `total` is
  the item count of *this window* and offsets page within it.
  `history_total` is the captured count of matching events for the whole
  traversal. A history that fits one window has no `next_cursor` and
  `history_total` equals `total`.
- **Continuation.** When the requested page exhausts a window and captured
  matching events remain, the response carries `next_cursor`. Send the same
  `workspace_id`, `task_id`, `event_kind`, and `limit` with `cursor` set to
  that value and no `snapshot` or `offset` argument at all; a cursor combined
  with either, including `offset: 0`, is refused.
  The reply is the first page of the next window, with its own `snapshot`.
  Absence of both `next_offset` and `next_cursor` means the traversal is
  complete. Never treat the first window as the complete history.
- **Bounds.** A window holds at most 1000 events and 1 MiB of rendered items.
  The store fetches at most 1001 candidates per window and charges every
  stored column against a 1 MiB source-byte budget. Only candidates inside
  both the 1000-event limit and that budget transfer their variable-width
  columns; the lookahead and every candidate past the budget return only
  fixed-width ID, time, and size metadata. A window that reaches a byte
  budget ends early and the cursor continues from its last event. Every
  response stays within 64 KiB of JSON text.
- **Retries.** If a cursor response is lost, repeat the identical cursor
  request: it returns the same window until you use that window's `snapshot`
  or its `next_cursor`, after which the consumed cursor is retired. Opening a
  window retires its predecessor's snapshot, so one traversal occupies one of
  its credential's snapshot slots no matter how many windows it spans.
  Capacity admission counts the cache after that replacement, so advancing an
  existing traversal is never refused for the slot it frees.
- **Errors and restarts.** `event <id> exceeds the 65536-byte output budget`
  and `event <id> exceeds the 1048576-byte event window budget` name an event
  that can never fit; the traversal does not advance past it, so narrow
  `event_kind`. `event history changed: restart read` means an event at or below
  the captured ceiling committed after capture (IDs are allocated before
  commit); start a fresh traversal without a cursor. `snapshot unavailable:
  restart read` covers a retired, expired, tampered, or foreign cursor or
  snapshot, a changed filter, a process restart, and another replica. Expiry
  stays at the first window's five-minute deadline and is never extended. A
  full process cache refuses new traversals with `snapshot capacity reached`
  and the earliest expiry; a credential at its own cap evicts its
  least-recently-used traversal instead.
- **Authorization.** Every page and cursor rechecks the user credential,
  membership, and `view_workspace` before any cache or store access; revoked
  access answers `workspace_not_found`. Redaction, payload allowlisting, exact
  integer IDs, and recorded attribution apply to every window, and no window
  read changes task, job, order, lease, queue, proposal, or event state.

For example, a task with 2105 renewal events read with `limit: 25` returns
`total: 1000` and `history_total: 2105` in its first window. That window has
40 pages: the first 39 carry `next_offset`, and the 40th carries
`next_cursor`. The second window again has `total: 1000`; the third has
`total: 105` and ends the traversal. `TestMCPTaskEventsTraverseLongHistoryAcrossWindows`
in `internal/httpapi/mcp_task_events_test.go` drives a comparable traversal
over native JSON-RPC and compares it with the complete ledger order.

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
| `get_task_status` | The task's next action exactly as `GET /v1/tasks/{id}/next-action` returns it: label, who acts next, kind, judgment, summary, question, choices with their effects, what happens if nobody acts, deadline, blockers, and the current order and attempt. For an agent question, `details` carries the checkpoint's decision request, `checkpoint_class`, `citations`, and the cited documents' `pending_proposals`; an `authority_conflict` checkpoint lists confirm and dismiss for each cited pending version before the answer choice. A human credential reads any task in the workspace. An agent or run-child credential must pass the `work_order_id` and `session_id` of an order its session claimed or submitted for that task; every refusal is the same error, whether or not the task exists. Read only: it never renews the lease or records an observation. Worker credentials are refused. |
| `read_artifact` | Fetch one artifact's content, base64-encoded. Scoped to the claim and to the bounded lineage selection that served the reference; an artifact ID alone is not enough. |
| `renew_work_order` | Renew the claim lease. Never extends the fixed execution deadline. Requires `claim_work`. |
| `report_progress` | Report progress text for the operator's timeline. Requires `claim_work`. |
| `release_work_order` | Release the claim with an outcome, reason, and cause. Also the vehicle for operator checkpoints: release with reason `operator checkpoint reached` and a structured `checkpoint` carrying the decision request (at most 1024 Unicode characters), `class: authority_conflict`, and document citations of the confirmed versions in conflict. A System Design clause may name a code-formatted heading. Requires `claim_work`. |

Every claim-bound tool requires `claim_work`, so a live claim never outlasts
its holder's binding: a user demoted to viewer keeps the claim (demotion
releases nothing) but every call under it is refused until the lease expires or
the binding is restored.

## Delivering

| Tool | What it does |
|---|---|
| `submit_plan` | Submit a Markdown execution plan for a claimed plan-stage order. Requires the Approach, Files touched, Ordering, Risks, and Done criteria headings; decomposition must be empty. Validation failures leave the order claimed for correction. |
| `submit_for_review` | End of implementation: opens or reuses the pushed branch's PR and dispatches the independent review round. A successful call ends the implementation claim. A launched session then exits; a self-claimed session continues the `conveyor-work` delivery loop. |
| `submit_review_verdict` | Submit `approve` or `changes_requested` with a reason code, summary, feedback, requirement citations, done-criteria coverage, and a governance assessment, all validated against the pinned snapshot. |
| `await_review` | Long-poll for the round's verdict on a submitted implementation order. A self-claimed implementation session calls it after submission; a session launched by `conveyor run` or a worker does not, because its launcher owns verdicts. |
| `request_plan_revision` | The repository-reality escape hatch: the approved plan cannot be executed as written. Requires `claim_work`, the caller's own live implement claim, and a rationale; the order returns to the queue behind an operator gate. |

## Proposing authority

All three require `claim_work` and the caller's own live claim on an
implement-stage order, and all three are fire-and-forget: propose, cite the
pending ID, keep working. A viewer is refused even while it holds a live claim.
The operator alone confirms, and confirmation never blocks implementation.
Proposals outside a claim go through the REST routes, which require
`propose_documents`.

At an authority-conflict checkpoint that the approved plan or work-order
contract directs, pass the optional boolean `authority_conflict_checkpoint:
true`. The result's guidance then directs the checkpoint release: cite the
pending ID in `checkpoint.decision_request` and call `release_work_order`
instead of `submit_for_review`. Without the flag the guidance stays
fire-and-forget. The server records nothing from the flag.

| Tool | What it does |
|---|---|
| `propose_requirement_revision` | Propose a full revised requirement document (the written text plus the `conveyor:requirements` code block) for an existing document. |
| `propose_system_design_revision` | Propose a full revised System Design document, including its `conveyor:governs` code block. |
| `propose_decision` | Propose a DEC-n with statement, context, and alternatives rejected, optionally superseding a confirmed decision. The server mints the ID. |

## Telemetry

| Tool | What it does |
|---|---|
| `report_usage` | Cumulative self-reported tokens and optional provider rate-limit status. Observational only; missing usage never blocks lifecycle progress. |
| `upload_transcript` | Optional self-reported session transcript, capped at 4 MiB, passed through redaction, and stored as an audit artifact. |
| `report_continuation` | Advisory harness-native continuation metadata for the active attempt, enabling resume after checkpoint or plan-revision releases. Requires `claim_work`; the launching client's user or worker credential only, never the agent session it launched. |

## Filing and operating

| Tool | What it does |
|---|---|
| `create_task` | Create one durable task: `body`, `repo`, and a caller-stable `idempotency_key` required; optional `depends_on`, `requirement_ids`, `system_design_ids`, and gate overrides. A task carries no hold (DEC-74): a new `hold: true` or a non-boolean `hold` is refused with `task_hold_retired`, while a retry whose key names an existing task still returns it. The title is generated; supplying one is an error. Requires `create_tasks`. An agent credential may call it when its owning user holds `create_tasks` in the workspace; for an agent, omitted gates take the workspace defaults, `true` turns a gate on, and `false` or a legacy level `L0` or `L1` is refused with `agent_gate_disable_forbidden` (a null or non-boolean gate is `invalid_agent_gate_override`). The task's `task.created` event records `trigger_provenance` naming the agent and its owning user. Worker credentials are refused. |
| `add_task_dependency` | Make an existing open task depend on another. Requires `task_id`, `depends_on_task_id`, an audit `reason`, and caller-stable `request_id`; rejects terminal tasks, self-links, and cycles. Requires `operate_gates`; human credentials only. |
| `set_assignee` | Set or clear a task's assignee as an audited act. Constrains claim eligibility, never queue order. Requires `set_assignee`; human credentials only. |
| `redispatch_work_order` | Return a stale queued order to the queue with a fresh deadline. Active and execution-timed-out orders are rejected. Requires `recover_work`; human credentials only. |

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
- Tool errors return in-band as text with `isError: true`, and the server
  logs each one with the request ID and tool name. A server-side database
  failure answers `<backend> database operation failed (request_id <id>)`.
  Quote that ID when reporting the failure: it finds the server's log line
  for the call. The backend logs the driver error just before that line, and
  the driver error never reaches the client.
