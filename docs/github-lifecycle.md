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
create state from `not_started` to `reconciling`, increments the durable create
attempt count, and clears the reconciliation-miss count. If GitHub accepts
creation but Conveyor loses the acknowledgement, retries are initially
reconciliation-only: each one exhaustively checks every issue for the exact
marker. The first two no-marker passes cannot authorize another create. After
those two durable misses establish a bounded no-marker window, exactly one new
create attempt is authorized and the miss counter resets. This also recovers when
the original command genuinely failed before GitHub created anything, without
turning every retry into a possible duplicate. Finding the marker advances the
state to `confirmed` and clears the misses. River's bounded attempts expose a
durable failure if the remote issue does not converge in time, and startup
reconciliation resumes the same durable state machine safely.

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

Each reviewed commit receives one idempotent aggregate `Conveyor / Code review`
commit status. It remains pending until the round completes, then reports
unanimous approval or requested changes. The existing factory PR comment is a
deterministic aggregate of all single-review or panel-seat verdicts. Each entry identifies its review round,
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
