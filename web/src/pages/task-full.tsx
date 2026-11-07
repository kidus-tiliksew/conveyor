import { Link, useParams } from '@tanstack/react-router'
import { ArrowLeft, ChevronDown, ChevronUp } from 'lucide-react'
import { useState } from 'react'
import { useWorkspaceSelection } from '../components/app-shell'
import { useCanonicalBlueprintRedirect } from '../components/blueprint/use-blueprint-route'
import { LineageExplorer } from '../components/lineage/lineage-explorer'
import { AttachmentsCard } from '../components/task/attachments-card'
import { isReviewable } from '../components/task/review-panel'
import { SectionHeading } from '../components/task/section-heading'
import { SpecCard } from '../components/task/spec-card'
import { TaskActivityTabs } from '../components/task/task-activity-tabs'
import { TaskContextCard } from '../components/task/task-context-card'
import { TaskAgentPrompt, TaskFacts, TaskIdentity, TaskStatusBadge } from '../components/task/task-header'
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
  const { workspace } = useWorkspaceSelection()
  const { taskId } = useParams({ from: '/w/$workspaceId/tasks/$taskId/full' })
  const { data: item, isLoading, error } = useTaskDetail(taskId)
  const { previousId, nextId, windowEdge } = useTaskOrder(taskId)
  const [scrolled, setScrolled] = useState(false)
  // A blueprint anchor has one home, and it is not this page.
  const redirecting = useCanonicalBlueprintRedirect(item?.task)

  return (
    <div className="flex h-full flex-col">
      <header className="flex min-h-14 shrink-0 flex-wrap items-center gap-1 border-b border-border px-4 py-2 lg:px-6">
        <Link
          to="/w/$workspaceId/tasks/$taskId"
          params={{ workspaceId: workspace, taskId }}
          aria-label="Back to board"
          className="-ml-2 shrink-0"
        >
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
          <span className="shrink-0 font-mono text-xs text-foreground" aria-current="page">
            {taskId}
          </span>
          {/* Once the title has scrolled away, the header keeps the reader oriented. */}
          {item && scrolled && (
            <>
              <span aria-hidden className="shrink-0 text-edge">
                ·
              </span>
              <span className="min-w-0 truncate text-[13px] text-muted">{item.task.title}</span>
              <span className="shrink-0">
                <TaskStatusBadge item={item} />
              </span>
            </>
          )}
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
      {item && !redirecting && <FullBody item={item} onScrolled={setScrolled} />}
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

// One grid, each section mounted once. Below lg the DOM order is the reading
// order: identity, properties, plan, context and the other inspector
// sections, then activity. From lg up identity spans the rows beside the
// properties and the top of the inspector, so neither column waits on the
// other's height.
function FullBody({ item, onScrolled }: { item: ActivityItem; onScrolled: (scrolled: boolean) => void }) {
  return (
    <section
      aria-label="Task content"
      onScroll={(event) => onScrolled(event.currentTarget.scrollTop > 160)}
      className="task-reading min-h-0 flex-1 overflow-y-auto lg:bg-local lg:bg-[linear-gradient(to_left,var(--color-surface)_23rem,var(--color-border)_23rem,var(--color-border)_calc(23rem+1px),transparent_calc(23rem+1px))]"
    >
      {/* The inspector rail is the scroll region's own background, attached
          locally so it spans the whole scroll height: the 20rem inspector, the
          1.5rem page padding, and half the 3rem column gap, then a 1px border. */}
      <div className="grid grid-cols-1 gap-y-5 px-4 pt-5 pb-5 sm:px-6 lg:grid-cols-[minmax(0,1fr)_20rem] lg:grid-rows-[auto_1fr_auto_auto] lg:gap-x-12 lg:gap-y-0">
        <div className={cn(readingMeasure, 'lg:col-start-1 lg:row-span-2 lg:row-start-1')}>
          <TaskIdentity item={item} variant="full" />
        </div>
        <section aria-label="Properties" className="min-w-0 lg:col-start-2 lg:row-start-1">
          <SectionHeading as="h2" className="mb-1 text-xs font-medium text-faint">
            Properties
          </SectionHeading>
          <TaskFacts
            item={item}
            variant="full"
            columns="one"
            className="mt-0 grid-cols-[5.5rem_minmax(0,1fr)] gap-x-3 gap-y-1.5 border-y-0 py-1"
          />
        </section>
        {item.spec && (
          <section aria-label="Execution plan" className={cn(readingMeasure, 'lg:col-start-1 lg:row-start-3 lg:mt-5')}>
            <SpecCard
              spec={item.spec}
              collapsible={false}
              overflowExpandable
              routeVariant="full"
              minimumSeatCount={item.summaries?.review_seat_policy?.minimum_seat_count}
            />
          </section>
        )}
        {/* Zero height from lg up: the inspector follows the properties directly
            and never stretches the rows that hold the reading column. It stays
            in view while the column scrolls, and scrolls itself when taller
            than the window. */}
        <section
          aria-label="Task inspector"
          className="min-w-0 space-y-5 lg:sticky lg:top-5 lg:col-start-2 lg:row-span-3 lg:row-start-2 lg:mt-5 lg:h-0 lg:self-start"
        >
          <div className="min-w-0 space-y-5 lg:max-h-[calc(100dvh-7rem)] lg:overflow-y-auto">
            <TaskContextCard taskId={item.task.id} taskState={item.task.state} context={item.task.context} compact />
            <TaskAgentPrompt item={item} />
            <AttachmentsCard attachments={item.attachments ?? []} />
            {/* While a gate is open the evidence belongs with the decision, and
              the gate card renders it there — not twice on one page. */}
            {!isReviewable(item) && (
              <AttachmentsCard attachments={item.verification_evidence ?? []} title="Verification evidence" />
            )}
          </div>
        </section>
        {/* The static relationship graph is gone: relationships are read in
            the explorer panel, and one page does not carry two renderings of
            the same lineage. */}
        <section
          aria-label="Activity"
          className={cn(readingMeasure, 'space-y-4 lg:col-start-1 lg:row-start-4 lg:mt-5')}
        >
          <TaskActivityTabs item={item} routeVariant="full" />
        </section>
      </div>
    </section>
  )
}
