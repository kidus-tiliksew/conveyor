import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { GitMerge, ThumbsUp, TriangleAlert, UserRound, type LucideIcon } from 'lucide-react'
import { mergeTask, reviewTask } from '../../lib/api'
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
// (see contracts.ts) — the comment is the operator's signal.

type GateTone = 'positive' | 'neutral' | 'alarm'

interface Gate {
  tone: GateTone
  icon: LucideIcon
  headline: string
  detail: string
  primaryLabel: string
}

function gateFor(task: Task, events: TaskEvent[]): Gate {
  if (task.state === 'approved') {
    return {
      tone: 'positive',
      icon: GitMerge,
      headline: 'Ready to merge',
      detail: 'Approved at the gate — Conveyor will merge the pull request and confirm it with GitHub.',
      primaryLabel: 'Merge pull request',
    }
  }
  if (task.state === 'parked') {
    return {
      tone: 'alarm',
      icon: TriangleAlert,
      headline: 'Parked — needs a human route',
      detail: 'Triage could not route this task on its own. Decide where it goes.',
      primaryLabel: 'Approve',
    }
  }
  // Walk back to the most recent incident, but stop at any human decision or
  // fresh dispatch — those supersede older incidents.
  for (let i = events.length - 1; i >= 0; i--) {
    const kind = events[i].kind
    if (kind === 'pipeline.bounce_limit') {
      return {
        tone: 'alarm',
        icon: TriangleAlert,
        headline: 'Bounce limit reached',
        detail: 'Implement and review went back and forth to the limit. A human closes the loop.',
        primaryLabel: 'Approve',
      }
    }
    if (kind === 'job.timeout') {
      return {
        tone: 'alarm',
        icon: TriangleAlert,
        headline: 'Job timed out',
        detail: 'The last job hit its wall-clock timeout before finishing.',
        primaryLabel: 'Approve',
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
  }
}

const toneStyles: Record<GateTone, { card: string; header: string; title: string; icon: string }> = {
  positive: { card: 'border-positive/30', header: 'bg-positive-soft', title: 'text-positive', icon: 'text-positive' },
  neutral: { card: 'border-primary/25', header: 'bg-primary-soft', title: 'text-primary', icon: 'text-primary' },
  alarm: { card: 'border-attention-dot/40', header: 'bg-attention-soft', title: 'text-attention', icon: 'text-attention-dot' },
}

const secondaryActions = interventionActions.filter((entry) => entry.action !== 'approve')

type GateMutation =
  | { kind: 'merge' }
  | { kind: 'review'; action: InterventionAction; comment: string }

export function ReviewPanel({ item }: { item: ActivityItem }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const [expanded, setExpanded] = useState<InterventionAction | null>(null)
  const [comment, setComment] = useState('')

  const gate = gateFor(item.task, item.events)
  const style = toneStyles[gate.tone]
  const Icon = gate.icon

  const mutation = useMutation({
    mutationFn: async (input: GateMutation) => {
      if (input.kind === 'merge') {
        await mergeTask(item.task.id, token)
        return
      }
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

  const expandedEntry = secondaryActions.find((entry) => entry.action === expanded)

  return (
    <section className={cn('rounded-lg border bg-background', style.card)} aria-label="Human gate">
      <div className={cn('flex flex-wrap items-center gap-3 rounded-t-lg px-4 py-3', style.header)}>
        <Icon className={cn('size-5 shrink-0', style.icon)} />
        <div className="min-w-48 flex-1">
          <h3 className={cn('text-sm font-semibold', style.title)}>{gate.headline}</h3>
          <p className="mt-0.5 text-xs leading-5 text-muted">{gate.detail}</p>
        </div>
        <Button
          disabled={!token || mutation.isPending}
          onClick={() => item.task.state === 'approved'
            ? mutation.mutate({ kind: 'merge' })
            : mutation.mutate({ kind: 'review', action: 'approve', comment: '' })}
        >
          {item.task.state === 'approved' ? <GitMerge /> : <ThumbsUp />}
          {mutation.isPending && !expanded ? (item.task.state === 'approved' ? 'Merging…' : 'Recording…') : gate.primaryLabel}
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
              onClick={() => toggle(entry.action)}
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
