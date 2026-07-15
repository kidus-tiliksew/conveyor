import { Link, useNavigate } from '@tanstack/react-router'
import { ChevronDown, ChevronUp, Maximize2, X } from 'lucide-react'
import type { ActivityItem } from '../../lib/types'
import { Button } from '../ui/button'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'
import { ReviewPanel } from './review-panel'
import { RedispatchCard, canRedispatch } from './redispatch-card'
import { SpecCard } from './spec-card'
import { TaskHeader } from './task-header'
import { Timeline } from './timeline'
import { useTaskDetail, useTaskOrder } from './use-task-detail'

// The task detail sheet (spec §13.3): the costed event history plus review
// actions, opened over the board so the reviewer never loses list context.
export function TaskSheet({ taskId }: { taskId: string }) {
  const navigate = useNavigate()
  const { data: item, isLoading, error } = useTaskDetail(taskId)
  const { previousId, nextId } = useTaskOrder(taskId)
  const close = () => void navigate({ to: '/' })

  return (
    <Sheet onClose={close} label="Task detail">
      <header className="flex shrink-0 items-center gap-1 border-b border-border px-4 py-2.5">
        <span className="mr-auto font-mono text-xs text-muted">{taskId}</span>
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
      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">
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
  const reviewable = item.needs_attention || item.task.state === 'approved'
  return (
    <div className="space-y-4">
      <TaskHeader item={item} variant="sheet" />
      {reviewable && <ReviewPanel item={item} />}
      {canRedispatch(item) && <RedispatchCard item={item} />}
      {item.spec && <SpecCard spec={item.spec} />}
      <Timeline item={item} />
    </div>
  )
}
