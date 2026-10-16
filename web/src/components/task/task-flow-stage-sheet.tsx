import { ChevronRight, X } from 'lucide-react'
import type { ReactNode } from 'react'
import type { TaskFlowAttempt, TaskFlowAttemptView, TaskFlowStage, TaskFlowStatus } from '../../lib/types'
import { absoluteTime, cn, formatSeconds, relativeTime } from '../../lib/utils'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'
import { Sheet } from '../ui/sheet'

// The badge tone for each server status; the label itself is the server's.
const statusBadge: Record<TaskFlowStatus, 'positive' | 'accent' | 'attention' | 'default' | 'outline'> = {
  done: 'positive',
  in_progress: 'accent',
  needs_operator: 'attention',
  waiting: 'outline',
  upcoming: 'default',
  off: 'default',
}

// One stage of the flow, opened from the chart (req-operator-next-action
// AC-7.6). It stacks over the task sheet or page through the shared Sheet, so
// Escape closes only this panel and focus returns to the stage node. Every
// sentence is the server's, printed as sent.
export function TaskFlowStageSheet({ stage, onClose }: { stage: TaskFlowStage; onClose: () => void }) {
  return (
    <Sheet onClose={onClose} label={`${stage.label} stage`} width="w-full md:w-[26rem]">
      <header className="flex shrink-0 items-center gap-2 border-b border-border px-4 py-2.5">
        <h2 className="min-w-0 truncate text-sm font-medium">{stage.label}</h2>
        <Badge variant={statusBadge[stage.status]} data-flow-panel-status={stage.status}>
          {stage.status_label}
        </Badge>
        <Button variant="ghost" size="icon" className="ml-auto shrink-0" aria-label="Close stage" onClick={onClose}>
          <X />
        </Button>
      </header>
      <div className="min-h-0 flex-1 space-y-5 overflow-y-auto px-4 py-4">
        <section aria-label="Stage" className="space-y-1.5 text-sm">
          <p data-flow-description className="text-muted">
            {stage.description}
          </p>
          {stage.missed_check_in && (
            <p data-flow-missed-check-in className="text-xs font-medium text-attention">
              {stage.missed_check_in}
            </p>
          )}
        </section>
        <CurrentAttemptCard stage={stage} />
        <Attempts attempts={stage.attempts} />
      </div>
    </Sheet>
  )
}

// The stage's current attempt, built from the flow's own attempt block. Facts
// the server does not send are omitted; the browser clock formats times but
// never judges the lease.
function CurrentAttemptCard({ stage }: { stage: TaskFlowStage }) {
  const attempt = stage.current_attempt
  return (
    <section aria-label="Current attempt" className="space-y-2">
      <h3 className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">Current attempt</h3>
      <div className="space-y-3 rounded-md border border-border bg-card p-3 text-sm">
        {attempt ? <AttemptFacts attempt={attempt} /> : <p data-flow-status-text>{stage.status_text}</p>}
      </div>
    </section>
  )
}

function AttemptFacts({ attempt }: { attempt: TaskFlowAttemptView }) {
  const who = [attempt.harness, attempt.model, attempt.user || attempt.claimed_by].filter(Boolean)
  return (
    <>
      {who.length > 0 && (
        <p data-flow-attempt-who className="flex items-center gap-2 font-medium">
          <span aria-hidden className="size-2 shrink-0 rounded-full bg-primary" />
          <span className="min-w-0 break-words">{who.join(' · ')}</span>
        </p>
      )}
      <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1.5">
        {attempt.claimed_at && <Fact term="Claimed">{absoluteTime(attempt.claimed_at)}</Fact>}
        {attempt.last_check_in && <Fact term="Last check-in">{relativeTime(attempt.last_check_in)}</Fact>}
        {attempt.progress && (
          <Fact term="Progress">
            <span data-flow-attempt-progress className="break-words">
              {attempt.progress}
            </span>
          </Fact>
        )}
        <Fact term="Elapsed">
          <span data-flow-attempt-elapsed>{formatSeconds(attempt.elapsed_seconds)}</span>
        </Fact>
      </dl>
      {(attempt.session_id || attempt.attempt_id) && (
        <details data-flow-attempt-ids className="group text-xs">
          <summary className="flex cursor-pointer list-none items-center gap-1 text-muted">
            <ChevronRight className="size-3.5 shrink-0 transition-transform group-open:rotate-90" />
            Session and attempt IDs
          </summary>
          <dl className="mt-1.5 grid grid-cols-[auto_minmax(0,1fr)] items-center gap-x-4 gap-y-1">
            {attempt.session_id && (
              <Fact term="Session">
                <CopyableId value={attempt.session_id} label="Copy session ID" />
              </Fact>
            )}
            {attempt.attempt_id && (
              <Fact term="Attempt">
                <CopyableId value={attempt.attempt_id} label="Copy attempt ID" />
              </Fact>
            )}
          </dl>
        </details>
      )}
    </>
  )
}

