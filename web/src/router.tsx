import { Outlet, createRootRoute, createRoute, createRouter, redirect, useParams } from '@tanstack/react-router'
import { AppShell } from './components/app-shell'
import { Board } from './components/board/board'
import { TaskCreateSheet } from './components/task/task-create-sheet'
import { TaskSheet } from './components/task/task-sheet'
import { CreateWorkspaceDialog } from './components/workspace/create-workspace-dialog'
import { BlueprintDetailPage } from './pages/blueprint-detail'
import { BlueprintsPage } from './pages/blueprints'
import { RequirementsPage } from './pages/requirements'
import { SystemDesignPage } from './pages/system-design'
import { SettingsPage } from './pages/settings'
import { TaskFullPage } from './pages/task-full'
import { TasksPage } from './pages/tasks'
import { WorkspacePage } from './pages/workspace'
import { MonitorPage } from './pages/monitor'
import { PlanningPage } from './pages/planning'

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
// Pathless layout: the board wraps "/", the task sheets, and the workspace
// modal, so it stays mounted (scroll, search) while overlays open over it.
const boardRoute = createRoute({ getParentRoute: () => rootRoute, id: 'board', component: BoardPage })
const boardIndexRoute = createRoute({ getParentRoute: () => boardRoute, path: '/', component: () => null })
const taskSheetRoute = createRoute({
  getParentRoute: () => boardRoute,
  path: '/tasks/$taskId',
  component: TaskSheetRoute,
})
const newTaskRoute = createRoute({ getParentRoute: () => boardRoute, path: '/new', component: TaskCreateSheet })
const createWorkspaceRoute = createRoute({
  getParentRoute: () => boardRoute,
  path: '/workspaces/new',
  component: CreateWorkspaceDialog,
})
const taskFullRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/tasks/$taskId/full',
  component: TaskFullPage,
})
// The list-first Tasks view (spec §21.58). It is a sibling
// of the task routes, not a layout over them: the list is the management
// surface, and a row hands off to the task's own detail page.
const tasksRoute = createRoute({ getParentRoute: () => rootRoute, path: '/tasks', component: TasksPage })
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
const settingsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/settings', component: SettingsPage })
const requirementsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/requirements',
  // `session` deep-links the document-scoped planning sidebar, so a reload
  // restores the assistant beside the same document (spec §21.57).
  validateSearch: (search: Record<string, unknown>): { requirement?: string; session?: string } => ({
    requirement: typeof search.requirement === 'string' ? search.requirement : undefined,
    session: typeof search.session === 'string' ? search.session : undefined,
  }),
  component: RequirementsPage,
})
const systemDesignRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/system-design',
  validateSearch: (search: Record<string, unknown>): { document?: string; session?: string } => ({
    document: typeof search.document === 'string' ? search.document : undefined,
    session: typeof search.session === 'string' ? search.session : undefined,
  }),
  component: SystemDesignPage,
})
// The planning-side blueprint surface (spec §21.49): the list, and the
// canonical detail route an anchor now owns. The task routes no longer render
// an anchor — they redirect here once the task loads — so a blueprint has one
// home and no second door back into the task costume.
const blueprintsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/blueprints', component: BlueprintsPage })
const blueprintDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/blueprints/$taskId',
  component: BlueprintDetailPage,
})
const planningRoute = createRoute({ getParentRoute: () => rootRoute, path: '/planning', component: PlanningPage })
const monitorRoute = createRoute({ getParentRoute: () => rootRoute, path: '/monitor', component: MonitorPage })

const routeTree = rootRoute.addChildren([
  boardRoute.addChildren([boardIndexRoute, taskSheetRoute, newTaskRoute, createWorkspaceRoute]),
  taskFullRoute,
  tasksRoute,
  activityRedirectRoute,
  workspaceRoute,
  requirementsRoute,
  systemDesignRoute,
  blueprintsRoute,
  blueprintDetailRoute,
  planningRoute,
  monitorRoute,
  settingsRoute,
])
export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
