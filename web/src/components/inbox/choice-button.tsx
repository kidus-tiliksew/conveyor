import { useMutation, useQueryClient } from '@tanstack/react-query'
import { type ReactNode, useRef, useState } from 'react'
import { GateMutationError, postNextActionChoice } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { type MutationWorkspace, taskDetailQueryKey } from '../../lib/query-keys'
import type { TaskRouteVariant } from '../../lib/task-route'
import type { NextActionChoice, NextActionDetails } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { TaskRestartControl } from '../task/task-restart-dialog'
import { useTaskDetail } from '../task/use-task-detail'
import { Button } from '../ui/button'
import { Textarea } from '../ui/input'
import { choiceBody } from './choice-bodies'
import { Kbd, useShortcutChoice } from './inbox-keyboard'
import { nextActionQueryKey, nextActionsQueryKey } from './use-next-actions'

// The server's choices for one next action, rendered generically on the inbox,
// the task-detail next-action card and the human gate (AC-1.4, AC-1.7, AC-1.8).
// Every server choice renders (DEC-61): its label, its effect and its endpoint.
// A choice the caller may not take, or whose input this client cannot build,
// renders disabled with the reason beside it rather than disappearing.
// Every choice shares one wrapping row with exactly one primary button
// (feature-operator-next-action NA-6): `primaryId` names it when the caller
// knows the decision's primary, as GateChoices does, and otherwise the first
// choice that renders enabled takes it. `leading` renders first in the row.
export function NextActionChoices({
  taskId,
  choices,
  details,
  onActed,
  primaryId,
  leading,
  routeVariant = 'sheet',
  onRestarted,
}: {
  taskId: string
  choices: NextActionChoice[]
  details?: NextActionDetails
  onActed?: (choice: NextActionChoice) => void
  primaryId?: string | null
  leading?: ReactNode
  routeVariant?: TaskRouteVariant
  onRestarted?: () => void
}) {
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const [composing, setComposing] = useState<NextActionChoice | null>(null)
  const [input, setInput] = useState('')
  const [restarting, setRestarting] = useState(false)
  // isPending lands a render after the click, so a double click would send
  // twice; the ref closes that window.
  const inFlight = useRef(false)
  // The Inbox binds `c` to one choice of the selected decision (AC-2.11);
  // elsewhere this is null and no choice carries a key.
  const shortcut = useShortcutChoice()

  const mutation = useMutation({
    onMutate: (): MutationWorkspace => ({ workspace }),
    mutationFn: async ({ choice, text }: { choice: NextActionChoice; text: string }) => {
      const contract = choiceBody(choice.id)
      if (!contract) throw new Error('This choice cannot be sent from this page.')
      const requestId = crypto.randomUUID()
      await postNextActionChoice(
        workspace,
        choice,
        contract.body(text, requestId, details),
        contract.idempotent ? requestId : undefined,
      )
    },
    onSuccess: (_result, { choice }) => {
      setComposing(null)
      setInput('')
      onActed?.(choice)
    },
    onSettled: async (_result, error, _variables, context) => {
      inFlight.current = false
      // A conflict means the task moved under the operator: refetch so the
      // current state renders beside the refusal. Any other failure leaves the
      // state as it was, so nothing is refetched.
      if (error != null && !(error instanceof GateMutationError && error.status === 409)) return
      const acted = context?.workspace ?? workspace
      await Promise.all([
        client.invalidateQueries({ queryKey: nextActionsQueryKey(acted) }),
        client.invalidateQueries({ queryKey: nextActionQueryKey(acted, taskId) }),
        client.invalidateQueries({ queryKey: ['activity'] }),
        client.invalidateQueries({ queryKey: ['task-operations', acted] }),
        client.invalidateQueries({ queryKey: ['pending-proposals', acted] }),
        client.invalidateQueries({ queryKey: taskDetailQueryKey(acted, taskId) }),
      ])
    },
  })

  const send = (choice: NextActionChoice, text: string) => {
    if (inFlight.current) return
    inFlight.current = true
    mutation.mutate({ choice, text })
  }

  if (choices.length === 0 && !leading) return null
  const contract = composing ? choiceBody(composing.id) : undefined
  const reasons = choices.map((choice) => {
    const body = choiceBody(choice.id)
    return !choice.allowed
      ? `Needs the ${choice.capability.replaceAll('_', ' ')} capability.`
      : !body
        ? 'This page cannot send this choice.'
        : (body.unavailable?.(details) ?? '')
  })
  const primary = primaryId !== undefined ? primaryId : (choices.find((_, index) => reasons[index] === '')?.id ?? null)
  return (
    <div data-next-action-choices className="min-w-0 space-y-2">
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        {leading}
        {choices.map((choice, index) => {
          const body = choiceBody(choice.id)
          const reason = reasons[index]
          const keyed = shortcut === choice.id && reason === ''
          return (
            // A long server label (a task-context choice names its target)
            // wraps inside the button rather than overflowing a phone-width
            // card; the label and endpoint stay whole.
            <span key={choice.id} className="inline-flex max-w-full min-w-0 flex-col gap-0.5">
              <span className="inline-flex max-w-full min-w-0 items-center gap-1.5">
                <Button
                  size="sm"
                  className="h-auto min-h-8 max-w-full py-1.5 text-left whitespace-normal [overflow-wrap:anywhere]"
                  variant={
                    choice.id === 'reject' ? 'ghost' : choice.id === primary && reason === '' ? 'default' : 'secondary'
                  }
                  title={`${choice.effect} (${choice.method} ${choice.path})`}
                  data-choice={choice.id}
                  data-shortcut={keyed ? 'c' : undefined}
                  data-endpoint={`${choice.method} ${choice.path}`}
                  aria-expanded={body?.input ? composing?.id === choice.id : undefined}
                  disabled={mutation.isPending || reason !== ''}
                  onClick={() => {
                    mutation.reset()
                    if (body?.dialog === 'restart') {
                      setComposing(null)
                      setRestarting(true)
                      return
                    }
                    if (body?.input) {
                      setComposing((current) => (current?.id === choice.id ? null : choice))
                      setInput('')
                      return
                    }
                    send(choice, '')
                  }}
                >
                  {mutation.isPending && mutation.variables?.choice.id === choice.id ? 'Recording…' : choice.label}
                </Button>
                {keyed && <Kbd>c</Kbd>}
              </span>
              {reason && <span className="text-[11px] text-muted">{reason}</span>}
            </span>
          )
        })}
      </div>
      {composing && contract?.input && (
        <div className="space-y-2">
          <p className="text-xs text-muted">{composing.effect}</p>
          <Textarea
            autoFocus
            aria-label={contract.input.label}
            placeholder={contract.input.label}
            className="min-h-16 text-sm"
            value={input}
            onChange={(event) => setInput(event.target.value)}
          />
          <div className="flex justify-end gap-2">
            <Button variant="ghost" size="sm" onClick={() => setComposing(null)}>
              Cancel
            </Button>
            <Button
              size="sm"
              disabled={mutation.isPending || (contract.input.required && !input.trim())}
              onClick={() => send(composing, input)}
            >
              {composing.label}
            </Button>
          </div>
        </div>
      )}
      {mutation.error != null && (
        <p className="text-xs text-failure">{errorMessage(mutation.error, 'Could not record this choice.')}</p>
      )}
      {restarting && (
        <RestartChoice
          taskId={taskId}
          routeVariant={routeVariant}
          onClose={() => setRestarting(false)}
          onRestarted={
            onRestarted &&
            (async () => {
              const acted = workspace
              await Promise.all([
                client.invalidateQueries({ queryKey: nextActionsQueryKey(acted) }),
                client.invalidateQueries({ queryKey: nextActionQueryKey(acted, taskId) }),
              ])
              onRestarted()
            })
          }
        />
      )}
    </div>
  )
}

// A `start_over` choice opens the task's restart dialog over the task detail
// read, so the act keeps the dialog's preview, validation and request ID
// (component-web-task-surfaces, Task header). Without `onRestarted` a success
// opens the successor task, as the task header does.
function RestartChoice({
  taskId,
  routeVariant,
  onClose,
  onRestarted,
}: {
  taskId: string
  routeVariant: TaskRouteVariant
  onClose: () => void
  onRestarted?: () => void
}) {
  const { data: item, error, refetch, isFetching } = useTaskDetail(taskId)
  if (item) {
    return (
      <TaskRestartControl
        item={item}
        variant={routeVariant}
        onClose={onClose}
        onRestarted={onRestarted && (() => onRestarted())}
      />
    )
  }
  if (error != null) {
    return (
      <div className="flex flex-wrap items-center gap-2 text-xs">
        <span className="text-failure">{errorMessage(error, 'Could not load this task.')}</span>
        <Button size="sm" variant="secondary" disabled={isFetching} onClick={() => void refetch()}>
          Retry
        </Button>
        <Button size="sm" variant="ghost" onClick={onClose}>
          Cancel
        </Button>
      </div>
    )
  }
  return (
    <p role="status" className="text-xs text-muted">
      Loading the task…
    </p>
  )
}
