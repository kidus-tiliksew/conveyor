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
  const close = () => setOpen(null)
  const acts = item.plan_acts
  if (!canOperate || !acts) return null
  const targets = planStateTargets.filter((entry) => acts[entry.flag] === true).map((entry) => entry.target)
  const canAttach = acts.attach === true
  // An open dialog stays mounted when a refetch withdraws its act, so a 409's
  // message stays in place (invariant 64). The dialog follows the current
  // plan_acts and cannot submit an act or target the server withdrew.
  if (targets.length === 0 && !canAttach && open === null) return null
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
      {open === 'state' && <ChangePlanStateDialog item={item} targets={targets} onClose={close} />}
      {open === 'attach' && <AttachPlanDialog item={item} admitted={canAttach} onClose={close} />}
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
  // After a recorded act: refreshing detail, or a refresh that failed.
  const [refresh, setRefresh] = useState<{ workspace: string; failed: boolean } | null>(null)
  // The act closes only once refreshed detail lands, so the section never
  // offers the acts of the state the act just left. A failed refresh keeps the
  // dialog open, with submission blocked and a read-only retry.
  const refreshDetail = async (workspace: string) => {
    setRefresh({ workspace, failed: false })
    try {
      await queryClient.invalidateQueries({ queryKey: taskDetailQueryKey(workspace, taskId) }, { throwOnError: true })
    } catch {
      setRefresh({ workspace, failed: true })
      return
    }
    onDone()
  }
  // The act's workspace travels in its variables, captured when the act
  // starts, so a workspace switch mid-request cannot redirect its refresh.
  const mutation = useMutation({
    mutationFn: ({ workspace, input }: { workspace: string; input: T }) => act(workspace, input),
    retry: (count, error) => error instanceof TypeError && count < 1,
    onSuccess: async (_result, variables) => {
      for (const queryKey of [
        ['activity', variables.workspace],
        nextActionQueryKey(variables.workspace, taskId),
        nextActionsQueryKey(variables.workspace),
      ]) {
        void queryClient.invalidateQueries({ queryKey })
      }
      await refreshDetail(variables.workspace)
    },
    onError: (error, variables) => {
      if (error instanceof PlanActError && error.status === 409) {
        void queryClient.invalidateQueries({ queryKey: taskDetailQueryKey(variables.workspace, taskId) })
      }
    },
  })
  return {
    mutation,
    // A recorded act is never submitted again from its dialog.
    recorded: mutation.isSuccess,
    refreshing: refresh !== null && !refresh.failed,
    refreshFailed: refresh?.failed === true,
    retryRefresh: () => {
      if (refresh) void refreshDetail(refresh.workspace)
    },
  }
}

// What a plan act's dialog says after its submission: the server's refusal,
// or a recorded act whose detail refresh failed, with a read-only retry.
function PlanActOutcome({
  act,
}: {
  act: { mutation: { error: Error | null }; refreshFailed: boolean; retryRefresh: () => void }
}) {
  if (act.mutation.error)
    return (
      <p role="alert" className="text-sm text-failure">
        {errorMessage(act.mutation.error)}
      </p>
    )
  if (!act.refreshFailed) return null
  return (
    <div role="alert" className="flex flex-wrap items-center justify-between gap-2 text-sm text-failure">
      <span>The change was recorded, but the task could not refresh.</span>
      <Button type="button" variant="secondary" size="sm" onClick={act.retryRefresh}>
        Retry refresh
      </Button>
    </div>
  )
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
  const [chosen, setChosen] = useState<PlanStateTarget | ''>(targets.length === 1 ? targets[0] : '')
  // A target the server withdrew while the dialog was open is no longer a
  // choice, and nothing replaces it without the operator's pick.
  const target = chosen && targets.includes(chosen) ? chosen : ''
  const [reason, setReason] = useState('')
  const requestIdFor = useActRequestId()
  const act = usePlanActMutation(
    item,
    (actWorkspace, input: Parameters<typeof changeTaskPlanState>[2]) =>
      changeTaskPlanState(actWorkspace, item.task.id, input),
    onClose,
  )
  const { mutation } = act
  const busy = mutation.isPending || act.refreshing
  const disabled = busy || act.recorded || !target || !reason.trim()
  return (
    <Dialog
      label="Change plan state"
      onClose={() => {
        if (!busy) onClose()
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
                onChange={() => setChosen(entry)}
              />
              {planStateLabels[entry]}
            </label>
          ))}
        </fieldset>
        {targets.length === 0 && (
          <p className="text-sm text-muted">The server no longer accepts a plan-state change for this task.</p>
        )}
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
        <PlanActOutcome act={act} />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
            {act.recorded ? 'Close' : 'Cancel'}
          </Button>
          <Button type="submit" disabled={disabled}>
            {busy ? 'Changing…' : 'Change plan state'}
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

function AttachPlanDialog({ item, admitted, onClose }: { item: ActivityItem; admitted: boolean; onClose: () => void }) {
  const id = useId()
  const { workspace } = useWorkspaceSelection()
  const [plan, setPlan] = useState('')
  const [baseCommit, setBaseCommit] = useState('')
  const [reason, setReason] = useState('')
  const requestIdFor = useActRequestId()
  const act = usePlanActMutation(
    item,
    (actWorkspace, input: Parameters<typeof attachTaskPlan>[2]) => attachTaskPlan(actWorkspace, item.task.id, input),
    onClose,
  )
  const { mutation } = act
  const busy = mutation.isPending || act.refreshing
  const commit = baseCommit.trim()
  const commitError =
    commit && !fullCommitID.test(commit)
      ? 'Base commit must be a full commit ID of 40 lowercase hexadecimal characters.'
      : ''
  const disabled = busy || act.recorded || !admitted || !plan.trim() || !reason.trim() || commitError !== ''
  return (
    <Dialog
      label="Attach plan"
      className="max-w-2xl"
      onClose={() => {
        if (!busy) onClose()
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
        {!admitted && (
          <p className="text-sm text-muted">The server no longer accepts an attached plan for this task.</p>
        )}
        <PlanActOutcome act={act} />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
            {act.recorded ? 'Close' : 'Cancel'}
          </Button>
          <Button type="submit" disabled={disabled}>
            {busy ? 'Attaching…' : 'Attach plan'}
          </Button>
        </div>
      </form>
    </Dialog>
  )
}
