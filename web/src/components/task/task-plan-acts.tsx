import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useId, useState } from 'react'
import { attachTaskPlan, changeTaskPlanState, PlanActError } from '../../lib/api'
import { planStateLabels } from '../../lib/contracts'
import { errorMessage } from '../../lib/errors'
import { taskDetailQueryKey } from '../../lib/query-keys'
import type { ActivityItem, PlanActs } from '../../lib/types'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { nextActionQueryKey, nextActionsQueryKey } from '../inbox/use-next-actions'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'
import { Input, Textarea } from '../ui/input'

type PlanStateTarget = 'needed' | 'not_required'

// The targets Change plan state offers, in the order invariant 61 names them,
// each behind the plan_acts flag that admits it.
const planStateTargets: ReadonlyArray<{ target: PlanStateTarget; flag: keyof PlanActs }> = [
  { target: 'needed', flag: 'set_needed' },
  { target: 'not_required', flag: 'set_not_required' },
]

const fullCommitID = /^[0-9a-f]{40}$/

// The operator's plan acts on task detail (component-web-task-surfaces
// invariants 61, 62, 64; req-task-plan-state AC-3.1, AC-3.2). `plan_acts` is
// the server's admission predicate, so the section offers exactly the acts it
// marks true and reads no orders, events, or plan versions to decide (DEC-61).
// The acts render only for operate_gates. Approval stays on the gate card.
export function TaskPlanActs({ item }: { item: ActivityItem }) {
  const canOperate = useWorkspaceCapability('operate_gates')
  const [open, setOpen] = useState<'state' | 'attach' | null>(null)
  const acts = item.plan_acts
  if (!canOperate || !acts) return null
  const targets = planStateTargets.filter((entry) => acts[entry.flag] === true).map((entry) => entry.target)
  const canAttach = acts.attach === true
  if (targets.length === 0 && !canAttach) return null
  return (
    <div className="flex flex-wrap items-center gap-1.5" data-plan-acts>
      {targets.length > 0 && (
        <Button variant="secondary" size="sm" onClick={() => setOpen('state')}>
          Change plan state
        </Button>
      )}
      {canAttach && (
        <Button variant="secondary" size="sm" onClick={() => setOpen('attach')}>
          Attach plan
        </Button>
      )}
      {open === 'state' && <ChangePlanStateDialog item={item} targets={targets} onClose={() => setOpen(null)} />}
      {open === 'attach' && <AttachPlanDialog item={item} onClose={() => setOpen(null)} />}
    </div>
  )
}

// One request ID per dialog: made when the dialog opens and reused while the
// input is unchanged, so a retry of the same act replays the recorded result.
// Changed input is a different act and takes a fresh ID (invariant 64).
function useActRequestId() {
  const [state, setState] = useState(() => ({ id: crypto.randomUUID(), input: '' }))
  return (input: unknown) => {
    const key = JSON.stringify(input)
    if (state.input === '' || state.input === key) {
      if (state.input === '') setState({ id: state.id, input: key })
      return state.id
    }
    const next = { id: crypto.randomUUID(), input: key }
    setState(next)
    return next.id
  }
}

// A plan act's mutation. The workspace is captured when the act starts: a
// success refreshes detail, activity, next action, and inbox there, and a 409
// refetches detail there. A 400, 403, or 409 is the server's answer and is
// never retried; only a network failure, which carries no answer, retries once.
function usePlanActMutation<T>(
  item: ActivityItem,
  act: (workspace: string, input: T) => Promise<unknown>,
  onDone: () => void,
) {
  const queryClient = useQueryClient()
  const taskId = item.task.id
  // The act's workspace travels in its variables, captured when the act
  // starts, so a workspace switch mid-request cannot redirect its refresh.
  return useMutation({
    mutationFn: ({ workspace, input }: { workspace: string; input: T }) => act(workspace, input),
    retry: (count, error) => error instanceof TypeError && count < 1,
    onSuccess: (_result, variables) => {
      for (const queryKey of [
        taskDetailQueryKey(variables.workspace, taskId),
        ['activity', variables.workspace],
        nextActionQueryKey(variables.workspace, taskId),
        nextActionsQueryKey(variables.workspace),
      ]) {
        void queryClient.invalidateQueries({ queryKey })
      }
      onDone()
    },
    onError: (error, variables) => {
      if (error instanceof PlanActError && error.status === 409) {
        void queryClient.invalidateQueries({ queryKey: taskDetailQueryKey(variables.workspace, taskId) })
      }
    },
  })
}

