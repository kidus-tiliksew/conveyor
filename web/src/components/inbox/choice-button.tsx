import { useMutation, useQueryClient } from '@tanstack/react-query'
import { type ReactNode, useEffect, useId, useRef, useState } from 'react'
import { GateMutationError, postNextActionChoice } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { type MutationWorkspace, taskDetailQueryKey } from '../../lib/query-keys'
import type { TaskRouteVariant } from '../../lib/task-route'
import type { DispositionAnswer, NextActionChoice, NextActionDetails } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { TaskRestartControl } from '../task/task-restart-dialog'
import { useTaskDetail } from '../task/use-task-detail'
import { Button } from '../ui/button'
import { Textarea } from '../ui/input'
import { choiceBody } from './choice-bodies'
import { DispositionList } from './disposition-list'
import { Kbd, useShortcutChoice } from './inbox-keyboard'
import { nextActionQueryKey, nextActionsQueryKey } from './use-next-actions'

// Every choice button shares one height and one class, so a disabled choice
// never changes the row's alignment; `choiceButtonClass` is exported for the
// merge-at-gate button, which renders in the same row.
export const choiceButtonClass = 'h-auto min-h-8 max-w-full py-1.5 text-left whitespace-normal [overflow-wrap:anywhere]'

// A disabled choice's reason as the reasons block under the row prints it.
export type ChoiceReason = { id: string; label: string; text: string; choice?: string }

