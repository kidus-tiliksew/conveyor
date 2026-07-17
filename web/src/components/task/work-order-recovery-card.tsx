import { useRef } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { RotateCcw, TriangleAlert } from 'lucide-react'
import { recoverWorkOrder } from '../../lib/api'
import type { ActivityItem, WorkOrder } from '../../lib/types'
import { useOperatorToken } from '../app-shell'
import { Button } from '../ui/button'

function recoveryOrder(item: ActivityItem) {
  return [...(item.work_orders ?? [])]
    .reverse()
    .find((order) => order.state === 'queued' && (order.last_attempt_outcome || order.retry_suppressed || order.next_retry_at))
}

export function hasWorkerRecovery(item: ActivityItem) {
  return recoveryOrder(item) != null
}

export function WorkOrderRecoveryCard({ item }: { item: ActivityItem }) {
  const order = recoveryOrder(item)
  if (!order) return null
  return <RecoveryState item={item} order={order} />
}

function RecoveryState({ item, order }: { item: ActivityItem; order: WorkOrder }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const requestId = useRef(crypto.randomUUID())
  const mutation = useMutation({
    mutationFn: () => recoverWorkOrder(order.id, token, requestId.current),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.id] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  const retry = order.next_retry_at ? `Next automatic retry ${new Date(order.next_retry_at).toLocaleString()}.` : ''
  const failureTime = order.last_failure_at ? ` at ${new Date(order.last_failure_at).toLocaleString()}` : ''
  const exitStatus = order.last_failure_exit_status != null ? ` (exit ${order.last_failure_exit_status})` : ''
  const failure = order.last_failure_message ? ` Last child failure${failureTime}${exitStatus}: ${order.last_failure_message}.` : ''
  return (
    <div className="space-y-2 rounded-lg border border-attention/40 bg-attention-soft px-3 py-2.5">
      <div className="flex items-start gap-2">
        <TriangleAlert className="mt-0.5 size-4 shrink-0 text-attention" />
        <p className="min-w-0 text-xs text-muted">
          Attempt {order.last_attempt_outcome ?? 'released'} · {order.automatic_retry_count ?? 0} automatic retries used. {retry}{failure}
          {order.retry_suppressed && ' Automatic retry is suppressed until an operator recovers this order.'}
        </p>
      </div>
      {(order.retry_suppressed || order.next_retry_at) && (
        <Button variant="secondary" size="sm" disabled={!token || mutation.isPending} onClick={() => mutation.mutate()}>
          <RotateCcw />
          {mutation.isPending ? 'Recovering…' : 'Recover work order'}
        </Button>
      )}
      {mutation.error != null && <p className="text-xs text-failure">{String(mutation.error)}</p>}
    </div>
  )
}
