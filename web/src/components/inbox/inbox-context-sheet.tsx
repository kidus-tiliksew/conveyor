import { Link } from '@tanstack/react-router'
import { X } from 'lucide-react'
import { buildTimeline, type TimelineEntry } from '../../lib/activity'
import type { ActivityItem } from '../../lib/types'
import { usePendingProposals } from '../app-shell'
import { SpecCard } from '../task/spec-card'
import { SystemDesignProposalCard, useSystemDesignProposals } from '../task/system-design-proposal-card'
import { useTaskDetail } from '../task/use-task-detail'
import { VerificationEntry } from '../task/verification-entry'
import { Button } from '../ui/button'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'
import { useNextAction } from './use-next-actions'

type JobEntry = Extract<TimelineEntry, { type: 'job' }>

// The context a decision needs, opened beside the Inbox without leaving it
// (AC-2.3). The section is chosen by the server's next-action kind; each one
// is the task-detail component that already renders that context.
export function InboxContextSheet({ taskId, kind, onClose }: { taskId: string; kind: string; onClose: () => void }) {
  const { data: item, isLoading, error } = useTaskDetail(taskId)
  return (
    <Sheet onClose={onClose} label="Decision context">
      <div className="flex items-center gap-2 border-b border-border px-4 py-3">
        <h2 className="min-w-0 flex-1 truncate text-sm font-semibold">{item?.task.title ?? taskId}</h2>
        <Link to="/tasks/$taskId/full" params={{ taskId }} className="text-xs font-medium text-primary hover:underline">
          Open task
        </Link>
        <Button variant="ghost" size="icon" aria-label="Close" onClick={onClose}>
          <X />
        </Button>
      </div>
      <div className="min-h-0 flex-1 space-y-4 overflow-y-auto px-4 py-4">
        {isLoading && <Skeleton className="h-32" />}
        {error && <p className="text-sm text-failure">Could not load this task.</p>}
        {item && <ContextSection item={item} kind={kind} />}
      </div>
    </Sheet>
  )
}

function ContextSection({ item, kind }: { item: ActivityItem; kind: string }) {
  if (kind === 'plan_approval' || kind === 'plan_revision')
    return item.spec ? (
      <SpecCard spec={item.spec} overflowExpandable routeVariant="sheet" />
    ) : (
      <p className="text-sm text-muted">No plan has been submitted.</p>
    )
  if (kind === 'agent_question') return <CheckpointContext item={item} />
  // Newest first, so `find` reads the latest run of a stage.
  const jobs = buildTimeline(item)
    .filter((entry): entry is JobEntry => entry.type === 'job')
    .reverse()
  if (kind === 'review_bounce_limit') {
    const reviewIndex = jobs.findIndex((entry) => entry.job.stage === 'review')
    const reply = jobs.slice(0, Math.max(reviewIndex, 0)).find((entry) => entry.job.stage === 'implement')
    if (reviewIndex < 0) return <p className="text-sm text-muted">No review has run yet.</p>
    return (
      <div className="space-y-3">
        <section aria-label="Latest reviewer feedback" className="rounded-lg border border-border px-3 py-3">
          <h3 className="text-xs font-medium text-muted">Latest reviewer feedback</h3>
          <p className="mt-1 whitespace-pre-wrap break-words text-sm">{jobs[reviewIndex].summary}</p>
        </section>
        {reply && (
          <section aria-label="Implementer reply" className="rounded-lg border border-border px-3 py-3">
            <h3 className="text-xs font-medium text-muted">Implementer reply</h3>
            <p className="mt-1 whitespace-pre-wrap break-words text-sm">{reply.summary}</p>
          </section>
        )}
      </div>
    )
  }
  if (kind === 'verification_disposition') {
    const verify = jobs.find((entry) => entry.job.stage === 'verify' && entry.job.started_at)
    return verify ? (
      <ol>
        <VerificationEntry item={item} job={verify.job} order={verify.order} />
      </ol>
    ) : (
      <p className="text-sm text-muted">No verification has run yet.</p>
    )
  }
  return <p className="text-sm text-muted">Open the task for its full history.</p>
}

function CheckpointContext({ item }: { item: ActivityItem }) {
  const proposals = useSystemDesignProposals(item.task)
  const pending = usePendingProposals().data?.items ?? []
  // The server names the order the agent is waiting in; its checkpoint is
  // the question and the documents it cites.
  const { data: action } = useNextAction(item.task.id)
  const checkpoint = item.work_orders?.find((order) => order.id === action?.current_order?.id)?.checkpoint
  return (
    <div className="space-y-3">
      <section aria-label="Agent checkpoint" className="rounded-lg border border-border px-3 py-3">
        <h3 className="text-xs font-medium text-muted">The agent asks</h3>
        <p className="mt-1 whitespace-pre-wrap break-words text-sm">
          {checkpoint?.decision_request || 'The agent stopped without a structured question.'}
        </p>
        {(checkpoint?.citations?.length ?? 0) > 0 && (
          <ul className="mt-2 space-y-1 text-xs text-muted">
            {checkpoint?.citations?.map((citation) => (
              <li key={`${citation.document_kind}:${citation.document_id}`} className="break-words">
                {citation.document_title} ({citation.document_id}) cites v{citation.cited_version}
                {citation.statement_or_section_id ? ` ${citation.statement_or_section_id}` : ''}; current v
                {citation.current_confirmed_version}
              </li>
            ))}
          </ul>
        )}
      </section>
      <SystemDesignProposalCard task={item.task} proposals={proposals} pending={pending} />
    </div>
  )
}
