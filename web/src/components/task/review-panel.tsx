import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { reviewTask } from '../../lib/api'
import { attentionReason } from '../../lib/activity'
import { interventionActions, reasonCodesByAction } from '../../lib/contracts'
import type { ActivityItem, InterventionAction } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useOperatorToken } from '../app-shell'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'
import { Select, Textarea } from '../ui/input'

// Review actions in place (spec §13.3 element 3 / §13.2): the reviewer acts
// from the timeline context. Every decision records a structured reason
// code — the primary training signal for self-improvement.
export function ReviewPanel({ item }: { item: ActivityItem }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const [action, setAction] = useState<InterventionAction>('approve')
  const [reasonCode, setReasonCode] = useState<string>(reasonCodesByAction.approve[0])
  const [comment, setComment] = useState('')

  const mutation = useMutation({
    mutationFn: () => reviewTask(item.task.id, token, { action, reasonCode, comment }),
    onSuccess: () => {
      setComment('')
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.id] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })

  const pickAction = (next: InterventionAction) => {
    setAction(next)
    setReasonCode(reasonCodesByAction[next][0])
    mutation.reset()
  }

  const hint = interventionActions.find((entry) => entry.action === action)?.hint

  return (
    <section className="rounded-lg border border-attention-dot/40 bg-background" aria-label="Human gate">
      <div className="rounded-t-lg bg-attention-soft px-4 py-3">
        <p className="flex items-center gap-1.5 text-xs font-bold uppercase tracking-[0.14em] text-attention">
          <span className="size-1.5 rounded-full bg-attention-dot" />
          Needs attention
        </p>
        <p className="mt-1 text-sm text-attention/90">{attentionReason(item.task, item.events)}</p>
      </div>
      <div className="space-y-3 px-4 py-4">
        <div className="flex flex-wrap gap-2" role="radiogroup" aria-label="Review action">
          {interventionActions.map((entry) => (
            <Button
              key={entry.action}
              variant={action === entry.action ? 'default' : 'outline'}
              size="sm"
              role="radio"
              aria-checked={action === entry.action}
              title={entry.hint}
              onClick={() => pickAction(entry.action)}
            >
              {entry.label}
            </Button>
          ))}
        </div>
        {action === 'pull_to_local' && item.checkout_available && item.checkout_command ? (
          <div className="flex items-center gap-1 rounded-md border border-border bg-background px-3 py-1.5">
            <code className="min-w-0 flex-1 truncate font-mono text-xs text-foreground/85">{item.checkout_command}</code>
            <CopyButton value={item.checkout_command} label="Copy checkout command" />
          </div>
        ) : action === 'pull_to_local' ? (
          <p className="rounded-md border border-border bg-background px-3 py-2 text-xs leading-5 text-muted">
            {item.checkout_guidance}
          </p>
        ) : null}
        <div className="grid gap-3 md:grid-cols-[200px_minmax(0,1fr)]">
          <label className="block">
            <span className="mb-1 block text-[11px] font-medium uppercase tracking-wider text-muted">Reason code</span>
            <Select value={reasonCode} onChange={(event) => setReasonCode(event.target.value)}>
              {reasonCodesByAction[action].map((code) => (
                <option key={code} value={code}>
                  {code}
                </option>
              ))}
            </Select>
          </label>
          <label className="block">
            <span className="mb-1 block text-[11px] font-medium uppercase tracking-wider text-muted">
              {action === 'redirect' ? 'Redirect feedback' : 'Comment'}
              {action === 'redirect' && <span className="ml-1 normal-case text-faint">— written feedback, not code; it returns the existing branch to the implementing agent</span>}
            </span>
            <Textarea
              value={comment}
              onChange={(event) => setComment(event.target.value)}
              placeholder={action === 'redirect' ? 'What should the agent change?' : 'Optional review note'}
            />
          </label>
        </div>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className={cn('text-xs', token ? 'text-faint' : 'text-attention')}>
            {token ? hint : 'Set the operator token in Settings to act.'}
          </p>
          <Button
            disabled={
              !token ||
              !reasonCode ||
              mutation.isPending ||
              (action === 'redirect' && !comment.trim()) ||
              (action === 'pull_to_local' && !item.checkout_available)
            }
            onClick={() => mutation.mutate()}
          >
            {mutation.isPending ? 'Recording…' : `Confirm ${action.replaceAll('_', ' ')}`}
          </Button>
        </div>
        {mutation.error && <p className="text-sm text-failure">{String(mutation.error)}</p>}
      </div>
    </section>
  )
}
