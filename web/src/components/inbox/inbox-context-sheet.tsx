import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { X } from 'lucide-react'
import { buildTimeline, type TimelineEntry } from '../../lib/activity'
import { fetchRequirement, fetchSystemDesign } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type {
  ActivityItem,
  NextActionContextProposal,
  RequirementView,
  SpecVersion,
  SystemDesignView,
} from '../../lib/types'
import { absoluteTime } from '../../lib/utils'
import { usePendingProposals, useWorkspaceSelection } from '../app-shell'
import { GateChoices } from '../task/merge-at-gate'
import { SpecCard } from '../task/spec-card'
import { SystemDesignProposalCard, useSystemDesignProposals } from '../task/system-design-proposal-card'
import { useTaskDetail } from '../task/use-task-detail'
import { VerificationEntry } from '../task/verification-entry'
import { Button } from '../ui/button'
import { MarkdownProse } from '../ui/markdown-prose'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'
import { planFiles, planHeadings, splitPlanSections } from './plan-sections'
import { useNextAction } from './use-next-actions'

type JobEntry = Extract<TimelineEntry, { type: 'job' }>

const planKinds = new Set(['plan_approval', 'plan_revision'])

// The context a decision needs, opened beside the Inbox without leaving it
// (AC-2.3). The section is chosen by the server's next-action kind; each one
// is the task-detail component that already renders that context. A plan
// decision repeats the card's choices in the sheet footer, so it can be taken
// where it is read (feature-operator-next-action NA-6).
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
      {planKinds.has(kind) && <PlanSheetChoices taskId={taskId} onActed={onClose} />}
    </Sheet>
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

// The task body and, for each proposed document, the version confirmed today.
// A context proposal attaches an existing document to the task, so nothing
// here is a proposed revision. The section is read-only (NA-6; AC-2.3).
function ProposedContext({ item }: { item: ActivityItem }) {
  const { data: action } = useNextAction(item.task.id)
  const proposals = action?.details?.context_proposals ?? []
  return (
    <div className="space-y-3" data-proposed-context>
      <section aria-label="Task" className="rounded-lg border border-border px-3 py-3">
        <h3 className="text-xs font-medium text-muted">The task</h3>
        {item.task.body ? (
          <MarkdownProse className="mt-1 text-sm">{item.task.body}</MarkdownProse>
        ) : (
          <p className="mt-1 text-sm text-muted">The task has no description.</p>
        )}
      </section>
      {proposals.map((proposal) => (
        <ProposedDocument key={`${proposal.kind}:${proposal.target_id}`} proposal={proposal} />
      ))}
    </div>
  )
}

function ProposedDocument({ proposal }: { proposal: NextActionContextProposal }) {
  const { workspace } = useWorkspaceSelection()
  const requirement = proposal.kind === 'requirement'
  // The document pages' own query keys and reads, so the sheet shares their
  // cache; the key carries the workspace and the query's signal aborts on close.
  const { data, error, isLoading, refetch, isFetching } = useQuery<RequirementView | SystemDesignView>({
    queryKey: [requirement ? 'requirement' : 'system-design', workspace, proposal.target_id],
    queryFn: ({ signal }) =>
      requirement
        ? fetchRequirement(proposal.target_id, workspace, signal)
        : fetchSystemDesign(proposal.target_id, workspace, signal),
    enabled: Boolean(workspace),
  })
  const document = data ? confirmedDocument(data) : undefined
  const title = document?.title || proposal.target_title || proposal.tier_label
  return (
    <section
      aria-label={title}
      data-proposed-document={proposal.kind}
      className="min-w-0 rounded-lg border border-border px-3 py-3"
    >
      <h3 className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-sm font-medium">
        <span className="text-[11px] font-medium uppercase tracking-[0.06em] text-muted">{proposal.tier_label}</span>
        <span className="min-w-0 break-words">{title}</span>
        {document?.current && <span className="text-xs text-muted">v{document.current.version}</span>}
      </h3>
      {isLoading && <Skeleton className="mt-2 h-16" />}
      {error != null && (
        <div className="mt-2 flex flex-wrap items-center gap-2 text-sm">
          <span className="text-failure">{errorMessage(error, 'Could not load this document.')}</span>
          <Button size="sm" variant="secondary" disabled={isFetching} onClick={() => void refetch()}>
            Retry
          </Button>
        </div>
      )}
      {document && !document.current && (
        <p className="mt-2 text-sm text-muted">This document has no confirmed version yet.</p>
      )}
      {document?.current && <MarkdownProse className="mt-2 text-sm">{document.current.content}</MarkdownProse>}
    </section>
  )
}

// The title and the current confirmed version of either document read.
function confirmedDocument(view: RequirementView | SystemDesignView) {
  if ('requirement' in view) return { title: view.requirement.title, current: view.current_version }
  return { title: view.document.title, current: view.current_version }
}

// The card's choices repeated under the plan, so the decision can be taken
// where it is read. It mounts only for a plan decision.
function PlanSheetChoices({ taskId, onActed }: { taskId: string; onActed: () => void }) {
  const { data: action } = useNextAction(taskId)
  if (!action || action.choices.length === 0) return null
  return (
    <div data-sheet-choices className="border-t border-border px-4 py-3 text-[13px]">
      <GateChoices taskId={taskId} action={action} onActed={onActed} />
    </div>
  )
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
