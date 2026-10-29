import { Link } from '@tanstack/react-router'
import { ChevronRight } from 'lucide-react'
import { buildTimeline, type TimelineEntry } from '../../lib/activity'
import { errorMessage } from '../../lib/errors'
import type { ActivityItem, SpecVersion } from '../../lib/types'
import { absoluteTime, relativeTime } from '../../lib/utils'
import { usePendingProposals, useWorkspaceSelection } from '../app-shell'
import { SpecCard } from '../task/spec-card'
import { SystemDesignProposalCard, useSystemDesignProposals } from '../task/system-design-proposal-card'
import { useTaskDetail } from '../task/use-task-detail'
import { VerificationEntry } from '../task/verification-entry'
import { Button } from '../ui/button'
import { MarkdownProse } from '../ui/markdown-prose'
import { Skeleton } from '../ui/skeleton'
import { planFiles, planHeadings, splitPlanSections } from './plan-sections'
import { useNextAction } from './use-next-actions'

type JobEntry = Extract<TimelineEntry, { type: 'job' }>

const planKinds = new Set(['plan_approval', 'plan_revision'])

// The context a decision needs, rendered in the selected decision's detail
// view without leaving the Inbox (req-operator-next-action AC-2.3, AC-2.8).
// The section is chosen by the server's next-action kind; each one is the
// task-detail component that already renders that context. The task read has
// its own loading and error-with-retry state, and the detail view is keyed by
// workspace and item, so a late read never shows under another selection.
export function DecisionContext({ taskId, kind }: { taskId: string; kind: string }) {
  const { data: item, error, refetch, isFetching } = useTaskDetail(taskId)
  return (
    <section aria-label="Decision context" data-decision-context={kind} aria-busy={!item && error == null}>
      {!item && error == null && <Skeleton className="h-32" />}
      {error != null && (
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-failure">{errorMessage(error, 'Could not load this task.')}</span>
          <Button size="sm" variant="secondary" disabled={isFetching} onClick={() => void refetch()}>
            Retry
          </Button>
        </div>
      )}
      {item && <ContextSection item={item} kind={kind} />}
    </section>
  )
}

