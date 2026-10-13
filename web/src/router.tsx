import { createRootRoute, createRoute, createRouter, Outlet, redirect, useParams } from '@tanstack/react-router'
import { AppShell } from './components/app-shell'
import { Board } from './components/board/board'
import { validateBoardSearch } from './components/board/board-search'
import { type ReviewSearch, validateReviewSearch } from './components/documents/document-review'
import { validateInboxSearch } from './components/inbox/inbox-search'
import { TaskSheet } from './components/task/task-sheet'
import { CreateWorkspaceDialog } from './components/workspace/create-workspace-dialog'
import { BlueprintDetailPage } from './pages/blueprint-detail'
import { BlueprintsPage } from './pages/blueprints'
import { InboxPage } from './pages/inbox'
import { MonitorPage } from './pages/monitor'
import { OnboardingPage } from './pages/onboarding'
import { PlanningPage } from './pages/planning'
import { RequirementsPage } from './pages/requirements'
import { SettingsPage } from './pages/settings'
import { SignInPage } from './pages/sign-in'
import { SystemDesignPage } from './pages/system-design'
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

// Deep-link target for PR comments and chat: the board stays
// visible with the task's detail sheet open over it.
function TaskSheetRoute() {
  const { taskId } = useParams({ strict: false }) as { taskId: string }
  return <TaskSheet taskId={taskId} />
}

const rootRoute = createRootRoute({ component: AppShell })
// Pathless layout: the board wraps "/", the task sheets, and the workspace
// modal, so it stays mounted (scroll, search) while overlays open over it. Its
// search is the view, intake and arriving list filters (REQ-6), which every
// child keeps.
const boardRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: 'board',
  validateSearch: validateBoardSearch,
  component: BoardPage,
})
const boardIndexRoute = createRoute({ getParentRoute: () => boardRoute, path: '/', component: () => null })
const taskSheetRoute = createRoute({
  getParentRoute: () => boardRoute,
  path: '/tasks/$taskId',
  component: TaskSheetRoute,
})
// Task intake lives on the Board's list view; the old address stays a
// redirect so existing links and bookmarks still land on intake.
const newTaskRedirectRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/new',
  beforeLoad: () => {
    throw redirect({ to: '/', search: { view: 'list', create: true } })
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
// The Tasks page became the Board's list view (AC-6.3). Its address redirects
// there, keeping intake and any filter it names; anything else is dropped. A
// legacy panel permalink (`?task=`) opens that task's sheet over the list.
const tasksRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/tasks',
  beforeLoad: ({ search }) => {
    const task = (search as Record<string, unknown>).task
    if (typeof task === 'string' && task) {
      throw redirect({ to: '/tasks/$taskId', params: { taskId: task }, search: { view: 'list' } })
    }
    throw redirect({ to: '/', search: { ...validateBoardSearch(search as Record<string, unknown>), view: 'list' } })
  },
  component: () => null,
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
const onboardingRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/onboarding',
  component: OnboardingPage,
})
const requirementsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/requirements',
  // `session` deep-links the document-scoped planning sidebar, so a reload
  // restores the assistant beside the same document.
  validateSearch: (search: Record<string, unknown>): { requirement?: string; session?: string } & ReviewSearch => ({
    ...validateReviewSearch(search),
    requirement: typeof search.requirement === 'string' ? search.requirement : undefined,
    session: typeof search.session === 'string' ? search.session : undefined,
  }),
  component: RequirementsPage,
})
const systemDesignRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/system-design',
  validateSearch: (search: Record<string, unknown>): { document?: string; session?: string } & ReviewSearch => ({
    ...validateReviewSearch(search),
    document: typeof search.document === 'string' ? search.document : undefined,
    session: typeof search.session === 'string' ? search.session : undefined,
  }),
  component: SystemDesignPage,
})
// The operator inbox (feature-operator-next-action NA-6).
const inboxRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/inbox',
  validateSearch: validateInboxSearch,
  component: InboxPage,
})
// Pending proposals are decisions in the Inbox now; the old address keeps its
// task, document and tier.
const pendingProposalsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/pending-proposals',
  beforeLoad: ({ search }) => {
    throw redirect({
      to: '/inbox',
      search: { ...validateInboxSearch(search as Record<string, unknown>), band: 'decisions' },
    })
  },
  component: () => null,
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
// `session` selects a planning session by deep link — the lineage explorer's
// destination for sessions and their bundles — ahead of the restored choice.
const planningRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/planning',
  validateSearch: (search: Record<string, unknown>): { session?: string } => ({
    session: typeof search.session === 'string' && search.session ? search.session : undefined,
  }),
  component: PlanningPage,
})
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
  inboxRoute,
  pendingProposalsRoute,
  blueprintsRoute,
  blueprintDetailRoute,
  planningRoute,
  monitorRoute,
  settingsRoute,
  signInRoute,
  onboardingRoute,
])
export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
