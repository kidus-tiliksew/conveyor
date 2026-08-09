import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { ThumbsUp, Undo2 } from 'lucide-react'
import { type PlanRevisionRequest } from '../../lib/activity'
import { reviewTask } from '../../lib/api'
import { planRevisionReasonCodes } from '../../lib/contracts'
import type { ActivityItem, InterventionAction } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useOperatorToken, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Textarea } from '../ui/input'

// The plan-revision decision card: the operator surface for the gate an
// implementing agent opens when it contests the approved execution plan
// (REQ-2 AC-2.1). It is deliberately its own card rather than another branch
// of the generic gate — the question is not "is this work good", it is "should
// the plan change", and it carries three fixed outcomes instead of the gate's
// derived ones. It renders as the timeline's live tail like every other
// decision point, and coexists with the generic work-order recovery card,
// which stays owned by its own component.

type PlanRevisionDecision = 'approve' | 'decline' | 'reject'

const decisions: Record<
  PlanRevisionDecision,
  {
    action: InterventionAction
    reasonCode: string
    label: string
    confirmLabel: string
    directionLabel: string
    directionHint: string
  }
> = {
  approve: {
    action: 'redirect',
    reasonCode: planRevisionReasonCodes.approve,
    label: 'Approve revision',
    confirmLabel: 'Send to planning',
    directionLabel: 'Revision direction',
    directionHint: 'Optional direction for the new plan.',
  },
  decline: {
    action: 'redirect',
    reasonCode: planRevisionReasonCodes.decline,
    label: 'Decline with direction',
    confirmLabel: 'Retry implementation',
    directionLabel: 'Implementation direction',
    directionHint: 'Required direction for the implementation retry.',
  },
  reject: {
    action: 'reject',
    reasonCode: planRevisionReasonCodes.reject,
    label: 'Reject task',
    confirmLabel: 'Reject task',
    directionLabel: 'Rejection note',
    directionHint: 'Why is this task rejected? Optional.',
  },
}

export function PlanRevisionDecisionCard({
  item,
  request,
  onDecisionRecorded,
}: {
  item: ActivityItem
  request: PlanRevisionRequest
  onDecisionRecorded?: () => void
}) {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const [decision, setDecision] = useState<PlanRevisionDecision | null>(null)
  const [direction, setDirection] = useState('')

  const mutation = useMutation({
    mutationFn: async (selected: PlanRevisionDecision) => {
      const contract = decisions[selected]
      // Direction is operator instruction the backend normalizes and carries
      // into the next order, so it is sent trimmed — never with the incidental
      // whitespace a textarea collects.
      await reviewTask(item.task.id, token, {
        action: contract.action,
        reasonCode: contract.reasonCode,
        comment: direction.trim(),
      })
    },
    onSuccess: async () => {
      setDecision(null)
      setDirection('')
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['task', workspace, item.task.id] }),
        queryClient.invalidateQueries({ queryKey: ['activity'] }),
      ])
      onDecisionRecorded?.()
    },
  })

  const select = (selected: PlanRevisionDecision) => {
    setDecision((current) => (current === selected ? null : selected))
    setDirection('')
    mutation.reset()
  }
  const selected = decision ? decisions[decision] : null

  return (
    <section className="rounded-lg border border-primary/25 bg-background" aria-label="Human gate">
      <div className="flex flex-wrap items-center gap-3 rounded-t-lg bg-primary-soft px-4 py-3">
        <Undo2 className="size-5 shrink-0 text-primary" />
        <div className="min-w-48 flex-1">
          <h3 className="text-sm font-semibold text-primary">Plan revision requested</h3>
          <p className="mt-0.5 text-xs leading-5 text-muted">
            The implementing agent is contesting plan v{request.planVersion}. Choose whether to revise it, retry the
            implementation with direction, or reject the task.
          </p>
        </div>
        <Button disabled={!token || mutation.isPending} onClick={() => select('approve')}>
          <ThumbsUp />
          {decisions.approve.label}
        </Button>
      </div>
      <div className="px-4 py-3">
        <dl className="grid gap-2 rounded-md bg-surface px-3 py-2.5 text-sm">
          <div className="flex items-baseline gap-2">
            <dt className="text-xs font-medium text-faint">Contested plan</dt>
            <dd className="font-medium text-foreground">v{request.planVersion}</dd>
          </div>
          <div>
            <dt className="text-xs font-medium text-faint">Agent rationale</dt>
            <dd className="mt-1 whitespace-pre-wrap leading-6 text-foreground/85">{request.rationale}</dd>
          </div>
        </dl>
        <fieldset className="mt-3 flex flex-wrap items-center gap-1">
          <legend className="float-left mr-1 text-xs text-faint">Decision:</legend>
          {(['decline', 'reject'] as const).map((entry) => (
            <button
              key={entry}
              type="button"
              aria-expanded={decision === entry}
              onClick={() => select(entry)}
              className={cn(
                'rounded-md px-2 py-1 text-xs font-medium transition-colors',
                decision === entry ? 'bg-raised text-foreground' : 'text-muted hover:bg-surface hover:text-foreground',
              )}
            >
              {decisions[entry].label}
            </button>
          ))}
          {!token && <span className="ml-auto text-xs text-attention">Set the operator token in Settings to act.</span>}
        </fieldset>
        {selected && decision && (
          <div className="mt-2">
            <Textarea
              autoFocus
              className="min-h-16 text-sm"
              aria-label={selected.directionLabel}
              value={direction}
              onChange={(event) => setDirection(event.target.value)}
              placeholder={selected.directionHint}
            />
            <div className="mt-1.5 flex items-center justify-end gap-2">
              <Button variant="ghost" size="sm" onClick={() => select(decision)}>
                Cancel
              </Button>
              <Button
                variant={decision === 'reject' ? 'destructive' : 'secondary'}
                size="sm"
                // Declining is how operator direction reaches the retried
                // implementation, so an empty one is not a decision the API
                // accepts (REQ-2 AC-2.3).
                disabled={!token || mutation.isPending || (decision === 'decline' && !direction.trim())}
                onClick={() => mutation.mutate(decision)}
              >
                {mutation.isPending ? 'Recording…' : selected.confirmLabel}
              </Button>
            </div>
          </div>
        )}
        {mutation.error != null && <p className="mt-2 text-sm text-failure">{String(mutation.error)}</p>}
      </div>
    </section>
  )
}
