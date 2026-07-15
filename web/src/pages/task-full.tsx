import { Link, useParams } from '@tanstack/react-router'
import { ArrowLeft, ChevronDown, ChevronUp } from 'lucide-react'
import type { ActivityItem } from '../lib/types'
import { ReviewPanel } from '../components/task/review-panel'
import { RedispatchCard, canRedispatch } from '../components/task/redispatch-card'
import { SpecCard } from '../components/task/spec-card'
import { TaskHeader } from '../components/task/task-header'
import { Timeline } from '../components/task/timeline'
import { useTaskDetail, useTaskOrder } from '../components/task/use-task-detail'
import { WorkOrders } from '../components/task/work-orders'
import { Button } from '../components/ui/button'
import { Skeleton } from '../components/ui/skeleton'

// The full task page: the specification and the audit timeline side by side,
// each with its own scroll, under a facts header strip — the layout for
// reading a big spec against a long event history without one burying the
// other.
export function TaskFullPage() {
  const { taskId } = useParams({ from: '/tasks/$taskId/full' })
  const { data: item, isLoading, error } = useTaskDetail(taskId)
  const { previousId, nextId } = useTaskOrder(taskId)

  return (
    <div className="flex h-full flex-col">
      <header className="flex shrink-0 items-center gap-1 border-b border-border px-4 py-2.5">
        <Link to="/tasks/$taskId" params={{ taskId }} aria-label="Back to board">
          <Button variant="ghost" size="icon" tabIndex={-1}>
            <ArrowLeft />
          </Button>
        </Link>
        <span className="mr-auto font-mono text-xs text-muted">{taskId}</span>
        <FullNavButton targetId={previousId} label="Previous task" icon={<ChevronUp />} />
        <FullNavButton targetId={nextId} label="Next task" icon={<ChevronDown />} />
      </header>
      {isLoading && (
        <div className="space-y-3 px-6 py-6">
          <Skeleton className="h-24" />
          <Skeleton className="h-64" />
        </div>
      )}
      {error != null && <p className="px-6 py-6 text-sm text-failure">{String(error)}</p>}
      {item && <FullBody item={item} />}
    </div>
  )
}

function FullNavButton({ targetId, label, icon }: { targetId?: string; label: string; icon: React.ReactNode }) {
  if (!targetId) {
    return (
      <Button variant="ghost" size="icon" aria-label={label} disabled>
        {icon}
      </Button>
    )
  }
  return (
    <Link to="/tasks/$taskId/full" params={{ taskId: targetId }} aria-label={label}>
      <Button variant="ghost" size="icon" tabIndex={-1}>
        {icon}
      </Button>
    </Link>
  )
}

function FullBody({ item }: { item: ActivityItem }) {
  const reviewable = item.needs_attention || item.task.state === 'approved'
  return (
    <>
      <div className="shrink-0 border-b border-border px-6 py-4">
        <TaskHeader item={item} variant="full" />
      </div>
      <div className="grid min-h-0 flex-1 grid-cols-1 lg:grid-cols-2">
        <section aria-label="Specification" className="min-h-0 overflow-y-auto border-b border-border px-6 py-4 lg:border-b-0 lg:border-r">
          {item.spec ? (
            <SpecCard spec={item.spec} collapsible={false} />
          ) : (
            <p className="rounded-lg border border-border bg-surface p-3 text-sm text-muted">No spec yet — the spec stage has not produced a version.</p>
          )}
        </section>
        <section aria-label="Activity" className="min-h-0 space-y-4 overflow-y-auto px-6 py-4">
          {reviewable && <ReviewPanel item={item} />}
          <WorkOrders orders={item.work_orders ?? []} />
          {canRedispatch(item) && <RedispatchCard item={item} />}
          <Timeline item={item} />
        </section>
      </div>
    </>
  )
}
