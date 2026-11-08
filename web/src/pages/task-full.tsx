import { Link, useParams } from '@tanstack/react-router'
import { ArrowLeft, ChevronDown, ChevronUp } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useWorkspaceSelection } from '../components/app-shell'
import { useCanonicalBlueprintRedirect } from '../components/blueprint/use-blueprint-route'
import { LineageExplorer } from '../components/lineage/lineage-explorer'
import { AttachmentsCard } from '../components/task/attachments-card'
import { NextActionCard } from '../components/task/next-action-card'
import { isReviewable } from '../components/task/review-panel'
import { SectionHeading } from '../components/task/section-heading'
import { SpecCard } from '../components/task/spec-card'
import { TaskActivityTabs, useTaskActivityView } from '../components/task/task-activity-tabs'
import { TaskContextCard } from '../components/task/task-context-card'
import { TaskAgentPrompt, TaskFacts, TaskIdentity } from '../components/task/task-header'
import { TaskLoadError } from '../components/task/task-load-error'
import { isTaskNotFound, useTaskDetail, useTaskOrder } from '../components/task/use-task-detail'
import { Button } from '../components/ui/button'
import { Skeleton } from '../components/ui/skeleton'
import type { ActivityItem } from '../lib/types'
import { cn } from '../lib/utils'

// The full task page keeps its route header fixed while the task content uses
// one native scroll region. From lg up the content is a reading column beside
// an inspector; below lg it is one column in DOM order
// (component-web-task-surfaces, Task detail composition).
export function TaskFullPage() {
  const { workspace } = useWorkspaceSelection()
  const { taskId } = useParams({ from: '/w/$workspaceId/tasks/$taskId/full' })
  const { data: item, isLoading, error, refetch, isFetching } = useTaskDetail(taskId)
  const { previousId, nextId, windowEdge } = useTaskOrder(taskId)
  const notFound = !item && error != null && isTaskNotFound(error)
  // A blueprint anchor has one home, and it is not this page.
  const redirecting = useCanonicalBlueprintRedirect(item?.task)
  // Gate tasks open scrolled down to the gate, so the bar takes over naming the
  // task whenever the page heading is out of view.
  const [heading, setHeading] = useState<HTMLHeadingElement | null>(null)
  const [headingHidden, setHeadingHidden] = useState(false)
  useEffect(() => {
    setHeadingHidden(false)
    if (!heading) return
    const observer = new IntersectionObserver(([entry]) => setHeadingHidden(!entry.isIntersecting))
    observer.observe(heading)
    return () => observer.disconnect()
  }, [heading])
  const barTitle = headingHidden ? item?.task.title : undefined

  return (
    <div className="flex h-full flex-col">
      <header className="pointer-coarse:[&_button]:min-h-10 pointer-coarse:[&_button]:min-w-10 flex shrink-0 flex-wrap items-center gap-1 border-b border-border px-4 py-2.5">
        <Link
          to="/w/$workspaceId/tasks/$taskId"
          params={{ workspaceId: workspace, taskId }}
          aria-label="Back to board"
          className="shrink-0"
        >
          <Button variant="ghost" size="icon" tabIndex={-1}>
            <ArrowLeft />
          </Button>
        </Link>
        {/* A breadcrumb, not a second title: the title joins it only while the
            reading column's heading is scrolled away. A phone has room for one
            name, and the title is the one that says what is being asked about. */}
        <nav aria-label="Breadcrumb" className="flex min-w-0 flex-1 basis-0 items-center gap-1.5 text-sm text-muted">
          <span className={cn('shrink-0', barTitle && 'max-sm:hidden')}>Board</span>
          <span aria-hidden className={cn('shrink-0 text-edge', barTitle && 'max-sm:hidden')}>
            /
          </span>
          <span
            className={cn('min-w-0 truncate font-mono text-xs text-foreground', barTitle && 'shrink-0 max-sm:sr-only')}
            aria-current="page"
          >
            {taskId}
          </span>
          {barTitle && (
            <>
              <span aria-hidden className="shrink-0 text-edge max-sm:hidden">
                /
              </span>
              <span className="min-w-0 truncate font-medium text-foreground" title={barTitle} data-breadcrumb-title>
                {barTitle}
              </span>
            </>
          )}
        </nav>
        {/* A task the read answers 404 for has no lineage and no Board
            neighbours to offer, so its header keeps only the way out. */}
        {!notFound && (
          <>
            {/* The corner affordance REQ-3 asks for: what this task connects to is
                one read away, and it costs nothing until it is opened. */}
            <LineageExplorer type="task" id={taskId} />
            {windowEdge && (
              // Below sm the note takes its own line under the controls so it
              // never squeezes the title out of the header.
              <span
                className="max-w-56 text-right text-[11px] leading-4 text-faint max-sm:order-last max-sm:max-w-none max-sm:basis-full max-sm:text-left"
                role="note"
              >
                {windowEdge}
              </span>
            )}
            <FullNavButton targetId={previousId} label="Previous task" icon={<ChevronUp />} />
            <FullNavButton targetId={nextId} label="Next task" icon={<ChevronDown />} />
          </>
        )}
      </header>
      {(isLoading || redirecting) && (
        <div className="space-y-3 px-6 py-6">
          <Skeleton className="h-24" />
          <Skeleton className="h-64" />
        </div>
      )}
      {error != null && !item && <TaskLoadError error={error} retrying={isFetching} onRetry={() => void refetch()} />}
      {item && !redirecting && <FullBody item={item} headingRef={setHeading} />}
    </div>
  )
}

