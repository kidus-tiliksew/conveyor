import { GitPullRequest } from 'lucide-react'
import type { ReactNode } from 'react'
import type { ImplementationDelivery, TimelineEntry } from '../../lib/activity'
import { cn } from '../../lib/utils'
import { Badge } from '../ui/badge'
import { MarkdownProse } from '../ui/markdown-prose'

// An implement job is the task's delivery, so like the verify and review
// stages it gets its own timeline card instead of the generic job card: what
// state the attempt is in, what the agent reported, and which pull request
// and head it delivered (component-web-task-surfaces invariant 43; task
// 261028-5c2ae2). The header says what happened: the attempt, its state, and
// the pull request chip. The footer says who and when. The timeline passes
// its own dot, footer, and audit disclosure in as slots so the card reuses
// them without importing the timeline.

type Status = 'submitted' | 'failed' | 'running' | 'queued'

const statusBadge: Record<Status, { label: string; variant: 'positive' | 'failure' | 'accent' | 'default' }> = {
  submitted: { label: 'Submitted', variant: 'positive' },
  failed: { label: 'Failed', variant: 'failure' },
  running: { label: 'Running', variant: 'accent' },
  queued: { label: 'Queued', variant: 'default' },
}

function statusOf(job: Extract<TimelineEntry, { type: 'job' }>['job']): Status {
  if (job.state === 'done') return 'submitted'
  if (job.state === 'failed') return 'failed'
  if (job.state === 'running') return 'running'
  return 'queued'
}

export function implementationDot(entry: Extract<TimelineEntry, { type: 'job' }>) {
  const status = statusOf(entry.job)
  const warning = entry.tone === 'warning'
  return cn(
    'bg-edge',
    status === 'submitted' && !warning && 'bg-positive',
    warning && 'bg-attention-dot',
    status === 'failed' && 'bg-failure',
    status === 'running' && 'animate-pulse bg-primary',
  )
}

export function ImplementationEntry({
  entry,
  dotSlot,
  footer,
  summaryAudit,
}: {
  entry: Extract<TimelineEntry, { type: 'job' }>
  dotSlot: (className: string) => ReactNode
  footer: ReactNode
  summaryAudit?: ReactNode
}) {
  const badge = statusBadge[statusOf(entry.job)]
  const warning = entry.tone === 'warning'
  const { delivery } = entry
  return (
    <li className="relative pl-7">
      {dotSlot(implementationDot(entry))}
      {/* The card is a size container: a card at least 28rem wide adds the
          head to the pull request chip. */}
      <article
        aria-label={entry.attempt ? `Implementation attempt ${entry.attempt}` : 'Implementation'}
        className={cn('@container rounded-lg bg-surface', warning && 'border border-attention/30 bg-attention-soft')}
        data-implementation-entry
      >
        <header className="flex flex-wrap items-center gap-x-2 gap-y-1 px-4 pt-3" data-job-header>
          <span className="whitespace-nowrap text-sm font-semibold text-foreground" data-implementation-title>
            Implementation
            {entry.attempt ? <span className="font-normal text-muted"> {entry.attempt}</span> : null}
          </span>
          <Badge variant={badge.variant}>{badge.label}</Badge>
          {delivery && <DeliveryChip delivery={delivery} />}
        </header>
        <MarkdownProse
          className="px-4 pt-1.5 pb-3 text-sm leading-6 text-foreground/85 [overflow-wrap:anywhere]"
          components={{ p: ({ children }) => <p className="whitespace-pre-line">{children}</p> }}
        >
          {entry.summary}
        </MarkdownProse>
        {summaryAudit && <div className="px-4 pb-3 text-xs text-muted">{summaryAudit}</div>}
        {footer}
      </article>
    </li>
  )
}

// The attempt's own pull request and head, never the task's latest head. The
// head shows only on a card at least 28rem wide. The chip's accessible name
// and title always carry the full head (component-web-task-surfaces invariant
// 43; DEC-127(4)).
function DeliveryChip({ delivery }: { delivery: ImplementationDelivery }) {
  const name = `Pull request #${delivery.number}, head ${delivery.headSha}`
  return (
    <a
      href={delivery.url}
      target="_blank"
      rel="noreferrer"
      aria-label={name}
      title={name}
      className="ml-auto inline-flex shrink-0 items-center gap-1 whitespace-nowrap rounded-md border border-border px-1.5 py-0.5 text-[11px] leading-4 text-primary hover:bg-raised/60"
      data-implementation-delivery
    >
      <GitPullRequest className="size-3.5" aria-hidden />#{delivery.number}
      <span className="hidden font-mono text-muted @min-[28rem]:inline" data-implementation-head>
        {' · '}
        {delivery.headSha.slice(0, 7)}
      </span>
    </a>
  )
}
