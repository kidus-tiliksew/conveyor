import { Outlet, createRootRoute, createRoute, createRouter, redirect, useParams } from '@tanstack/react-router'
import { AppShell } from './components/app-shell'
import { Board } from './components/board/board'
import { TaskSheet } from './components/task/task-sheet'
import { CreateWorkspacePage } from './pages/create-workspace'
import { NewTaskPage } from './pages/new-task'
import { RequirementsPage } from './pages/requirements'
import { SettingsPage } from './pages/settings'
import { TaskFullPage } from './pages/task-full'
import { WorkspacePage } from './pages/workspace'

// The board is a layout route: the task sheet mounts into its Outlet, so the
// board (scroll position, search) stays alive while a task is open.
function BoardPage() {
  return (
    <div className="flex h-full flex-col">
      <Board />
      <Outlet />
    </div>
  )
}

// Deep-link target for PR comments and chat (spec §17.0): the board stays
// visible with the task's detail sheet open over it.
function TaskSheetRoute() {
  const { taskId } = useParams({ strict: false }) as { taskId: string }
  return <TaskSheet taskId={taskId} />
}

const rootRoute = createRootRoute({ component: AppShell })
// Pathless layout: the board wraps both "/" and the task-sheet route, so it
// stays mounted (scroll, search) while a sheet opens over it.
const boardRoute = createRoute({ getParentRoute: () => rootRoute, id: 'board', component: BoardPage })
const boardIndexRoute = createRoute({ getParentRoute: () => boardRoute, path: '/', component: () => null })
const taskSheetRoute = createRoute({ getParentRoute: () => boardRoute, path: '/tasks/$taskId', component: TaskSheetRoute })
const taskFullRoute = createRoute({ getParentRoute: () => rootRoute, path: '/tasks/$taskId/full', component: TaskFullPage })
// Legacy deep links from before the board became the home page.
const activityRedirectRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/activity',
  beforeLoad: () => {
    throw redirect({ to: '/' })
  },
  component: () => null,
})
const workspaceRoute = createRoute({ getParentRoute: () => rootRoute, path: '/workspace', component: WorkspacePage })
const createWorkspaceRoute = createRoute({ getParentRoute: () => rootRoute, path: '/workspaces/new', component: CreateWorkspacePage })
const newTaskRoute = createRoute({ getParentRoute: () => rootRoute, path: '/new', component: NewTaskPage })
const settingsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/settings', component: SettingsPage })
const requirementsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/requirements', component: RequirementsPage })

const routeTree = rootRoute.addChildren([
  boardRoute.addChildren([boardIndexRoute, taskSheetRoute]),
  taskFullRoute,
  activityRedirectRoute,
  workspaceRoute,
  createWorkspaceRoute,
  requirementsRoute,
  newTaskRoute,
  settingsRoute,
])
export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
