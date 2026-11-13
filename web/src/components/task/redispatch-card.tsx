import { useMutation, useQueryClient } from '@tanstack/react-query'
import { RotateCcw } from 'lucide-react'
import { useRef } from 'react'
import { dependencyBlockedImplementationOrder, failedTriage, unsatisfiableDependencyOrder } from '../../lib/activity'
import { redispatchTask, reviewTask } from '../../lib/api'
import { type MutationWorkspace, taskDetailQueryKey } from '../../lib/query-keys'
import type { ActivityItem, NextAction } from '../../lib/types'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { readableError } from '../ui/error-state'

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
        await reviewTask(workspace, item.task.id, {
          action: 'redirect',
          reasonCode: 'changes-requested',
          comment:
            'Retry triage from its recorded recovery stage after the failed attempt. Reassess the task before any downstream work.',
        })
        acceptedJob.current = failure.jobId
      }
      // Refresh summaries before detail: the live card remains pending until
      // all mounted task surfaces have refreshed. Never synthesize task state.
      await queryClient.invalidateQueries({ queryKey: ['activity', workspace] }, { throwOnError: true })
      await queryClient.invalidateQueries(
        { queryKey: taskDetailQueryKey(workspace, item.task.id) },
        { throwOnError: true },
      )
      const refreshed = queryClient.getQueryData<ActivityItem>(taskDetailQueryKey(workspace, item.task.id))
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

// The task's redispatch route, as the server states it in a choice's path.
function redispatchPaths(taskId: string) {
  return new Set([`/v1/tasks/${taskId}/redispatch`, `/v1/tasks/${encodeURIComponent(taskId)}/redispatch`])
}

// Whether the server's next action already offers the redispatch act: any
// choice whose path is the task's redispatch route, whatever its ID or label
// (`recover` at parked, `redispatch` at stalled and dispatch_exhausted). Such a
// choice is the act's one button, under the server's label (DEC-61;
// req-operator-next-action AC-1.4, AC-1.7).
export function offersRedispatchChoice(action: Pick<NextAction, 'choices'>, taskId: string) {
  const paths = redispatchPaths(taskId)
  return (action.choices ?? []).some((choice) => paths.has(choice.path))
}

// The card covers what the next action offers no choice for: a queued task
// whose claim has not started, and a closed task with a decided stage. A
// parked task's redispatch is always the server's choice. The card waits for
// the next action, so a slow read never shows a second button for a choice
// about to arrive; a failed read leaves the card, since then no choice renders.
export function canRedispatch(item: ActivityItem, nextAction: NextAction | undefined, nextActionFailed: boolean) {
  if (dependencyBlockedImplementationOrder(item) || unsatisfiableDependencyOrder(item)) return false
  if (item.task.state !== 'queued' && item.task.state !== 'closed') return false
  // A closed task redispatches only at a decided stage; the route refuses
  // one without (core.TaskRedispatchStage).
  if (item.task.state === 'closed' && !item.task.next_stage) return false
  if (nextAction) return !offersRedispatchChoice(nextAction, item.task.id)
  return nextActionFailed
}

// Task management beyond the gate: nudge a stuck queued task, or reopen a
// closed task with a decided stage.
export function RedispatchCard({ item }: { item: ActivityItem }) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => redispatchTask(workspace, item.task.id),
    onMutate: (): MutationWorkspace => ({ workspace }),
    onSuccess: (_data, _variables, started) => {
      if (started) void queryClient.invalidateQueries({ queryKey: taskDetailQueryKey(started.workspace, item.task.id) })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  return (
    <div className="flex items-center justify-between gap-3 rounded-lg border border-border bg-surface px-3 py-2.5">
      <p className="text-xs text-muted">
        {item.task.state === 'queued'
          ? 'Waiting for an agent to pick this task up.'
          : 'Closed — redispatch resumes at the decided stage.'}
      </p>
      <Button variant="secondary" size="sm" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
        <RotateCcw />
        {mutation.isPending ? 'Dispatching…' : 'Redispatch'}
      </Button>
      {mutation.error != null && (
        <p className="text-xs text-failure">{readableError(mutation.error, 'Something went wrong. Try again.')}</p>
      )}
    </div>
  )
}
