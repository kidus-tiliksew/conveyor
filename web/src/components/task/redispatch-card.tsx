import { useMutation, useQueryClient } from '@tanstack/react-query'
import { RotateCcw } from 'lucide-react'
import { useRef } from 'react'
import { redispatchTask, reviewTask } from '../../lib/api'
import { dependencyBlockedImplementationOrder, failedTriage, unsatisfiableDependencyOrder } from '../../lib/activity'
import type { ActivityItem } from '../../lib/types'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'

// Failed triage has no decided next stage, so /redispatch cannot recover it.
// The existing operator redirect resolves the server's recorded recovery stage.
export function FailedTriageCard({ item }: { item: ActivityItem }) {
  const queryClient = useQueryClient()
  const { workspace } = useWorkspaceSelection()
  const canOperate = useWorkspaceCapability('operate_gates')
  const failure = failedTriage(item)
  const acceptedJob = useRef<string | undefined>(undefined)
  const submitting = useRef(false)
  const mutation = useMutation({
    mutationFn: async () => {
      if (!failure || !canOperate || failure.repairRequired) return
      if (acceptedJob.current !== failure.jobId) {
        await reviewTask(item.task.id, {
          action: 'redirect',
          reasonCode: 'changes-requested',
          comment:
            'Retry triage from its recorded recovery stage after the failed attempt. Reassess the task before any downstream work.',
        })
        acceptedJob.current = failure.jobId
      }
      // Refresh summaries before detail: the live card remains pending until
      // all mounted task surfaces have refreshed. Never synthesize task state.
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['activity', workspace] }, { throwOnError: true }),
        queryClient.invalidateQueries({ queryKey: ['task-operations', workspace] }, { throwOnError: true }),
      ])
      await queryClient.invalidateQueries({ queryKey: ['task', workspace, item.task.id] }, { throwOnError: true })
      const refreshed = queryClient.getQueryData<ActivityItem>(['task', workspace, item.task.id])
      if (!refreshed || failedTriage(refreshed)?.jobId === failure.jobId) {
        throw new Error(
          'Retry was accepted, but recovery is not yet confirmed by refreshed task data. Refresh status to check again.',
        )
      }
    },
    onSettled: () => {
      submitting.current = false
    },
  })
  if (!failure) return null
  return (
    <section aria-label="Triage recovery" className="rounded-lg border border-attention/30 bg-surface px-3 py-3">
      <h3 className="text-sm font-semibold">Triage failed</h3>
      <p className="mt-2 whitespace-pre-wrap break-words text-xs text-foreground">{failure.reason}</p>
      <p className="mt-2 text-xs text-muted">{failure.guidance}</p>
      {failure.repairRequired && (
        <p className="mt-2 text-xs text-muted">
          Retry is unavailable while this attempt records invalid input. Repair the input through the authorized
          recovery workflow; refreshing this view alone does not repair it.
        </p>
      )}
      {canOperate ? (
        <Button
          className="mt-3"
          variant="secondary"
          size="sm"
          disabled={mutation.isPending || failure.repairRequired}
          onClick={() => {
            if (submitting.current) return
            submitting.current = true
            mutation.mutate()
          }}
        >
          <RotateCcw />
          {mutation.isPending
            ? 'Checking recovery…'
            : acceptedJob.current === failure.jobId
              ? 'Refresh status'
              : 'Retry triage'}
        </Button>
      ) : (
        <p className="mt-2 text-xs text-muted">An operator with permission to operate gates can recover this task.</p>
      )}
      {mutation.error && (
        <p role="alert" className="mt-2 text-xs text-failure">
          {mutation.error.message}
        </p>
      )}
    </section>
  )
}

export function canRedispatch(item: ActivityItem) {
  if (dependencyBlockedImplementationOrder(item) || unsatisfiableDependencyOrder(item)) return false
  if (
    item.pending_authority === true &&
    (item.work_orders ?? []).some((order) => order.stage === 'review' && order.state === 'queued')
  )
    return false
  return item.task.state === 'queued' || item.task.state === 'closed' || item.task.state === 'parked'
}

// Task management beyond the gate: nudge a stuck queued task, recover a
// parked task at its recorded recovery stage, or reopen a closed task with a
// decided stage.
export function RedispatchCard({ item }: { item: ActivityItem }) {
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => redispatchTask(item.task.id),
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
      <Button variant="secondary" size="sm" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
        <RotateCcw />
        {mutation.isPending ? (parked ? 'Resuming…' : 'Dispatching…') : parked ? 'Resume task' : 'Redispatch'}
      </Button>
      {mutation.error != null && <p className="text-xs text-failure">{String(mutation.error)}</p>}
    </div>
  )
}