// The server's choices for one next action, rendered generically on the inbox,
// the task-detail next-action card and the human gate (AC-1.4, AC-1.7, AC-1.8).
// Every server choice renders (DEC-61): its label, its effect and its endpoint.
// A choice the caller may not take, or whose input this client cannot build,
// renders disabled with its reason. The reasons print as one block under the
// row, one line per disabled choice naming it, and each disabled button points
// at its line through aria-describedby; a reason never sits under its own
// button, where it would change the row's height and alignment.
// Every choice shares one wrapping row with exactly one primary button
// (component-next-action): `primaryId` names it when the caller
// knows the decision's primary, as GateChoices does, and otherwise the first
// choice that renders enabled takes it. `leading` renders first in the row and
// `leadingReason` joins the reasons block for it. `status` renders above the
// row. The `grid` layout, which only the merge-approval gate uses, stacks on a
// phone: `leading` on its own full-width line with the other choices in two
// equal-width columns below it. From `sm` up it is the same one row as every
// other decision (component-web-task-surfaces, Next-action card).
// The `footer` layout is the Inbox detail's one action row: buttons at their
// natural width from md up, filling the line below it.
export function NextActionChoices({
  taskId,
  choices,
  details,
  onActed,
  primaryId,
  cautionId,
  leading,
  leadingReason,
  status,
  layout = 'row',
  routeVariant = 'sheet',
  onRestarted,
}: {
  taskId: string
  choices: NextActionChoice[]
  details?: NextActionDetails
  onActed?: (choice: NextActionChoice) => void
  primaryId?: string | null
  // The choice that overrides a warning stated above the row. It keeps its role
  // but wears a caution outline, so it does not contradict that warning.
  cautionId?: string
  leading?: ReactNode
  leadingReason?: ChoiceReason
  status?: ReactNode
  layout?: 'row' | 'grid' | 'footer'
  routeVariant?: TaskRouteVariant
  onRestarted?: () => void
}) {
  const reasonPrefix = useId()
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const [composing, setComposing] = useState<NextActionChoice | null>(null)
  const [input, setInput] = useState('')
  // The operator's answer for each attempt of a disposition set, by run ID.
  const [answers, setAnswers] = useState<Record<string, DispositionAnswer>>({})
  const [restarting, setRestarting] = useState(false)
  // A disposition set the server states again with other attempts, or another
  // task, clears every answer, so no answer carries over to an attempt the
  // operator has not seen (component-next-action invariant 15).
  const dispositionKey = `${taskId}\u0000${(details?.dispositions ?? []).map((entry) => `${entry.context_id}/${entry.run_id}/${entry.input_digest}/${(entry.operation_ids ?? []).join(',')}`).join('|')}`
  useEffect(() => {
    void dispositionKey
    setAnswers({})
  }, [dispositionKey])
  // A choice low on the page opens its input past the fold, confirm included.
  const composerRef = useRef<HTMLDivElement>(null)
  const composingId = composing?.id
  useEffect(() => {
    if (composingId) composerRef.current?.scrollIntoView({ block: 'nearest' })
  }, [composingId])
  // isPending lands a render after the click, so a double click would send
  // twice; the ref closes that window.
  const inFlight = useRef(false)
  // The Inbox binds `c` to one choice of the selected decision (AC-2.11);
  // elsewhere this is null and no choice carries a key.
  const shortcut = useShortcutChoice()

  const mutation = useMutation({
    onMutate: (): MutationWorkspace => ({ workspace }),
    mutationFn: async ({
      choice,
      text,
      chosen,
    }: {
      choice: NextActionChoice
      text: string
      chosen: Record<string, DispositionAnswer>
    }) => {
      const contract = choiceBody(choice.id)
      if (!contract) throw new Error('This choice cannot be sent from this page.')
      const requestId = crypto.randomUUID()
      await postNextActionChoice(
        workspace,
        choice,
        contract.body(text, requestId, details, chosen),
        contract.idempotent ? requestId : undefined,
      )
    },
    onSuccess: (_result, { choice }) => {
      setComposing(null)
      setInput('')
      setAnswers({})
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
        client.invalidateQueries({ queryKey: ['pending-proposals', acted] }),
        client.invalidateQueries({ queryKey: taskDetailQueryKey(acted, taskId) }),
      ])
    },
  })

  const send = (choice: NextActionChoice, text: string) => {
    if (inFlight.current) return
    inFlight.current = true
    mutation.mutate({ choice, text, chosen: answers })
  }

  if (choices.length === 0 && !leading && !status) return null
  const contract = composing ? choiceBody(composing.id) : undefined
  const dispositionEntries = contract?.composer === 'dispositions' ? (details?.dispositions ?? []) : []
  const unanswered =
    contract?.composer === 'dispositions' &&
    (dispositionEntries.length === 0 || dispositionEntries.some((entry) => !answers[entry.run_id]))
  const reasons = choices.map((choice) => {
    const body = choiceBody(choice.id)
    return !choice.allowed
      ? `Needs the ${choice.capability.replaceAll('_', ' ')} capability.`
      : !body
        ? 'This page cannot send this choice.'
        : (body.unavailable?.(details) ?? '')
  })
  const primary = primaryId !== undefined ? primaryId : (choices.find((_, index) => reasons[index] === '')?.id ?? null)
  const grid = layout === 'grid'
  const footer = layout === 'footer'
  const reasonId = (index: number) => `${reasonPrefix}-reason-${index}`
  const reasonLines: ChoiceReason[] = [
    ...(leadingReason ? [leadingReason] : []),
    ...choices.flatMap((choice, index) =>
      reasons[index] ? [{ id: reasonId(index), label: choice.label, text: reasons[index], choice: choice.id }] : [],
    ),
  ]
  return (
    <div data-next-action-choices className="min-w-0 space-y-2">
      {status}
      <div
        data-choice-row={layout}
        className={
          grid
            ? 'grid w-full min-w-0 grid-cols-2 items-stretch gap-2 sm:flex sm:flex-wrap'
            : 'flex min-w-0 flex-wrap items-stretch gap-2'
        }
      >
        {leading && (grid ? <div className="col-span-2 flex min-w-0 sm:contents">{leading}</div> : leading)}
        {choices.map((choice, index) => {
          const body = choiceBody(choice.id)
          const reason = reasons[index]
          const keyed = shortcut === choice.id && reason === ''
          return (
            // A long server label (a task-context choice names its target)
            // wraps inside the button rather than overflowing a phone-width
            // card; the label and endpoint stay whole. The wrapper stretches
            // to the line's height, so every button on a line is equally tall.
            <span
              key={choice.id}
              className={`inline-flex max-w-full min-w-0 items-stretch gap-1.5${grid ? ' w-full sm:w-auto' : ''}${footer ? ' max-md:flex-1 max-md:basis-[calc(50%-0.25rem)]' : ''}`}
            >
              <Button
                size="sm"
                className={cn(
                  grid
                    ? `${choiceButtonClass} flex-1 justify-center`
                    : footer
                      ? `${choiceButtonClass} max-md:flex-1 max-md:justify-center`
                      : choiceButtonClass,
                  choice.id === cautionId &&
                    'border-attention/50 text-attention hover:border-attention hover:bg-attention-soft',
                )}
                variant={
                  choice.id === 'reject'
                    ? 'ghost'
                    : choice.id === cautionId
                      ? 'outline'
                      : choice.id === 'close'
                        ? 'destructive'
                        : choice.id === primary && reason === ''
                          ? 'default'
                          : 'secondary'
                }
                title={`${choice.effect} (${choice.method} ${choice.path})`}
                data-choice={choice.id}
                data-shortcut={keyed ? 'c' : undefined}
                data-endpoint={`${choice.method} ${choice.path}`}
                aria-expanded={body?.input ? composing?.id === choice.id : undefined}
                aria-describedby={reason ? reasonId(index) : undefined}
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
                    setAnswers({})
                    return
                  }
                  send(choice, '')
                }}
              >
                {mutation.isPending && mutation.variables?.choice.id === choice.id ? 'Recording…' : choice.label}
              </Button>
              {keyed && <Kbd className="self-center">c</Kbd>}
            </span>
          )
        })}
      </div>
      {reasonLines.length > 0 && (
        <ul data-choice-reasons className="space-y-0.5 text-xs text-muted">
          {reasonLines.map((line) => (
            <li key={line.id} id={line.id} data-choice-reason={line.choice}>
              {line.label}: {line.text}
            </li>
          ))}
        </ul>
      )}
      {composing && contract?.input && (
        <div ref={composerRef} className="space-y-2">
          <p className="text-xs text-muted">{composing.effect}</p>
          {contract.composer === 'dispositions' && (
            <DispositionList
              entries={dispositionEntries}
              answers={answers}
              disabled={mutation.isPending}
              onAnswer={(runId, answer) => setAnswers((current) => ({ ...current, [runId]: answer }))}
            />
          )}
          <Textarea
            autoFocus
            aria-label={contract.input.label}
            placeholder={contract.input.label}
            className="min-h-16 text-sm"
            value={input}
            onChange={(event) => setInput(event.target.value)}
          />
          <div className="flex justify-end gap-2">
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                setComposing(null)
                setAnswers({})
              }}
            >
              Cancel
            </Button>
            <Button
              size="sm"
              variant={composing.id === 'reject' || composing.id === 'close' ? 'danger' : 'default'}
              disabled={mutation.isPending || (contract.input.required && !input.trim()) || unanswered}
              onClick={() => send(composing, input)}
            >
              {composing.label.replace(/…$/, '')}
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
