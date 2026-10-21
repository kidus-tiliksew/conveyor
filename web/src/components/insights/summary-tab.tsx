// The Summary tab (req-requirement-insights REQ-2; feature-requirement-insights
// RI-7): the stored generated headline, then the sections the server computes
// from the requirement's serving tasks, shown as served. The browser derives
// no next action, grouping, critical path, or forecast of its own (DEC-61).
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { RefreshCw, Sparkles } from 'lucide-react'
import { useEffect, useRef } from 'react'
import {
  fetchRequirementInsightSummary,
  generateRequirementInsightSummary,
  RequirementInsightError,
} from '../../lib/api'
import type { RequirementInsightSummary, RequirementInsightTask } from '../../lib/types'
import { absoluteTime, cn, relativeTime } from '../../lib/utils'
import { statusMeta, statusOf } from '../board/map/graph-model'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Skeleton } from '../ui/skeleton'
import {
  actorMeta,
  formatDate,
  formatNumber,
  hoverProps,
  insightErrorText,
  insightKey,
  insightRetry,
  Notice,
  type RowHover,
  SectionHeading,
  TaskRow,
  tabBody,
} from './shared'

export function SummaryTab({
  workspace,
  requirementId,
  onHover,
  onActivateTask,
}: {
  workspace: string
  requirementId: string
  onHover: RowHover
  onActivateTask: (id: string) => void
}) {
  const queryClient = useQueryClient()
  const key = insightKey(workspace, requirementId, 'summary')
  const summary = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => fetchRequirementInsightSummary(workspace, requirementId, signal),
    retry: insightRetry,
  })
  // A result lands on the key of the workspace and requirement that asked,
  // even when it arrives after the panel moved on. A 409 says the stored
  // headline is already current, so the read is refetched without a notice
  // (AC-2.6).
  const generation = useMutation({
    mutationFn: () => generateRequirementInsightSummary(workspace, requirementId),
    retry: false,
    onSuccess: (next) => queryClient.setQueryData<RequirementInsightSummary>(key, next),
    onError: (error) => {
      if (error instanceof RequirementInsightError && error.status === 409)
        void queryClient.invalidateQueries({ queryKey: key, exact: true })
    },
  })
  // Each opening of the tab decides once, on its first answered read,
  // whether to generate: only when no headline is stored and the member may
  // create tasks (AC-2.5). Refetches, rerenders, and StrictMode's repeated
  // effects within the opening never send a second request.
  const decided = useRef(false)
  const data = summary.data
  useEffect(() => {
    if (!data || decided.current) return
    decided.current = true
    if (data.headline === null && data.can_generate) generation.mutate()
  }, [data])

  if (summary.isPending) return <SummarySkeleton />
  if (summary.error || !data)
    return <Notice tone="failure">Summary unavailable: {insightErrorText(summary.error)}</Notice>

  const { headline } = data
  const failure =
    generation.error && !(generation.error instanceof RequirementInsightError && generation.error.status === 409)
      ? generation.error
      : undefined
  return (
    <div className={tabBody}>
      {headline ? (
        <section aria-label="Generated headline" className="mb-4">
          <div className="mb-2 flex items-start gap-2 text-[11px] text-faint">
            <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-1.5 gap-y-1 pt-0.5">
              <Sparkles aria-hidden className="size-3.5 text-primary" />
              <span className="font-medium text-muted">Generated · informative</span>
              <span>·</span>
              <span className="font-mono text-[10.5px]">{headline.model}</span>
              <span>·</span>
              <time dateTime={headline.generated_at} title={absoluteTime(headline.generated_at)}>
                {relativeTime(headline.generated_at)}
              </time>
              {!headline.current && <Badge className="py-0 text-[10.5px]">Out of date</Badge>}
            </div>
            <Button
              size="sm"
              variant="outline"
              className="h-6 shrink-0 px-2 text-[11px] [&_svg]:size-3"
              // Offered only to a member who may create tasks, and only once
              // the requirement's inputs have changed (AC-2.6).
              disabled={!data.can_generate || headline.current || generation.isPending}
              title={
                !data.can_generate
                  ? 'Regenerating needs permission to create tasks'
                  : headline.current
                    ? 'The headline is current'
                    : undefined
              }
              onClick={() => generation.mutate()}
            >
              <RefreshCw className={cn(generation.isPending && 'animate-spin')} />
              Regenerate
            </Button>
          </div>
          {/* Plain text: the headline is never rendered as markup. */}
          <p className={cn('text-[13px] leading-relaxed text-markdown', !headline.current && 'text-muted')}>
            {headline.text}
          </p>
        </section>
      ) : generation.isPending ? (
        <div className="mb-4 space-y-2" role="status" aria-label="Generating a summary">
          <p className="flex items-center gap-1.5 text-[11px] text-faint">
            <Sparkles aria-hidden className="size-3.5 animate-pulse text-primary" />
            Generating a summary…
          </p>
          <Skeleton className="h-3.5 w-full" />
          <Skeleton className="h-3.5 w-11/12" />
          <Skeleton className="h-3.5 w-3/5" />
        </div>
      ) : !data.can_generate ? (
        <div className="mb-4">
          <Notice>No summary exists yet. A member who may create tasks can generate one.</Notice>
        </div>
      ) : null}
      {failure && (
        <div className="mb-4">
          <Notice tone="failure">{generationFailure(failure)}</Notice>
        </div>
      )}

      {data.groups.map((group) => {
        const meta = actorMeta[group.actor] ?? actorMeta.none
        return (
          <section key={group.actor} aria-label={`${meta.label} · ${group.count}`}>
            <SectionHeading count={group.count}>
              <meta.Icon aria-hidden className={cn('size-3', meta.tone)} />
              {meta.label}
            </SectionHeading>
            {group.tasks.map((task) => (
              <TaskRow key={task.id} task={task} onHover={onHover} onActivate={onActivateTask} />
            ))}
          </section>
        )
      })}

      <section aria-label="Critical path">
        <SectionHeading>
          Critical path · {data.critical_path.length} {data.critical_path.length === 1 ? 'task' : 'tasks'}
        </SectionHeading>
        {data.critical_path.tasks.length ? (
          <ol className="ml-1.5 border-l border-dashed border-edge pl-3">
            {data.critical_path.tasks.map((task) => (
              <li key={task.id} className="relative">
                <ChainDot task={task} />
                <button
                  type="button"
                  data-task-row={task.id}
                  onClick={() => onActivateTask(task.id)}
                  {...hoverProps(onHover, [task.id])}
                  className="block w-full rounded-md px-1.5 py-1 text-left text-xs text-muted hover:bg-raised hover:text-foreground focus-visible:bg-raised focus-visible:outline-none"
                >
                  <span className="text-foreground">{task.title}</span>
                  <span className="ml-1.5 font-mono text-[10.5px] text-faint">{task.next_action.label}</span>
                </button>
              </li>
            ))}
          </ol>
        ) : (
          <p className="text-xs text-muted">No open serving tasks.</p>
        )}
      </section>

      <section aria-label="Forecast">
        <SectionHeading>Forecast</SectionHeading>
        <Forecast summary={data} />
      </section>

      <p className="mt-4 text-[11px] leading-relaxed text-faint">
        Hover a task to find it on the map; select it to trace it there. The headline is generated from task states and
        next actions. It is informative and never authority.
      </p>
    </div>
  )
}

