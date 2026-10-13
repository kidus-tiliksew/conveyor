import { useQuery } from '@tanstack/react-query'
import { ExternalLink, GitMerge, TriangleAlert, UserRound } from 'lucide-react'
import { failedTriage, latestPlanRevisionRequest, mergeGateReview } from '../../lib/activity'
import { fetchCallerIdentity } from '../../lib/api'
import type { ActivityItem, Task } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { useNextAction } from '../inbox/use-next-actions'
import { Disclosure } from '../ui/disclosure'
import { AttachmentsCard } from './attachments-card'
import { GateChoices } from './merge-at-gate'
import { PlanRevisionDecisionCard } from './plan-revision-decision-card'
import { VerificationVerdictLine } from './verification-entry'
import { VerificationReviewResult } from './verification-stage'

// The human gate rendered as a verdict, not an alarm: the card leads with the
// server's statement of what the pipeline is waiting for and offers exactly the
// server's choices (feature-operator-next-action NA-4, NA-6). Amber stays
// reserved for states that are genuinely stuck; a clean approval reads as good
// news. The card renders as the event timeline's live tail
// (component-web-task-surfaces): the decision point is where the story
// currently ends, and acting on it resolves in place into the recorded
// intervention entry.

export type GateTone = 'positive' | 'neutral' | 'alarm'

// The next-action kinds that hold a task at an operator decision on its work.
const gateKinds: Record<string, true> = {
  plan_approval: true,
  plan_revision: true,
  merge_approval: true,
  review_bounce_limit: true,
  job_failure: true,
  merge_conflict: true,
  merge_execution: true,
}

// Whether the task is holding at a human gate — the gate card renders (and
// the timeline opens scrolled to it) only when the server says the operator
// owns a gate decision. A failed triage is the same job_failure gate, but its
// recovery card owns the decision, so the gate card stays out of its way.
export function isReviewable(item: ActivityItem): boolean {
  const action = item.next_action
  return action?.actor === 'operator' && gateKinds[action.kind] === true && !failedTriage(item)
}

// Request changes follows the server's assignee-aware rule. Identity loading
// and failures are deliberately fail-closed so capability alone never exposes
// an action that an assigned task would refuse.
export function useCanRequestTaskChanges(task: Task): boolean {
  const canRequestChanges = useWorkspaceCapability('request_changes')
  const canSetAssignee = useWorkspaceCapability('set_assignee')
  const { workspace } = useWorkspaceSelection()
  const identity = useQuery({
    queryKey: ['caller-identity', workspace],
    queryFn: () => fetchCallerIdentity(workspace),
    enabled: Boolean(workspace),
    retry: false,
  })
  return Boolean(
    identity.data &&
      canRequestChanges &&
      (!task.assignee || task.assignee.user_id === identity.data.id || canSetAssignee),
  )
}

// The gate's tone, exposed so the timeline can tint the rail dot to match.
export function gateTone(kind: string | undefined): GateTone {
  if (kind === 'merge_conflict' || kind === 'review_bounce_limit' || kind === 'job_failure') return 'alarm'
  if (kind === 'merge_execution' || kind === 'merging') return 'positive'
  return 'neutral'
}

// One sentence, written once: the composer states it before the feedback is
// typed and the recorded timeline entry states it after, so the promise a
// person acted on is the promise the record keeps (REQ-6).
export const changesComposerHint = 'This feedback goes to the next implementation run word for word.'

