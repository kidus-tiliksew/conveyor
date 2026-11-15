import type { NextAction } from '../../lib/types'
import { fullTime, relativeTime } from '../../lib/utils'

// The deadline a next action states, absolute and relative. A next action
// without one says nothing rather than inventing a horizon.
export function NextActionDeadline({ deadline }: { deadline: string | null | undefined }) {
  if (!deadline) return null
  return (
    <p className="text-xs text-muted" data-next-action-deadline>
      Deadline <time dateTime={deadline}>{fullTime(deadline)}</time> ({relativeTime(deadline)})
    </p>
  )
}

// The line under a decision: when it is due. It prints once, wherever the
// choices are. The if-ignored sentence and the statement's identifiers stay
// off the dashboard (req-operator-next-action AC-1.9; component-next-action
// invariant 41).
export function NextActionNotes({ action }: { action: NextAction }) {
  return <NextActionDeadline deadline={action.deadline} />
}
