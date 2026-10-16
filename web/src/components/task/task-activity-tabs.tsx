import { type KeyboardEvent, useId, useRef, useState } from 'react'
import type { ActivityItem } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { NextActionCard } from './next-action-card'
import { isReviewable } from './review-panel'
import { TaskFlowChart } from './task-flow'
import { Timeline } from './timeline'

export type TaskActivityView = 'timeline' | 'flow'

// The per-browser activity view: shared across tasks, surfaces, and
// workspaces, and never sent to the server (component-web-dashboard,
// Server-state discipline; req-operator-next-action AC-7.4).
export const taskActivityViewKey = 'conveyor-task-activity-view'

type ViewStorage = Pick<Storage, 'getItem' | 'setItem'>

function browserStorage(): ViewStorage | undefined {
  try {
    return window.localStorage
  } catch {
    return undefined
  }
}

// A missing, malformed, or inaccessible value reads as Timeline.
export function readTaskActivityView(storage: ViewStorage | undefined = browserStorage()): TaskActivityView {
  try {
    return storage?.getItem(taskActivityViewKey) === 'flow' ? 'flow' : 'timeline'
  } catch {
    return 'timeline'
  }
}

// Best effort: a refused write keeps the choice for this page only.
export function writeTaskActivityView(view: TaskActivityView, storage: ViewStorage | undefined = browserStorage()) {
  try {
    storage?.setItem(taskActivityViewKey, view)
  } catch {
    // Storage is unavailable; the in-page choice still applies.
  }
}

const views: { id: TaskActivityView; label: string }[] = [
  { id: 'timeline', label: 'Timeline' },
  { id: 'flow', label: 'Flow' },
]

// The activity area of task detail on the Board sheet and the full task route
// (component-web-task-surfaces, Activity tabs and the task flow view): the one
// next-action card, then Timeline and Flow tabs with only the active panel
// mounted. The Timeline tab is the default and reads no flow; the Flow tab's
// chart is the only task-detail reader of /flow (AC-7.4).
export function TaskActivityTabs({ item, routeVariant }: { item: ActivityItem; routeVariant: 'sheet' | 'full' }) {
  const { workspace } = useWorkspaceSelection()
  const [view, setView] = useState<TaskActivityView>(() => readTaskActivityView())
  const tabs = useRef<Record<TaskActivityView, HTMLButtonElement | null>>({ timeline: null, flow: null })
  const id = useId()
  const select = (next: TaskActivityView, focus = false) => {
    setView(next)
    writeTaskActivityView(next)
    if (focus) tabs.current[next]?.focus()
  }
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const index = views.findIndex((candidate) => candidate.id === view)
    const target = {
      ArrowRight: views[(index + 1) % views.length],
      ArrowLeft: views[(index - 1 + views.length) % views.length],
      Home: views[0],
      End: views[views.length - 1],
    }[event.key]
    if (!target) return
    event.preventDefault()
    select(target.id, true)
  }
  // While Flow is shown the timeline's gate card is not mounted, so the
  // next-action card carries the server's choices itself.
  const gateOpen = view === 'timeline' && isReviewable(item)

  return (
    <div className="space-y-4">
      <NextActionCard taskId={item.task.id} gateOpen={gateOpen} />
      <div className="space-y-3">
        <div
          role="tablist"
          aria-label="Task activity view"
          className="flex gap-1 border-b border-border"
          onKeyDown={onKeyDown}
        >
          {views.map((candidate) => {
            const selected = candidate.id === view
            return (
              <button
                key={candidate.id}
                ref={(element) => {
                  tabs.current[candidate.id] = element
                }}
                type="button"
                role="tab"
                id={`${id}-${candidate.id}-tab`}
                aria-selected={selected}
                aria-controls={`${id}-${candidate.id}-panel`}
                tabIndex={selected ? 0 : -1}
                data-activity-tab={candidate.id}
                className={cn(
                  '-mb-px border-b-2 px-3 py-1.5 text-sm outline-none focus-visible:ring-2 focus-visible:ring-primary/40',
                  selected
                    ? 'border-primary font-medium text-foreground'
                    : 'border-transparent text-muted hover:text-foreground',
                )}
                onClick={() => select(candidate.id)}
              >
                {candidate.label}
              </button>
            )
          })}
        </div>
        <div
          role="tabpanel"
          id={`${id}-${view}-panel`}
          aria-labelledby={`${id}-${view}-tab`}
          data-activity-panel={view}
        >
          {view === 'timeline' ? (
            <Timeline item={item} routeVariant={routeVariant} />
          ) : (
            <TaskFlowChart key={`${workspace}:${item.task.id}`} taskId={item.task.id} />
          )}
        </div>
      </div>
    </div>
  )
}
