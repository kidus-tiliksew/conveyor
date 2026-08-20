import { Outlet, createRootRoute, createRoute, createRouter, redirect, useParams } from '@tanstack/react-router'
import { AppShell } from './components/app-shell'
import { Board } from './components/board/board'
import { TaskSheet } from './components/task/task-sheet'
import { CreateWorkspaceDialog } from './components/workspace/create-workspace-dialog'
import { BlueprintDetailPage } from './pages/blueprint-detail'
import { BlueprintsPage } from './pages/blueprints'
import { RequirementsPage } from './pages/requirements'
import { SystemDesignPage } from './pages/system-design'
import { SettingsPage } from './pages/settings'
import { SignInPage } from './pages/sign-in'
import { TaskFullPage } from './pages/task-full'
import { TasksPage } from './pages/tasks'
import { WorkspacePage } from './pages/workspace'
import { MonitorPage } from './pages/monitor'
import { PlanningPage } from './pages/planning'
import { PendingProposalsPage } from './pages/pending-proposals'

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

// Deep-link target for PR comments and chat: the board stays
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
// Task intake moved to the Tasks view (AC-2.1). The board's old address stays
// as a redirect so existing links and bookmarks land on the surface that now
// owns creation instead of on a dead route.
const newTaskRedirectRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/new',
  beforeLoad: () => {
    throw redirect({ to: '/tasks', search: { create: true } })
  },
  component: () => null,
})
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
// The list-first Tasks view, which is also where tasks are
// created and inspected (REQ-2). Both of the surfaces it hosts are search
// params on this one route rather than child paths: the list stays mounted —
// its page, its filters, its scroll — behind whichever sheet is open, and the
// address of the surface stays `/tasks`, so intake and an open panel highlight
// the Tasks view rather than announcing some other page.
const tasksRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/tasks',
  // `task` is the open detail panel, which makes the panel's own address the
  // permalink an operator can share (AC-2.2); `create` is task intake (AC-2.1).
  validateSearch: (search: Record<string, unknown>): { task?: string; create?: boolean } => ({
    task: typeof search.task === 'string' && search.task ? search.task : undefined,
    create: search.create === true || search.create === 'true' ? true : undefined,
  }),
  component: TasksPage,
})
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
const settingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/settings',
  validateSearch: (search: Record<string, unknown>): { welcome?: boolean } => ({
    welcome: search.welcome === true || search.welcome === 'true' ? true : undefined,
  }),
  component: SettingsPage,
})
const signInRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/sign-in',
  validateSearch: (search: Record<string, unknown>): { token?: string } => ({
    token: typeof search.token === 'string' && search.token ? search.token : undefined,
  }),
  component: SignInPage,
})
const requirementsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/requirements',
  // `session` deep-links the document-scoped planning sidebar, so a reload
  // restores the assistant beside the same document.
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
const pendingProposalsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/pending-proposals',
  validateSearch: (
    search: Record<string, unknown>,
  ): { task?: string; document?: string; tier?: 'requirement' | 'system_design' } => ({
    task: typeof search.task === 'string' && search.task ? search.task : undefined,
    document: typeof search.document === 'string' && search.document ? search.document : undefined,
    tier: search.tier === 'requirement' || search.tier === 'system_design' ? search.tier : undefined,
  }),
  component: PendingProposalsPage,
})
// The planning-side blueprint surface: the list, and the
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
  boardRoute.addChildren([boardIndexRoute, taskSheetRoute, createWorkspaceRoute]),
  taskFullRoute,
  tasksRoute,
  newTaskRedirectRoute,
  activityRedirectRoute,
  workspaceRoute,
  requirementsRoute,
  systemDesignRoute,
  pendingProposalsRoute,
  blueprintsRoute,
  blueprintDetailRoute,
  planningRoute,
  monitorRoute,
  settingsRoute,
  signInRoute,
])
export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
