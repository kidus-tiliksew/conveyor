import { Link, useNavigate } from '@tanstack/react-router'
import { ChevronDown, ChevronUp, Maximize2, X } from 'lucide-react'
import type { ActivityItem } from '../../lib/types'
import { useCanonicalBlueprintRedirect } from '../blueprint/use-blueprint-route'
import { LineageExplorer } from '../lineage/lineage-explorer'
import { Button } from '../ui/button'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'
import { AttachmentsCard } from './attachments-card'
import { isReviewable } from './review-panel'
import { SpecCard } from './spec-card'
import { TaskActivityTabs } from './task-activity-tabs'
import { TaskContextCard } from './task-context-card'
import { TaskHeader } from './task-header'
import { useTaskDetail, useTaskOrder } from './use-task-detail'

// The task detail sheet: the costed event timeline — which
// carries the review actions as its live tail — opened over the board so the
// reviewer never loses list context. The Board's list view opens it the same
// way, and closing returns to whichever view the Board was showing.
export function TaskSheet({ taskId }: { taskId: string }) {
  const navigate = useNavigate()
  const { data: item, isLoading, error } = useTaskDetail(taskId)
  const boardOrder = useTaskOrder(taskId)
  const previousId = boardOrder.previousId
  const nextId = boardOrder.nextId
  const close = () => void navigate({ to: '/', search: (prev) => prev })
  // A blueprint anchor has one home, and it is not this sheet.
  const redirecting = useCanonicalBlueprintRedirect(item?.task)

  return (
    <Sheet onClose={close} label="Task detail">
      <header className="pointer-coarse:[&_button]:min-h-10 pointer-coarse:[&_button]:min-w-10 flex shrink-0 flex-wrap items-center gap-1 border-b border-border px-4 py-2.5">
        {/* The title takes whatever width the fixed-size controls leave and
            truncates; the controls never shrink under their own content. */}
        {/* A 10rem basis keeps the title readable: the controls wrap before
            they squeeze it. */}
        <span className="min-w-0 flex-1 basis-40 truncate text-sm font-medium text-muted">{item?.task.title}</span>
        {/* The same corner affordance the full route carries (REQ-3): the
            panel opens over this one without dismissing it. */}
        <LineageExplorer type="task" id={taskId} />
        {boardOrder.windowEdge && (
          // Below sm the note takes its own line under the controls so it
          // never squeezes the title out of the header.
          <span
            className="max-w-56 text-right text-[10px] leading-4 text-faint max-sm:order-last max-sm:max-w-none max-sm:basis-full max-sm:text-left"
            role="note"
          >
            {boardOrder.windowEdge}
          </span>
        )}
        <SheetNavButton targetId={previousId} label="Previous task" icon={<ChevronUp />} />
        <SheetNavButton targetId={nextId} label="Next task" icon={<ChevronDown />} />
        <Link to="/tasks/$taskId/full" params={{ taskId }} aria-label="Open full task page" className="shrink-0">
          <Button variant="ghost" size="icon" tabIndex={-1}>
            <Maximize2 />
          </Button>
        </Link>
        <Button variant="ghost" size="icon" className="shrink-0" aria-label="Close panel" onClick={close}>
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

function SheetNavButton({ targetId, label, icon }: { targetId?: string; label: string; icon: React.ReactNode }) {
  if (!targetId) {
    return (
      <Button variant="ghost" size="icon" className="shrink-0" aria-label={label} disabled>
        {icon}
      </Button>
    )
  }
  return (
    <Link
      to="/tasks/$taskId"
      params={{ taskId: targetId }}
      search={(prev) => prev}
      aria-label={label}
      className="shrink-0"
    >
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
      <TaskContextCard taskId={item.task.id} taskState={item.task.state} context={item.task.context} />
      {item.spec && (
        <SpecCard
          key={`${item.spec.task_id}-${item.spec.version}`}
          spec={item.spec}
          overflowExpandable
          routeVariant="sheet"
          minimumSeatCount={item.summaries?.review_seat_policy?.minimum_seat_count}
        />
      )}
      {/* While a gate is open the evidence belongs with the decision, and the
          gate card renders it there — showing it twice on one page does not. */}
      {!isReviewable(item) && (
        <AttachmentsCard attachments={item.verification_evidence ?? []} title="Verification evidence" />
      )}
      <AttachmentsCard attachments={item.attachments ?? []} />
      {/* Relationships are read in the explorer panel, not inline: the panel
          is the one rendering of this task's lineage. */}
      <TaskActivityTabs item={item} routeVariant="sheet" />
    </div>
  )
}
