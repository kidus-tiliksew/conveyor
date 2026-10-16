import { Link } from '@tanstack/react-router'
import { Workflow } from 'lucide-react'
import { useId } from 'react'
import { humanizeClaimRefusal, reviewDiagnosticBadge, taskWasRestarted } from '../../lib/activity'
import { useShowStageProgress } from '../../lib/board-preferences'
import type { ActivitySummary } from '../../lib/types'
import { cn, relativeTime } from '../../lib/utils'
import { useWorkspaceMembers } from '../app-shell'
import { AssigneeChip } from '../task/assignee-chip'
import { Badge } from '../ui/badge'
import { StageStripe } from './stage-stripe'

export type CardStatusTone = 'failure' | 'operator' | 'working' | 'merging' | 'waiting'

// The status mark's tone, first match wins (feature-operator-next-action NA-6):
// a recorded failure outranks the actor, so a stalled operator item still reads
// as a failure. A deliberate checkpoint is the exception: its release message
// is not a failure, so an agent question reads as the operator's. The tone maps
// the server's actor and kind; it derives no actor, kind, or reason of its own
// (AC-1.7).
export function cardStatusTone(item: ActivitySummary): CardStatusTone {
  const action = item.next_action
  if (item.forge_failure || (item.stalled?.last_failure && !isAgentQuestion(item))) return 'failure'
  if (action?.actor === 'operator') return 'operator'
  if ((action?.actor === 'agent' || action?.actor === 'factory') && action.kind === 'working') return 'working'
  if (action?.kind === 'merging') return 'merging'
  return 'waiting'
}

// A checkpoint release also matches the stalled row, which the server records
// only as a secondary signal; the next action's label and summary state the
// pause, so no surface prints its release message as a failure.
export function isAgentQuestion(item: { next_action?: { kind: string } }) {
  return item.next_action?.kind === 'agent_question'
}

const statusMarks: Record<CardStatusTone, string> = {
  failure: 'bg-failure',
  operator: 'bg-attention-dot',
  working: 'bg-primary',
  merging: 'bg-positive',
  waiting: 'border-[1.5px] border-faint bg-transparent',
}

const statusLabels: Record<CardStatusTone, string> = {
  failure: 'text-failure',
  operator: 'font-medium text-attention',
  working: 'text-foreground',
  merging: 'text-positive',
  waiting: 'text-muted',
}

const tooltip =
  'pointer-events-none absolute z-20 rounded-md bg-foreground px-2.5 py-1.5 text-[11px] leading-4 text-background opacity-0 shadow-md transition-opacity'

