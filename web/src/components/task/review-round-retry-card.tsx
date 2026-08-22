import { useRef, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { RotateCcw, TriangleAlert } from 'lucide-react'
import { retryReviewRound } from '../../lib/api'
import type { ActivityItem } from '../../lib/types'
import { useDashboardSession } from '../app-shell'
import { Button } from '../ui/button'
import { Textarea } from '../ui/input'

export function hasReviewRoundRetry(item: ActivityItem) {
  return item.review_recovery?.needed === true
}

export function ReviewRoundRetryCard({ item }: { item: ActivityItem }) {
  const recovery = item.review_recovery
  const token = useDashboardSession()
  const queryClient = useQueryClient()
  const requestId = useRef(crypto.randomUUID())
  const [reason, setReason] = useState('')
  const mutation = useMutation({
    mutationFn: () => retryReviewRound(item.task.id, requestId.current, reason.trim()),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.id] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  if (!recovery?.needed) return null
  return (
    <section
      className="space-y-3 rounded-lg border border-attention/40 bg-attention-soft px-3 py-3"
      aria-label="Review round recovery"
    >
      <div className="flex items-start gap-2">
        <TriangleAlert className="mt-0.5 size-4 shrink-0 text-attention" />
        <div className="min-w-0 text-xs leading-5 text-muted">
          <p className="font-medium text-attention">Review round {recovery.prior_round} needs operator attention</p>
          <p>{recovery.reason}. The previous round and its verdicts will remain as history.</p>
          {recovery.timed_out_orders.map((order) => (
            <p key={order.id} className="mt-1 font-mono text-[11px]">
              Seat {order.review_seat ?? '?'} · {order.id} · timed out
              {order.execution_deadline ? ` at ${new Date(order.execution_deadline).toLocaleString()}` : ''}
              {order.last_failure_message ? ` · ${order.last_failure_message}` : ''}
            </p>
          ))}
          {(recovery.inconsistent_orders ?? []).map((order) => (
            <p key={order.id} className="mt-1 font-mono text-[11px]">
              Seat {order.review_seat ?? '?'} · {order.id} · completed verdict with contradictory{' '}
              {order.last_attempt_outcome ?? 'failed child'} outcome
            </p>
          ))}
        </div>
      </div>
      <Textarea
        aria-label="Review retry reason"
        value={reason}
        onChange={(event) => setReason(event.target.value)}
        placeholder="Why should Conveyor retry this review round?"
        rows={2}
      />
      <Button
        variant="secondary"
        size="sm"
        disabled={!token || !reason.trim() || mutation.isPending}
        onClick={() => mutation.mutate()}
      >
        <RotateCcw />
        {mutation.isPending ? 'Retrying…' : 'Retry review round'}
      </Button>
      {mutation.data && (
        <p className="text-xs text-positive">
          Review round {mutation.data.new_round} is queued with {mutation.data.work_orders.length} seats.
        </p>
      )}
      {mutation.error != null && <p className="text-xs text-failure">{String(mutation.error)}</p>}
    </section>
  )
}
