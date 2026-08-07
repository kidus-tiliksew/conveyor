import { Link, useNavigate } from '@tanstack/react-router'
import { ChevronDown, ChevronUp, Maximize2, X } from 'lucide-react'
import type { ActivityItem } from '../../lib/types'
import { useCanonicalBlueprintRedirect } from '../blueprint/use-blueprint-route'
import { LineageGraphCard } from '../lineage/lineage-graph-card'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'
import { AttachmentsCard } from './attachments-card'
import { isReviewable } from './review-panel'
import { SpecCard } from './spec-card'
import { TaskHeader } from './task-header'
import { TaskContextCard } from './task-context-card'
import { Timeline } from './timeline'
import { useTaskDetail, useTaskOrder } from './use-task-detail'

// How a surface other than the board mounts this same composition as its own
// right panel (AC-2.2). The Tasks list supplies the order it is showing and its
// own open/close navigation; without it the panel would read the unfiltered
// board feed to find neighbours, which is exactly the whole-workspace load the
// list was rewritten to avoid (AC-2.3).
export interface TaskPanelSurface {
  order: string[]
  permalink: string
  close: () => void
  select: (taskId: string) => void
}

// The task detail sheet (spec §13.3): the costed event timeline — which
// carries the review actions as its live tail — opened over the board so the
// reviewer never loses list context. The Tasks list opens the same composition
// in its own panel rather than forking it (AC-2.2).
export function TaskSheet({ taskId, panel }: { taskId: string; panel?: TaskPanelSurface }) {
  const navigate = useNavigate()
  const { data: item, isLoading, error } = useTaskDetail(taskId)
  const boardOrder = useTaskOrder(taskId, !panel)
  const index = panel ? panel.order.indexOf(taskId) : -1
  const previousId = panel ? (index > 0 ? panel.order[index - 1] : undefined) : boardOrder.previousId
  const nextId = panel
    ? index >= 0 && index < panel.order.length - 1
      ? panel.order[index + 1]
      : undefined
    : boardOrder.nextId
  const close = panel ? panel.close : () => void navigate({ to: '/' })
  // A blueprint anchor has one home, and it is not this sheet (spec §21.49).
  const redirecting = useCanonicalBlueprintRedirect(item?.task)

  return (
    <Sheet onClose={close} label="Task detail">
      <header className="flex shrink-0 items-center gap-1 border-b border-border px-4 py-2.5">
        <span className="mr-auto truncate text-sm font-medium text-muted">{item?.task.title}</span>
        <SheetNavButton targetId={previousId} label="Previous task" icon={<ChevronUp />} panel={panel} />
        <SheetNavButton targetId={nextId} label="Next task" icon={<ChevronDown />} panel={panel} />
        {/* The panel's own address is shareable, so it is offered as one
            (AC-2.2); the full route stays one click away for the deep link. */}
        {panel && <CopyButton value={panel.permalink} label="Copy link to this task" />}
        <Link to="/tasks/$taskId/full" params={{ taskId }} aria-label="Open full task page">
          <Button variant="ghost" size="icon" tabIndex={-1}>
            <Maximize2 />
          </Button>
        </Link>
        <Button variant="ghost" size="icon" aria-label="Close panel" onClick={close}>
          <X />
        </Button>
      </header>
      <div className="task-sheet-body min-h-0 flex-1 overflow-x-hidden overflow-y-auto px-4 pb-8 pt-4">
        {(isLoading || redirecting) && (
          <div className="space-y-3">
            <Skeleton className="h-16" />
            <Skeleton className="h-40" />
            <Skeleton className="h-64" />
          </div>
        )}
        {error != null && <p className="text-sm text-failure">{String(error)}</p>}
        {item && !redirecting && <SheetBody item={item} />}
      </div>
    </Sheet>
  )
}

function SheetNavButton({
  targetId,
  label,
  icon,
  panel,
}: {
  targetId?: string
  label: string
  icon: React.ReactNode
  panel?: TaskPanelSurface
}) {
  if (!targetId) {
    return (
      <Button variant="ghost" size="icon" aria-label={label} disabled>
        {icon}
      </Button>
    )
  }
  // On a hosting surface, stepping through neighbours moves the panel rather
  // than leaving for the board's own sheet route.
  if (panel) {
    return (
      <Button variant="ghost" size="icon" aria-label={label} onClick={() => panel.select(targetId)}>
        {icon}
      </Button>
    )
  }
  return (
    <Link to="/tasks/$taskId" params={{ taskId: targetId }} aria-label={label}>
      <Button variant="ghost" size="icon" tabIndex={-1}>
        {icon}
      </Button>
    </Link>
  )
}

function SheetBody({ item }: { item: ActivityItem }) {
  return (
    <div className="space-y-4">
      <TaskHeader item={item} variant="sheet" />
      <TaskContextCard context={item.task.context} />
      {item.spec && (
        <SpecCard
          key={`${item.spec.task_id}-${item.spec.version}`}
          spec={item.spec}
          overflowExpandable
          routeVariant="sheet"
        />
      )}
      {/* While a gate is open the evidence belongs with the decision, and the
          gate card renders it there — showing it twice on one page does not. */}
      {!isReviewable(item.task) && (
        <AttachmentsCard attachments={item.verification_evidence ?? []} title="Verification evidence" />
      )}
      <AttachmentsCard attachments={item.attachments ?? []} />
      {item.lineage_graph && <LineageGraphCard graph={item.lineage_graph} />}
      <Timeline item={item} routeVariant="sheet" />
    </div>
  )
}