function FullNavButton({ targetId, label, icon }: { targetId?: string; label: string; icon: React.ReactNode }) {
  const { workspace } = useWorkspaceSelection()
  if (!targetId) {
    return (
      <Button variant="ghost" size="icon" className="shrink-0" aria-label={label} disabled>
        {icon}
      </Button>
    )
  }
  return (
    <Link
      to="/w/$workspaceId/tasks/$taskId/full"
      params={{ workspaceId: workspace, taskId: targetId }}
      aria-label={label}
      className="shrink-0"
    >
      <Button variant="ghost" size="icon" tabIndex={-1}>
        {icon}
      </Button>
    </Link>
  )
}

// The reading column keeps a measure of about 760 px however wide the page is.
const readingMeasure = 'w-full min-w-0 lg:mx-auto lg:max-w-[47.5rem]'

// A plan can still arrive only while no later stage has begun: a closed task, or
// one already past planning, has nothing to wait for and says nothing about it.
function planStillAhead(item: ActivityItem) {
  if (['approved', 'merged', 'closed'].includes(item.task.state)) return false
  return !(item.work_orders ?? []).some((order) => order.stage !== 'spec')
}

// One grid, each section mounted once. Below lg the DOM order is the reading
// order: identity, next action, plan, properties, context and the other
// inspector sections, then activity. From lg up the side column spans both
// rows and sticks with its own scrolling, so Properties and Context stay beside
// the timeline however long the page runs.
function FullBody({
  item,
  headingRef,
}: {
  item: ActivityItem
  headingRef: (heading: HTMLHeadingElement | null) => void
}) {
  const { view, select } = useTaskActivityView()
  return (
    <section
      aria-label="Task content"
      className="min-h-0 flex-1 overflow-y-auto lg:bg-local lg:bg-[linear-gradient(to_left,var(--color-surface)_23rem,var(--color-border)_23rem,var(--color-border)_calc(23rem+1px),transparent_calc(23rem+1px))]"
    >
      {/* The inspector rail is the scroll region's own background, attached
          locally so it spans the whole scroll height: the 20rem inspector, the
          1.5rem page padding, and half the 3rem column gap, then a 1px border. */}
      <div className="grid grid-cols-1 gap-y-5 px-4 pt-5 pb-6 sm:px-6 lg:grid-cols-[minmax(0,1fr)_20rem] lg:grid-rows-[auto_1fr] lg:gap-x-12 lg:pt-6">
        <div className={cn(readingMeasure, 'space-y-5 lg:col-start-1 lg:row-start-1')}>
          <TaskIdentity item={item} variant="full" headingRef={headingRef} />
          <NextActionCard item={item} routeVariant="full" view={view} />
          {(item.spec || planStillAhead(item)) && (
            <section aria-label="Execution plan">
              {item.spec ? (
                <SpecCard
                  spec={item.spec}
                  collapsible={false}
                  overflowExpandable
                  routeVariant="full"
                  minimumSeatCount={item.summaries?.review_seat_policy?.minimum_seat_count}
                />
              ) : (
                <p className="text-sm text-faint">
                  No execution plan yet — the planning stage has not produced a version.
                </p>
              )}
            </section>
          )}
        </div>
        {/* The negative margins carry the column's scrollbar and focus outlines
            out to the rail's edges; its padding brings the content back. It
            fills the visible height, so a tooltip near its last row has the
            whole rail to open into before the scrolling clips it. */}
        <div className="min-w-0 space-y-5 [scrollbar-width:thin] lg:sticky lg:top-0 lg:col-start-2 lg:row-span-2 lg:row-start-1 lg:-mx-6 lg:-my-6 lg:h-[calc(100dvh-4.5rem)] lg:self-start lg:overflow-y-auto lg:p-6">
          <section aria-label="Properties" className="min-w-0">
            <SectionHeading as="h2" className="mb-1 text-xs font-medium text-faint">
              Properties
            </SectionHeading>
            <TaskFacts item={item} variant="full" columns="one" className="mt-0 gap-y-1.5 border-y-0 py-1" />
          </section>
          <section aria-label="Task inspector" className="min-w-0 space-y-5">
            <TaskContextCard taskId={item.task.id} taskState={item.task.state} context={item.task.context} compact />
            <TaskAgentPrompt item={item} />
            <AttachmentsCard attachments={item.attachments ?? []} />
            {/* While a gate is open the evidence belongs with the decision, and
                the gate card renders it there — not twice on one page. */}
            {!isReviewable(item) && (
              <AttachmentsCard attachments={item.verification_evidence ?? []} title="Verification evidence" />
            )}
          </section>
        </div>
        {/* The static relationship graph is gone: relationships are read in
            the explorer panel, and one page does not carry two renderings of
            the same lineage. */}
        <section aria-label="Activity" className={cn(readingMeasure, 'space-y-4 lg:col-start-1 lg:row-start-2')}>
          <TaskActivityTabs item={item} routeVariant="full" view={view} onSelectView={select} />
        </section>
      </div>
    </section>
  )
}