// What the gate is approving, laid out beside the decision rather than left to
// be reconstructed from the timeline. Every line is evidence the pipeline
// already recorded, so a line with nothing behind it is absent instead of
// filled in: a repository delivered without GitHub shows no pull request.
// Secondary identity — commit SHAs — is muted and mono, with the full value in
// the tooltip, per the surface's visual economy.
//
// The base and head the pipeline recorded bound a commit range and nothing
// more: the browser contract carries no diff stat or changed-file list, so the
// range is labelled as the range it is. Naming it anything that implies a
// summary of the change would tell a person the card weighed something it never
// saw, and the pull request beside it is where the diff actually lives (REQ-6).
function MergeGateReviewCard({ item }: { item: ActivityItem }) {
  const review = mergeGateReview(item)
  const { branch, base_branch: baseBranch } = item.task
  if (!review.headSHA && !review.pullRequest && !review.verdict && !branch) return null
  const verdictLabel =
    review.verdict &&
    (review.verdict.verdict === 'approve'
      ? review.verdict.seats > 1
        ? `${review.verdict.seats} factory reviewers approved this`
        : 'Factory review approved this'
      : 'Factory review asked for changes')
  return (
    <section
      aria-label="What you are approving"
      className="mb-3 rounded-md border border-border bg-surface/40 px-3 py-2.5"
    >
      <h4 className="text-[11px] font-semibold uppercase tracking-[0.12em] text-faint">What you are approving</h4>
      <dl className="mt-2 grid gap-x-6 gap-y-1.5 text-xs sm:grid-cols-[auto_1fr]">
        {branch && (
          <>
            <dt className="text-muted">Change</dt>
            <dd className="min-w-0 truncate">
              <span className="font-mono text-foreground/90">{branch}</span>
              {baseBranch && <span className="text-muted"> into {baseBranch}</span>}
            </dd>
          </>
        )}
        {review.headSHA && (
          <>
            <dt className="text-muted">Reviewed at</dt>
            <dd className="min-w-0">
              <Disclosure
                triggerClassName="font-mono text-foreground/90"
                content={review.headSHA}
                title={review.headSHA}
              >
                {review.headSHA.slice(0, 8)}
              </Disclosure>
            </dd>
          </>
        )}
        {review.headSHA && review.baseSHA && (
          <>
            <dt className="text-muted">Commit range</dt>
            <dd className="min-w-0">
              <Disclosure
                triggerClassName="font-mono text-foreground/90"
                content={`${review.baseSHA} … ${review.headSHA}`}
                title={`${review.baseSHA} … ${review.headSHA}`}
              >
                {review.baseSHA.slice(0, 8)} … {review.headSHA.slice(0, 8)}
              </Disclosure>
            </dd>
          </>
        )}
        {review.pullRequest && (
          <>
            <dt className="text-muted">Pull request</dt>
            <dd className="min-w-0 truncate">
              <a
                href={review.pullRequest.url}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 text-primary hover:underline"
              >
                {review.pullRequest.repository ?? 'Pull request'}
                {review.pullRequest.number ? `#${review.pullRequest.number}` : ''}
                <ExternalLink className="size-3" aria-hidden="true" />
              </a>
            </dd>
          </>
        )}
        {review.verdict && (
          <>
            <dt className="text-muted">Review</dt>
            <dd className="min-w-0">
              <span className={review.verdict.verdict === 'approve' ? 'text-positive' : 'text-attention'}>
                {verdictLabel}
              </span>
              {review.verdict.summary && <span className="block text-muted leading-5">{review.verdict.summary}</span>}
            </dd>
          </>
        )}
      </dl>
    </section>
  )
}

const toneStyles: Record<GateTone, { card: string; header: string; title: string; icon: string }> = {
  positive: { card: 'border-positive/30', header: 'bg-positive-soft', title: 'text-positive', icon: 'text-positive' },
  neutral: { card: 'border-primary/25', header: 'bg-primary-soft', title: 'text-primary', icon: 'text-primary' },
  alarm: {
    card: 'border-attention-dot/40',
    header: 'bg-attention-soft',
    title: 'text-attention',
    icon: 'text-attention-dot',
  },
}

// A contested execution plan is a different question from "is this work good",
// so it gets its own card laying out the agent's rationale (REQ-2 AC-2.1).
// Whether the plan revision is the open decision is the server's next action.
export function ReviewPanel({ item, onDecisionRecorded }: { item: ActivityItem; onDecisionRecorded?: () => void }) {
  const canOperate = useWorkspaceCapability('operate_gates')
  const revisionRequest = item.next_action?.kind === 'plan_revision' ? latestPlanRevisionRequest(item.events) : null
  if (revisionRequest)
    return canOperate ? (
      <PlanRevisionDecisionCard item={item} request={revisionRequest} onDecisionRecorded={onDecisionRecorded} />
    ) : null

  return <GenericReviewPanel item={item} onDecisionRecorded={onDecisionRecorded} />
}

function GenericReviewPanel({ item, onDecisionRecorded }: { item: ActivityItem; onDecisionRecorded?: () => void }) {
  const { data: action } = useNextAction(item.task.id)
  if (!action || action.kind === 'terminal') return null
  const tone = gateTone(action.kind)
  const style = toneStyles[tone]
  const Icon = tone === 'alarm' ? TriangleAlert : tone === 'positive' ? GitMerge : UserRound
  const mergeGate = item.at_merge_gate
  return (
    <section className={cn('rounded-lg border bg-background', style.card)} aria-label="Human gate">
      <div className={cn('flex flex-wrap items-center gap-3 rounded-t-lg px-4 py-3', style.header)}>
        <Icon className={cn('size-5 shrink-0', style.icon)} />
        <div className="min-w-48 flex-1">
          {/* The label, not the summary: NextActionCard above already states the
              summary once per surface (DEC-61; feature-operator-next-action NA-6). */}
          <h3 className={cn('text-sm font-semibold', style.title)}>{action.label}</h3>
          {action.question && <p className="mt-0.5 text-xs leading-5 text-muted">{action.question}</p>}
        </div>
      </div>
      <div className="space-y-3 px-4 py-2.5">
        {mergeGate && <MergeGateReviewCard item={item} />}
        {mergeGate && <VerificationVerdictLine item={item} />}
        {mergeGate && <VerificationReviewResult item={item} />}
        <AttachmentsCard attachments={item.verification_evidence ?? []} title="Verification evidence" />
        <GateChoices taskId={item.task.id} action={action} onActed={() => onDecisionRecorded?.()} />
      </div>
    </section>
  )
}
