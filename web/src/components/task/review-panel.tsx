import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { GitMerge, ThumbsUp, TriangleAlert, Undo2, UserRound, type LucideIcon } from 'lucide-react'
import { fixMergeConflict, mergeTask, reviewTask } from '../../lib/api'
import { defaultReasonCode, interventionActions } from '../../lib/contracts'
import type { ActivityItem, InterventionAction, Task, TaskEvent } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useOperatorToken } from '../app-shell'
import { Button } from '../ui/button'
import { Textarea } from '../ui/input'

// The human gate rendered as a verdict, not an alarm (spec §13.3): the card
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
  return task.state === 'awaiting_human' || task.state === 'parked' || task.state === 'approved'
}

// The gate's tone, exposed so the timeline can tint the rail dot to match.
export function gateTone(task: Task, events: TaskEvent[], readiness?: ActivityItem['merge_readiness']): GateTone {
	return gateFor(task, events, readiness).tone
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
		if (readiness?.state === 'UNKNOWN' || readiness?.state === 'STALE') return {
			tone: 'neutral', icon: GitMerge, headline: readiness.state === 'STALE' ? 'Approval changed — refreshing review' : 'Checking merge readiness',
			detail: readiness.state === 'STALE' ? 'The pull-request head changed after approval. Conveyor has started the configured refresh flow.' : 'GitHub is still calculating mergeability. Conveyor will re-read it with bounded backoff.',
			primaryLabel: 'Readiness pending', primaryAction: 'pending',
		}
		if (readiness?.state === 'CONFLICTING') return {
			tone: 'alarm', icon: TriangleAlert, headline: 'Merge blocked by conflicts',
			detail: 'Conveyor will dispatch an implementation order to merge the base branch, resolve conflicts, validate, and refresh review.',
			primaryLabel: 'Fix merge conflict', primaryAction: 'fix',
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
  if (task.state === 'parked') {
    return {
      tone: 'alarm',
      icon: TriangleAlert,
      headline: 'Parked — needs a human route',
      detail: 'Triage could not route this task on its own. Decide where it goes.',
      primaryLabel: 'Approve',
      primaryAction: 'approve',
    }
  }
  // Walk back to the most recent incident, but stop at any human decision or
  // fresh dispatch — those supersede older incidents.
  for (let i = events.length - 1; i >= 0; i--) {
    const kind = events[i].kind
    if (kind === 'pipeline.bounce_limit') {
      // Check-in, not failure (spec §21.17): the loop is alive and feedback
      // resumes it with a fresh unsupervised round window.
      return {
        tone: 'alarm',
        icon: TriangleAlert,
        headline: 'Review loop checked in',
        detail: 'Implement and review used their unsupervised rounds without converging. Send feedback to resume with a fresh window, or decide the task here.',
        primaryLabel: 'Resume with feedback',
        primaryAction: 'redirect',
      }
    }
    if (kind === 'job.timeout') {
      return {
        tone: 'alarm',
        icon: TriangleAlert,
        headline: 'Job timed out',
        detail: 'The last job hit its wall-clock timeout before finishing.',
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

const toneStyles: Record<GateTone, { card: string; header: string; title: string; icon: string }> = {
  positive: { card: 'border-positive/30', header: 'bg-positive-soft', title: 'text-positive', icon: 'text-positive' },
  neutral: { card: 'border-primary/25', header: 'bg-primary-soft', title: 'text-primary', icon: 'text-primary' },
  alarm: { card: 'border-attention-dot/40', header: 'bg-attention-soft', title: 'text-attention', icon: 'text-attention-dot' },
}

// The gate's context-matched primary is excluded from the secondary row;
// approve taken from the row records immediately (it needs no comment).
const secondaryActionsFor = (primary: GatePrimary) =>
  interventionActions.filter((entry) => entry.action !== (primary === 'redirect' ? 'redirect' : 'approve'))

type GateMutation =
  | { kind: 'merge' }
	| { kind: 'fix' }
  | { kind: 'review'; action: InterventionAction; comment: string }

export function ReviewPanel({ item }: { item: ActivityItem }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const [expanded, setExpanded] = useState<InterventionAction | null>(null)
  const [comment, setComment] = useState('')

	const gate = gateFor(item.task, item.events, item.merge_readiness)
  const style = toneStyles[gate.tone]
  const Icon = gate.icon

  const mutation = useMutation({
    mutationFn: async (input: GateMutation) => {
		if (input.kind === 'merge') {
        await mergeTask(item.task.id, token)
        return
		}
		if (input.kind === 'fix') { await fixMergeConflict(item.task.id, token); return }
      await reviewTask(item.task.id, token, { action: input.action, comment: input.comment, reasonCode: defaultReasonCode[input.action] })
    },
    onSuccess: () => {
      setExpanded(null)
      setComment('')
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.id] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })

  const toggle = (action: InterventionAction) => {
    setExpanded((current) => (current === action ? null : action))
    setComment('')
    mutation.reset()
  }

  const secondaryActions = secondaryActionsFor(gate.primaryAction)
  const expandedEntry = interventionActions.find((entry) => entry.action === expanded && entry.action !== 'approve')

  return (
    <section className={cn('rounded-lg border bg-background', style.card)} aria-label="Human gate">
      <div className={cn('flex flex-wrap items-center gap-3 rounded-t-lg px-4 py-3', style.header)}>
        <Icon className={cn('size-5 shrink-0', style.icon)} />
        <div className="min-w-48 flex-1">
          <h3 className={cn('text-sm font-semibold', style.title)}>{gate.headline}</h3>
          <p className="mt-0.5 text-xs leading-5 text-muted">{gate.detail}</p>
        </div>
        <Button
			disabled={!token || mutation.isPending || gate.primaryAction === 'pending'}
          onClick={() => {
				if (gate.primaryAction === 'merge') return mutation.mutate({ kind: 'merge' })
				if (gate.primaryAction === 'fix') return mutation.mutate({ kind: 'fix' })
				if (gate.primaryAction === 'pending') return
            if (gate.primaryAction === 'redirect') return toggle('redirect')
            mutation.mutate({ kind: 'review', action: 'approve', comment: '' })
          }}
        >
			{gate.primaryAction === 'merge' ? <GitMerge /> : gate.primaryAction === 'fix' ? <TriangleAlert /> : gate.primaryAction === 'redirect' ? <Undo2 /> : <ThumbsUp />}
          {mutation.isPending && !expanded ? (gate.primaryAction === 'merge' ? 'Merging…' : 'Recording…') : gate.primaryLabel}
        </Button>
      </div>
      <div className="px-4 py-2.5">
        <div className="flex flex-wrap items-center gap-1" role="group" aria-label="Other decisions">
          <span className="mr-1 text-xs text-faint">Instead:</span>
          {secondaryActions.map((entry) => (
            <button
              key={entry.action}
              type="button"
              aria-expanded={expanded === entry.action}
              title={entry.hint}
              onClick={() => entry.action === 'approve'
                ? mutation.mutate({ kind: 'review', action: 'approve', comment: '' })
                : toggle(entry.action)}
              className={cn(
                'rounded-md px-2 py-1 text-xs font-medium transition-colors',
                expanded === entry.action ? 'bg-raised text-foreground' : 'text-muted hover:bg-surface hover:text-foreground',
              )}
            >
              {entry.label}
            </button>
          ))}
          {!token && <span className="ml-auto text-xs text-attention">Set the operator token in Settings to act.</span>}
        </div>
        {expandedEntry && (
          <div className="mt-2">
            <Textarea
              autoFocus
              className="min-h-16 text-sm"
              aria-label={expanded === 'redirect' ? 'Redirect feedback' : 'Rejection note'}
              value={comment}
              onChange={(event) => setComment(event.target.value)}
              placeholder={
                expanded === 'redirect'
                  ? 'What should the agent change? Feedback, not code.'
                  : 'Why is this rejected? Optional.'
              }
            />
            <div className="mt-1.5 flex items-center justify-end gap-2">
              <Button variant="ghost" size="sm" onClick={() => toggle(expanded!)}>
                Cancel
              </Button>
              <Button
                variant={expanded === 'reject' ? 'destructive' : 'secondary'}
                size="sm"
                disabled={!token || mutation.isPending || (expanded === 'redirect' && !comment.trim())}
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
