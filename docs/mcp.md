# MCP reference

Conveyor's MCP server is the protocol boundary agents work through. It lives
at `<server>/mcp`, speaks streamable HTTP, and authenticates with a bearer
token: a personal access token for interactive sessions, or the worker
credential the launcher injects for dispatched ones. `conveyor mcp install`
registers it with detected Claude Code and Codex clients using an
environment-backed token reference.

Workspace scope follows the same rule as REST: pass `workspace_id`, or omit
it only when the credential belongs to exactly one workspace. Worker
credentials are pinned to their own workspace automatically.

Five tools are reserved for human credentials and refuse agent and dispatched
worker sessions: `create_task`, `add_task_dependency`, `set_assignee`,
`redispatch_work_order`, and `report_continuation`.

The agent-facing discipline for using these tools well is the
[work playbook](playbooks/conveyor-work.md); this page is the tool
inventory.

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
