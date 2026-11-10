# MCP reference

Conveyor's MCP server is the protocol boundary agents work through. It lives
at `<server>/mcp`, speaks streamable HTTP, and authenticates with a bearer
token: the personal access token of the person who started the agent
session. Every work order is claimed by such a session over MCP; Conveyor
launches no agent and has no worker or run-order plane (DEC-97(1)).
`conveyor mcp install`
creates a named personal connection for each saved server. Codex and Claude
retrieve the saved credential through native header helpers; Cursor and OpenCode
use separate server-specific token variables. Endpoints are literal canonical
URLs. [Personal connections](#personal-connections) describes naming,
two-server installation, migration, restart, and native connection
troubleshooting. Config/parser acceptance is separate from native
initialization and tools-list results.

Workspace scope follows the same rule as REST: pass `workspace_id`, or omit
it only when the credential belongs to exactly one workspace. The operator
investigation tools below always require an explicit `workspace_id`.

Every tool names one capability, checked against the live role binding of the
credential's owning user on every call. A refusal answers `workspace_not_found`,
exactly like an unknown workspace.

A session acts as the person whose token it holds, with that person's
capabilities, and its acts are recorded as that person's (DEC-116(2)). A stored
agent-class credential is refused at the mount with the same 401 as an unknown
bearer. No tool here decides a gate, cancels a task, writes configuration,
manages users or grants, or issues a token. That tool list, the work-order
instructions, and the operator's confirmation of every proposal are where the
rules of conduct hold; they rest on no credential check, and the same token
reaches any REST route its owner's role allows (DEC-116(4)).

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
The CLI's normal server-bound environment precedence is unchanged.

### Migration and connection checks

Only installer-marked registrations are migrated automatically. A matching
legacy `conveyor` registration moves to the selected name while nested tool
policies, timeouts, and enablement survive. Manual registrations remain
intact; `--name <existing-name> --adopt` permits adoption only when the
endpoint matches and no conflicting credential source is configured.
Unsupported TOML layouts, duplicate members, ambiguous endpoints, and name
collisions are refused before publication. Codex adoption needs conventional
`[mcp_servers.name]` tables.

Legacy Cursor and OpenCode shared-environment registrations that an earlier
worker attachment wrote migrate only with `--adopt` and a matching
`CONVEYOR_ADDR` endpoint; otherwise they remain intact. Files are written atomically with owner-only permissions, and
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
`view_workspace` capability. A viewer can use them without a claim. Agent
credentials are refused, except on `get_document` and `search_documents` under
a live claim ([Design document reads](#design-document-reads)). Missing
membership and missing capability both return `workspace_not_found`, including
on snapshot reuse.
Reads never call lifecycle reconciliation, claim work, renew leases, enqueue
jobs, or create/confirm proposals. The one record a read can write is the
observational consultation of a claim-bound System Design read
([Design document reads](#design-document-reads)).

| Tool | Required fields beyond `workspace_id` | Optional filters / selection |
|---|---|---|
| `list_workspaces` | None | Returns only the caller's own workspace memberships; use a known workspace ID as the explicit authorization scope. |
| `list_repositories` | None | Returns repository `name` and `base_branch`; configuration, URLs, credentials, and checkout paths are omitted. |
| `list_tasks` | None | `state`: `active` (default), `terminal`, or `all`; exact `repository`; `query` matches task ID, title, source, or branch. |
| `get_task` | `task_id` | Includes terminal tasks and body; excludes execution setup and work-order credentials. `plan_state` is `not_required`, `needed`, `awaiting_approval`, or `completed`, and no `spec_approval` is served. `next_action` carries the server-stated `label`, `actor`, `kind`, `judgment`, `summary`, `deadline`, and `blocked_by`; `get_task_status` returns the full statement. |
| `list_operator_inbox` | None | The inbox `GET /v1/next-actions` serves, in the same server order: decisions first, then tasks blocked by other tasks, then acts needing no judgment, with pending document proposals among the decisions. `band`: `decisions`, `blocked`, or `automatic`; exact `state`, `repository`, `serves_requirement`, `governing_design`, and `assignee`; `query` matches as the REST `q` filter does. |
| `list_task_events` | `task_id` | Exact `event_kind`; oldest timestamp first, then ascending event ID. Long histories arrive in bounded windows; see [Task event windows](#task-event-windows). |
| `get_task_context` | `task_id` | `proposal_state`: `all` (default), `proposed`, `confirmed`, or `dismissed`. Attachments are always included. |
| `list_documents` | `kind`: `requirement`, `system_design`, or `reference` | `query` matches ID/title; `include_archived` defaults false. Requirement/design listings contain confirmed document identities only. |
| `get_document` | `kind`, `document_id` | Positive integer `version` selects an explicit immutable version; omission selects current. `include_archived` is required for archived/deleted content. `outline` and `section_id` read part of a document; see [Design document reads](#design-document-reads). |
| `search_documents` | `query` | Literal, case-insensitive text search; see [Design document reads](#design-document-reads). `document_ids`, `kind`, `version`, `include_archived`. |
| `list_document_events` | `kind`, `document_id` | `event_kind`, `include_archived`; includes archive/restore history with recorded actors and times. |
| `list_decisions` | None | `query`; `include_history` adds superseded decisions to the default confirmed list. |
| `get_decision` | `decision_id` | `include_history` is required for superseded or pending records; neither grants active authority. |

Every tool accepts `limit` (integer 1–100, default 25). Every tool except
`search_documents` also accepts `offset` (integer 0–1000, default 0) and
`snapshot` (opaque 32-character hex token). Unknown
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
results: narrow filters or reduce the page size. A document too large for that
response is refused with a message naming the outline and section reads. A
single task too large for it must be read through its existing authenticated
REST surface. Task candidates are collected through existing 200-row store pages;
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

Events expose their recorded `actor_id`, `actor_role`, and `at`. An event
recorded under a retired agent credential also carries `actor_owner_user_id`,
the user who owns that credential; an unknown `agent:<id>` carries none, and
its `actor_id` stays as recorded (DEC-116(3)). Proposal
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

### Design document reads

A session finds and reads design text in parts, so every document is readable
whatever its size (`req-design-delivery` REQ-2, REQ-3, REQ-4; DEC-103). These
reads change no state, with one exception: a successful read of System Design
text under a live claim appends an observational consultation event; see
[Consultation](#consultation).

**Who may call them.** A credential's owner needs `view_workspace`, as every read
here does. A session holding a work order also passes the `work_order_id` and
`session_id` of its own live claim on every `get_document` and
`search_documents` call. Those arguments play no part in authorization: they only
attribute the session's consultation. A `get_document` call attributes one only
when:

- the owning user's live binding in the workspace holds `claim_work`;
- the named order exists in that workspace, its session is the named session,
  its claimant is the credential's owner, and no worker owns it; and
- the claim is live: state `claimed`, an unexpired lease, and an unelapsed
  execution deadline.

Otherwise the read is served and records nothing.

**Outline.** `get_document` with `outline: true` returns the document's
metadata and authority labels plus `outline`, a list of `{section_id, heading,
level, size_bytes}`, and no content, statements, or other body-derived payload:

```json
{"workspace_id": "demo", "kind": "system_design", "document_id": "component-http-api", "outline": true}
```

```json
{"items": [{"id": "component-http-api", "version": 82, "selection": "current", "version_status": "confirmed", "active_authority": true,
  "outline": [{"section_id": "s0", "heading": "", "level": 0, "size_bytes": 412},
              {"section_id": "s1", "heading": "Router", "level": 2, "size_bytes": 2048}],
  "section_offset": 0, "total_sections": 2, "complete": true, "truncated": false}],
 "total": 1, "offset": 0, "limit": 25, "evidence": "..."}
```

An outline too large for one response sets `truncated: true` and
`next_section_offset`. Repeat the call with `section_offset` set to that value
and `version` set to the returned version; the pages together return every
heading. A heading over 4 KiB is cut and labeled `heading_truncated: true`; its
section's text keeps it whole. A document with no confirmed version returns
`authority_absent: true` and an empty outline unless a `version` is named.

**Sections.** A section runs from its heading through the next recognized
heading. Sections split at ATX `##` and `###` headings, including those with a
closing `#` sequence or CRLF line ends, and never inside a backtick or tilde
code fence; an unclosed fence runs to the end of the document. `#` and `####`
headings stay inside their section. Text before the first heading is one section
with an empty `heading` and `level: 0`, and a heading-free document is one such
section. Empty content has no sections. `section_id` is the source-order
ordinal `s0`, `s1`, and so on, so duplicate headings stay distinct. **An ID is
valid only within one version:** name the same `version` when using IDs from an
earlier read. `size_bytes` is the UTF-8 byte size of the redacted text.

**Section read.** `get_document` with `section_id` returns `text`, the
`version` it came from, `section_id`, `heading`, `level`, `size_bytes`, and the
same authority labels as a whole-document read. `outline` and `section_id` are
mutually exclusive. An unknown ID is refused with a pointer to the outline.

```json
{"workspace_id": "demo", "kind": "system_design", "document_id": "component-http-api", "section_id": "s1"}
```

**Paging a large section.** Sections page by `byte_offset`, apart from the
`snapshot` and `offset` protocol, which these modes refuse. A page that does
not finish the section sets `truncated: true`, `complete: false`, and
`next_byte_offset`. The first response supplies the version, and every later
page must name it:

```json
{"workspace_id": "demo", "kind": "system_design", "document_id": "component-http-api", "section_id": "s1", "version": 82, "byte_offset": 61440}
```

The last page sets `complete: true`, `truncated: false`, and has no
`next_byte_offset`. Offsets count bytes of the redacted text. Pages end on UTF-8
boundaries and fit the 64 KiB response after redaction and JSON escaping, so a
page can hold fewer bytes than the limit. Concatenating the pages' `text`
reproduces the redacted section. An offset inside a character or past the end
is refused.

**Oversized whole documents.** A `get_document` with neither option behaves as
it always has for a document within the limit. Over the limit it answers
`read exceeds 65536-byte output budget: this document is too large for a
whole-document read; call get_document with outline=true ...`.

**Search.** `search_documents` takes `query` (required, at most 256 bytes),
`document_ids` (1–32 IDs), `kind`, `version`, `include_archived`, `limit`
(1–100, default 25), and no `offset` or `snapshot`. Omitting `kind` searches
every kind; `document_ids` and `kind` intersect.

```json
{"workspace_id": "demo", "query": "lease renewal", "kind": "system_design"}
```

```json
{"items": [{"document_id": "component-work-orders", "kind": "system_design", "version": 85, "selection": "current", "historical": false,
            "version_status": "confirmed", "active_authority": true, "informative": false, "archived": false,
            "section_id": "s12", "heading": "Claims", "section_size_bytes": 3120, "line": "Lease renewal never extends the deadline."}],
 "total": 1, "limit": 25, "documents_searched": 37, "truncated": false, "excerpts_truncated": 0, "evidence": "..."}
```

- **Matching.** The query is literal text, never a pattern, and matches
  case-insensitively within one line. Each matching line is one match, in
  document ID, section, and line order. Matching runs on redacted text, so a
  query cannot probe a hidden secret and cannot match a redaction marker.
- **Versions.** Without `version`, a search covers confirmed current requirement
  and design versions and current informative references, which keep
  `informative: true` and `active_authority: false`. A named `version` searches
  that version of each document in scope and may be proposed or historical,
  with the same labels and no more authority than a `get_document` of it.
  Archived or deleted documents need `include_archived`.
- **Truncation.** A response stops at `limit` or at the 64 KiB budget, then sets
  `truncated: true`, `truncated_by` (`limit` or `output_bytes`), and `advice` on
  narrowing by `document_ids`, `kind`, `version`, or `query`. Search has no
  continuation. `total` counts the matches this response holds.
- **Long lines.** A matching line over 2 KiB arrives as an excerpt containing
  the match, labeled `line_truncated: true` with the full `line_bytes`.
  `excerpts_truncated` counts them, apart from `truncated`.
- **Empty results.** No match returns `items: []`, `total: 0`, and `advice`.
  `skipped_documents` lists each named document that was not searched and why:
  `not_found`, `archived: include_archived required`, `no_confirmed_version:
  name a version`, or `version_not_found`.

**Cache.** Each server process holds parsed versions in a least-recently-used
cache keyed by workspace, kind, document ID, and version, bounded at 16 MiB of
body and derived text and 128 entries. A version larger than the bound is served
but not retained. Concurrent misses of one version share one store read. The
cache holds immutable content and a settled status only: confirmed, dismissed,
and retired are terminal. Membership, claims, archive state, and the current
version are read on every call, and a pending proposal named explicitly rereads
its row on every call, so changes show immediately. A search of resident versions
reads no document body. A cold or evicted version is read once, and a named
version absent from a document costs one row lookup on each call.

#### Consultation

A `system_design.consulted` event records that a session actually read design
text, with the served `version`, `work_order_id`, and `session_id`
(`req-design-delivery` REQ-5; DEC-103).

- **What records.** A `get_document` of `kind: system_design` that delivers body
  text within the output limit: a whole-document read, a section read, or any
  page of a section. Requirement and reference reads record none.
- **What records nothing.** An outline, a search and its internal cache fills, a
  listing, `get_work_order`, claim, renewal, context refresh, an oversized
  whole-document refusal, a missing version or section, and any refused read.
  Being named in a work order's list is not being read.
- **Attribution.** Pass the `work_order_id` and `session_id` of your own live
  claim. Every call rechecks them as described under **Who may call them**, so a
  later page or a cache hit attributes nothing a stale claim could not read. A
  user credential that omits them, or names anything but its own live claim,
  still reads and records nothing.
- **One record per read key.** Workspace, session, work order, document, and
  version record one event across repeats, pages, concurrent calls, and server
  processes. A new version, order, or session records its own.
- **Observational.** A ledger failure is logged and leaves the response
  unchanged, and a later read of that version records it. Drift judgment reads
  the delivering task's attached context, never these records. Events written
  earlier stay unchanged, including those of the earlier rule that recorded every
  listed design when a work order was delivered, and one with the same read key
  counts as that key's record.

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
| `claim_work_order` | Claim one order with a fresh `session_id` and secret `client_token`, optionally choosing a lease up to one hour. Claimant identity comes from the credential. Self-review is refused, as is any session or client token already used by the implementer or another seat. The response is the order without pinned bodies, plus `governance_references`; see [Work order delivery](#work-order-delivery). |
| `get_work_order` | The stage context: role prompt with the served requirement statements and the design and decision lists, task, approved plan, triage brief, bounce history, lineage context, artifact references, and (for review) the diff and PR description. It carries no document body; see [Work order delivery](#work-order-delivery). `authority_source` says whether authority is `pinned` (claim-time snapshot) or `live`. |
| `get_task_status` | The task's next action exactly as `GET /v1/tasks/{id}/next-action` returns it: label, who acts next, kind, judgment, summary, question, choices with their effects, what happens if nobody acts, deadline, blockers, and the current order and attempt. For an agent question, `details` carries the checkpoint's decision request, `checkpoint_class`, `citations`, and the cited documents' `pending_proposals`; an `authority_conflict` checkpoint lists confirm and dismiss for each cited pending version before the answer choice. Any workspace member reads any task in the workspace. Read only: it never renews the lease or records an observation. |
| `read_artifact` | Fetch one artifact's content, base64-encoded. Scoped to the claim and to the bounded lineage selection that served the reference; an artifact ID alone is not enough. |
| `renew_work_order` | Renew the claim lease. Never extends the fixed execution deadline. The response adds `governance_references`, as a claim's does. Requires `claim_work`. |
| `report_progress` | Report progress text for the operator's timeline. Requires `claim_work`. |
| `release_work_order` | Release the claim with an outcome, reason, and cause. Also the vehicle for operator checkpoints: release with reason `operator checkpoint reached` and a structured `checkpoint` carrying the decision request (at most 1024 Unicode characters), `class: authority_conflict`, and document citations of the confirmed versions in conflict. No proposal needs to exist. A System Design clause may name a code-formatted heading. Requires `claim_work`. |

Every claim-bound tool requires `claim_work`, so a live claim never outlasts
its holder's binding: a user demoted to viewer keeps the claim (demotion
releases nothing) but every call under it is refused until the lease expires or
the binding is restored.


### Work order delivery

A work order names its governing documents and carries none of their bodies,
so its response stays readable (`req-design-delivery` REQ-1;
`req-260802-72fc68` AC-2.2; `req-task-centric-delivery` AC-1.2; DEC-103). The
server keeps the complete snapshot: verdict validation and the
context-freshness comparison read it, not this list. Being named in this list
records no [consultation](#consultation).

**`get_work_order`.** `role_prompt` holds the role instructions, each served
requirement statement, and the two lists below, each once. `approved_spec.content`
holds the whole plan; the role points at its `## Done criteria` section rather
than repeating it. The response has no `governance_snapshot`, no
`served_requirements`, and no snapshot on `work_order`, which still names its
pin by `governance_snapshot_digest`. `lineage_context` keeps sibling and
dependency outcomes, evidence, and omission counts, without served requirement
text or the walk's node and link lists. With a plan of 12 KiB, 37 designs, and
103 long decisions, the response stays under 64 KiB excluding the review diff.

**Designs.** Each design is listed by ID, title, and version, in three labeled
groups: attached to the task, in attachment order; covering the task's diff;
then every other design governing the repository, both by ID. A design in two
groups is listed once, and attachment wins. A submission's diff-derived
attachments form the diff group, joined on a review order by the designs its
delivered diff reaches. With no diff that group is absent. A review or verify
order lists its pinned versions, never newer ones.

**Decisions.** Each confirmed decision is one line: its ID and a summary of its
statement, with whitespace normalized and cut at 240 characters and 240 UTF-8
bytes, ellipsis included. Superseded decisions are named with their status and
replacement, apart from that list, and are never active authority.

**Reading further.** Use `search_documents` and the `outline` and `section_id`
modes of `get_document` ([Design document reads](#design-document-reads)), and
name the listed `version`. A session passes `work_order_id` and `session_id` on
each call, and they record the consultation. A review reads every section,
including every page of a large one, of each design attached to the task or
covering the diff (`req-design-delivery` AC-6.1). `get_decision` and
`list_decisions` read a decision's full record.

**Claim, renewal, and refresh.** `claim_work_order`, `renew_work_order`, and
`refresh_work_order_context` carry no role prompt, so each adds one list at the
response root:

```json
{"governance_references": {
  "designs": [{"id": "component-http-api", "title": "HTTP API", "version": 82, "group": "attached"}],
  "decisions": [{"id": "DEC-103", "status": "confirmed", "summary": "Work orders name documents…"},
                {"id": "DEC-9", "status": "superseded", "superseded_by": "DEC-40"}]}}
```

`group` is `attached`, `diff_scoped`, or `repository`. The lifecycle act has
already succeeded when this list is read, so a failed read adds
`governance_references_error` instead of failing the call; `get_work_order`
delivers the list. These responses have no diff to read, so their diff group
holds submission attachments only. Refresh and `get_work_order` omit the current
`authority_references` from `context_freshness.snapshot` and keep
`authority_digest`, and `prior_authority_references` names only what changed.

**Events.** New `work_order.claimed` and `work_order.updated` events name the
order's pin by `governance_snapshot_digest` and, when the writer holds the pin,
in `governance_references` by ID, title, version, and status, without a summary
or group. Events already written stay unchanged, and readers keep accepting
their embedded snapshots.

## Delivering

| Tool | What it does |
|---|---|
| `submit_plan` | Submit a Markdown execution plan for a claimed plan-stage order. Requires the Approach, Files touched, Ordering, Risks, and Done criteria headings; decomposition must be empty. Optional `base_commit` is the planner checkout's `HEAD`, 40 lowercase hexadecimal characters, and the plan version records it with origin `plan_order`. Validation failures, a malformed `base_commit` included, leave the order claimed for correction and record no plan version. No submission approves a plan: the plan state moves to plan awaiting approval and the task waits at the plan approval gate for a person's decision, so the result's `approved` is always false. |
| `submit_for_review` | End of implementation: opens or reuses the pushed branch's PR and dispatches the independent review round. A successful call ends the implementation claim, and the session continues the `conveyor-work` delivery loop. |
| `submit_review_verdict` | Submit `approve` or `changes_requested` with a reason code, summary, feedback, requirement citations, done-criteria coverage, and a governance assessment, all validated against the pinned snapshot. |
| `await_review` | Long-poll for the round's verdict on a submitted implementation order. The implementation session calls it after submission; a verifier or reviewer it started does not. |
| `request_plan_revision` | The repository-reality escape hatch, on any implement claim. On a task whose planning is completed, it asks to revise the approved plan that cannot be executed as written. On a task with no plan required, it asks that the task be planned and contests no plan version. Requires `claim_work`, the caller's own live implement claim, and a rationale; the order returns to the queue behind an operator gate. An approved request sets planning needed and returns the task to the plan stage; a declined one returns it to implementation and keeps its plan state. |

## Proposing authority

A session holding a work order authors no requirement, System Design, or
decision version (DEC-102). The MCP surface has no tool that creates one.

`propose_requirement_revision`, `propose_system_design_revision`, and
`propose_decision` are retired. A call to one is refused and its error names the
two remedies: `request_plan_revision` when the approved plan conflicts with the
repository while confirmed documents stand, and the authority-conflict checkpoint
on `release_work_order` when the work would require changing a confirmed
document. The checkpoint needs no pending proposal.

Document changes go through operator-side planning: `conveyor plan push`,
in-product planning sessions, and the REST routes, which require
`propose_documents`. Only an operator confirms them.

## Telemetry

| Tool | What it does |
|---|---|
| `report_usage` | Cumulative self-reported tokens and optional provider rate-limit status, stored with the usage record on the order. Observational only; missing usage never blocks lifecycle progress. `source` admits only `self_reported`; `worker_fallback` is refused. |
| `upload_transcript` | Optional self-reported session transcript, capped at 4 MiB, passed through redaction, and stored as an audit artifact. |

## Filing and operating

| Tool | What it does |
|---|---|
| `create_task` | Create one durable task: `body`, `repo`, and a caller-stable `idempotency_key` required; optional `depends_on`, `requirement_ids`, `system_design_ids`, `plan_state` (`needed` or `not_required`), `plan`, `plan_base_commit`, and gate overrides. `spec_approval` is the legacy alias of `plan_state`, `true` as `needed` and `false` as `not_required`. A `plan` passes the content rules of a submitted plan and becomes plan version 1 with origin `intake` at plan state `awaiting_approval`; `plan_base_commit` is the full commit ID it was read against. `plan_state` with `plan`, an invalid plan, a disagreeing `spec_approval` and `plan_state`, and `plan_base_commit` without `plan` or not 40 lowercase hex characters are refused with `invalid_plan_intake`. MCP never records a plan approval: any `plan_approved` argument, null included, is refused with `invalid_agent_gate_override`. Every refusal precedes the idempotency lookup and records nothing. A task carries no hold (DEC-125(3)): a new `hold: true` or a non-boolean `hold` is refused with `task_hold_retired`, while a retry whose key names an existing task still returns it. The title is generated; supplying one is an error. Requires `create_tasks` on the credential's owner. An omitted gate takes the workspace default and a supplied boolean applies, `false` included. A null or non-boolean gate, a non-string level, or an unrecognized level is refused with `invalid_agent_gate_override` before any lookup, for every caller, so a mistyped gate never takes the workspace default. The task records MCP as its source and the person as its creator, enters triage like any other task, and grants the session nothing further (DEC-116(5)). |
| `add_task_dependency` | Make an existing open task depend on another. Requires `task_id`, `depends_on_task_id`, an audit `reason`, and caller-stable `request_id`; rejects terminal tasks, self-links, and cycles. Requires `operate_gates`. |
| `set_assignee` | Set or clear a task's assignee as an audited act. Constrains claim eligibility, never queue order. Requires `set_assignee`. |

No tool recovers or redispatches a work order (`req-security-boundaries`
AC-1.2). Those are operator routes that require `recover_work`:
`POST /v1/work-orders/{id}/recover` (`conveyor order recover`) and
`POST /v1/tasks/{id}/redispatch` (`conveyor order redispatch`).

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
