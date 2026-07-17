# GitHub lifecycle coordination

Phase 5.3 projects Conveyor's durable lifecycle onto GitHub without making
GitHub the source of truth (spec §21.12 change 5, amended by §21.15).

## Issue on spec approval

Approving a spec transactionally records the approved version before Conveyor
queues one workspace-scoped GitHub publication intent. The issue contains the
exact approved specification, including its intent and acceptance criteria,
and an HTML task marker. The task detail, activity, requirements, and REST task
read models expose the publication state, repository, source provenance, issue
number, and URL.

For `github:<owner>/<repo>#<number>` intake, Conveyor validates that the source
repository matches the configured task repository, preserves the original
issue body, and appends or updates the approved-spec section on that issue.
Other tasks create a new issue. Before creating, the publisher exhaustively
pages the repository's issues in stable creation order and looks for the exact
task marker; it does not rely on GitHub's eventually indexed or capped search.
Immediately before the external create call, Conveyor durably advances the
create state from `not_started` to `reconciling`. If GitHub accepts creation but
Conveyor loses the acknowledgement, every later attempt is reconciliation-only:
an initial lookup miss remains retryable and can never authorize a second
create. Finding the marker advances the state to `confirmed`. River's bounded
attempts expose a durable failure if the remote issue does not converge in
time, and startup reconciliation can retry that same lookup safely.

Publication failures are visible as `github_issue.publication_retry` or
`github_issue.publication_failed` events. Startup reconciliation recreates a
missing approval outbox record and re-enqueues unfinished publications in the
same workspace. Operators should fix GitHub authentication or repository
configuration and allow reconciliation to retry; they should not create a
second issue manually.

## PR at submit

The implementing agent still owns commits and pushes. Conveyor creates no ref,
stub commit, push-event match, or draft PR. At `submit_for_review`, the factory
requires the approved issue association to be published, then opens or reuses
the pushed branch PR and reconciles its body to include `Closes #N`. A source
issue is the same `#N`, so a successful merge closes the original issue.

## Review trail and resolutions

Every review work order retains its own idempotent `Conveyor / Code review`
Check Run. The existing factory PR comment is a deterministic aggregate of all
single-review or panel-seat verdicts. Each entry identifies its review round,
seat/work order, verdict, reason, and feedback. Requested changes remain in the
history and are labelled `unresolved`, `resolved`, or `superseded` as later
rounds arrive; a retry updates the same check/comment instead of duplicating
it. Publication outcomes remain visible in the Conveyor event stream.

The durable Conveyor review state is authoritative. GitHub is the auditable
projection and can be rebuilt after partial forge failures. Repository slugs,
issue associations, and publication jobs are always checked in explicit
workspace scope.

## Explicit boundary

Phase 5.3 does not create a PR before `submit_for_review`, does not create draft
PRs on first push, and adds no push-event matching, draft-to-ready transition,
or orphan-draft cleanup. It also does not implement Phase 5.4 verification
evidence gating.
