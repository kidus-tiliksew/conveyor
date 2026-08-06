import { Link, useParams } from '@tanstack/react-router'
import { ArrowLeft, ChevronDown, ChevronUp } from 'lucide-react'
import type { ActivityItem } from '../lib/types'
import { useCanonicalBlueprintRedirect } from '../components/blueprint/use-blueprint-route'
import { LineageGraphCard } from '../components/lineage/lineage-graph-card'
import { AttachmentsCard } from '../components/task/attachments-card'
import { isReviewable } from '../components/task/review-panel'
import { SpecCard } from '../components/task/spec-card'
import { TaskHeader } from '../components/task/task-header'
import { TaskContextCard } from '../components/task/task-context-card'
import { Timeline } from '../components/task/timeline'
import { useTaskDetail, useTaskOrder } from '../components/task/use-task-detail'
import { Button } from '../components/ui/button'
import { Skeleton } from '../components/ui/skeleton'

// The full task page keeps its route header fixed while the task content uses
// one native scroll region. That region includes the task description so a
// long description cannot push the specification and activity out of reach.
export function TaskFullPage() {
  const { taskId } = useParams({ from: '/tasks/$taskId/full' })
  const { data: item, isLoading, error } = useTaskDetail(taskId)
  const { previousId, nextId } = useTaskOrder(taskId)
  // A blueprint anchor has one home, and it is not this page (spec §21.49).
  const redirecting = useCanonicalBlueprintRedirect(item?.task)

  return (
    <div className="flex h-full flex-col">
      <header className="flex shrink-0 items-center gap-1 border-b border-border px-4 py-2.5">
        <Link to="/tasks/$taskId" params={{ taskId }} aria-label="Back to board">
          <Button variant="ghost" size="icon" tabIndex={-1}>
            <ArrowLeft />
          </Button>
        </Link>
        <span className="mr-auto truncate text-sm font-medium text-muted">{item?.task.title}</span>
        <FullNavButton targetId={previousId} label="Previous task" icon={<ChevronUp />} />
        <FullNavButton targetId={nextId} label="Next task" icon={<ChevronDown />} />
      </header>
      {(isLoading || redirecting) && (
        <div className="space-y-3 px-6 py-6">
          <Skeleton className="h-24" />
          <Skeleton className="h-64" />
        </div>
      )}
      {error != null && <p className="px-6 py-6 text-sm text-failure">{String(error)}</p>}
      {item && !redirecting && <FullBody item={item} />}
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
  return (
    <section aria-label="Task content" className="min-h-0 flex-1 overflow-y-auto">
      <div className="shrink-0 border-b border-border px-6 py-4">
        <TaskHeader item={item} variant="full" />
      </div>
      <div className="grid grid-cols-1 lg:grid-cols-2">
        <section
          aria-label="Specification"
          className="space-y-4 border-b border-border px-6 py-4 lg:border-b-0 lg:border-r"
        >
          <TaskContextCard context={item.task.context} />
          {item.spec ? (
            <SpecCard spec={item.spec} collapsible={false} routeVariant="full" />
          ) : (
            <p className="rounded-lg border border-border bg-surface p-3 text-sm text-muted">
              No spec yet — the spec stage has not produced a version.
            </p>
          )}
          {/* While a gate is open the evidence belongs with the decision, and
              the gate card renders it there — not twice on one page. */}
          {!isReviewable(item.task) && (
            <AttachmentsCard attachments={item.verification_evidence ?? []} title="Verification evidence" />
          )}
          <AttachmentsCard attachments={item.attachments ?? []} />
        </section>
        <section aria-label="Activity" className="space-y-4 px-6 py-4">
          {item.lineage_graph && <LineageGraphCard graph={item.lineage_graph} />}
          <Timeline item={item} routeVariant="full" />
        </section>
      </div>
    </section>
  )
}
