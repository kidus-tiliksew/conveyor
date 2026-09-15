# Reconciliation: README direct push

## Exact occurrence

- Workspace: `demo`; task: `260915-00ccb2`; approved plan: version 3.
- Repository: `kidus-tiliksew/conveyor` (registered as `conveyor`).
- Commit: `6c76388faf5fe52a8a0421c60ee9d5f407c654b8`.
- First and only parent: `4864149e29d8db9957eac3b8295875585de606e7`.
- Source: <https://github.com/kidus-tiliksew/conveyor/commit/6c76388faf5fe52a8a0421c60ee9d5f407c654b8>.
- Author and committer: Kidus Tiliksew (`kidus-tiliksew`).
- Author and commit timestamp: `2026-09-15T14:08:33Z`.
- Subject: `docs(readme): clarify Conveyor description`.
- Changed paths: only `README.md`, modified with 14 additions and 14 deletions.
- Observation identity: `direct_push:conveyor:6c76388faf5fe52a8a0421c60ee9d5f407c654b8`.

The GitHub connector's commit read and REST commit-detail read supplied the
metadata and complete patch on 2026-09-15. The launched shallow checkout at
`947b81e57e9b12e064e99739fbd320488059b3a7` did not supply the occurrence diff.
The commit describes two edits: clarify the introduction and move workspace
access instructions into a Contributing section. Its message reports
`git diff --check`; that historical claim is not validation of this delivery.

## Classification against confirmed authority

The prose preserves human confirmation of requirements, designs, and decisions,
agent execution on operator machines, and required human approvals. It changes
neither executable behavior nor the confirmed document corpus. No requirement
amendment or intentional change to governed behavior is needed.

`component-monitor-drift v3` governs monitor implementation paths, not README.
However, `component-runtime v7` explicitly includes `README.md` in its governs
fence. The changed introduction agrees with that document's control-plane and
operator-owned execution model. This is a governed direct push, even though its
content is consistent with the design. It must not be classified as an
out-of-scope change or as factory-reviewed delivery.

`req-260811-228be6 v5` AC-4.3 requires drift when a direct push touches a
document's governed scope regardless of task context. Its other requirements
cover staleness, signal presentation and resolution, and submission-time design
attachment; this README edit changes none of those contracts.
`req-delivery-and-forge v7` REQ-4 requires ordinary idempotent task intake,
occurrence provenance, audited reconciliation, and operator-confirmed corpus
changes. `req-260820-6a468a v2` provides task-authored revision proposals when
authority must change; no such revision is required for this correction.

## Monitor correction and verification boundary

`GitHubSource` used the first-parent comparison only to distinguish an empty
push from a changed tree. It discarded the file names. `Service.Process`
therefore recorded repository drift, but `recordSystemDesignDrift` returned
early because the observation had no changed paths. The code failed to produce
the design-specific drift required by AC-4.3 for this occurrence.

The correction carries the comparison paths into direct-push observations.
Existing service intake, poll retries, workspace checks, persistence, and audited
resolution remain the mechanisms that consume them. No store or migration
change is required. Incomplete comparison evidence must fail rather than be
treated as an empty push or a complete governed-scope evaluation.

The regression in `internal/monitor/service_test.go` sends the exact occurrence
through `GitHubSource`, new poller instances, and the memory service/store. It
checks matching design drift, absence of unrelated design drift, ordinary task
intake, provenance, occurrence audits, and persisted redelivery deduplication.
The fixture's design version is local test data, not a claim to reproduce the
live corpus version history. Source tests cover comparison validation.

This evidence follows `component-monitor-drift v3` and
`component-verification-strategy v6`. Fixture tests do not prove the daemon's
live GitHub App polling or either database backend. The work-order submission
records the fresh Make validation results and any environment blocks.

Repository drift remains distinct from document drift: the confirmed monitor
design requires a repository record for every drift-class task observation,
while document records require matching scope. Existing unresolved records
remain subject to operator-recorded audited outcomes. This reconciliation does
not resolve live drift or retroactively approve the direct push.
