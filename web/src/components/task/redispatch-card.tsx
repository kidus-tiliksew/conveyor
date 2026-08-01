import { useMutation, useQueryClient } from '@tanstack/react-query'
import { RotateCcw } from 'lucide-react'
import { redispatchTask } from '../../lib/api'
import { dependencyBlockedImplementationOrder } from '../../lib/activity'
import type { ActivityItem } from '../../lib/types'
import { useOperatorToken } from '../app-shell'
import { Button } from '../ui/button'

export function canRedispatch(item: ActivityItem) {
  if (dependencyBlockedImplementationOrder(item)) return false
  return item.task.state === 'queued' || item.task.state === 'closed' || item.task.state === 'parked'
}

// Task management beyond the gate: nudge a stuck queued task, recover a
// parked task at its recorded recovery stage, or reopen a closed task with a
// decided stage.
export function RedispatchCard({ item }: { item: ActivityItem }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => redispatchTask(item.task.id, token),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.id] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  const parked = item.task.state === 'parked'
  return (
    <div className="flex items-center justify-between gap-3 rounded-lg border border-border bg-surface px-3 py-2.5">
      <p className="text-xs text-muted">
        {item.task.state === 'queued'
          ? 'Queued — re-enqueue if dispatch stalled.'
          : parked
            ? 'Parked by triage — resume from the recorded recovery stage when this work is ready.'
            : 'Closed — redispatch resumes at the decided stage.'}
      </p>
      <Button variant="secondary" size="sm" disabled={!token || mutation.isPending} onClick={() => mutation.mutate()}>
        <RotateCcw />
        {mutation.isPending ? (parked ? 'Resuming…' : 'Dispatching…') : (parked ? 'Resume task' : 'Redispatch')}
      </Button>
      {mutation.error != null && <p className="text-xs text-failure">{String(mutation.error)}</p>}
    </div>
  )
}