function ContextSection({ item, kind }: { item: ActivityItem; kind: string }) {
  if (planKinds.has(kind))
    return item.spec ? (
      <PlanContext item={item} spec={item.spec} />
    ) : (
      <p className="text-sm text-muted">No plan has been submitted.</p>
    )
  if (kind === 'agent_question') return <CheckpointContext item={item} />
  if (kind === 'context_proposal') return <ProposedContext item={item} />
  if (kind === 'parked') return <ParkReason item={item} />
  if (kind === 'dispatch_exhausted') return <DispatchFailure item={item} />
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

// What triage recorded when it parked the task, read from the bounded
// detail's complete-ledger summary, never the event page
// (feature-operator-next-action NA-6; component-http-api, Bounded detail view).
function ParkReason({ item }: { item: ActivityItem }) {
  const reason = item.summaries?.park_reason
  if (!reason) return <p className="text-sm text-muted">Triage recorded no reason.</p>
  const lists = [
    { label: 'Open questions', entries: reason.questions ?? [] },
    { label: 'Risks', entries: reason.risks ?? [] },
  ].filter((list) => list.entries.length > 0)
  return (
    <div className="space-y-2" data-park-reason>
      <section aria-label="Triage summary" className="rounded-lg border border-border px-3 py-3">
        <h4 className="text-xs font-medium text-muted">Triage summary</h4>
        <p className="mt-1 whitespace-pre-wrap break-words text-sm">{reason.summary || 'Triage stated no summary.'}</p>
      </section>
      {lists.map((list) => (
        <section key={list.label} aria-label={list.label} className="rounded-lg border border-border px-3 py-3">
          <h4 className="text-xs font-medium text-muted">{list.label}</h4>
          <ul className="mt-1 list-disc space-y-0.5 pl-5 text-sm">
            {list.entries.map((entry, index) => (
              <li key={`${index}:${entry}`} className="break-words">
                {entry}
              </li>
            ))}
          </ul>
        </section>
      ))}
      <p className="text-xs text-faint">
        Recorded by triage <time dateTime={reason.at}>{relativeTime(reason.at)}</time>
        {reason.truncated ? ' · shortened; open the task for the full text' : ''}
      </p>
    </div>
  )
}

// The dispatch failure that exhausted dispatch: the latest recorded one.
function DispatchFailure({ item }: { item: ActivityItem }) {
  const failure = item.summaries?.dispatch_failure
  if (!failure) return <p className="text-sm text-muted">Dispatch recorded no failure.</p>
  return (
    <div className="space-y-2" data-dispatch-failure>
      <section aria-label="Last dispatch failure" className="rounded-lg border border-border px-3 py-3">
        <h4 className="text-xs font-medium text-muted">
          Last dispatch failure
          {failure.attempt != null && failure.max_attempts != null
            ? ` · Attempt ${failure.attempt} of ${failure.max_attempts}`
            : ''}
        </h4>
        <p className="mt-1 whitespace-pre-wrap break-words rounded-md bg-failure-soft px-2.5 py-2 font-mono text-xs text-failure">
          {failure.error}
        </p>
      </section>
      <p className="text-xs text-faint">
        Recorded by dispatch <time dateTime={failure.at}>{relativeTime(failure.at)}</time>
        {failure.truncated ? ' · shortened; open the task for the full text' : ''}
      </p>
    </div>
  )
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

// The task body, folded behind a one-line disclosure that expands to the full
// body and a link to the full task. The section renders no proposed
// document's content and issues no document read: a checklist row's title
// opens the document page. The section is read-only (NA-6; AC-2.3).
function ProposedContext({ item }: { item: ActivityItem }) {
  const { workspace } = useWorkspaceSelection()
  const body = item.task.body?.trim() ?? ''
  if (!body) return <p className="text-sm text-muted">The task has no description.</p>
  return (
    <details data-task-body className="group min-w-0 rounded-lg border border-border px-3 py-2.5">
      <summary className="flex min-w-0 cursor-pointer list-none items-baseline gap-1.5 text-xs text-muted [&::-webkit-details-marker]:hidden">
        <ChevronRight
          aria-hidden="true"
          className="size-3.5 shrink-0 self-center text-faint transition-transform group-open:rotate-90"
        />
        <span className="shrink-0 font-medium text-foreground">The task</span>
        <span aria-hidden="true">·</span>
        <span className="min-w-0 truncate">{firstLine(body)}</span>
      </summary>
      <MarkdownProse className="mt-2 text-sm">{body}</MarkdownProse>
      <Link
        to="/w/$workspaceId/tasks/$taskId/full"
        params={{ workspaceId: workspace, taskId: item.task.id }}
        className="mt-2 inline-block text-xs font-medium text-primary hover:underline"
      >
        Full task →
      </Link>
    </details>
  )
}

// The body's first line of prose, skipping Markdown headings, without list
// or quote markers. A body of headings alone reads its first heading.
function firstLine(body: string) {
  const lines = body.split('\n').filter((text) => text.trim() !== '')
  const line = lines.find((text) => !/^\s{0,3}#{1,6}\s/.test(text)) ?? lines[0] ?? ''
  return line
    .replace(/^\s{0,3}(#{1,6}\s+|[-*+]\s+|\d+[.)]\s+|>\s*)/, '')
    .replace(/[*_`]/g, '')
    .trim()
}

// A plan split at its five required headings, under a header naming the
// version, the served requirements, and who submitted it. A plan without
// those headings renders whole through the task-detail plan card.
function PlanContext({ item, spec }: { item: ActivityItem; spec: SpecVersion }) {
  const sections = splitPlanSections(spec.content)
  const planner = item.work_orders
    ?.filter((order) => order.stage === 'spec' && order.agent)
    .sort((a, b) => a.queue_entered_at.localeCompare(b.queue_entered_at))
    .at(-1)
  const submittedBy = planner
    ? ` by the plan agent (${[planner.agent, planner.model].filter(Boolean).join(' · ')})`
    : ''
  const requirements = item.task.context?.requirements ?? []
  return (
    <section aria-label="Plan" data-plan-context className="space-y-4">
      <header className="space-y-1.5">
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="text-base font-semibold">Plan v{spec.version}</h3>
          {requirements.map((requirement) => (
            <span
              key={requirement.id}
              className="inline-flex min-w-0 max-w-full items-center gap-1 rounded border border-border px-1.5 py-0.5 text-xs"
            >
              <span className="font-mono text-primary">{requirement.id}</span>
              <span className="min-w-0 truncate">{requirement.title}</span>
            </span>
          ))}
        </div>
        <p className="text-xs text-muted" data-plan-submitted>
          Submitted {absoluteTime(spec.created_at)}
          {submittedBy}.
        </p>
      </header>
      {sections ? (
        <>
          {sections.preamble && (
            <div data-plan-preamble>
              <MarkdownProse className="text-sm">{sections.preamble}</MarkdownProse>
            </div>
          )}
          {planHeadings.map((heading) => (
            <section key={heading} aria-label={heading} data-plan-section={heading}>
              <h4 className="mb-1.5 text-[11px] font-semibold uppercase tracking-[0.08em] text-muted">{heading}</h4>
              {heading === 'Files touched' ? (
                <PlanFiles section={sections.sections[heading]} />
              ) : (
                <MarkdownProse className="text-sm">{sections.sections[heading]}</MarkdownProse>
              )}
            </section>
          ))}
        </>
      ) : (
        <SpecCard spec={spec} overflowExpandable routeVariant="sheet" />
      )}
    </section>
  )
}

function PlanFiles({ section }: { section: string }) {
  const { intro, files } = planFiles(section)
  if (files.length === 0) return <MarkdownProse className="text-sm">{section}</MarkdownProse>
  return (
    <div className="space-y-2">
      {intro && (
        <div data-plan-files-intro>
          <MarkdownProse className="text-sm">{intro}</MarkdownProse>
        </div>
      )}
      <ul className="divide-y divide-border rounded-md border border-border">
        {files.map((file, index) => (
          <li key={`${index}:${file.path}`} className="min-w-0 px-3 py-2">
            <code className="break-all font-mono text-xs">{file.path}</code>
            {file.note && <p className="mt-0.5 break-words text-xs text-muted">{file.note}</p>}
          </li>
        ))}
      </ul>
    </div>
  )
}
