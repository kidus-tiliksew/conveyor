// The Documents tab (req-requirement-insights REQ-4; feature-requirement-insights
// RI-7): the System Design documents, decisions, and acceptance criteria the
// server associates with the requirement, in its order, each with the
// evidence that associates it. A criterion is only ever "cited by a task",
// never satisfied (AC-4.3).
import { useQuery } from '@tanstack/react-query'
import { ArrowUpRight, ChevronDown, FileCode2, Scale } from 'lucide-react'
import { useId, useState } from 'react'
import { fetchRequirementInsightDocuments } from '../../lib/api'
import type { RequirementInsightDecision, RequirementInsightDesign } from '../../lib/types'
import { cn } from '../../lib/utils'
import { Skeleton } from '../ui/skeleton'
import {
  hoverProps,
  insightErrorText,
  insightKey,
  insightRetry,
  Notice,
  type RowHover,
  SectionHeading,
  tabBody,
} from './shared'

const tag = 'rounded-[5px] px-1.5 text-[10.5px] leading-4'
const decisionTag: Record<RequirementInsightDecision['status'], string> = {
  confirmed: 'bg-positive-soft text-positive',
  proposed: 'bg-attention-soft text-attention',
  superseded: 'bg-raised text-muted',
  dismissed: 'bg-raised text-muted',
}

const tasks = (count: number) => `${count} ${count === 1 ? 'task' : 'tasks'}`

export function DocumentsTab({
  workspace,
  requirementId,
  onHover,
}: {
  workspace: string
  requirementId: string
  onHover: RowHover
}) {
  const documents = useQuery({
    queryKey: insightKey(workspace, requirementId, 'documents'),
    queryFn: ({ signal }) => fetchRequirementInsightDocuments(workspace, requirementId, signal),
    retry: insightRetry,
  })
  if (documents.isPending)
    return (
      <div className="space-y-2" aria-busy="true">
        <Skeleton className="h-12 w-full" />
        <Skeleton className="h-12 w-full" />
        <Skeleton className="h-12 w-full" />
      </div>
    )
  const data = documents.data
  if (documents.error || !data)
    return <Notice tone="failure">Documents unavailable: {insightErrorText(documents.error)}</Notice>

  const uncited = data.acceptance_criteria.filter((criterion) => criterion.citing_task_ids.length === 0)
  return (
    <div className={tabBody}>
      <section aria-label="System designs">
        <SectionHeading count={data.designs.length}>System designs</SectionHeading>
        {data.designs.length ? (
          data.designs.map((design) => <DesignRow key={design.id} design={design} onHover={onHover} />)
        ) : (
          <p className="text-xs text-muted">
            No System Design is attached to a serving task or names this requirement.
          </p>
        )}
      </section>

      <section aria-label="Decisions">
        <SectionHeading count={data.decisions.length}>Decisions</SectionHeading>
        {data.decisions.length ? (
          data.decisions.map((decision) => <DecisionRow key={decision.id} decision={decision} onHover={onHover} />)
        ) : (
          <p className="text-xs text-muted">No decision names, is cited by, or is cited for this requirement.</p>
        )}
      </section>

      <section aria-label="Acceptance criteria">
        <SectionHeading>Acceptance criteria</SectionHeading>
        {data.total ? (
          <>
            <ul className="m-0 flex list-none flex-wrap gap-[3px] p-0">
              {data.acceptance_criteria.map((criterion) => {
                const cited = criterion.citing_task_ids.length > 0
                return (
                  <li key={criterion.id} className="min-w-9 flex-1">
                    <button
                      type="button"
                      data-criterion={criterion.id}
                      data-cited={cited}
                      aria-label={`${criterion.id}: ${cited ? `cited by ${tasks(criterion.citing_task_ids.length)}` : 'not cited by a task'}`}
                      title={`${criterion.id}: ${criterion.statement}\n${cited ? `Cited by ${tasks(criterion.citing_task_ids.length)}` : 'Not cited by a task'}`}
                      {...hoverProps(onHover, criterion.citing_task_ids)}
                      className={cn(
                        'grid h-5 w-full place-items-center rounded-[3px] border font-mono text-[9.5px]',
                        cited
                          ? 'border-positive/40 bg-positive-soft text-positive'
                          : 'border-dashed border-attention-dot text-attention',
                      )}
                    >
                      {criterion.id.replace(/^AC-/, '')}
                    </button>
                  </li>
                )
              })}
            </ul>
            <p className="mt-1.5 text-[11px] text-muted">
              <span className="font-medium text-foreground">
                {data.cited_count} of {data.total} cited
              </span>{' '}
              by a task.{' '}
              {uncited.length > 0 &&
                `${uncited.map((criterion) => criterion.id).join(', ')} ${uncited.length === 1 ? 'is' : 'are'} not cited by any task.`}
            </p>
          </>
        ) : (
          <p className="text-xs text-muted">The requirement's current version states no acceptance criteria.</p>
        )}
      </section>

      <p className="mt-4 text-[11px] leading-relaxed text-faint">
        Hover a row to find the tasks it names on the map. A cited criterion is named in a task's body; citation is not
        satisfaction.
      </p>
    </div>
  )
}

