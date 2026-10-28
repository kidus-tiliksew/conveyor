import { GitPullRequest } from 'lucide-react'
import type { ReactNode } from 'react'
import type { TimelineEntry } from '../../lib/activity'
import { absoluteTime, cn } from '../../lib/utils'
import { Badge } from '../ui/badge'
import { MarkdownProse } from '../ui/markdown-prose'

// An implement job is the task's delivery, so like the verify and review
// stages it gets its own timeline card instead of the generic job card: what
// state the attempt is in, what the agent reported, and which pull request
// and head it delivered (component-web-task-surfaces, Work-order timeline
// entries; task 261028-5c2ae2). The timeline passes its own dot, model
// metadata, audit disclosure, and transcript captures in as slots so the card
// reuses them without importing the timeline.

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
  meta,
  summaryAudit,
  transcripts,
}: {
  entry: Extract<TimelineEntry, { type: 'job' }>
  dotSlot: (className: string) => ReactNode
  meta: ReactNode
  summaryAudit?: ReactNode
  transcripts?: ReactNode
}) {
  const badge = statusBadge[statusOf(entry.job)]
  const warning = entry.tone === 'warning'
  const { delivery } = entry
  return (
    <li className="relative pl-7">
      {dotSlot(implementationDot(entry))}
      <article
        aria-label={entry.attempt ? `Implementation attempt ${entry.attempt}` : 'Implementation'}
        className={cn('rounded-lg border border-border bg-card', warning && 'border-attention/40 bg-attention-soft')}
        data-implementation-entry
      >
        <header className="flex flex-wrap items-center gap-x-2.5 gap-y-1 px-4 pt-3" data-job-header>
          <span className="text-[13px] font-semibold text-foreground">Implementation</span>
          <Badge variant={badge.variant}>{badge.label}</Badge>
          {entry.attempt ? <span className="text-xs text-muted">Attempt {entry.attempt}</span> : null}
          {meta}
          <time className="ml-auto text-[11px] text-faint">{absoluteTime(entry.at)}</time>
        </header>
        <MarkdownProse
          className="px-4 pt-1.5 pb-3 text-sm leading-6 text-foreground/85"
          components={{ p: ({ children }) => <p className="whitespace-pre-line">{children}</p> }}
        >
          {entry.summary}
        </MarkdownProse>
        {summaryAudit && <div className="px-4 pb-3 text-xs text-muted">{summaryAudit}</div>}
        {transcripts && <div className="px-4 pb-3">{transcripts}</div>}
        {delivery && (
          <footer
            className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 border-t border-border px-4 py-2 text-xs text-muted"
            data-implementation-delivery
          >
            <a
              href={delivery.url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-primary hover:underline"
            >
              <GitPullRequest className="size-3.5" aria-hidden />#{delivery.number}
            </a>
            <span className="font-mono text-[11px] text-muted" title={delivery.headSha}>
              {delivery.headSha.slice(0, 7)}
            </span>
          </footer>
        )}
      </article>
    </li>
  )
}
