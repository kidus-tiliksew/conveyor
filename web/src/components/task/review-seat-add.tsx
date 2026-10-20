import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { type FormEvent, useId, useRef, useState } from 'react'
import { addReviewSeats, ReviewSeatAdditionError } from '../../lib/api'
import { type MutationWorkspace, taskDetailQueryKey } from '../../lib/query-keys'
import type { ReviewSeatAdditionResult } from '../../lib/types'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Input, Textarea } from '../ui/input'

// The route's `add` bound, the INT column (feature-review-seat-minimum RS-5).
const maxSeatAddition = 2_147_483_647
// Refusals that mean the panel shows a stale round; the detail is refreshed.
const staleRoundCodes = new Set(['review_round_not_open', 'review_round_settled'])

interface SeatAdditionInput {
  request_id: string
  reason: string
  add: number
  workspace: string
}

// The operator's "Add reviewer seat" control on an open review round
// (feature-review-seat-minimum RS-7; req-review-gates-evidence AC-2.5,
// AC-2.6). The served operate_gates capability decides visibility; the HTTP
// command stays authoritative when settlement races the click. A refusal
// stays visible after the refresh it triggers closes the round to additions.
export function ReviewSeatAdd({ taskId, round, open }: { taskId: string; round: number; open: boolean }) {
  const canOperateGates = useWorkspaceCapability('operate_gates')
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const countId = useId()
  const reasonId = useId()
  const [formOpen, setFormOpen] = useState(false)
  const [count, setCount] = useState('1')
  const [reason, setReason] = useState('')
  const [invalid, setInvalid] = useState<string>()
  const [refusal, setRefusal] = useState<string>()
  const [added, setAdded] = useState<ReviewSeatAdditionResult>()
  // One request ID per {add, reason} attempt: a retry of unchanged inputs
  // reuses it, so an ambiguous transport failure never adds seats twice.
  const attempt = useRef<{ id: string; add: number; reason: string }>(undefined)
  const refresh = (started: MutationWorkspace | undefined) => {
    if (started) void queryClient.invalidateQueries({ queryKey: taskDetailQueryKey(started.workspace, taskId) })
    void queryClient.invalidateQueries({ queryKey: ['activity'] })
  }
  const mutation = useMutation({
    mutationFn: ({ workspace: target, ...input }: SeatAdditionInput) => addReviewSeats(taskId, target, input),
    onMutate: (input): MutationWorkspace => ({ workspace: input.workspace }),
    onSuccess: (result, _input, started) => {
      attempt.current = undefined
      setAdded(result)
      setFormOpen(false)
      setCount('1')
      setReason('')
      refresh(started)
    },
    onError: (error, _input, started) => {
      if (error instanceof ReviewSeatAdditionError) {
        setRefusal(error.message)
        if (error.code && staleRoundCodes.has(error.code)) refresh(started)
        return
      }
      setRefusal(`Conveyor could not be reached: ${error.message}. Retrying sends the same request.`)
    },
  })

  if (!canOperateGates || (!open && !refusal)) return null

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (mutation.isPending) return
    const text = count.trim()
    const add = Number(text)
    if (!/^\d+$/.test(text) || !Number.isSafeInteger(add) || add < 1 || add > maxSeatAddition) {
      setInvalid(`Enter a whole number of seats from 1 to ${maxSeatAddition}.`)
      return
    }
    const trimmed = reason.trim()
    if (!trimmed) {
      setInvalid('Give a reason for adding seats.')
      return
    }
    setInvalid(undefined)
    setRefusal(undefined)
    const prior = attempt.current
    const id = prior && prior.add === add && prior.reason === trimmed ? prior.id : crypto.randomUUID()
    attempt.current = { id, add, reason: trimmed }
    mutation.mutate({ request_id: id, reason: trimmed, add, workspace })
  }

  return (
    <div className="space-y-2 border-t border-border px-4 py-2.5">
      {open && !formOpen && (
        <Button
          variant="secondary"
          size="sm"
          onClick={() => {
            setFormOpen(true)
            setAdded(undefined)
          }}
        >
          <Plus />
          Add reviewer seat
        </Button>
      )}
      {open && formOpen && (
        <form className="space-y-2" aria-label={`Add reviewer seats to round ${round}`} onSubmit={submit} noValidate>
          <div className="flex flex-wrap items-end gap-3">
            <div className="space-y-1">
              <label htmlFor={countId} className="block text-xs font-medium text-muted">
                Seats to add
              </label>
              <Input
                id={countId}
                type="number"
                inputMode="numeric"
                min={1}
                max={maxSeatAddition}
                step={1}
                value={count}
                onChange={(event) => setCount(event.target.value)}
                className="w-24"
              />
            </div>
          </div>
          <div className="space-y-1">
            <label htmlFor={reasonId} className="block text-xs font-medium text-muted">
              Reason
            </label>
            <Textarea
              id={reasonId}
              required
              rows={2}
              value={reason}
              onChange={(event) => setReason(event.target.value)}
              placeholder="Why does this round need more reviewers?"
            />
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" size="sm" disabled={mutation.isPending}>
              {mutation.isPending ? 'Adding…' : 'Add seats'}
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              disabled={mutation.isPending}
              onClick={() => {
                setFormOpen(false)
                setInvalid(undefined)
              }}
            >
              Cancel
            </Button>
          </div>
        </form>
      )}
      {invalid && (
        <p role="alert" className="text-xs text-failure">
          {invalid}
        </p>
      )}
      {refusal && (
        <p role="alert" className="text-xs text-failure">
          {refusal}
        </p>
      )}
      {added && (
        <p role="status" className="text-xs text-positive">
          Round {added.review_round} now has {added.seat_count} {added.seat_count === 1 ? 'seat' : 'seats'}.
        </p>
      )}
    </div>
  )
}