function DesignRow({ design, onHover }: { design: RequirementInsightDesign; onHover: RowHover }) {
  return (
    <a
      href={`/system-design?document=${encodeURIComponent(design.id)}`}
      data-design={design.id}
      {...hoverProps(onHover, design.attached_task_ids)}
      className="group flex gap-2.5 rounded-lg p-2 hover:bg-raised focus-visible:bg-raised focus-visible:outline-none"
    >
      <span className="grid size-7 shrink-0 place-items-center rounded-md border border-border bg-card text-muted">
        <FileCode2 aria-hidden className="size-3.5" />
      </span>
      <span className="min-w-0 flex-1">
        <span className="block text-xs font-medium leading-snug text-foreground">{design.title}</span>
        <span className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-faint">
          <code className="font-mono text-[10.5px] text-muted">{design.id}</code>
          <span className={cn(tag, 'bg-positive-soft text-positive')}>v{design.current_version}</span>
          {design.revision_pending && (
            <span className={cn(tag, 'bg-attention-soft text-attention')}>Revision pending</span>
          )}
          <span>attached to {tasks(design.attached_task_count)}</span>
          {design.names_requirement && <span>names this requirement</span>}
        </span>
      </span>
      <ArrowUpRight aria-hidden className="size-3.5 shrink-0 self-center text-faint group-hover:text-foreground" />
    </a>
  )
}

function DecisionRow({ decision, onHover }: { decision: RequirementInsightDecision; onHover: RowHover }) {
  const [open, setOpen] = useState(false)
  const id = useId()
  const citing = decision.citing_task_ids.length
  return (
    <div className={cn('rounded-lg', open && 'bg-raised/60')}>
      <button
        type="button"
        data-decision={decision.id}
        aria-expanded={open}
        aria-controls={`${id}-details`}
        onClick={() => setOpen((value) => !value)}
        {...hoverProps(onHover, decision.citing_task_ids)}
        className="group flex w-full gap-2.5 rounded-lg p-2 text-left hover:bg-raised focus-visible:bg-raised focus-visible:outline-none"
      >
        <span className="grid size-7 shrink-0 place-items-center rounded-md border border-border bg-card text-muted">
          <Scale aria-hidden className="size-3.5" />
        </span>
        <span className="min-w-0 flex-1">
          <span className={cn('block text-xs font-medium leading-snug text-foreground', !open && 'line-clamp-2')}>
            {decision.statement}
          </span>
          <span className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-faint">
            <code className="font-mono text-[10.5px] text-muted">{decision.id}</code>
            <span className={cn(tag, 'capitalize', decisionTag[decision.status])}>{decision.status}</span>
            {decision.names_requirement && <span>names this requirement</span>}
            {decision.cited_by_requirement && <span>cited by the requirement</span>}
            {citing > 0 && <span>cited by {tasks(citing)}</span>}
          </span>
        </span>
        <ChevronDown
          aria-hidden
          className={cn('size-3.5 shrink-0 self-center text-faint transition-transform', open && 'rotate-180')}
        />
      </button>
      {open && (
        <dl id={`${id}-details`} className="m-0 space-y-1.5 px-2 pb-2.5 pl-[46px] text-xs">
          <div>
            <dt className="text-[10.5px] font-semibold uppercase tracking-wide text-faint">Statement</dt>
            <dd className="m-0 whitespace-pre-wrap text-markdown">{decision.statement}</dd>
          </div>
          <div>
            <dt className="text-[10.5px] font-semibold uppercase tracking-wide text-faint">Context</dt>
            <dd className="m-0 whitespace-pre-wrap text-markdown">{decision.context || 'None recorded.'}</dd>
          </div>
          <div>
            <dt className="text-[10.5px] font-semibold uppercase tracking-wide text-faint">Status</dt>
            <dd className="m-0 capitalize text-markdown">{decision.status}</dd>
          </div>
        </dl>
      )}
    </div>
  )
}
