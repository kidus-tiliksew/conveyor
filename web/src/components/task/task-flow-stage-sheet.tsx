import { ChevronRight, X } from 'lucide-react'
import { type ReactNode, useState } from 'react'
import { stageLabels } from '../../lib/contracts'
import type { TaskFlowAttempt, TaskFlowAttemptView, TaskFlowRaw, TaskFlowStage, TaskFlowStatus } from '../../lib/types'
import { absoluteTime, formatSeconds, relativeTime } from '../../lib/utils'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'
import { Sheet } from '../ui/sheet'

// The badge tone for each server status word; the word itself is shown as sent.
export const flowStatusBadge: Record<TaskFlowStatus, 'positive' | 'accent' | 'attention' | 'default' | 'outline'> = {
  done: 'positive',
  in_progress: 'accent',
  needs_operator: 'attention',
  waiting: 'outline',
  upcoming: 'default',
  off: 'default',
}

// One stage of the flow, opened from the chart (NA-7). It stacks over the task
// detail sheet or panel, so Escape closes only this sheet. The status is the
// server's word as sent. The client adds no stage descriptions.
export function TaskFlowStageSheet({
  stage,
  raw,
  onClose,
}: {
  stage: TaskFlowStage
  raw: TaskFlowRaw
  onClose: () => void
}) {
  const [showRaw, setShowRaw] = useState(false)
  const label = stageLabels[stage.id] ?? stage.id
  const current = stage.attempts.filter((attempt) => !attempt.superseded)
  const superseded = stage.attempts.filter((attempt) => attempt.superseded)

  return (
    <Sheet onClose={onClose} label={`${label} stage`} width="w-full md:w-[26rem]">
      <header className="flex shrink-0 items-center gap-2 border-b border-border px-4 py-2.5">
        <h2 className="mr-auto truncate text-sm font-medium">{label}</h2>
        {stage.status === 'off' ? (
          <span className="text-xs text-faint">off for this task</span>
        ) : (
          <Badge variant={flowStatusBadge[stage.status]}>{stage.status}</Badge>
        )}
        <Button variant="ghost" size="icon" aria-label="Close stage" onClick={onClose}>
          <X />
        </Button>
      </header>
      <div className="min-h-0 flex-1 space-y-5 overflow-y-auto px-4 py-4">
        {stage.current_attempt && <CurrentAttempt attempt={stage.current_attempt} />}
        <section aria-label="Attempts" className="space-y-2">
          <h3 className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">Attempts</h3>
          {stage.attempts.length === 0 && <p className="text-sm text-faint">—</p>}
          {current.length > 0 && <AttemptList attempts={current} />}
          {superseded.length > 0 && (
            <details data-flow-superseded className="group rounded-md border border-border">
              <summary className="flex cursor-pointer list-none items-center gap-1.5 px-3 py-2 text-xs font-medium text-muted">
                <ChevronRight className="size-3.5 shrink-0 text-faint transition-transform group-open:rotate-90" />
                Superseded
                <Badge variant="mono">{superseded.length}</Badge>
              </summary>
              <div className="border-t border-border px-3 py-2">
                <AttemptList attempts={superseded} />
              </div>
            </details>
          )}
        </section>
        <section className="space-y-2">
          <Button variant="ghost" size="sm" aria-expanded={showRaw} onClick={() => setShowRaw((open) => !open)}>
            {showRaw ? 'Hide raw state' : 'Show raw state'}
          </Button>
          {showRaw && (
            <pre
              data-flow-raw
              className="overflow-x-auto rounded-md border border-border bg-surface p-3 font-mono text-xs text-muted"
            >
              {JSON.stringify(raw, null, 2)}
            </pre>
          )}
        </section>
      </div>
    </Sheet>
  )
}

function CurrentAttempt({ attempt }: { attempt: TaskFlowAttemptView }) {
  const leaseExpiry = attempt.lease_expires_at ? new Date(attempt.lease_expires_at).getTime() : Number.NaN
  return (
    <section aria-label="Current attempt" className="space-y-2">
      <h3 className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">Current attempt</h3>
      <dl className="grid grid-cols-[auto_minmax(0,1fr)] items-center gap-x-4 gap-y-1.5 text-sm">
        <Field term="Harness">{attempt.harness || '—'}</Field>
        <Field term="Model">{attempt.model || '—'}</Field>
        <Field term="User">{attempt.user || attempt.claimed_by || '—'}</Field>
        <Field term="Session">
          <CopyableId value={attempt.session_id} label="Copy session ID" />
        </Field>
        <Field term="Attempt">
          <CopyableId value={attempt.attempt_id} label="Copy attempt ID" />
        </Field>
        <Field term="Claimed at">{attempt.claimed_at ? absoluteTime(attempt.claimed_at) : '—'}</Field>
        <Field term="Last check-in">
          <span className="flex flex-wrap items-center gap-2">
            {relativeTime(attempt.last_check_in ?? undefined)}
            {leaseExpiry < Date.now() && <Badge variant="attention">missed check-in</Badge>}
          </span>
        </Field>
        <Field term="Lease">{relativeTime(attempt.lease_expires_at ?? undefined)}</Field>
        <Field term="Progress">{attempt.progress || '—'}</Field>
        <Field term="Elapsed">{formatSeconds(attempt.elapsed_seconds)}</Field>
      </dl>
    </section>
  )
}

function Field({ term, children }: { term: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-xs text-muted">{term}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </>
  )
}

function CopyableId({ value, label }: { value?: string; label: string }) {
  if (!value) return '—'
  return (
    <span className="flex min-w-0 items-center gap-1">
      <span className="min-w-0 truncate font-mono text-xs">{value}</span>
      <CopyButton value={value} label={label} />
    </span>
  )
}

function AttemptList({ attempts }: { attempts: TaskFlowAttempt[] }) {
  return (
    <ol className="space-y-2">
      {attempts.map((attempt) => (
        <li key={`${attempt.order_id}-${attempt.attempt_id}-${attempt.started_at}`} className="space-y-0.5 text-xs">
          <p className="flex flex-wrap items-center gap-1.5 font-mono">
            <span className="text-foreground">{attempt.order_id}</span>
            {attempt.outcome && <Badge variant="mono">{attempt.outcome}</Badge>}
            {attempt.auto && <Badge variant="mono">auto</Badge>}
          </p>
          <p className="text-faint">
            {attempt.started_at ? absoluteTime(attempt.started_at) : '—'}
            {' → '}
            {attempt.ended_at ? absoluteTime(attempt.ended_at) : '—'}
          </p>
        </li>
      ))}
    </ol>
  )
}
