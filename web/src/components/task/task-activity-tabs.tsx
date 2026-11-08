import { useId, useState } from 'react'
import type { ActivityItem } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Tabs, tabPanelProps } from '../ui/tabs'
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

// The chosen view belongs to the whole task detail: the next-action card reads
// it too, because the tab decides which surface carries the open gate.
export function useTaskActivityView() {
  const [view, setView] = useState<TaskActivityView>(() => readTaskActivityView())
  const select = (next: TaskActivityView) => {
    setView(next)
    writeTaskActivityView(next)
  }
  return { view, select }
}

// The activity area of task detail on the Board sheet and the full task route
// (component-web-task-surfaces, Activity tabs and the task flow view): Timeline
// and Flow tabs with only the active panel mounted. The Timeline tab is the
// default and reads no flow; the Flow tab's chart is the only task-detail
// reader of /flow (AC-7.4).
export function TaskActivityTabs({
  item,
  routeVariant,
  view,
  onSelectView,
}: {
  item: ActivityItem
  routeVariant: 'sheet' | 'full'
  view: TaskActivityView
  onSelectView: (view: TaskActivityView) => void
}) {
  const { workspace } = useWorkspaceSelection()
  const id = useId()

  return (
    <div className="space-y-3">
      <Tabs
        id={id}
        aria-label="Task activity view"
        items={views.map((candidate) => ({
          id: candidate.id,
          label: candidate.label,
          attributes: { 'data-activity-tab': candidate.id },
        }))}
        value={view}
        onChange={onSelectView}
      />
      <div {...tabPanelProps(id, view)} data-activity-panel={view}>
        {view === 'timeline' ? (
          <Timeline item={item} routeVariant={routeVariant} />
        ) : (
          <TaskFlowChart key={`${workspace}:${item.task.id}`} taskId={item.task.id} />
        )}
      </div>
    </div>
  )
}
