import { useId, useRef, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { OctagonX } from 'lucide-react'
import { preemptWorkOrder } from '../../lib/api'
import type { ActivityItem, WorkOrder } from '../../lib/types'
import { Button } from '../ui/button'
import { Textarea } from '../ui/input'

export function claimedWorkOrder(item: ActivityItem): WorkOrder | undefined {
  return (item.work_orders ?? []).find((order) => order.state === 'claimed')
}

// Stopping a running attempt is a rare deliberate act, not a standing alarm.
// It rests as one quiet trigger beside the "an attempt is running" statement —
// never inside the event list, where an always-open amber box read as if the
// routine lease renewal above it were the problem. Only the confirmation is
// destructive, and the copy is what the operator needs to decide; the protocol
// mechanics (work-order revocation, renewal discovery, checkpoint before the
// next claim) live in its tooltip.
export function WorkOrderPreemptControl({ order }: { order: WorkOrder }) {
  const queryClient = useQueryClient()
  const [expanded, setExpanded] = useState(false)
  const [reason, setReason] = useState('')
  const panelID = useId()
  const requestId = useRef(crypto.randomUUID())
  const mutation = useMutation({
    mutationFn: () => preemptWorkOrder(order.id, reason.trim(), requestId.current),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['task'] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void queryClient.invalidateQueries({ queryKey: ['work-orders'] })
    },
  })
  return (
    <div className="space-y-2.5">
      <div className="flex justify-end">
        <Button
          variant="ghost"
          size="sm"
          aria-expanded={expanded}
          aria-controls={expanded ? panelID : undefined}
          onClick={() => setExpanded((open) => !open)}
        >
          <OctagonX />
          Stop attempt
        </Button>
      </div>
      {expanded && (
        <div id={panelID} className="space-y-2.5 rounded-lg border border-border bg-surface/50 px-3 py-3">
          <p
            className="text-xs leading-5 text-muted"
            title="Preemption revokes only this work order. The worker discovers it at the next renewal and checkpoints dirty implementation work before the next claim."
          >
            The agent stops within about a minute. Work it has already done is saved first. The task itself is not
            cancelled — it can run again.
          </p>
          <Textarea
            aria-label="Reason for stopping this attempt"
            placeholder="Why should this attempt stop?"
            value={reason}
            onChange={(event) => setReason(event.target.value)}
            disabled={mutation.isPending || mutation.isSuccess}
          />
          <Button
            variant="destructive"
            size="sm"
            disabled={!reason.trim() || mutation.isPending || mutation.isSuccess}
            onClick={() => mutation.mutate()}
          >
            <OctagonX />
            {mutation.isPending ? 'Stopping…' : mutation.isSuccess ? 'Stopped' : 'Stop the attempt'}
          </Button>
          {mutation.error != null && <p className="text-xs text-failure">{String(mutation.error)}</p>}
        </div>
      )}
    </div>
  )
}
