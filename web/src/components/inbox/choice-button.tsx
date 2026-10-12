import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { GateMutationError, postNextActionChoice } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { type MutationWorkspace, taskDetailQueryKey } from '../../lib/query-keys'
import type { NextActionChoice } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Textarea } from '../ui/input'
import { choiceBodies } from './choice-bodies'
import { nextActionQueryKey, nextActionsQueryKey } from './use-next-actions'

// The server's choices for one next action, rendered generically on the inbox,
// the task-detail next-action card and the human gate (AC-1.4, AC-1.7, AC-1.8).
// Every server choice renders (DEC-61): its label, its effect and its endpoint.
// A choice the caller may not take, or whose input this client cannot build,
// renders disabled with the reason beside it rather than disappearing.
export function NextActionChoices({
  taskId,
  choices,
  onActed,
}: {
  taskId: string
  choices: NextActionChoice[]
  onActed?: (choice: NextActionChoice) => void
}) {
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const [composing, setComposing] = useState<NextActionChoice | null>(null)
  const [input, setInput] = useState('')
  // isPending lands a render after the click, so a double click would send
  // twice; the ref closes that window.
  const inFlight = useRef(false)

  const mutation = useMutation({
    onMutate: (): MutationWorkspace => ({ workspace }),
    mutationFn: async ({ choice, text }: { choice: NextActionChoice; text: string }) => {
      const contract = choiceBodies[choice.id]
      const requestId = crypto.randomUUID()
      await postNextActionChoice(
        workspace,
        choice,
        contract.body(text, requestId),
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

  if (choices.length === 0) return null
  const contract = composing ? choiceBodies[composing.id] : undefined
  return (
    <div data-next-action-choices className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        {choices.map((choice, index) => {
          const reason = !choice.allowed
            ? `Needs the ${choice.capability.replaceAll('_', ' ')} capability.`
            : !choiceBodies[choice.id]
              ? 'Take this from the CLI or MCP; it needs input this page cannot send yet.'
              : ''
          return (
            <span key={choice.id} className="inline-flex flex-col gap-0.5">
              <Button
                size="sm"
                variant={index === 0 && reason === '' ? 'default' : 'secondary'}
                title={`${choice.effect} (${choice.method} ${choice.path})`}
                data-choice={choice.id}
                data-endpoint={`${choice.method} ${choice.path}`}
                aria-expanded={choiceBodies[choice.id]?.input ? composing?.id === choice.id : undefined}
                disabled={mutation.isPending || reason !== ''}
                onClick={() => {
                  mutation.reset()
                  if (choiceBodies[choice.id].input) {
                    setComposing((current) => (current?.id === choice.id ? null : choice))
                    setInput('')
                    return
                  }
                  send(choice, '')
                }}
              >
                {mutation.isPending && mutation.variables?.choice.id === choice.id ? 'Recording…' : choice.label}
              </Button>
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
    </div>
  )
}
