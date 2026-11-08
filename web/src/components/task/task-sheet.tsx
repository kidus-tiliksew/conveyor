import { useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { ChevronDown, ChevronUp, Maximize2, X } from 'lucide-react'
import { useEffect, useState } from 'react'
import type { ActivityItem } from '../../lib/types'
import {
  blueprintsQueryOptions,
  callerIdentityQueryOptions,
  useWorkspaceSelection,
  workspaceMembersQueryOptions,
} from '../app-shell'
import { useCanonicalBlueprintRedirect } from '../blueprint/use-blueprint-route'
import { nextActionQueryOptions } from '../inbox/use-next-actions'
import { LineageExplorer } from '../lineage/lineage-explorer'
import { Button } from '../ui/button'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'
import { AttachmentsCard } from './attachments-card'
import { NextActionCard } from './next-action-card'
import { isReviewable } from './review-panel'
import { SpecCard } from './spec-card'
import { TaskActivityTabs, useTaskActivityView } from './task-activity-tabs'
import { TaskContextCard } from './task-context-card'
import { TaskAgentPrompt, TaskFacts, TaskIdentity } from './task-header'
import { TaskLoadError } from './task-load-error'
import { isTaskNotFound, taskHasVerifyStage, useTaskDetail, useTaskOrder, useTaskVerification } from './use-task-detail'

// The task detail sheet: the costed event timeline — which
// carries the review actions as its live tail — opened over the board so the
// reviewer never loses Board context. The kanban and map views open it the
// same way, and closing returns to whichever view the Board was showing.
export function TaskSheet({ taskId }: { taskId: string }) {
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const { data: item, isLoading, error, refetch, isFetching } = useTaskDetail(taskId)
  useSheetPrefetch(taskId)
  // The summary family has zero garbage-collection time, so this observer is
  // what keeps the first summary page alive from the cold open until the
  // verification entries mount with the body and share it. It reads while the
  // detail is pending and afterwards only for a task with a verify stage.
  useTaskVerification(taskId, item ? taskHasVerifyStage(item) : true)
  const boardOrder = useTaskOrder(taskId)
  const previousId = boardOrder.previousId
  const nextId = boardOrder.nextId
  const notFound = !item && error != null && isTaskNotFound(error)
  const close = () =>
    void navigate({ to: '/w/$workspaceId/', params: { workspaceId: workspace }, search: (prev) => prev })
  // A blueprint anchor has one home, and it is not this sheet.
  const redirecting = useCanonicalBlueprintRedirect(item?.task)
  // The body opens with the title, so the header names the task only once that
  // heading has scrolled away, as the full page's bar does.
  const [heading, setHeading] = useState<HTMLHeadingElement | null>(null)
  const [headingHidden, setHeadingHidden] = useState(false)
  useEffect(() => {
    if (!heading) {
      setHeadingHidden(false)
      return
    }
    const observer = new IntersectionObserver(([entry]) => setHeadingHidden(!entry.isIntersecting))
    observer.observe(heading)
    return () => observer.disconnect()
  }, [heading])

  return (
    <Sheet onClose={close} label="Task detail">
      <header className="pointer-coarse:[&_button]:min-h-10 pointer-coarse:[&_button]:min-w-10 flex shrink-0 flex-wrap items-center gap-1 border-b border-border px-4 py-2.5">
        {/* The title takes whatever width the fixed-size controls leave and
            truncates, so every control, Close included, stays on this one row. */}
        <span data-sheet-title className="min-w-0 flex-1 basis-0 truncate text-sm font-medium text-muted">
          {headingHidden ? item?.task.title : <span className="sr-only">{item?.task.title}</span>}
        </span>
        {/* A task the read answers 404 for has no lineage and no Board
            neighbours to offer, so its header keeps only the way out. */}
        {!notFound && (
          <>
            {/* The same corner affordance the full route carries (REQ-3): the
                panel opens over this one without dismissing it. */}
            <LineageExplorer type="task" id={taskId} />
            {boardOrder.windowEdge && (
              // Below sm the note takes its own line under the controls so it
              // never squeezes the title out of the header.
              <span
                className="max-w-56 text-right text-[11px] leading-4 text-faint max-sm:order-last max-sm:max-w-none max-sm:basis-full max-sm:text-left"
                role="note"
              >
                {boardOrder.windowEdge}
              </span>
            )}
            <SheetNavButton targetId={previousId} label="Previous task" icon={<ChevronUp />} />
            <SheetNavButton targetId={nextId} label="Next task" icon={<ChevronDown />} />
          </>
        )}
        <Link
          to="/w/$workspaceId/tasks/$taskId/full"
          params={{ workspaceId: workspace, taskId }}
          aria-label="Open full task page"
          className="shrink-0"
        >
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
        {error != null && !item && <TaskLoadError error={error} retrying={isFetching} onRetry={() => void refetch()} />}
        {item && !redirecting && <SheetBody item={item} headingRef={setHeading} />}
      </div>
    </Sheet>
  )
}

// Starts every read the sheet body needs that does not depend on the bounded
// detail when the sheet opens, instead of when the detail returns
// (component-web-task-surfaces, Sheet read scheduling). Each prefetch uses the
// option factory of the observer that later reads it, so that observer finds
// a fresh entry and sends nothing. prefetchQuery never rejects: a failed read
// leaves its entry in the error state for its consumer to retry and show. A
// repeated run for the same task joins the read already in flight, and each
// read fills only the key of the workspace it captured.
function useSheetPrefetch(taskId: string) {
  const queryClient = useQueryClient()
  const { workspace } = useWorkspaceSelection()
  useEffect(() => {
    if (!workspace) return
    void queryClient.prefetchQuery(callerIdentityQueryOptions(workspace))
    void queryClient.prefetchQuery(workspaceMembersQueryOptions(workspace))
    void queryClient.prefetchQuery(blueprintsQueryOptions(workspace))
    void queryClient.prefetchQuery(nextActionQueryOptions(workspace, taskId))
  }, [queryClient, workspace, taskId])
}

function SheetNavButton({ targetId, label, icon }: { targetId?: string; label: string; icon: React.ReactNode }) {
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
      to="/w/$workspaceId/tasks/$taskId"
      params={{ workspaceId: workspace, taskId: targetId }}
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

// What the task is, then what it needs from you, then the work's plan; reference
// facts follow, so the next action is never below them.
function SheetBody({
  item,
  headingRef,
}: {
  item: ActivityItem
  headingRef: (heading: HTMLHeadingElement | null) => void
}) {
  const { view, select } = useTaskActivityView()
  return (
    <div className="space-y-4">
      <TaskIdentity item={item} variant="sheet" headingRef={headingRef} />
      <NextActionCard item={item} routeVariant="sheet" view={view} />
      {item.spec && (
        <SpecCard
          key={`${item.spec.task_id}-${item.spec.version}`}
          spec={item.spec}
          overflowExpandable
          routeVariant="sheet"
          minimumSeatCount={item.summaries?.review_seat_policy?.minimum_seat_count}
        />
      )}
      <TaskFacts item={item} variant="sheet" columns="one" className="mt-0" />
      <TaskAgentPrompt item={item} />
      <TaskContextCard taskId={item.task.id} taskState={item.task.state} context={item.task.context} />
      {/* While a gate is open the evidence belongs with the decision, and the
          gate card renders it there — showing it twice on one page does not. */}
      {!isReviewable(item) && (
        <AttachmentsCard attachments={item.verification_evidence ?? []} title="Verification evidence" />
      )}
      <AttachmentsCard attachments={item.attachments ?? []} />
      {/* Relationships are read in the explorer panel, not inline: the panel
          is the one rendering of this task's lineage. */}
      <TaskActivityTabs item={item} routeVariant="sheet" view={view} onSelectView={select} />
    </div>
  )
}
