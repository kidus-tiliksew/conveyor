import { Link, useParams } from '@tanstack/react-router'
import { ArrowLeft, ChevronDown, ChevronUp } from 'lucide-react'
import { useCanonicalBlueprintRedirect } from '../components/blueprint/use-blueprint-route'
import { LineageExplorer } from '../components/lineage/lineage-explorer'
import { AttachmentsCard } from '../components/task/attachments-card'
import { isReviewable } from '../components/task/review-panel'
import { SectionHeading } from '../components/task/section-heading'
import { SpecCard } from '../components/task/spec-card'
import { TaskActivityTabs } from '../components/task/task-activity-tabs'
import { TaskContextCard } from '../components/task/task-context-card'
import { TaskAgentPrompt, TaskFacts, TaskIdentity } from '../components/task/task-header'
import { useTaskDetail, useTaskOrder } from '../components/task/use-task-detail'
import { Button } from '../components/ui/button'
import { Skeleton } from '../components/ui/skeleton'
import type { ActivityItem } from '../lib/types'
import { cn } from '../lib/utils'

// The full task page keeps its route header fixed while the task content uses
// one native scroll region. From lg up the content is a reading column beside
// an inspector; below lg it is one column in DOM order
// (component-web-task-surfaces, Task detail composition).
export function TaskFullPage() {
  const { taskId } = useParams({ from: '/tasks/$taskId/full' })
  const { data: item, isLoading, error } = useTaskDetail(taskId)
  const { previousId, nextId, windowEdge } = useTaskOrder(taskId)
  // A blueprint anchor has one home, and it is not this page.
  const redirecting = useCanonicalBlueprintRedirect(item?.task)

  return (
    <div className="flex h-full flex-col">
      <header className="flex shrink-0 flex-wrap items-center gap-1 border-b border-border px-4 py-2.5">
        <Link to="/tasks/$taskId" params={{ taskId }} aria-label="Back to board" className="shrink-0">
          <Button variant="ghost" size="icon" tabIndex={-1}>
            <ArrowLeft />
          </Button>
        </Link>
        {/* A breadcrumb, not a second title: the title reads once, in the
            reading column. */}
        <nav aria-label="Breadcrumb" className="flex min-w-0 flex-1 basis-40 items-center gap-1.5 text-sm text-muted">
          <span className="shrink-0">Board</span>
          <span aria-hidden className="shrink-0 text-edge">
            /
          </span>
          <span className="min-w-0 truncate font-mono text-xs text-foreground" aria-current="page">
            {taskId}
          </span>
        </nav>
        {/* The corner affordance REQ-3 asks for: what this task connects to is
            one read away, and it costs nothing until it is opened. */}
        <LineageExplorer type="task" id={taskId} />
        {windowEdge && (
          // Below sm the note takes its own line under the controls so it
          // never squeezes the title out of the header.
          <span
            className="max-w-56 text-right text-[10px] leading-4 text-faint max-sm:order-last max-sm:max-w-none max-sm:basis-full max-sm:text-left"
            role="note"
          >
            {windowEdge}
          </span>
        )}
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
      <Button variant="ghost" size="icon" className="shrink-0" aria-label={label} disabled>
        {icon}
      </Button>
    )
  }
  return (
    <Link to="/tasks/$taskId/full" params={{ taskId: targetId }} aria-label={label} className="shrink-0">
      <Button variant="ghost" size="icon" tabIndex={-1}>
        {icon}
      </Button>
    </Link>
  )
}

// The reading column keeps a measure of about 760 px however wide the page is.
const readingMeasure = 'w-full min-w-0 lg:mx-auto lg:max-w-[47.5rem]'

// One grid, each section mounted once. Below lg the DOM order is the reading
// order: identity, properties, plan, context and the other inspector
// sections, then activity. From lg up the header row pairs identity with the
// properties, and the remaining inspector sections span the plan and activity
// rows, so neither column waits on the other's height.
function FullBody({ item }: { item: ActivityItem }) {
  return (
    <section
      aria-label="Task content"
      className="min-h-0 flex-1 overflow-y-auto lg:bg-local lg:bg-[linear-gradient(to_left,var(--color-surface)_23rem,var(--color-border)_23rem,var(--color-border)_calc(23rem+1px),transparent_calc(23rem+1px))]"
    >
      {/* The inspector rail is the scroll region's own background, attached
          locally so it spans the whole scroll height: the 20rem inspector, the
          1.5rem page padding, and half the 3rem column gap, then a 1px border. */}
      <div className="grid grid-cols-1 gap-y-5 px-4 pt-5 pb-6 sm:px-6 lg:grid-cols-[minmax(0,1fr)_20rem] lg:grid-rows-[auto_auto_1fr] lg:gap-x-12 lg:gap-y-5 lg:pt-6">
        <div className="grid grid-cols-1 gap-y-5 lg:col-span-2 lg:grid-cols-[minmax(0,1fr)_20rem] lg:gap-x-12">
          <div className={readingMeasure}>
            <TaskIdentity item={item} variant="full" />
          </div>
          <section aria-label="Properties" className="min-w-0 lg:pr-0">
            <SectionHeading as="h2" className="mb-1 text-xs font-medium text-faint">
              Properties
            </SectionHeading>
            <TaskFacts item={item} variant="full" columns="one" className="mt-0 gap-y-1.5 border-y-0 py-1" />
          </section>
        </div>
        <section aria-label="Execution plan" className={cn(readingMeasure, 'lg:col-start-1 lg:row-start-2')}>
          {item.spec ? (
            <SpecCard spec={item.spec} collapsible={false} overflowExpandable routeVariant="full" />
          ) : (
            <p className="rounded-lg border border-border bg-surface p-3 text-sm text-muted">
              No execution plan yet — the planning stage has not produced a version.
            </p>
          )}
        </section>
        <section aria-label="Task inspector" className="min-w-0 space-y-5 lg:col-start-2 lg:row-span-2 lg:row-start-2">
          <TaskContextCard taskId={item.task.id} taskState={item.task.state} context={item.task.context} compact />
          <TaskAgentPrompt item={item} />
          <AttachmentsCard attachments={item.attachments ?? []} />
          {/* While a gate is open the evidence belongs with the decision, and
              the gate card renders it there — not twice on one page. */}
          {!isReviewable(item) && (
            <AttachmentsCard attachments={item.verification_evidence ?? []} title="Verification evidence" />
          )}
        </section>
        {/* The static relationship graph is gone: relationships are read in
            the explorer panel, and one page does not carry two renderings of
            the same lineage. */}
        <section aria-label="Activity" className={cn(readingMeasure, 'space-y-4 lg:col-start-1 lg:row-start-3')}>
          <TaskActivityTabs item={item} routeVariant="full" />
        </section>
      </div>
    </section>
  )
}
