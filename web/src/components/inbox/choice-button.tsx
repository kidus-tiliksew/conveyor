import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { postNextActionChoice } from '../../lib/api'
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
// The button text is the choice's label and its hint the choice's effect; a
// choice the caller may not take, or whose body this client cannot build, is
// not rendered at all.
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
  const visible = choices.filter((choice) => choice.allowed && choiceBodies[choice.id])

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
    onSettled: async (_result, _error, _variables, context) => {
      inFlight.current = false
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

  if (visible.length === 0) return null
  const contract = composing ? choiceBodies[composing.id] : undefined
  return (
    <div data-next-action-choices className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        {visible.map((choice, index) => (
          <Button
            key={choice.id}
            size="sm"
            variant={index === 0 ? 'default' : 'secondary'}
            title={choice.effect}
            aria-expanded={choiceBodies[choice.id].input ? composing?.id === choice.id : undefined}
            disabled={mutation.isPending}
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
        ))}
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