// The chain's dot takes the map's treatment of the task's stated next
// action: filled once an agent can take it, outlined otherwise.
function ChainDot({ task }: { task: RequirementInsightTask }) {
  const status = statusOf({ next_action: task.next_action })
  return (
    <span
      aria-hidden
      className={cn(
        'absolute -left-[17px] top-[9px] size-2 rounded-full border-[1.5px] bg-surface',
        statusMeta[status].ring,
        (status === 'ready' || status === 'running') && statusMeta[status].dot,
      )}
    />
  )
}

function Forecast({ summary }: { summary: RequirementInsightSummary }) {
  const { forecast } = summary
  if (!forecast)
    return (
      <p className="rounded-lg border border-border bg-card px-3 py-2 text-xs text-muted">
        No forecast exists: no serving task was delivered in the last 14 days.
      </p>
    )
  const weekly = formatNumber(forecast.rate_per_day * 7)
  return (
    <p className="rounded-lg border border-border bg-card px-3 py-2 text-xs leading-relaxed text-markdown">
      At the last 14 days' rate of{' '}
      <b className="font-semibold text-foreground">
        {weekly} {weekly === '1' ? 'task' : 'tasks'} a week
      </b>
      , the open tasks finish around <b className="font-semibold text-foreground">{formatDate(forecast.day)}</b>.
    </p>
  )
}

// Names the failure (AC-2.7): the server's error code and its redacted
// detail.
function generationFailure(error: Error) {
  if (error instanceof RequirementInsightError) {
    if (error.code === 'insight_model_unconfigured')
      return `No model is configured to generate summaries (${error.code}).`
    if (error.code === 'insight_generation_failed')
      return `Summary generation failed (${error.code})${error.detail ? `: ${error.detail}` : '.'}`
    return `Summary generation failed (${error.code ?? `HTTP ${error.status}`}): ${error.message}`
  }
  return `Summary generation failed: ${error.message}`
}

function SummarySkeleton() {
  return (
    <div className="space-y-2" aria-busy="true">
      <Skeleton className="h-3.5 w-2/5" />
      <Skeleton className="h-3.5 w-full" />
      <Skeleton className="h-3.5 w-11/12" />
      <Skeleton className="mt-5 h-8 w-full" />
      <Skeleton className="h-8 w-full" />
    </div>
  )
}
