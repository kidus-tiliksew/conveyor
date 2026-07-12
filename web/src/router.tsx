import { createRootRoute, createRoute, createRouter, useParams } from '@tanstack/react-router'
import { AppShell } from './components/app-shell'
import { ActivityList } from './components/activity/activity-list'
import { TaskPanel } from './components/task/task-panel'
import { HomePage } from './pages/home'
import { NewTaskPage } from './pages/new-task'
import { SettingsPage } from './pages/settings'
import { WorkspacePage } from './pages/workspace'

function ActivityPage() {
  return <ActivityList />
}

// Deep-link target for PR comments and chat (spec §17.0): the feed stays
// visible with the task's detail panel open beside it.
function TaskRoute() {
  const { taskId } = useParams({ from: '/tasks/$taskId' })
  return (
    <div className="flex h-full">
      <div className="hidden min-w-0 flex-1 md:block">
        <ActivityList selectedId={taskId} />
      </div>
      <TaskPanel taskId={taskId} />
    </div>
  )
}

const rootRoute = createRootRoute({ component: AppShell })
const homeRoute = createRoute({ getParentRoute: () => rootRoute, path: '/', component: HomePage })
const activityRoute = createRoute({ getParentRoute: () => rootRoute, path: '/activity', component: ActivityPage })
const taskRoute = createRoute({ getParentRoute: () => rootRoute, path: '/tasks/$taskId', component: TaskRoute })
const workspaceRoute = createRoute({ getParentRoute: () => rootRoute, path: '/workspace', component: WorkspacePage })
const newTaskRoute = createRoute({ getParentRoute: () => rootRoute, path: '/new', component: NewTaskPage })
const settingsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/settings', component: SettingsPage })

const routeTree = rootRoute.addChildren([homeRoute, activityRoute, taskRoute, workspaceRoute, newTaskRoute, settingsRoute])
export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
