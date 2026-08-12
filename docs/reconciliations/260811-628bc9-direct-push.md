# Reconciliation: direct-push merge `f5d42c24`

## Signal

- Task: `260811-628bc9`
- Observed commit: `f5d42c247e432657ec54341a792037247b602866`
- Source: <https://github.com/kidus-tiliksew/conveyor/commit/f5d42c247e432657ec54341a792037247b602866>
- Commit subject: `Merge branch 'main' of https://github.com/kidus-tiliksew/conveyor`
- Resolution: awaiting an audited operator reconciliation decision

## Provenance correction

The observed commit is not parentless and is not a root snapshot. Git records it
as a two-parent merge:

- first parent: `6e4021c352d58b859b6fa43e1a89297166aa9005`
- second parent: `7d2aaf1368c90ec37ae0cac8bf023079ccffd5a4`

Comparing the merge to each parent separates the out-of-pipeline contribution
from the already lineaged work. Relative to the second parent, the only change
is `CLAUDE.md`. Relative to the first parent, the merge imports changes already
delivered by PRs #459, #460, #461, and #463 plus regenerated dashboard assets.
The monitor signal therefore must not be reconciled as a full-tree addition.

## Confirmed authority checked

No served Requirement document is attached to this task. The implementation
context supplied these current confirmed authorities: `design-260805-973cd4`
v29, `design-database` v6, `design-document-corpus` v2,
`design-git-delivery` v2, `design-harness-execution` v2, `design-http-api` v8,
`design-lineage-graph` v2, `design-monitor-drift` v3,
`design-system-architecture` v4, `design-task-lifecycle` v7, and
`design-web-dashboard` v5, together with confirmed DEC-1 through DEC-4 and
DEC-6 through DEC-19. DEC-5 is superseded by DEC-18.

## Area classification

| Observed area | Provenance | Authority comparison | Classification |
| --- | --- | --- | --- |
| `CLAUDE.md` assignee and queue-order guidance | Unique delta from second parent; direct commit `6e4021c3` | It states that assignment constrains claim eligibility without affecting queue order and that dependencies plus oldest-first determine ordering. This matches confirmed DEC-18 and `design-task-lifecycle` v7. It also correctly identifies DEC-18 as superseding DEC-5. | Supported by current authority, but its direct-push provenance still needs an audited operator reconciliation outcome. No requirement amendment is indicated. |
| Pending-authority activity projection in `internal/httpapi/server.go`, `web/src/lib/activity.ts`, `web/src/lib/types.ts`, and `web/tests/task-full.spec.ts` | PR #463, task branch `conveyor/task-260811-acfb6f` | `design-http-api` v8 defines the derived `pending_authority` activity signal and `design-web-dashboard` v5 requires proposal-gated tasks in the Needs operator grouping without changing lifecycle. | Independently supported and lineaged; not a unique direct-push change. |
| Review context includes the open PR description in `internal/workorder/service.go` and tests | PR #459, task branch `conveyor/task-260811-4e0237` | `design-260805-973cd4` v29 explicitly defines best-effort PR-description assembly as a distinct review-context field. | Independently supported and lineaged; not a unique direct-push change. |
| Generated PR lifecycle-region reconciliation in `internal/trigger/github/github.go` and tests | PR #461, task branch `conveyor/task-260811-0afb67` | `design-git-delivery` v2 governs the GitHub boundary and preserving agent-authored PR content; `design-260805-973cd4` v29 makes the PR description review evidence context. | Independently supported and lineaged; not a unique direct-push change. |
| PostgreSQL heartbeat lease precision assertion | PR #460, task branch `conveyor/task-260811-399aa4` | The test normalizes the expected timestamp to PostgreSQL microsecond precision. It changes no lifecycle or persistence behavior and is consistent with `design-database` v6. | Independently supported and lineaged; not a unique direct-push change. |
| Embedded dashboard bundle | Generated output accompanying PR #463 | `design-web-dashboard` v5 embeds the SPA in `conveyord`; DEC-16 requires the generated bundle to accompany its web source change. | Independently supported generated output; not a separate governance change. |

No monitor implementation, planning implementation, queue implementation, or
task-lifecycle transition was uniquely introduced by the direct-push side of
the merge. The governance-sensitive code visible in a first-parent diff came
from already merged task branches on the second-parent history.

## Operator decision required

The evidence supports retaining the unique `CLAUDE.md` change without a
Requirement or System Design revision: its content restates confirmed DEC-18
and `design-task-lifecycle` v7 rather than introducing new behavior. The
remaining act is an operator-owned, audited monitor reconciliation that records
the occurrence as an intentional, authority-consistent change (or the
equivalent accepted resolution supported by the monitor surface).

If the operator instead considers direct modification of repository agent
guidance unacceptable despite its substantive consistency, the concrete
alternative is to select rejection/remediation and authorize a follow-up that
reverts only commit `6e4021c3`'s `CLAUDE.md` delta. The PR-derived code and
generated bundle must not be reverted as part of that decision.

This record does not itself resolve monitor drift, confirm or revise authority,
or select either operator outcome. No approved Requirement, System Design
document, decision, or repository behavior has been rewritten.
