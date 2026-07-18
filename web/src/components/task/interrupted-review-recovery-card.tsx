import { useRef } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { RotateCcw, TriangleAlert } from 'lucide-react'
import { recoverInterruptedReviewRound } from '../../lib/api'
import type { ActivityItem } from '../../lib/types'
import { useOperatorToken } from '../app-shell'
import { Button } from '../ui/button'

export function InterruptedReviewRecoveryCard({ item }: { item: ActivityItem }) {
  const recovery = item.interrupted_review_recovery
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const requestId = useRef(crypto.randomUUID())
  const mutation = useMutation({
    mutationFn: () => recoverInterruptedReviewRound(item.task.id, token, requestId.current),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.id] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  if (!recovery?.needed) return null
  return (
    <section className="space-y-3 rounded-lg border border-attention/40 bg-attention-soft px-3 py-3" aria-label="Interrupted review round recovery">
      <div className="flex items-start gap-2">
        <TriangleAlert className="mt-0.5 size-4 shrink-0 text-attention" />
        <div className="min-w-0 text-xs leading-5 text-muted">
          <p className="font-medium text-attention">Review round {recovery.review_round} was interrupted</p>
          <p>{recovery.reason}. Only the interrupted seats below will be requeued; completed verdicts are retained.</p>
          {recovery.eligible_orders.map((order) => (
            <p key={order.id} className="mt-1 font-mono text-[11px]">
              Seat {order.review_seat ?? '?'} · {order.id} · {order.last_attempt_outcome ?? 'retry suppressed'}
            </p>
          ))}
          {recovery.retained_orders.map((order) => (
            <p key={order.id} className="mt-1 font-mono text-[11px] text-positive">
              Seat {order.review_seat ?? '?'} · completed verdict retained
            </p>
          ))}
        </div>
      </div>
      <Button variant="secondary" size="sm" disabled={!token || mutation.isPending} onClick={() => mutation.mutate()}>
        <RotateCcw />
        {mutation.isPending ? 'Recovering…' : 'Recover interrupted review round'}
      </Button>
      {mutation.data && <p className="text-xs text-positive">Recovered {mutation.data.recovered_orders.length} interrupted seat{mutation.data.recovered_orders.length === 1 ? '' : 's'}; {mutation.data.retained_orders.length} completed verdict{mutation.data.retained_orders.length === 1 ? '' : 's'} retained.</p>}
      {mutation.error != null && <p className="text-xs text-failure">Recovery failed without partial changes: {String(mutation.error)}</p>}
    </section>
  )
}
