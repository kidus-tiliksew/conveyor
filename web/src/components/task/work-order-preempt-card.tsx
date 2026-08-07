import { useRef, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { OctagonX } from 'lucide-react'
import { preemptWorkOrder } from '../../lib/api'
import type { ActivityItem, WorkOrder } from '../../lib/types'
import { useOperatorToken } from '../app-shell'
import { Button } from '../ui/button'
import { Textarea } from '../ui/input'

export function claimedWorkOrder(item: ActivityItem): WorkOrder | undefined {
  return (item.work_orders ?? []).find((order) => order.state === 'claimed')
}

export function WorkOrderPreemptCard({ item, order = claimedWorkOrder(item) }: { item: ActivityItem; order?: WorkOrder }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const [reason, setReason] = useState('')
  const requestId = useRef(crypto.randomUUID())
  const mutation = useMutation({
    mutationFn: () => preemptWorkOrder(order!.id, token, reason.trim(), requestId.current),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['task'] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void queryClient.invalidateQueries({ queryKey: ['work-orders'] })
    },
  })
  if (!order) return null
  return (
    <div className="space-y-3 rounded-lg border border-attention/50 bg-attention-soft px-3 py-3">
      <div className="flex items-start gap-2">
        <OctagonX className="mt-0.5 size-4 shrink-0 text-attention" aria-hidden />
        <div className="text-xs leading-5 text-muted">
          <p className="font-medium text-attention">Stop this claimed attempt</p>
          <p>
            Preemption revokes only this work order. The worker stops it at the next renewal, within one renewal
            interval, and checkpoints dirty implementation work before the next claim.
          </p>
        </div>
      </div>
      <Textarea
        aria-label="Reason for preempting work order"
        placeholder="Why should this attempt stop?"
        value={reason}
        onChange={(event) => setReason(event.target.value)}
        disabled={mutation.isPending || mutation.isSuccess}
      />
      <Button
        variant="destructive"
        size="sm"
        disabled={!token || !reason.trim() || mutation.isPending || mutation.isSuccess}
        onClick={() => mutation.mutate()}
      >
        <OctagonX />
        {mutation.isPending ? 'Preempting…' : mutation.isSuccess ? 'Preempted' : 'Preempt attempt'}
      </Button>
      {mutation.error != null && <p className="text-xs text-failure">{String(mutation.error)}</p>}
    </div>
  )
}
