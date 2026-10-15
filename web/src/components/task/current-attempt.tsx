import { createContext, type ReactNode, useContext } from 'react'
import type { NextAction, NextActionAttempt, WorkOrder } from '../../lib/types'
import { absoluteTime, formatSeconds, relativeTime } from '../../lib/utils'
import { CopyButton } from '../ui/copy-button'

// The attempt the server states as current: the order the next action names
// and the attempt claimed on it (req-operator-next-action AC-7.2).
interface CurrentAttemptValue {
  orderId: string
  attempt: NextActionAttempt
}

const CurrentAttemptContext = createContext<CurrentAttemptValue | null>(null)

export function CurrentAttemptProvider({ action, children }: { action?: NextAction; children: ReactNode }) {
  const value =
    action?.current_order?.id && action.attempt ? { orderId: action.current_order.id, attempt: action.attempt } : null
  return <CurrentAttemptContext.Provider value={value}>{children}</CurrentAttemptContext.Provider>
}

// The activity read and the next-action read refresh independently, so an
// entry shows attempt facts only when its order is the one the next action
// names and, when both sides carry one, its attempt is the same attempt. A
// previous attempt's entry therefore never reads as current.
export function matchingAttempt(current: CurrentAttemptValue | null, order?: WorkOrder) {
  if (!current || !order || order.id !== current.orderId) return undefined
  const attemptId = current.attempt.attempt_id
  if (order.attempt_id && attemptId && order.attempt_id !== attemptId) return undefined
  return current.attempt
}

// The current attempt's facts on the timeline entry it belongs to: a job
// entry, a claimed order without a job, or a review seat. Only the server's
// fields render; the client never judges the lease against its own clock.
export function CurrentAttempt({ order, className }: { order?: WorkOrder; className?: string }) {
  const attempt = matchingAttempt(useContext(CurrentAttemptContext), order)
  if (!attempt) return null
  const user = attempt.user || attempt.claimed_by
  const facts: [string, ReactNode][] = []
  if (attempt.harness) facts.push(['Harness', attempt.harness])
  if (attempt.model) facts.push(['Model', attempt.model])
  if (user) facts.push(['User', user])
  if (attempt.claimed_at) facts.push(['Claimed', absoluteTime(attempt.claimed_at)])
  if (attempt.last_check_in) facts.push(['Last check-in', relativeTime(attempt.last_check_in)])
  if (attempt.progress) facts.push(['Progress', attempt.progress])
  facts.push(['Elapsed', formatSeconds(attempt.elapsed_seconds)])
  const ids = [
    ['Session', attempt.session_id, 'Copy session ID'],
    ['Attempt', attempt.attempt_id, 'Copy attempt ID'],
  ].filter((row): row is [string, string, string] => Boolean(row[1]))
  return (
    <section aria-label="Current attempt" data-current-attempt={order?.id} className={className}>
      <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1 text-xs">
        {facts.map(([term, value]) => (
          <div key={term} className="contents">
            <dt className="text-muted">{term}</dt>
            <dd className="min-w-0 break-words text-foreground/85">{value}</dd>
          </div>
        ))}
      </dl>
      {ids.length > 0 && (
        <details className="mt-1.5 text-xs text-muted">
          <summary className="cursor-pointer select-none">Show IDs</summary>
          <dl className="mt-1 grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1">
            {ids.map(([term, value, label]) => (
              <div key={term} className="contents">
                <dt>{term}</dt>
                <dd className="flex min-w-0 items-center gap-1">
                  <span className="min-w-0 truncate font-mono text-[11px] text-foreground/80">{value}</span>
                  <CopyButton value={value} label={label} />
                </dd>
              </div>
            ))}
          </dl>
        </details>
      )}
    </section>
  )
}