function ChangePlanStateDialog({
  item,
  targets,
  onClose,
}: {
  item: ActivityItem
  targets: PlanStateTarget[]
  onClose: () => void
}) {
  const id = useId()
  const { workspace } = useWorkspaceSelection()
  const [target, setTarget] = useState<PlanStateTarget | ''>(targets.length === 1 ? targets[0] : '')
  const [reason, setReason] = useState('')
  const requestIdFor = useActRequestId()
  const mutation = usePlanActMutation(
    item,
    (actWorkspace, input: Parameters<typeof changeTaskPlanState>[2]) =>
      changeTaskPlanState(actWorkspace, item.task.id, input),
    onClose,
  )
  const disabled = mutation.isPending || !target || !reason.trim()
  return (
    <Dialog
      label="Change plan state"
      onClose={() => {
        if (!mutation.isPending) onClose()
      }}
    >
      <form
        className="space-y-4 p-5"
        onSubmit={(event) => {
          event.preventDefault()
          if (disabled || !target) return
          const body = { plan_state: target, reason: reason.trim() }
          mutation.mutate({ workspace, input: { ...body, request_id: requestIdFor(body) } })
        }}
      >
        <h2 className="text-lg font-semibold">Change plan state</h2>
        <p className="text-sm text-muted">
          Now: {planStateLabels[item.task.plan_state] ?? item.task.plan_state}. Earlier plan versions stay readable.
        </p>
        <fieldset className="space-y-1.5">
          <legend className="text-sm font-medium">New plan state</legend>
          {targets.map((entry) => (
            <label key={entry} className="flex items-center gap-2 text-sm">
              <input
                type="radio"
                name={`${id}-target`}
                value={entry}
                checked={target === entry}
                onChange={() => setTarget(entry)}
              />
              {planStateLabels[entry]}
            </label>
          ))}
        </fieldset>
        <div>
          <label htmlFor={`${id}-reason`} className="text-sm font-medium">
            Reason (required)
          </label>
          <Textarea
            id={`${id}-reason`}
            required
            value={reason}
            onChange={(event) => setReason(event.target.value)}
            className="min-h-16 text-sm"
          />
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
            {mutation.isPending ? 'Changing…' : 'Change plan state'}
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

function AttachPlanDialog({ item, onClose }: { item: ActivityItem; onClose: () => void }) {
  const id = useId()
  const { workspace } = useWorkspaceSelection()
  const [plan, setPlan] = useState('')
  const [baseCommit, setBaseCommit] = useState('')
  const [reason, setReason] = useState('')
  const requestIdFor = useActRequestId()
  const mutation = usePlanActMutation(
    item,
    (actWorkspace, input: Parameters<typeof attachTaskPlan>[2]) => attachTaskPlan(actWorkspace, item.task.id, input),
    onClose,
  )
  const commit = baseCommit.trim()
  const commitError =
    commit && !fullCommitID.test(commit)
      ? 'Base commit must be a full commit ID of 40 lowercase hexadecimal characters.'
      : ''
  const disabled = mutation.isPending || !plan.trim() || !reason.trim() || commitError !== ''
  return (
    <Dialog
      label="Attach plan"
      className="max-w-2xl"
      onClose={() => {
        if (!mutation.isPending) onClose()
      }}
    >
      <form
        className="space-y-4 p-5"
        onSubmit={(event) => {
          event.preventDefault()
          if (disabled) return
          const body = { plan, ...(commit ? { base_commit: commit } : {}), reason: reason.trim() }
          mutation.mutate({ workspace, input: { ...body, request_id: requestIdFor(body) } })
        }}
      >
        <h2 className="text-lg font-semibold">Attach plan</h2>
        <p className="text-sm text-muted">
          The plan becomes the task's next plan version and waits for approval at the plan gate.
        </p>
        <div>
          <label htmlFor={`${id}-plan`} className="text-sm font-medium">
            Plan (Markdown)
          </label>
          <Textarea
            id={`${id}-plan`}
            required
            value={plan}
            onChange={(event) => setPlan(event.target.value)}
            className="min-h-40 font-mono text-xs"
          />
        </div>
        <div>
          <label htmlFor={`${id}-commit`} className="text-sm font-medium">
            Base commit (optional)
          </label>
          <Input
            id={`${id}-commit`}
            value={baseCommit}
            aria-invalid={commitError ? true : undefined}
            onChange={(event) => setBaseCommit(event.target.value)}
            className="font-mono"
            placeholder="40-character commit ID"
          />
          {commitError && (
            <p role="alert" className="mt-1 text-xs text-failure">
              {commitError}
            </p>
          )}
        </div>
        <div>
          <label htmlFor={`${id}-reason`} className="text-sm font-medium">
            Reason (required)
          </label>
          <Textarea
            id={`${id}-reason`}
            required
            value={reason}
            onChange={(event) => setReason(event.target.value)}
            className="min-h-16 text-sm"
          />
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
            {mutation.isPending ? 'Attaching…' : 'Attach plan'}
          </Button>
        </div>
      </form>
    </Dialog>
  )
}
