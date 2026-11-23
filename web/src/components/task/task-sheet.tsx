import { useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { ChevronDown, ChevronUp, Maximize2, X } from 'lucide-react'
import { type ReactNode, useEffect, useState } from 'react'
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
import { TaskActivityTabs, useTaskActivityView } from './task-activity-tabs'
import { TaskContextCard } from './task-context-card'
import { TaskAgentPrompt, TaskFacts, TaskIdentity } from './task-header'
import { TaskControlsPlaceholder, TaskLoadError } from './task-load-error'
import { TaskPlanSection } from './task-plan-section'
import { taskHasVerifyStage, useTaskDetail, useTaskOrder, useTaskVerification } from './use-task-detail'

// How a host page other than the Board opens the task detail sheet over
// itself (component-web-task-surfaces invariant 39; component-crates invariant
// 37): the host supplies the task order that Previous and Next step through,
// what selecting a neighbour does, and what closing does.
export interface TaskSheetHost {
  // Task IDs in the host's order. The sheet takes neighbours from this list
  // alone and never sorts it or falls back to the Board's order.
  order: readonly string[]
  onSelect: (taskId: string) => void
  onClose: () => void
}

// The task detail sheet: the costed event timeline — which
// carries the review actions as its live tail — opened over the board so the
// reviewer never loses Board context. The kanban and map views open it the
// same way, and closing returns to whichever view the Board was showing.
// A host page such as a crate page opens the same sheet over itself through
// `host`, and the Board's order, window note, and links then take no part.
export function TaskSheet({ taskId, host }: { taskId: string; host?: TaskSheetHost }) {
  if (host) return <HostedTaskSheet taskId={taskId} host={host} />
  return <BoardTaskSheet taskId={taskId} />
}

// The Board's adapter: neighbours follow the Board's loaded page of tasks, the
// window note says when a neighbour is on another page, and the links keep the
// Board's search.
function BoardTaskSheet({ taskId }: { taskId: string }) {
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const boardOrder = useTaskOrder(taskId)
  const close = () =>
    void navigate({ to: '/w/$workspaceId/', params: { workspaceId: workspace }, search: (prev) => prev })
  return (
    <TaskSheetFrame
      taskId={taskId}
      onClose={close}
      note={boardOrder.windowEdge}
      previous={<SheetNavLink targetId={boardOrder.previousId} label="Previous task" icon={<ChevronUp />} />}
      next={<SheetNavLink targetId={boardOrder.nextId} label="Next task" icon={<ChevronDown />} />}
    />
  )
}

// A host page's adapter: neighbours come from the host's order by position, and
// a task the order does not hold has none.
function HostedTaskSheet({ taskId, host }: { taskId: string; host: TaskSheetHost }) {
  const index = host.order.indexOf(taskId)
  const previousId = index > 0 ? host.order[index - 1] : undefined
  const nextId = index >= 0 && index < host.order.length - 1 ? host.order[index + 1] : undefined
  return (
    <TaskSheetFrame
      taskId={taskId}
      onClose={host.onClose}
      previous={
        <SheetNavButton targetId={previousId} label="Previous task" icon={<ChevronUp />} onSelect={host.onSelect} />
      }
      next={<SheetNavButton targetId={nextId} label="Next task" icon={<ChevronDown />} onSelect={host.onSelect} />}
    />
  )
}

// The one sheet composition every host shares (component-web-task-surfaces
// invariant 32). Its adapter supplies the close action, the neighbour
// controls, and any note about them.
function TaskSheetFrame({
  taskId,
  onClose,
  note,
  previous,
  next,
}: {
  taskId: string
  onClose: () => void
  note?: string
  previous: ReactNode
  next: ReactNode
}) {
  const { workspace } = useWorkspaceSelection()
  const { data: item, isLoading, error, refetch, isFetching } = useTaskDetail(taskId)
  useSheetPrefetch(taskId)
  // The summary family has zero garbage-collection time, so this observer is
  // what keeps the first summary page alive from the cold open until the
  // verification entries mount with the body and share it. It reads while the
  // detail is pending and afterwards only for a task with a verify stage.
  useTaskVerification(taskId, item ? taskHasVerifyStage(item) : true)
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
    <Sheet onClose={onClose} label="Task detail">
      <header className="pointer-coarse:[&_button]:min-h-10 pointer-coarse:[&_button]:min-w-10 flex shrink-0 flex-wrap items-center gap-1 border-b border-border px-4 py-2.5">
        {/* The title takes whatever width the fixed-size controls leave and
            truncates, so every control, Close included, stays on this one row. */}
        <span data-sheet-title className="min-w-0 flex-1 basis-0 truncate text-sm font-medium text-muted">
          {headingHidden ? item?.task.title : <span className="sr-only">{item?.task.title}</span>}
        </span>
        {/* Until a task is loaded (a pending first read, a 404, or another
            failure) there is no lineage and no Board neighbour to offer, so the
            header keeps only the way out; a pending read reserves the
            controls' space so nothing moves when the task arrives. The explorer
            is the corner affordance REQ-3 asks for. */}
        {item ? (
          <LineageExplorer type="task" id={taskId} />
        ) : (
          error == null && <TaskControlsPlaceholder part="explorer" />
        )}
        {/* The Board-window note is Board context, not a task control, so it
            shows in every state and never shifts the controls when they arrive.
            A host page other than the Board supplies no note. */}
        {note && (
          // Below sm the note takes its own line under the controls so it
          // never squeezes the title out of the header.
          <span
            className="max-w-56 text-right text-[11px] leading-4 text-faint max-sm:order-last max-sm:max-w-none max-sm:basis-full max-sm:text-left"
            role="note"
          >
            {note}
          </span>
        )}
        {item ? (
          <>
            {previous}
            {next}
          </>
        ) : (
          error == null && <TaskControlsPlaceholder part="navigation" />
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
        <Button variant="ghost" size="icon" className="shrink-0" aria-label="Close panel" onClick={onClose}>
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

function DisabledNavButton({ label, icon }: { label: string; icon: ReactNode }) {
  return (
    <Button variant="ghost" size="icon" className="shrink-0" aria-label={label} disabled>
      {icon}
    </Button>
  )
}

function SheetNavLink({ targetId, label, icon }: { targetId?: string; label: string; icon: ReactNode }) {
  const { workspace } = useWorkspaceSelection()
  if (!targetId) return <DisabledNavButton label={label} icon={icon} />
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

function SheetNavButton({
  targetId,
  label,
  icon,
  onSelect,
}: {
  targetId?: string
  label: string
  icon: ReactNode
  onSelect: (taskId: string) => void
}) {
  if (!targetId) return <DisabledNavButton label={label} icon={icon} />
  return (
    <Button variant="ghost" size="icon" className="shrink-0" aria-label={label} onClick={() => onSelect(targetId)}>
      {icon}
    </Button>
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
      <TaskPlanSection item={item} variant="sheet" />
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
