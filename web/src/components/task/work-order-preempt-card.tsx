import { useMutation, useQueryClient } from '@tanstack/react-query'
import { OctagonX } from 'lucide-react'
import { useEffect, useId, useRef, useState } from 'react'
import { preemptWorkOrder } from '../../lib/api'
import type { ActivityItem, WorkOrder } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { readableError } from '../ui/error-state'
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
// With `onOpenChange` the caller owns the trigger, such as a "Stop attempt…"
// menu item, and the control renders only the confirmation, with a Cancel
// button. The mutation and its request ID live as long as the control stays
// mounted, whichever form opens it.
export function WorkOrderPreemptControl({
  order,
  open,
  onOpenChange,
}: {
  order: WorkOrder
  open?: boolean
  onOpenChange?: (open: boolean) => void
}) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const [ownExpanded, setOwnExpanded] = useState(false)
  const controlled = onOpenChange !== undefined
  const expanded = controlled ? open === true : ownExpanded
  const [reason, setReason] = useState('')
  const panelID = useId()
  const requestId = useRef(crypto.randomUUID())
  const mutation = useMutation({
    mutationFn: () => preemptWorkOrder(workspace, order.id, reason.trim(), requestId.current),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['task'] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void queryClient.invalidateQueries({ queryKey: ['work-orders'] })
    },
  })
  // A confirmation opened from a menu takes focus, so the menu's closing
  // never strands it on the trigger.
  useEffect(() => {
    if (controlled && expanded) document.getElementById(`${panelID}-reason`)?.focus()
  }, [controlled, expanded, panelID])
  return (
    <div className="space-y-2.5">
      {!controlled && (
        <div className="flex justify-end">
          <Button
            variant="ghost"
            size="sm"
            aria-expanded={expanded}
            aria-controls={expanded ? panelID : undefined}
            onClick={() => setOwnExpanded((value) => !value)}
          >
            <OctagonX />
            Stop attempt
          </Button>
        </div>
      )}
      {expanded && (
        <div
          id={panelID}
          data-preempt-confirmation
          className="space-y-2.5 rounded-lg border border-border bg-surface/50 px-3 py-3"
        >
          <p
            className="text-xs leading-5 text-muted"
            title="Preemption revokes only this work order. The agent session sees the loss at its next lease renewal."
          >
            The agent stops within about a minute. Work it has already done is saved first. The task itself is not
            cancelled — it can run again.
          </p>
          <Textarea
            id={`${panelID}-reason`}
            aria-label="Reason for stopping this attempt"
            placeholder="Why should this attempt stop?"
            value={reason}
            onChange={(event) => setReason(event.target.value)}
            disabled={mutation.isPending || mutation.isSuccess}
          />
          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="destructive"
              size="sm"
              disabled={!reason.trim() || mutation.isPending || mutation.isSuccess}
              onClick={() => mutation.mutate()}
            >
              <OctagonX />
              {mutation.isPending ? 'Stopping…' : mutation.isSuccess ? 'Stopped' : 'Stop the attempt'}
            </Button>
            {controlled && !mutation.isSuccess && (
              <Button variant="ghost" size="sm" disabled={mutation.isPending} onClick={() => onOpenChange(false)}>
                Cancel
              </Button>
            )}
          </div>
          {mutation.error != null && (
            <p className="text-xs text-failure">{readableError(mutation.error, 'Something went wrong. Try again.')}</p>
          )}
        </div>
      )}
    </div>
  )
}