// One board card (req-operator-next-action AC-3.1): the title, then one row
// holding the server's next-action label behind a status mark, the recency, and
// the assignee. The column already names the stage, so the card carries no
// stage stripe unless the viewer opts in (AC-3.3), and the summary sentence
// opens on hover or focus instead of repeating down every card. Only the mark
// and label carry colour; an operator item is never tinted, so a column of
// decisions stays calm and the column header counts them.
export function TaskCard({ item, selected }: { item: ActivitySummary; selected: boolean }) {
  const { data: members = [] } = useWorkspaceMembers()
  const showStageProgress = useShowStageProgress()
  const summaryId = useId()
  const lastAt = item.last_event_at || item.task.created_at
  const action = item.next_action?.kind === 'terminal' ? undefined : item.next_action
  const operator = action?.actor === 'operator'
  const tone = cardStatusTone(item)
  const reviewDiagnostic = reviewDiagnosticBadge(item)
  const unsatisfiable = item.stalled?.unsatisfiable_edge === true
  const blockingIDs = item.stalled?.blocking_task_ids ?? item.task.blocking_task_ids ?? []
  const blockingTitles = blockingIDs.map(
    (id) => item.task.dependencies?.find((dependency) => dependency.id === id)?.title ?? id,
  )
  const restarted = taskWasRestarted(item.task)
  const chips = Boolean(restarted || reviewDiagnostic)
  return (
    <Link
      to="/tasks/$taskId"
      params={{ taskId: item.task.id }}
      aria-describedby={action?.summary ? summaryId : undefined}
      className={cn(
        'group/card relative block rounded-lg border bg-card p-3 transition-colors',
        selected ? 'border-primary bg-primary-soft/40' : 'border-border hover:border-edge',
      )}
      data-operator={operator || undefined}
    >
      <div className="flex items-start gap-2">
        <p className="line-clamp-2 min-w-0 flex-1 text-sm font-medium leading-snug">{item.task.title}</p>
        {blockingIDs.length > 0 && <DependencyMark titles={blockingTitles} unsatisfiable={unsatisfiable} />}
      </div>
      {showStageProgress && <StageStripe taskId={item.task.id} />}
      <div className="mt-2 flex min-w-0 items-center gap-2 text-xs">
        {action && (
          <>
            <span
              aria-hidden
              data-status-mark={tone}
              className={cn('size-2 shrink-0 rounded-full', statusMarks[tone])}
            />
            <span data-next-action-label className={cn('min-w-0 truncate', statusLabels[tone])}>
              {action.label}
            </span>
          </>
        )}
        <span className="ml-auto shrink-0 whitespace-nowrap font-mono text-[11px] text-faint">
          {relativeTime(lastAt)}
        </span>
        {/* The same chip the Tasks rows carry, so a colleague's assignment reads
            identically on both surfaces (REQ-4, AC-4.3). */}
        {item.task.assignee && (
          <AssigneeChip assignee={item.task.assignee} compact className="shrink-0 text-[11px] text-muted" />
        )}
      </div>
      {chips && (
        <div className="mt-2 flex flex-wrap items-center gap-1.5">
          {restarted && <Badge variant="mono">restarted</Badge>}
          {reviewDiagnostic && <Badge variant={reviewDiagnostic.variant}>{reviewDiagnostic.label}</Badge>}
        </div>
      )}
      {item.stalled?.last_failure && !isAgentQuestion(item) && (
        <p className="mt-2 line-clamp-2 text-[11px] leading-4 text-failure">
          {humanizeClaimRefusal(item.stalled.last_failure, item.task.assignee, members)}
        </p>
      )}
      {item.forge_failure && (
        <p className="mt-2 line-clamp-2 text-[11px] leading-4 text-failure">
          {item.forge_failure.category && (
            <>
              <span className="font-mono">{item.forge_failure.category}</span>
              {' · '}
            </>
          )}
          {item.forge_failure.surface}: {item.forge_failure.detail}
        </p>
      )}
      {action?.summary && (
        // Hovering the dependency mark shows its own tooltip, so the summary
        // steps aside rather than stacking two over one card.
        // aria-hidden keeps the summary out of the link's name; the link still
        // reads it as its description through aria-describedby.
        <span
          id={summaryId}
          role="tooltip"
          aria-hidden="true"
          data-next-action-summary
          className={cn(
            tooltip,
            'left-2 right-2 top-full mt-1 group-focus-visible/card:opacity-100 group-hover/card:opacity-100 group-has-[[data-dependency-mark]:hover]/card:opacity-0',
          )}
        >
          {action.summary}
        </span>
      )}
    </Link>
  )
}

// Waiting on other tasks reads as one mark beside the title; the blockers are
// named on hover or focus (AC-3.4). A blocker that closed without merging
// takes the attention tone, since the task cannot proceed until someone acts.
function DependencyMark({ titles, unsatisfiable }: { titles: string[]; unsatisfiable: boolean }) {
  const explanation = unsatisfiable
    ? `Needs attention: ${titles.join(', ')} closed without merging`
    : `Waiting for ${titles.join(', ')}`
  return (
    <span
      role="img"
      aria-label={explanation}
      data-dependency-mark={unsatisfiable ? 'unsatisfiable' : 'waiting'}
      className={cn(
        'group/dependency relative mt-0.5 inline-flex shrink-0',
        unsatisfiable ? 'text-attention' : 'text-faint',
      )}
    >
      <Workflow aria-hidden className="size-[15px]" />
      <span
        role="tooltip"
        className={cn(
          tooltip,
          'right-0 top-full mt-1.5 w-60 group-hover/dependency:opacity-100 group-focus-visible/card:opacity-100',
        )}
      >
        {unsatisfiable ? (
          explanation
        ) : (
          <>
            <span className="block font-semibold">Waits on</span>
            {titles.map((title) => (
              <span key={title} className="block">
                {title}
              </span>
            ))}
          </>
        )}
      </span>
    </span>
  )
}
