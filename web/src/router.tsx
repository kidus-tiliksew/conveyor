import { createRootRoute, createRoute, createRouter, Outlet, redirect, useParams } from '@tanstack/react-router'
import { AppShell } from './components/app-shell'
import { Board } from './components/board/board'
import { validateBoardSearch } from './components/board/board-search'
import { type ReviewSearch, validateReviewSearch } from './components/documents/document-review'
import { validateInboxSearch } from './components/inbox/inbox-search'
import { TaskSheet } from './components/task/task-sheet'
import { CreateWorkspaceDialog } from './components/workspace/create-workspace-dialog'
import { LegacyAddress, PageNotFound, WorkspaceLayout } from './components/workspace/workspace-address'
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

// The root renders the shell. An address that matches no route is either
// unscoped, which LegacyAddress resolves under a workspace, or an unknown
// page inside a workspace (req-workspace-addresses AC-1.8, AC-1.9).
const rootRoute = createRootRoute({ component: AppShell, notFoundComponent: LegacyAddress })
// Every workspace page lives under the workspace its address names
// (req-workspace-addresses AC-1.1). WorkspaceLayout renders the not-found page
// for a workspace outside the caller's memberships (AC-1.4).
const workspaceLayoutRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/w/$workspaceId',
  component: WorkspaceLayout,
  notFoundComponent: PageNotFound,
})
// Pathless layout: the board wraps the index and the task sheets, so it stays
// mounted (scroll, search) while overlays open over it. Its search is the
// view, intake and arriving list filters (REQ-6), which every child keeps.
const boardRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
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
// Task intake opens over the Board (AC-6.9); the old address stays a redirect
// so existing links and bookmarks still land on intake.
const newTaskRedirectRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
  path: '/new',
  beforeLoad: ({ params }) => {
    throw redirect({ to: '/w/$workspaceId/', params, search: { create: true }, replace: true })
  },
  component: () => null,
})
// Workspace creation holds no workspace data, so its address stays unscoped
// and it opens over the shell alone, not over a Board.
const createWorkspaceRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/workspaces/new',
  component: CreateWorkspaceDialog,
})
const taskFullRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
  path: '/tasks/$taskId/full',
  component: TaskFullPage,
})
// The Tasks page retired into the Board, which offers no list view. Its address
// redirects to the kanban view, keeping intake and any filter it names; the
// view and the map's own fields are dropped (AC-6.3). A legacy panel permalink
// (`?task=`) opens that task's sheet over the Board (AC-6.10).
const tasksRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
  path: '/tasks',
  beforeLoad: ({ params, search }) => {
    const task = (search as Record<string, unknown>).task
    if (typeof task === 'string' && task) {
      throw redirect({
        to: '/w/$workspaceId/tasks/$taskId',
        params: { workspaceId: params.workspaceId, taskId: task },
        replace: true,
      })
    }
    throw redirect({
      to: '/w/$workspaceId/',
      params,
      search: {
        ...validateBoardSearch(search as Record<string, unknown>),
        view: undefined,
        arrangement: undefined,
        scope: undefined,
        focus: undefined,
        insight: undefined,
      },
      replace: true,
    })
  },
  component: () => null,
})
// Legacy deep links from before the board became the home page.
const activityRedirectRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
  path: '/activity',
  beforeLoad: ({ params }) => {
    throw redirect({ to: '/w/$workspaceId/', params, replace: true })
  },
  component: () => null,
})
const workspaceRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
  path: '/workspace',
  component: WorkspacePage,
})
const settingsRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
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
  getParentRoute: () => workspaceLayoutRoute,
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
  getParentRoute: () => workspaceLayoutRoute,
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
  getParentRoute: () => workspaceLayoutRoute,
  path: '/inbox',
  validateSearch: validateInboxSearch,
  component: InboxPage,
})
// Pending proposals are decisions in the Inbox now; the old address keeps its
// task, document and tier.
const pendingProposalsRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
  path: '/pending-proposals',
  beforeLoad: ({ params, search }) => {
    throw redirect({
      to: '/w/$workspaceId/inbox',
      params,
      search: { ...validateInboxSearch(search as Record<string, unknown>), band: 'decisions' },
      replace: true,
    })
  },
  component: () => null,
})
// The planning-side blueprint surface: the list, and the
// canonical detail route an anchor now owns. The task routes no longer render
// an anchor — they redirect here once the task loads — so a blueprint has one
// home and no second door back into the task costume.
const blueprintsRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
  path: '/blueprints',
  component: BlueprintsPage,
})
const blueprintDetailRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
  path: '/blueprints/$taskId',
  component: BlueprintDetailPage,
})
// `session` selects a planning session by deep link — the lineage explorer's
// destination for sessions and their bundles — ahead of the restored choice.
const planningRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
  path: '/planning',
  validateSearch: (search: Record<string, unknown>): { session?: string } => ({
    session: typeof search.session === 'string' && search.session ? search.session : undefined,
  }),
  component: PlanningPage,
})
const monitorRoute = createRoute({
  getParentRoute: () => workspaceLayoutRoute,
  path: '/monitor',
  component: MonitorPage,
})

const routeTree = rootRoute.addChildren([
  workspaceLayoutRoute.addChildren([
    boardRoute.addChildren([boardIndexRoute, taskSheetRoute]),
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
  ]),
  createWorkspaceRoute,
  signInRoute,
  onboardingRoute,
])
// The Board keeps its trailing slash, so its address reads `/w/<id>/`.
export const router = createRouter({ routeTree, trailingSlash: 'preserve' })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
