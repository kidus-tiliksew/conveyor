import { skipToken, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useEffect, useId, useState } from 'react'
import { restartOrderCount, restartPullRequestURL, taskPendingRestartProposals } from '../../lib/activity'
import { restartTask } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { relatedTaskRoute, type TaskRouteVariant } from '../../lib/task-route'
import type { ActivityItem, TaskRestartInput } from '../../lib/types'
import { usePendingProposals, useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'
import { Input, Textarea } from '../ui/input'

export function TaskRestartControl({ item, variant }: { item: ActivityItem; variant: TaskRouteVariant }) {
  const [open, setOpen] = useState(false)
  const canOperate = useWorkspaceCapability('operate_gates')
  if (!canOperate || item.task.state === 'merged' || item.task.state === 'closed') return null
  return (
    <>
      <Button variant="ghost" size="sm" onClick={() => setOpen(true)}>
        Start over
      </Button>
      {open && <TaskRestartDialog item={item} variant={variant} onClose={() => setOpen(false)} />}
    </>
  )
}

function TaskRestartDialog({
  item,
  variant,
  onClose,
}: {
  item: ActivityItem
  variant: TaskRouteVariant
  onClose: () => void
}) {
  const id = useId()
  const [reason, setReason] = useState('')
  const [note, setNote] = useState('')
  const [requestId] = useState(() => crypto.randomUUID())
  const [submitted, setSubmitted] = useState<TaskRestartInput | null>(null)
  const canConfirm = useWorkspaceCapability('confirm_documents')
  const pending = usePendingProposals()
  const queryClient = useQueryClient()
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const proposalsUsable = !pending.isPending && !pending.isError && Array.isArray(pending.data?.items)
  const proposals = taskPendingRestartProposals(pending.data?.items ?? [], item.task.id)
  const missingAuthority = proposals.length > 0 && !canConfirm
  const orderCount = restartOrderCount(item)
  const pr = restartPullRequestURL(item)
  const reasonLength = Array.from(reason.trim()).length
  const noteLength = Array.from(note).length
  const mutation = useMutation({
    mutationFn: (input: TaskRestartInput) => restartTask(item.task.id, input),
    // Fetch rejects with TypeError on network failure. Server refusals stay visible.
    retry: (count, error) => error instanceof TypeError && count < 1,
    onSuccess: async (result) => {
      for (const key of ['task', 'activity', 'pending-proposals', 'task-operations', 'tasks', 'workspace']) {
        void queryClient.invalidateQueries({ queryKey: [key, workspace] })
      }
      queryClient.setQueryData(['task-restart-notice', workspace, result.successor.id], Date.now())
      await navigate({ to: relatedTaskRoute(variant), params: { taskId: result.successor.id } })
      onClose()
    },
  })
  const disabled =
    mutation.isPending ||
    !proposalsUsable ||
    missingAuthority ||
    reasonLength === 0 ||
    reasonLength > 200 ||
    noteLength > 2000
  return (
    <Dialog
      label="Start over"
      onClose={() => {
        if (!mutation.isPending) onClose()
      }}
    >
      <form
        className="space-y-4 p-5"
        onSubmit={(event) => {
          event.preventDefault()
          if (disabled) return
          // Preserve both key and payload if an ambiguous network result needs a retry (AC-7.5).
          const input = submitted ?? { reason: reason.trim(), note: note || undefined, request_id: requestId }
          setSubmitted(input)
          mutation.mutate(input)
        }}
      >
        <h2 className="text-lg font-semibold">Start over</h2>
        <section className="space-y-2 text-sm text-muted" aria-label="Restart preview">
          <p>This task will be cancelled.</p>
          {orderCount !== undefined && <p>{orderCount} non-terminal work orders will be cancelled.</p>}
          {proposalsUsable ? (
            proposals.length > 0 ? (
              <div>
                <p>
                  {proposals.length} pending document {proposals.length === 1 ? 'proposal' : 'proposals'} from this task
                  will be dismissed with your note:
                </p>
                <ul className="mt-1 list-disc pl-5">
                  {proposals.map((proposal) => (
                    <li key={`${proposal.tier}/${proposal.id}/${proposal.version}`}>
                      {proposal.title || proposal.id}
                      {proposal.version ? ` v${proposal.version}` : ''}
                    </li>
                  ))}
                </ul>
              </div>
            ) : (
              <p>No pending document proposals from this task.</p>
            )
          ) : (
            <p role="status">
              {pending.isError
                ? 'Pending proposals could not be loaded. Retry before starting over.'
                : 'Loading pending proposals…'}
            </p>
          )}
          {pending.isError && (
            <Button type="button" variant="outline" size="sm" onClick={() => void pending.refetch()}>
              Retry preview
            </Button>
          )}
          {pr && (
            <p>
              Pull request{' '}
              <a href={pr} target="_blank" rel="noreferrer" className="text-primary underline">
                {pr}
              </a>{' '}
              will be closed. A forge failure will be recorded without undoing the restart.
            </p>
          )}
          <p>
            A new task with a new branch will be created with the same body and pinned documents, open dependencies, and
            frozen policy.
          </p>
        </section>
        {missingAuthority && (
          <p role="alert" className="text-sm text-attention">
            An operator with confirm_documents must start over this task because it has pending document proposals.
          </p>
        )}
        <div>
          <label htmlFor={`${id}-reason`} className="text-sm font-medium">
            Reason (required, up to 200 characters)
          </label>
          <Input
            id={`${id}-reason`}
            required
            value={reason}
            disabled={Boolean(submitted)}
            onChange={(event) => setReason(event.target.value)}
          />
          {reasonLength > 200 && (
            <p role="alert" className="text-sm text-failure">
              Reason must be at most 200 characters.
            </p>
          )}
        </div>
        <div>
          <label htmlFor={`${id}-note`} className="text-sm font-medium">
            What went wrong, for the next session (optional)
          </label>
          <Textarea
            id={`${id}-note`}
            value={note}
            disabled={Boolean(submitted)}
            onChange={(event) => setNote(event.target.value)}
            aria-describedby={`${id}-counter`}
          />
          <p id={`${id}-counter`} className="text-xs text-muted" aria-live="polite">
            {noteLength} / 2000 characters
          </p>
          {noteLength > 2000 && (
            <p role="alert" className="text-sm text-failure">
              Note must be at most 2000 characters.
            </p>
          )}
        </div>
        {mutation.isError && (
          <p role="alert" className="text-sm text-failure">
            {errorMessage(mutation.error)}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" disabled={mutation.isPending} onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={disabled}>
            {mutation.isPending ? 'Starting over…' : submitted ? 'Retry start over' : 'Start over'}
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// The destination header owns the brief success notice so it survives navigation.
export function TaskRestartNotice({ taskId }: { taskId: string }) {
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const { data } = useQuery<number>({
    queryKey: ['task-restart-notice', workspace, taskId],
    queryFn: skipToken,
    enabled: false,
  })
  useEffect(() => {
    if (!data) return
    const timer = window.setTimeout(
      () => client.setQueryData(['task-restart-notice', workspace, taskId], 0),
      Math.max(0, data + 6000 - Date.now()),
    )
    return () => window.clearTimeout(timer)
  }, [client, data, taskId, workspace])
  return data ? (
    <div
      role="status"
      className="fixed bottom-5 right-5 z-50 rounded-lg border border-border bg-card p-4 text-sm shadow-lg"
    >
      Started over as {taskId}
    </div>
  ) : null
}
