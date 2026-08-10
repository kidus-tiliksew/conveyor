import { useEffect, useRef, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Clock3, Link2, RotateCcw, TriangleAlert } from 'lucide-react'
import { recoverWorkOrder } from '../../lib/api'
import { deriveCurrentExecutionState, pendingPlanRevisionRequest, type CurrentExecutionState } from '../../lib/activity'
import type { ActivityItem } from '../../lib/types'
import { useOperatorToken } from '../app-shell'
import { Button } from '../ui/button'
import { TaskContextAttachmentDialog } from './task-context-attachment-dialog'

export function hasWorkerRecovery(item: ActivityItem) {
  const state = deriveCurrentExecutionState(item)
  return (
    pendingPlanRevisionRequest(item.events) == null &&
    state != null &&
    state.kind !== 'running' &&
    state.kind !== 'dependency_waiting' &&
    state.kind !== 'dependency_attention'
  )
}

export function WorkOrderRecoveryCard({
  item,
  state = deriveCurrentExecutionState(item),
}: {
  item: ActivityItem
  state?: CurrentExecutionState
}) {
  if (
    pendingPlanRevisionRequest(item.events) != null ||
    !state ||
    state.kind === 'running' ||
    state.kind === 'dependency_waiting' ||
    state.kind === 'dependency_attention'
  )
    return null
  return <RecoveryState item={item} state={state} />
}

function retryCountdown(at: string, now: number) {
  const seconds = Math.max(0, Math.ceil((new Date(at).getTime() - now) / 1000))
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.ceil(seconds / 60)
  if (minutes < 60) return `${minutes}m`
  return `${Math.ceil(minutes / 60)}h`
}

function RecoveryState({ item, state }: { item: ActivityItem; state: CurrentExecutionState }) {
  const { order } = state
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const requestId = useRef(crypto.randomUUID())
  const [checkoutResolved, setCheckoutResolved] = useState(false)
  const [direction, setDirection] = useState('')
  const [attachingContext, setAttachingContext] = useState(false)
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    if (state.kind !== 'retry_pending') return
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [state.kind])
  const checkpointReleased = order.last_failure_message === 'operator checkpoint reached'
  const mutation = useMutation({
    mutationFn: () => recoverWorkOrder(order.id, token, requestId.current, direction.trim() || undefined),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.id] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })

  if (state.kind === 'retry_pending') {
    return (
      <div className="space-y-1 rounded-lg border border-primary/25 bg-primary-soft/40 px-3 py-2.5 text-xs text-muted">
        <p className="flex items-center gap-2 font-medium text-foreground">
          <Clock3 className="size-4 text-primary" aria-hidden />
          {order.next_retry_at ? `Retrying in ${retryCountdown(order.next_retry_at, now)}` : 'Retrying automatically'}
        </p>
        <p>
          Conveyor will start the next attempt automatically. No recovery action is available while this retry is
          pending.
        </p>
      </div>
    )
  }

  const checkoutBlocked = state.kind === 'checkout_blocked'
  const actionLabel =
    state.action === 'retry_implementation' || checkoutBlocked ? 'Retry implementation' : 'Recover work order'
  const canRecover =
    Boolean(token) &&
    !mutation.isPending &&
    (!checkoutBlocked || checkoutResolved) &&
    (!checkpointReleased || direction.trim().length > 0)
  return (
    <div className="space-y-3 rounded-lg border border-attention/50 bg-attention-soft px-3 py-3">
      <div className="flex items-start gap-2">
        <TriangleAlert className="mt-0.5 size-4 shrink-0 text-attention" aria-hidden />
        <div className="min-w-0 space-y-1 text-xs leading-5 text-muted">
          <p className="font-medium text-attention">{state.title}</p>
          <p>{state.nextAction}</p>
        </div>
      </div>
      <div className="space-y-2 text-xs leading-5 text-muted">
        {checkpointReleased ? (
          <p>
            A decision is required before this work can continue. Recovery without direction will repeat the checkpoint.
          </p>
        ) : (
          <p>You can add an optional instruction for the next attempt.</p>
        )}
        {checkpointReleased && order.progress?.trim() && (
          <div className="space-y-1.5">
            <p className="font-medium text-foreground">Agent checkpoint message</p>
            <pre className="max-h-48 overflow-auto whitespace-pre-wrap rounded border border-attention/30 bg-surface p-2 font-sans text-sm text-foreground">
              {order.progress}
            </pre>
          </div>
        )}
        <div className="flex flex-wrap items-center justify-between gap-2 rounded border border-attention/30 bg-surface/60 p-2">
          <span>
            <span className="font-medium text-foreground">Attached context: </span>
            {(item.task.context?.requirements?.length ?? 0) + (item.task.context?.designs?.length ?? 0) === 0
              ? 'None'
              : `${item.task.context?.requirements?.length ?? 0} requirement(s), ${item.task.context?.designs?.length ?? 0} design document(s)`}
          </span>
          <Button variant="secondary" size="sm" disabled={!token} onClick={() => setAttachingContext(true)}>
            <Link2 aria-hidden /> Attach context
          </Button>
        </div>
        <label className="block space-y-1.5">
          <span className="font-medium text-foreground">Operator direction</span>
          <textarea
            aria-label="Operator direction"
            maxLength={4096}
            rows={4}
            value={direction}
            onChange={(event) => setDirection(event.target.value)}
            placeholder="State the decision or instruction the agent should follow."
            className="w-full resize-y rounded-md border border-attention/30 bg-surface px-3 py-2 text-sm text-foreground outline-none placeholder:text-faint focus-visible:ring-2 focus-visible:ring-primary"
          />
        </label>
      </div>
      {order.last_failure_detail && (
        <details className="text-xs text-muted">
          <summary className="cursor-pointer rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary">
            Show technical details
          </summary>
          <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap rounded border border-attention/30 bg-surface p-2 font-mono">
            {order.last_failure_detail}
          </pre>
        </details>
      )}
      {checkoutBlocked && (
        <div className="space-y-2 text-xs leading-5 text-muted">
          <p>
            Review the affected files, then commit, stash, or otherwise resolve those changes in the primary checkout.
            Conveyor will not clean, commit, stash, or discard them.
          </p>
          <label className="flex cursor-pointer items-start gap-2 rounded border border-attention/30 bg-surface/60 p-2 focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-primary">
            <input
              type="checkbox"
              className="mt-0.5 size-4 accent-primary"
              checked={checkoutResolved}
              onChange={(event) => setCheckoutResolved(event.target.checked)}
            />
            <span>I resolved the primary checkout changes.</span>
          </label>
        </div>
      )}
      <Button variant="secondary" size="sm" disabled={!canRecover} onClick={() => mutation.mutate()}>
        <RotateCcw aria-hidden />
        {mutation.isPending ? 'Retrying…' : actionLabel}
      </Button>
      {!token && <p className="text-xs text-muted">Operator authorization is required to retry.</p>}
      {mutation.error != null && <p className="text-xs text-failure">{String(mutation.error)}</p>}
      {attachingContext && (
        <TaskContextAttachmentDialog task={item.task} token={token} onClose={() => setAttachingContext(false)} />
      )}
    </div>
  )
}
