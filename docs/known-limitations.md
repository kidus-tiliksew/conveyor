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

## Work-order leases are claim-bounded

A claim may request a lease of at most one hour. Expired claims return to the
queue when work orders are listed or claimed. Phase 4.7 has no heartbeat tool;
long work should claim again after expiry or choose a lease matching the
expected operation.

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

## Mechanical verification is repository-owned

Conveyor opens the pull request and audits review, but repository CI performs
tests and other mechanical verification. A managed verification agent and
runner return only if Phase 8 is explicitly activated.
