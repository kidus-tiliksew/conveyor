# Known limitations

These are the accepted boundaries of the Phase 4.7 implementation. Historical
sandbox-era limitations were retired with the execution-plane demolition in
spec §21.4.

## External work-order usage is self-reported

Operator-owned MCP agents report token and USD usage through `report_usage`.
Conveyor persists it with `self_reported: true` as observational audit data;
it does not allocate spending or gate claims, progress updates, or submissions
from those values. Wall-clock timeouts remain enforced independently. Conveyor
cannot independently verify an external provider's bill.

In-process stages record exact input/output/cached token counts returned by the
Responses API. Their transcripts pass through the normal redaction path and
USD usage is calculated from Conveyor's explicit standard-rate catalog, which
currently covers GPT-5.6 Luna and the GPT-5.4 family, including GPT-5.4's
documented long-context multiplier. Unknown models fail closed until their
rates are added; non-standard service tiers or regional uplifts are not
represented.

## Work-order clocks are independent

A claim may request a lease of at most one hour. Expired claims return to the
queue when work orders are listed or claimed, but reclaiming never extends the
fixed execution deadline that began at the first successful claim. Unclaimed
orders have a separate configurable queue-retention timeout (default `24h`);
after that they become explicitly `stale` and require
`redispatch_work_order`. Phase 4.7 has no heartbeat tool; long work should
claim again after expiry or choose a lease matching the expected operation.

## Task worktrees are local operator state

Task intake stores a canonical branch name and selected base as metadata; it
does not create a local or remote ref. The operator-owned implementation agent
must resolve a dedicated checkout with `conveyor checkout <task-id>` and then
push that exact branch before review (spec §21.8). Worktree registrations and
paths remain local to the operator's clone; Conveyor stores the assigned branch
and base but does not centrally track or clean those local directories. The CLI
therefore performs the safety checks and returns the local path on each use.

## Artifacts are stored in Postgres

Artifacts are content-addressed and limited to 25 MiB per upload. This is
appropriate for context files and dogfood scale, not large binaries. Object
storage is intentionally outside the accepted pre-Beta scope.

Artifact links carry a durable semantic role. `task_context` identifies
operator- or user-supplied context that an in-process stage may receive;
`generated_audit` and `generated_output` remain task/workspace-owned and
downloadable but are never model inputs. In particular, successful, failed,
and self-reported transcripts keep their existing content-addressed,
redacted audit path without accumulating on retries or human redirects.
Migration 027 classifies historical artifact-backed transcripts from their
existing transcript/job/task relationship, while retaining unclassified
historical attachments as task context.

## In-process image and failure diagnostics

OpenAI Responses requests send supported PNG, JPEG, non-animated GIF, and WEBP
artifacts as Base64 data URLs in `input_image.image_url`; images are never sent
as generic `input_file` parts. Conveyor validates the declared image format and
the configured model family before provider submission. The configured GPT-5.6
family, including `gpt-5.6-luna` and `gpt-5.6-terra`, is explicitly
image-capable. Other documented vision families are recognized in the
in-process capability table; an unknown or unsupported model fails locally
with an actionable diagnostic. Conveyor does not silently switch models.

Durable failure evidence identifies the phase (`attachment_preparation`,
`attachment_validation`, `client_validation`, `capability_validation`,
`provider_response`, `retry_exhausted`, or `response_validation`) and records
safe provider/model, attachment type/count, attempt count, HTTP status,
provider code, and upstream request ID fields when available. Credentials,
authorization headers, and binary attachment bytes are omitted and the
transcript still passes through normal redaction. Transport failures, HTTP 429
responses, and retryable 5xx responses retain the bounded three-attempt policy.

## Mechanical verification is repository-owned

Conveyor opens the pull request and audits review, but repository CI performs
tests and other mechanical verification. A managed verification agent and
runner return only if Phase 8 is explicitly activated.