function Fact({ term, children }: { term: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-xs text-muted">{term}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
  )
}

function CopyableId({ value, label }: { value: string; label: string }) {
  return (
    <span className="flex min-w-0 items-center gap-1">
      <span className="min-w-0 break-all font-mono text-[11px]">{value}</span>
      <CopyButton value={value} label={label} />
    </span>
  )
}

// The stage's attempts newest first: the server lists episodes oldest first,
// so the newest is expanded and every earlier one waits behind a collapsed
// disclosure that states how many were superseded.
function Attempts({ attempts }: { attempts: TaskFlowAttempt[] }) {
  if (attempts.length === 0) return null
  const numbered = attempts.map((attempt, index) => ({ attempt, number: index + 1 })).reverse()
  const [latest, ...earlier] = numbered
  const superseded = earlier.filter(({ attempt }) => attempt.superseded).length
  return (
    <section aria-label="Attempts" className="space-y-2">
      <h3 className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">Attempts</h3>
      <ol className="overflow-hidden rounded-md border border-border">
        <AttemptRow attempt={latest.attempt} number={latest.number} latest />
      </ol>
      {earlier.length > 0 && (
        <details data-flow-earlier-attempts className="group rounded-md border border-border">
          <summary className="flex cursor-pointer list-none items-center gap-1.5 px-3 py-2 text-xs font-medium text-muted">
            <ChevronRight className="size-3.5 shrink-0 transition-transform group-open:rotate-90" />
            {earlier.length} earlier {earlier.length === 1 ? 'attempt' : 'attempts'}
            {superseded > 0 && `, ${superseded} superseded`}
          </summary>
          <ol className="border-t border-border">
            {earlier.map(({ attempt, number }) => (
              <AttemptRow key={attemptKey(attempt)} attempt={attempt} number={number} />
            ))}
          </ol>
        </details>
      )}
    </section>
  )
}

const attemptKey = (attempt: TaskFlowAttempt) =>
  `${attempt.order_id}:${attempt.attempt_id}:${attempt.started_at ?? ''}:${attempt.ended_at ?? ''}`

function AttemptRow({
  attempt,
  number,
  latest = false,
}: {
  attempt: TaskFlowAttempt
  number: number
  latest?: boolean
}) {
  return (
    <li
      data-flow-attempt={number}
      className={cn(
        'space-y-1 border-b border-border px-3 py-2 text-xs last:border-b-0',
        latest && 'bg-primary-soft/40',
      )}
    >
      <p className="flex flex-wrap items-center gap-1.5">
        <span className={cn('font-medium', latest ? 'text-primary' : 'text-foreground')}>Attempt {number}</span>
        <span className="font-mono text-muted">{attempt.order_id}</span>
        {attempt.outcome && <Badge variant="mono">{attempt.outcome}</Badge>}
        {attempt.auto && <Badge variant="mono">auto</Badge>}
        {attempt.superseded && <Badge variant="mono">superseded</Badge>}
      </p>
      <p className="text-faint">
        {attempt.started_at ? absoluteTime(attempt.started_at) : '—'}
        {' → '}
        {attempt.ended_at ? absoluteTime(attempt.ended_at) : '—'}
      </p>
      {attempt.attempt_id && <p className="break-all font-mono text-[11px] text-faint">{attempt.attempt_id}</p>}
    </li>
  )
}
