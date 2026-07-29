import { Link, useNavigate } from '@tanstack/react-router'
import { ChevronDown, ChevronUp, Maximize2, X } from 'lucide-react'
import type { ActivityItem } from '../../lib/types'
import { Button } from '../ui/button'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'
import { AttachmentsCard } from './attachments-card'
import { SpecCard } from './spec-card'
import { TaskHeader } from './task-header'
import { Timeline } from './timeline'
import { useTaskDetail, useTaskOrder } from './use-task-detail'

// The task detail sheet (spec §13.3): the costed event timeline — which
// carries the review actions as its live tail — opened over the board so the
// reviewer never loses list context.
export function TaskSheet({ taskId }: { taskId: string }) {
  const navigate = useNavigate()
  const { data: item, isLoading, error } = useTaskDetail(taskId)
  const { previousId, nextId } = useTaskOrder(taskId)
  const close = () => void navigate({ to: '/' })

  return (
    <Sheet onClose={close} label="Task detail">
      <header className="flex shrink-0 items-center gap-1 border-b border-border px-4 py-2.5">
        <span className="mr-auto truncate text-sm font-medium text-muted">{item?.task.title}</span>
        <SheetNavButton targetId={previousId} label="Previous task" icon={<ChevronUp />} />
        <SheetNavButton targetId={nextId} label="Next task" icon={<ChevronDown />} />
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
        {isLoading && (
          <div className="space-y-3">
            <Skeleton className="h-16" />
            <Skeleton className="h-40" />
            <Skeleton className="h-64" />
          </div>
        )}
        {error != null && <p className="text-sm text-failure">{String(error)}</p>}
        {item && <SheetBody item={item} />}
      </div>
    </Sheet>
  )
}

function SheetNavButton({ targetId, label, icon }: { targetId?: string; label: string; icon: React.ReactNode }) {
  if (!targetId) {
    return (
      <Button variant="ghost" size="icon" aria-label={label} disabled>
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
      {item.spec && <SpecCard key={`${item.spec.task_id}-${item.spec.version}`} spec={item.spec} overflowExpandable routeVariant="sheet" />}
      <AttachmentsCard attachments={item.verification_evidence ?? []} title="Verification evidence" />
      <AttachmentsCard attachments={item.attachments ?? []} />
      <Timeline item={item} />
    </div>
  )
}
