import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ExternalLink, GitMerge, ThumbsUp, TriangleAlert, Undo2, UserRound, type LucideIcon } from 'lucide-react'
import { mergeGateReview, pendingPlanRevisionRequest } from '../../lib/activity'
import { fetchCallerIdentity, fixMergeConflict, mergeTask, requestTaskChanges, reviewTask } from '../../lib/api'
import { defaultReasonCode, interventionActions } from '../../lib/contracts'
import type { ActivityItem, InterventionAction, Task, TaskEvent } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Textarea } from '../ui/input'
import { AttachmentsCard } from './attachments-card'
import { PlanRevisionDecisionCard } from './plan-revision-decision-card'

// The human gate rendered as a verdict, not an alarm: the card
// leads with what the pipeline is waiting for and one context-matched primary
// action. Amber stays reserved for states that are genuinely stuck; a clean
// approval reads as good news. Reason codes are auto-derived per action
// (see contracts.ts) — the comment is the operator's signal. The card renders
// as the event timeline's live tail (§13.3 element 3): the decision point is
// where the story currently ends, and acting on it resolves in place into
// the recorded intervention entry.

export type GateTone = 'positive' | 'neutral' | 'alarm'

// Whether the task is holding at a human gate — the gate card renders (and
// the timeline opens scrolled to it) only in these states.
export function isReviewable(task: Task): boolean {
  return task.state === 'awaiting_human' || task.state === 'approved'
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
    queryFn: () => fetchCallerIdentity(),
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
export function gateTone(task: Task, events: TaskEvent[], readiness?: ActivityItem['merge_readiness']): GateTone {
  return gateFor(task, events, readiness).tone
}

export function gateNeedsRecoveryCapability(
  task: Task,
  events: TaskEvent[],
  readiness?: ActivityItem['merge_readiness'],
): boolean {
  return gateFor(task, events, readiness).primaryAction === 'fix'
}
type GatePrimary = 'merge' | 'approve' | 'redirect' | 'fix' | 'pending'

interface Gate {
  tone: GateTone
  icon: LucideIcon
  headline: string
  detail: string
  primaryLabel: string
  primaryAction: GatePrimary
}

function gateFor(task: Task, events: TaskEvent[], readiness?: ActivityItem['merge_readiness']): Gate {
  if (task.state === 'approved') {
    if (readiness?.state === 'CONFLICTING')
      return {
        tone: 'alarm',
        icon: TriangleAlert,
        headline: 'Merge blocked by conflicts',
        detail:
          'Conveyor will dispatch an implementation order to merge the base branch, resolve conflicts, validate, and refresh review.',
        primaryLabel: 'Fix merge conflict',
        primaryAction: 'fix',
      }
    if (readiness?.state !== 'MERGEABLE')
      return {
        tone: 'neutral',
        icon: GitMerge,
        headline: readiness?.state === 'STALE' ? 'Approval changed — refreshing review' : 'Checking merge readiness',
        detail:
          readiness?.state === 'STALE'
            ? 'The pull-request head changed after approval. Conveyor has started the configured refresh flow.'
            : 'Merge readiness is not available yet. Conveyor will re-read GitHub with bounded backoff.',
        primaryLabel: 'Readiness pending',
        primaryAction: 'pending',
      }
    return {
      tone: 'positive',
      icon: GitMerge,
      headline: 'Ready to merge',
      detail: 'Approved at the gate — Conveyor will merge the pull request and confirm it with GitHub.',
      primaryLabel: 'Merge pull request',
      primaryAction: 'merge',
    }
  }
  // Walk back to the most recent incident, but stop at any human decision or
  // fresh dispatch — those supersede older incidents.
  for (let i = events.length - 1; i >= 0; i--) {
    const kind = events[i].kind
    if (kind === 'pipeline.bounce_limit') {
      // Check-in, not failure: the loop is alive and feedback
      // resumes it with a fresh unsupervised round window.
      return {
        tone: 'alarm',
        icon: TriangleAlert,
        headline: 'Review loop checked in',
        detail:
          'Implement and review used their unsupervised rounds without converging. Send feedback to resume with a fresh window, or decide the task here.',
        primaryLabel: 'Resume with feedback',
        primaryAction: 'redirect',
      }
    }
    if (kind === 'job.timeout') {
      return {
        tone: 'alarm',
        icon: TriangleAlert,
        headline: 'The last stage timed out',
        detail: 'It ran past its time limit and was stopped before finishing.',
        primaryLabel: 'Approve',
        primaryAction: 'approve',
      }
    }
    if (kind === 'job.created' || kind.startsWith('intervention.')) break
  }
  return {
    tone: 'neutral',
    icon: UserRound,
    headline: 'Your review, please',
    detail: 'The line is holding at the human gate. Review the work below, then record a decision.',
    primaryLabel: 'Approve',
    primaryAction: 'approve',
  }
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
              <span className="font-mono text-foreground/90" title={review.headSHA}>
                {review.headSHA.slice(0, 8)}
              </span>
            </dd>
          </>
        )}
        {review.headSHA && review.baseSHA && (
          <>
            <dt className="text-muted">Commit range</dt>
            <dd className="min-w-0">
              <span className="font-mono text-foreground/90" title={`${review.baseSHA} … ${review.headSHA}`}>
                {review.baseSHA.slice(0, 8)} … {review.headSHA.slice(0, 8)}
              </span>
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

// The gate's context-matched primary is excluded from the secondary row;
// approve taken from the row records immediately (it needs no comment).
const secondaryActionsFor = (primary: GatePrimary) =>
  interventionActions.filter((entry) => entry.action !== (primary === 'redirect' ? 'redirect' : 'approve'))

type GateMutation =
  | { kind: 'merge' }
  | { kind: 'fix' }
  | { kind: 'review'; action: InterventionAction; comment: string }

// A contested execution plan is a different question from "is this work good",
// so it gets its own card rather than another branch of the gate's derived
// actions (REQ-2 AC-2.1).
export function ReviewPanel({ item, onDecisionRecorded }: { item: ActivityItem; onDecisionRecorded?: () => void }) {
  const canOperate = useWorkspaceCapability('operate_gates')
  const revisionRequest = pendingPlanRevisionRequest(item.events)
  if (revisionRequest)
    return canOperate ? (
      <PlanRevisionDecisionCard item={item} request={revisionRequest} onDecisionRecorded={onDecisionRecorded} />
    ) : null

  return <GenericReviewPanel item={item} onDecisionRecorded={onDecisionRecorded} />
}

function GenericReviewPanel({ item, onDecisionRecorded }: { item: ActivityItem; onDecisionRecorded?: () => void }) {
  const canRecover = useWorkspaceCapability('recover_work')
  const canOperate = useWorkspaceCapability('operate_gates')
  const canRequestChanges = useCanRequestTaskChanges(item.task)
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const [expanded, setExpanded] = useState<InterventionAction | null>(null)
  const [comment, setComment] = useState('')

  const gate = gateFor(item.task, item.events, item.merge_readiness)
  const mergeGate = item.at_merge_gate
  const style = toneStyles[gate.tone]
  const Icon = gate.icon

  const mutation = useMutation({
    mutationFn: async (input: GateMutation) => {
      if (input.kind === 'merge') {
        await mergeTask(item.task.id)
        return
      }
      if (input.kind === 'fix') {
        await fixMergeConflict(item.task.id)
        return
      }
      if (input.kind === 'review' && input.action === 'redirect' && mergeGate) {
        await requestTaskChanges(item.task.id, input.comment)
        return
      }
      await reviewTask(item.task.id, {
        action: input.action,
        comment: input.comment,
        reasonCode: defaultReasonCode[input.action],
      })
    },
    // Keep the mutation pending until the refetched task/activity data lands.
    // Returning a promise from onSuccess holds mutation.isPending true across
    // the invalidation (TanStack Query v5); otherwise isPending flips false the
    // instant onSuccess returns, and gateFor re-renders the idle label from the
    // still-stale task.state for a frame before the control settles — the
    // reported merge/approve flash.
    onSuccess: async (_result, input) => {
      setExpanded(null)
      setComment('')
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['task', workspace, item.task.id] }),
        queryClient.invalidateQueries({ queryKey: ['activity'] }),
      ])
      if (input.kind === 'review' && (input.action === 'approve' || input.action === 'redirect')) {
        onDecisionRecorded?.()
      }
    },
  })

  const toggle = (action: InterventionAction) => {
    setExpanded((current) => (current === action ? null : action))
    setComment('')
    mutation.reset()
  }

  const canUsePrimary =
    (gate.primaryAction === 'fix' ? canRecover : canOperate) ||
    (mergeGate && canRequestChanges && gate.primaryAction === 'redirect')
  const secondaryActions = secondaryActionsFor(gate.primaryAction).filter(
    (entry) => canOperate || (mergeGate && canRequestChanges && entry.action === 'redirect'),
  )
  const expandedEntry = interventionActions.find((entry) => entry.action === expanded && entry.action !== 'approve')
  const gateComposer = mergeGate && expanded === 'redirect'

  return (
    <section className={cn('rounded-lg border bg-background', style.card)} aria-label="Human gate">
      <div className={cn('flex flex-wrap items-center gap-3 rounded-t-lg px-4 py-3', style.header)}>
        <Icon className={cn('size-5 shrink-0', style.icon)} />
        <div className="min-w-48 flex-1">
          <h3 className={cn('text-sm font-semibold', style.title)}>{gate.headline}</h3>
          <p className="mt-0.5 text-xs leading-5 text-muted">{gate.detail}</p>
        </div>
        {canUsePrimary && (
          <Button
            disabled={mutation.isPending || gate.primaryAction === 'pending'}
            onClick={() => {
              if (gate.primaryAction === 'merge') return mutation.mutate({ kind: 'merge' })
              if (gate.primaryAction === 'fix') return mutation.mutate({ kind: 'fix' })
              if (gate.primaryAction === 'pending') return
              if (gate.primaryAction === 'redirect') return toggle('redirect')
              mutation.mutate({ kind: 'review', action: 'approve', comment: '' })
            }}
          >
            {gate.primaryAction === 'merge' ? (
              <GitMerge />
            ) : gate.primaryAction === 'fix' ? (
              <TriangleAlert />
            ) : gate.primaryAction === 'redirect' ? (
              <Undo2 />
            ) : (
              <ThumbsUp />
            )}
            {mutation.isPending && !expanded
              ? gate.primaryAction === 'merge'
                ? 'Merging…'
                : 'Recording…'
              : gate.primaryLabel}
          </Button>
        )}
      </div>
      <div className="px-4 py-2.5">
        {mergeGate && <MergeGateReviewCard item={item} />}
        <AttachmentsCard attachments={item.verification_evidence ?? []} title="Verification evidence" />
        <fieldset
          className={cn('flex flex-wrap items-center gap-1', (item.verification_evidence?.length ?? 0) > 0 && 'mt-3')}
        >
          <legend className="float-left mr-1 text-xs text-faint">Instead:</legend>
          {secondaryActions.map((entry) => (
            <button
              key={entry.action}
              type="button"
              aria-expanded={expanded === entry.action}
              title={entry.hint}
              onClick={() =>
                entry.action === 'approve'
                  ? mutation.mutate({ kind: 'review', action: 'approve', comment: '' })
                  : toggle(entry.action)
              }
              className={cn(
                'rounded-md px-2 py-1 text-xs font-medium transition-colors',
                expanded === entry.action
                  ? 'bg-raised text-foreground'
                  : 'text-muted hover:bg-surface hover:text-foreground',
              )}
            >
              {entry.label}
            </button>
          ))}
        </fieldset>
        {expandedEntry && (
          <div className="mt-2">
            {/* At the merge gate the feedback is not a note filed against a
                decision — it is the whole brief the next run works from, and it
                travels word for word. Saying so before the box is typed into is
                what makes the required field read as necessary rather than as
                one more form control (REQ-6). */}
            {gateComposer && <p className="mb-1.5 text-xs leading-5 text-muted">{changesComposerHint}</p>}
            <Textarea
              autoFocus
              className="min-h-16 text-sm"
              aria-label={
                expanded !== 'redirect' ? 'Rejection note' : gateComposer ? 'Changes you want' : 'Redirect feedback'
              }
              value={comment}
              onChange={(event) => setComment(event.target.value)}
              placeholder={
                expanded === 'redirect'
                  ? 'What should the agent change? Feedback, not code.'
                  : 'Why is this rejected? Optional.'
              }
            />
            <div className="mt-1.5 flex flex-wrap items-center justify-end gap-2">
              {gateComposer && !comment.trim() && (
                <p className="mr-auto text-xs text-muted">Feedback is required to send this back.</p>
              )}
              <Button variant="ghost" size="sm" onClick={() => toggle(expanded!)}>
                Cancel
              </Button>
              <Button
                variant={expanded === 'reject' ? 'destructive' : 'secondary'}
                size="sm"
                disabled={mutation.isPending || (expanded === 'redirect' && !comment.trim())}
                onClick={() => mutation.mutate({ kind: 'review', action: expanded!, comment })}
              >
                {mutation.isPending ? 'Recording…' : expandedEntry.confirmLabel}
              </Button>
            </div>
          </div>
        )}
        {mutation.error != null && <p className="mt-2 text-sm text-failure">{String(mutation.error)}</p>}
      </div>
    </section>
  )
}
