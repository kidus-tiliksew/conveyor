// Pieces the requirement insight tabs share (feature-requirement-insights
// RI-7): the query keys, the actor presentation, UTC day formatting, and the
// task row whose hover names its task to the map.
import { Ban, CircleDashed, Cog, Hand, type LucideIcon, Play } from 'lucide-react'
import type { FocusEvent, PointerEvent, ReactNode } from 'react'
import { RequirementInsightError } from '../../lib/api'
import type { NextActionActor, RequirementInsightTask } from '../../lib/types'
import { cn } from '../../lib/utils'
import { statusMeta, statusOf } from '../board/map/graph-model'
import { statusIcon } from '../board/map/task-node'

// Every tab's read is keyed by workspace and requirement, so a workspace
// switch or a late answer never lands in another requirement's panel.
export function insightKey(workspace: string, requirementId: string, view: 'summary' | 'progress' | 'documents') {
  return ['requirement-insights', workspace, requirementId, view] as const
}

// A refused read is not retried; a transport or server failure is retried
// once, as the dashboard's other reads are.
export function insightRetry(count: number, error: unknown) {
  if (error instanceof RequirementInsightError && error.status < 500) return false
  return count < 1
}

export function insightErrorText(error: unknown) {
  if (error instanceof RequirementInsightError && error.code === 'requirement_not_found')
    return 'This requirement has no confirmed version to show insights for.'
  return error instanceof Error ? error.message : String(error)
}

// The next-action actors in the order the server sends groups and the
// status mix (AC-2.1, AC-3.3), each with the map's own icon and token.
export const actorMeta: Record<NextActionActor, { label: string; Icon: LucideIcon; tone: string; fill: string }> = {
  operator: { label: 'Operator', Icon: Hand, tone: 'text-attention', fill: 'bg-attention-dot' },
  agent: { label: 'Agent', Icon: Play, tone: 'text-primary', fill: 'bg-primary' },
  factory: { label: 'Factory', Icon: Cog, tone: 'text-muted', fill: 'bg-muted' },
  dependency: { label: 'Dependency', Icon: Ban, tone: 'text-failure', fill: 'bg-edge' },
  none: { label: 'No actor', Icon: CircleDashed, tone: 'text-faint', fill: 'bg-raised' },
}

// Days and weeks are UTC calendar days (RI-3), so they are formatted in UTC
// rather than regrouped in the viewer's zone.
const dayFormat = new Intl.DateTimeFormat('en', { month: 'short', day: 'numeric', timeZone: 'UTC' })
const dateFormat = new Intl.DateTimeFormat('en', { month: 'short', day: 'numeric', year: 'numeric', timeZone: 'UTC' })
export const formatDay = (value: string) => dayFormat.format(new Date(value))
export const formatDate = (value: string) => dateFormat.format(new Date(value))

// A number with at most one decimal place, without a trailing ".0".
export function formatNumber(value: number) {
  return Number.isFinite(value) ? String(Math.round(value * 10) / 10) : '0'
}

// A cycle time in the largest unit that reads, with its unit apart.
export function formatDuration(seconds: number): { value: string; unit: string } {
  if (seconds >= 86_400) return { value: formatNumber(seconds / 86_400), unit: 'd' }
  if (seconds >= 3_600) return { value: formatNumber(seconds / 3_600), unit: 'h' }
  return { value: formatNumber(Math.max(0, seconds) / 60), unit: 'm' }
}

// What a row names to the map while the pointer or keyboard focus rests on
// it (req-requirement-insights AC-1.3).
export type RowHover = (taskIds: string[] | undefined) => void

export function hoverProps(onHover: RowHover, taskIds: string[]) {
  return {
    onPointerEnter: (_event: PointerEvent) => onHover(taskIds),
    onPointerLeave: (_event: PointerEvent) => onHover(undefined),
    onFocus: (_event: FocusEvent) => onHover(taskIds),
    onBlur: (_event: FocusEvent) => onHover(undefined),
  }
}

// The tab body's first heading sits at the top without the gap the others
// take.
export const tabBody = '[&>section:first-child>h3]:mt-0'

export function SectionHeading({ children, count }: { children: ReactNode; count?: number }) {
  return (
    <h3 className="mb-1.5 mt-5 flex items-center gap-1.5 text-[11px] font-semibold uppercase tracking-wide text-faint">
      {children}
      {count !== undefined && (
        <span className="rounded-md bg-raised px-1.5 font-mono text-[10.5px] font-normal tracking-normal text-muted">
          {count}
        </span>
      )}
    </h3>
  )
}

// One serving task: the map's status icon for its stated next action, its
// title, and the action's label verbatim (DEC-61). Activating it selects the
// task on the map (AC-1.4).
export function TaskRow({
  task,
  onHover,
  onActivate,
}: {
  task: RequirementInsightTask
  onHover: RowHover
  onActivate: (id: string) => void
}) {
  const status = statusOf({ next_action: task.next_action })
  const Icon = statusIcon[status]
  return (
    <button
      type="button"
      data-task-row={task.id}
      onClick={() => onActivate(task.id)}
      {...hoverProps(onHover, [task.id])}
      className="flex w-full items-start gap-2 rounded-md px-2 py-1.5 text-left hover:bg-raised focus-visible:bg-raised focus-visible:outline-none"
    >
      <Icon aria-hidden className={cn('mt-0.5 size-3.5 shrink-0', statusMeta[status].tone)} />
      <span className="min-w-0 flex-1">
        <span className="line-clamp-2 text-[12.5px] font-medium leading-snug text-foreground">{task.title}</span>
        <span className="mt-0.5 block text-[11px] text-muted">
          {task.next_action.label}
          <span className="font-mono text-faint"> · {task.id}</span>
        </span>
      </span>
    </button>
  )
}

// A notice beside the computed sections, such as a generation failure.
export function Notice({ tone = 'muted', children }: { tone?: 'muted' | 'failure'; children: ReactNode }) {
  return (
    <div
      role={tone === 'failure' ? 'alert' : 'status'}
      className={cn(
        'rounded-lg border px-3 py-2 text-xs leading-relaxed',
        tone === 'failure' ? 'border-failure/30 bg-failure-soft text-failure' : 'border-border bg-card text-muted',
      )}
    >
      {children}
    </div>
  )
}
