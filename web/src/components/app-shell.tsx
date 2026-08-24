import { type QueryClient, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, Outlet, useNavigate, useRouterState } from '@tanstack/react-router'
import {
  Activity,
  BellRing,
  FileCode2,
  FolderGit2,
  Kanban,
  ListChecks,
  LogOut,
  type LucideIcon,
  Plus,
  Settings,
  SunMoon,
  Workflow,
} from 'lucide-react'
import { createContext, type ReactNode, useContext, useEffect, useState } from 'react'
import {
  fetchActivity,
  fetchBlueprints,
  fetchCallerIdentity,
  fetchPendingProposals,
  fetchWorkspace,
  fetchWorkspaceMembers,
  fetchWorkspaces,
  signOutDashboardSession,
} from '../lib/api'
import { roleCapabilities, type WorkspaceCapability } from '../lib/workspace-capabilities'
import { cn } from '../lib/utils'
import { ThemeProvider, useTheme } from './theme-provider'
import { Badge } from './ui/badge'
import { Button } from './ui/button'

const WorkspaceContext = createContext<{ workspace: string; setWorkspace: (value: string) => void }>({
  workspace: '',
  setWorkspace: () => {},
})

export function useWorkspaceSelection() {
  return useContext(WorkspaceContext)
}

// Dashboard affordances consume the same fixed role bundles as the server.
// This is presentation only: every request still crosses the server capability
// boundary, while a viewer never sees a control that can only be refused.
export function useWorkspaceCapability(capability: WorkspaceCapability) {
  const { workspace } = useWorkspaceSelection()
  const identity = useQuery({
    queryKey: ['caller-identity', workspace],
    queryFn: () => fetchCallerIdentity(),
    enabled: Boolean(workspace),
    retry: false,
  })
  return Boolean(identity.data?.role && roleCapabilities[identity.data.role]?.includes(capability))
}

type ActivityFilter = Record<string, string | string[] | undefined>

// TanStack keeps each server representation independently addressable, while
// this queue keeps simultaneous consumers from issuing overlapping activity
// reads. WorkspaceProvider owns the only refresh clock below; queries merely
// describe the page they need (design-web-dashboard).
const activityRequestTails = new WeakMap<QueryClient, Promise<void>>()

function enqueueActivityRequest<T>(queryClient: QueryClient, request: () => Promise<T>): Promise<T> {
  const previous = activityRequestTails.get(queryClient) ?? Promise.resolve()
  const current = previous.catch(() => undefined).then(request)
  activityRequestTails.set(
    queryClient,
    current.then(
      () => undefined,
      () => undefined,
    ),
  )
  return current
}

// List-valued filter members are disjunctions, so their order has no semantic
// meaning. Canonicalizing them makes equivalent Board selections share one
// Query entry and one conditional-revalidation history.
function normalizeActivityFilter(filter?: ActivityFilter): Record<string, string[]> {
  return Object.fromEntries(
    Object.entries(filter ?? {})
      .sort(([left], [right]) => left.localeCompare(right))
      .flatMap(([key, value]) => {
        const values = (Array.isArray(value) ? value : value ? [value] : []).slice().sort()
        return values.length > 0 ? [[key, values] as const] : []
      }),
  )
}

// Every activity consumer reads a server-paged representation from the one
// workspace cache family. WorkspaceProvider serializes and refreshes every
// active member, so adding a filtered Board page does not add another polling
// lifecycle or overlap the unfiltered task-detail consumers.
export function useActivity(filter?: ActivityFilter, enabled = true, offset = 0, limit = 100) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const normalizedFilter = normalizeActivityFilter(filter)
  const queryKey = ['activity', workspace, { filter: normalizedFilter, limit, offset }] as const
  return useQuery({
    queryKey,
    queryFn: () => {
      const previous = queryClient.getQueryData<import('../lib/types').ActivityPage>(queryKey)
      return enqueueActivityRequest(queryClient, () =>
        fetchActivity({
          limit,
          offset,
          filter: normalizedFilter,
          // Only the unfiltered first page has a stable merge boundary. Later
          // pages can shift when newer tasks arrive, while a filtered task can
          // leave its result without appearing in the delta. Those pages use
          // ETag revalidation with full response bodies instead.
          cursor: offset === 0 && Object.keys(normalizedFilter).length === 0 ? previous?.cursor : undefined,
          etag: previous?.etag,
          previous,
        }),
      )
    },
    enabled: enabled && !!workspace,
    placeholderData: (previous) => previous,
    refetchInterval: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
}

export function useWorkspaceMembers() {
  const { workspace } = useWorkspaceSelection()
  return useQuery({
    queryKey: ['workspace-members', workspace],
    queryFn: () => fetchWorkspaceMembers(workspace),
    enabled: Boolean(workspace),
    staleTime: 60_000,
    retry: false,
  })
}

// Blueprint anchors left the activity feed, so this projection
// is the only place the dashboard can resolve one — the Blueprints surface,
// the anchor's own detail, and a child's parent reference all read it.
export function useBlueprints() {
  const { workspace } = useWorkspaceSelection()
  return useQuery({
    queryKey: ['blueprints', workspace],
    queryFn: fetchBlueprints,
    enabled: !!workspace,
    refetchInterval: 15_000,
  })
}

export function useWorkspace() {
  const { workspace } = useWorkspaceSelection()
  return useQuery({
    queryKey: ['workspace', workspace],
    queryFn: fetchWorkspace,
    enabled: !!workspace,
    staleTime: 60_000,
    retry: 1,
  })
}

export function usePendingProposals() {
  const { workspace } = useWorkspaceSelection()
  return useQuery({
    queryKey: ['pending-proposals', workspace],
    queryFn: fetchPendingProposals,
    enabled: Boolean(workspace),
    staleTime: 15_000,
    refetchInterval: () => (document.visibilityState === 'visible' ? 15_000 : false),
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    refetchOnReconnect: 'always',
  })
}

export function AppShell() {
  const pathname = useRouterState({ select: (state) => state.location.pathname })
  if (pathname === '/sign-in' || pathname === '/onboarding') {
    return (
      <ThemeProvider>
        <Outlet />
      </ThemeProvider>
    )
  }
  return <AuthenticatedAppShell />
}

function AuthenticatedAppShell() {
  const navigate = useNavigate()
  const workspaces = useQuery({
    queryKey: ['workspaces'],
    queryFn: fetchWorkspaces,
    retry: false,
  })
  useEffect(() => {
    if (workspaces.isError) void navigate({ to: '/sign-in', replace: true })
  }, [navigate, workspaces.isError])

  return (
    <ThemeProvider>
      {workspaces.isPending || workspaces.isError ? (
        <div className="grid h-screen place-items-center bg-background text-sm text-muted">Opening Conveyor…</div>
      ) : (
        <WorkspaceProvider workspaces={workspaces.data}>
          <div className="flex h-screen overflow-hidden">
            <IconRail />
            <NavSidebar />
            <main className="min-w-0 flex-1 overflow-hidden bg-background">
              <Outlet />
            </main>
          </div>
        </WorkspaceProvider>
      )}
    </ThemeProvider>
  )
}

function WorkspaceProvider({
  workspaces,
  children,
}: {
  workspaces: import('../lib/types').WorkspaceRecord[]
  children: ReactNode
}) {
  const queryClient = useQueryClient()
  const [workspace, setWorkspaceState] = useState(() => localStorage.getItem('conveyor-workspace') ?? '')
  const setWorkspace = (value: string) => {
    void queryClient.cancelQueries()
    if (value) localStorage.setItem('conveyor-workspace', value)
    else localStorage.removeItem('conveyor-workspace')
    setWorkspaceState(value)
    queryClient.removeQueries({ predicate: (query) => query.queryKey[0] !== 'workspaces' })
  }
  useEffect(() => {
    if (workspaces.length === 1 && !workspaces.some((item) => item.id === workspace)) setWorkspace(workspaces[0].id)
    else if (!workspaces.some((item) => item.id === workspace) && workspace) setWorkspace('')
  }, [workspaces, workspace])
  useEffect(() => {
    if (!workspace) return
    let disposed = false
    let refreshRunning = false
    let refreshIsCold = false
    let refreshPending = false
    let coldPending = false

    // One workspace clock refreshes all active activity representations in
    // sequence. Calls that arrive while a pass is running coalesce into one
    // follow-up pass, so focus/reconnect/visibility cannot create competing
    // request loops (design-web-dashboard).
    const refreshActivity = async (cold: boolean) => {
      if (refreshRunning) {
        // An ordinary tick cannot improve on a pass already in flight. A
        // cold trigger needs one follow-up only when the current pass still
        // carries conditional state.
        if (cold && !refreshIsCold) {
          refreshPending = true
          coldPending = true
        }
        return
      }
      refreshRunning = true
      let coldPass = cold
      do {
        refreshIsCold = coldPass
        refreshPending = false
        const queries = queryClient.getQueryCache().findAll({ queryKey: ['activity', workspace], type: 'active' })
        for (const query of queries) {
          if (disposed) break
          if (coldPass) {
            queryClient.setQueryData<import('../lib/types').ActivityPage>(query.queryKey, (current) =>
              current ? { ...current, cursor: undefined, etag: undefined } : current,
            )
          }
          try {
            await query.fetch()
          } catch {
            // Each query exposes its own error state to its consumer. One
            // failed representation must not prevent the others refreshing.
          }
        }
        coldPass = coldPending
        coldPending = false
      } while (refreshPending && !disposed && document.visibilityState === 'visible')
      refreshIsCold = false
      refreshRunning = false
    }

    const scheduledRefresh = () => {
      if (document.visibilityState === 'visible') void refreshActivity(false)
    }
    const coldRefresh = () => void refreshActivity(true)
    const visibleRefresh = () => {
      if (document.visibilityState === 'visible') coldRefresh()
    }
    const interval = window.setInterval(scheduledRefresh, 15_000)
    window.addEventListener('online', coldRefresh)
    window.addEventListener('focus', coldRefresh)
    document.addEventListener('visibilitychange', visibleRefresh)
    return () => {
      disposed = true
      window.clearInterval(interval)
      window.removeEventListener('online', coldRefresh)
      window.removeEventListener('focus', coldRefresh)
      document.removeEventListener('visibilitychange', visibleRefresh)
    }
  }, [queryClient, workspace])
  return <WorkspaceContext.Provider value={{ workspace, setWorkspace }}>{children}</WorkspaceContext.Provider>
}

// The rail is the workspace switcher (§21.10: workspace context is explicit
// everywhere): one initials tile per workspace, "+" creates a new one.
function IconRail() {
  const canManageWorkspace = useWorkspaceCapability('manage_workspace')
  const navigate = useNavigate()
  const { workspace: selected, setWorkspace } = useWorkspaceSelection()
  const { data: workspaces } = useQuery({
    queryKey: ['workspaces'],
    queryFn: fetchWorkspaces,
  })
  // Land on the board after a switch so an open task sheet from the previous
  // workspace can't linger pointing at a task the new context can't resolve.
  const switchTo = (id: string) => {
    if (id !== selected) setWorkspace(id)
    void navigate({ to: '/' })
  }
  return (
    <nav
      aria-label="Workspaces"
      className="flex w-14 shrink-0 flex-col items-center border-r border-border bg-rail py-3"
    >
      <div className="flex min-h-0 w-full flex-1 flex-col items-center gap-6 overflow-y-auto py-1">
        {(workspaces ?? []).map((item) => (
          <button
            key={item.id}
            type="button"
            title={item.name}
            aria-label={`Switch to ${item.name}`}
            aria-current={item.id === selected ? 'true' : undefined}
            onClick={() => switchTo(item.id)}
            className={cn(
              'grid size-8 shrink-0 place-items-center rounded-[9px] text-xs font-semibold tracking-[-0.01em] transition-[background-color,color,box-shadow] duration-150 focus-visible:outline-2 focus-visible:outline-offset-[5px] focus-visible:outline-primary',
              item.id === selected
                ? 'bg-workspace-active text-workspace-active-foreground ring-2 ring-workspace-active-ring ring-offset-[3px] ring-offset-rail'
                : 'bg-workspace-tile text-workspace-tile-foreground hover:bg-workspace-tile-hover hover:text-foreground',
            )}
          >
            {initials(item.name || item.id)}
          </button>
        ))}
        {canManageWorkspace && (
          <Link
            to="/workspaces/new"
            title="Add workspace"
            aria-label="Add workspace"
            className="grid size-8 shrink-0 place-items-center rounded-[9px] text-workspace-tile-foreground transition-colors hover:bg-workspace-tile hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-[5px] focus-visible:outline-primary"
          >
            <Plus className="size-5" aria-hidden="true" />
          </Link>
        )}
      </div>
    </nav>
  )
}

function initials(name: string) {
  const parts = name.split(/[\s_-]+/).filter(Boolean)
  if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase()
  return name.slice(0, 2).toUpperCase()
}

function NavSidebar() {
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const { workspace: selected } = useWorkspaceSelection()
  const { data: workspaces } = useQuery({
    queryKey: ['workspaces'],
    queryFn: fetchWorkspaces,
  })
  const { data: workspace } = useWorkspace()
  const { data: proposals } = usePendingProposals()
  // The bounded pending-proposals projection owns the workspace attention
  // total. Reading it here avoids mounting a second unfiltered activity query
  // beside the Board's filter-keyed activity request.
  const attention = proposals?.attention?.total ?? 0
  const pendingProposalAttention = proposals?.attention?.pending_proposal_count ?? 0
  const requirementAttention = new Set(
    (proposals?.items ?? []).filter((item) => item.tier === 'requirement').map((item) => item.id),
  ).size

  const currentName = workspaces?.find((item) => item.id === selected)?.name ?? workspace?.workspace ?? 'Conveyor'
  const signOut = useMutation({
    mutationFn: signOutDashboardSession,
    onSettled: () => {
      localStorage.removeItem('conveyor-workspace')
      queryClient.clear()
      void navigate({ to: '/sign-in' })
    },
  })

  return (
    <nav className="flex w-64 shrink-0 flex-col border-r border-border bg-rail" aria-label="Primary">
      <div className="px-4 py-4">
        <p className="truncate text-sm font-semibold tracking-tight">{currentName}</p>
        <p className="mt-0.5 text-[11px] text-faint">Conveyor · software factory</p>
      </div>
      <div className="flex-1 space-y-0.5 px-2">
        <NavItem to="/" icon={Kanban} label="Board">
          {attention > 0 && <Badge variant="attention">{attention}</Badge>}
        </NavItem>
        {/* The list-first delivery surface. It matches exactly:
            a task's own detail route belongs to the Board it opens over. */}
        <NavItem to="/tasks" icon={ListChecks} label="Tasks" exact />
        <NavItem to="/workspace" icon={FolderGit2} label="Workspace" />
        <NavItem to="/requirements" icon={Workflow} label="Requirements">
          {requirementAttention > 0 && <Badge variant="attention">{requirementAttention}</Badge>}
        </NavItem>
        <NavItem to="/system-design" icon={FileCode2} label="System Design" />
        <NavItem to="/pending-proposals" icon={BellRing} label="Pending proposals">
          {pendingProposalAttention > 0 && <Badge variant="attention">{pendingProposalAttention}</Badge>}
        </NavItem>
        {/* Exactly the operating surfaces §21.61 accepts, and no others
            (REQ-4, AC-4.1). Planning and Blueprint history are parked, not
            retired: their routes stay mounted for deep links, the §21.49
            anchor redirect is untouched, and blueprint history reaches through
            task detail — only these two entries left the sidebar. */}
        <NavItem to="/monitor" icon={Activity} label="Monitor" />
        <NavItem to="/settings" icon={Settings} label="Settings" />
      </div>
      <Button
        variant="ghost"
        className="mx-2 justify-start text-muted"
        disabled={signOut.isPending}
        onClick={() => signOut.mutate()}
      >
        <LogOut />
        {signOut.isPending ? 'Signing out…' : 'Sign out'}
      </Button>
      <ThemeSwitcher />
    </nav>
  )
}

function ThemeSwitcher() {
  const { choice, setChoice } = useTheme()
  return (
    <label className="m-2 flex items-center gap-2 rounded-lg border border-border bg-surface px-2.5 py-2 text-xs text-muted">
      <SunMoon className="size-4 shrink-0" aria-hidden="true" />
      <span className="shrink-0">Theme</span>
      <select
        aria-label="Theme"
        value={choice}
        onChange={(event) => setChoice(event.target.value as typeof choice)}
        className="min-w-0 flex-1 cursor-pointer rounded border border-border bg-card px-1.5 py-1 text-xs text-foreground outline-none hover:border-edge focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
      >
        <option value="light">Light</option>
        <option value="dark">Dark</option>
        <option value="system">System</option>
      </select>
    </label>
  )
}

function NavItem({
  to,
  icon: Icon,
  label,
  exact,
  children,
}: {
  to: string
  icon: LucideIcon
  label: string
  exact?: boolean
  children?: ReactNode
}) {
  const pathname = useRouterState({ select: (state) => state.location.pathname })
  // The board stays highlighted while any of its overlays (task sheet, full
  // page, workspace modal) is open. A trailing slash keeps the Tasks list
  // itself out of that set — it is its own surface, and the sheets it now hosts
  // (intake, the detail panel) are search params on `/tasks`, so they stay
  // highlighted as the Tasks view they open over (AC-2.1, AC-2.2).
  const active =
    to === '/'
      ? pathname === '/' || pathname.startsWith('/tasks/') || pathname === '/workspaces/new'
      : exact
        ? pathname === to
        : pathname === to || pathname.startsWith(`${to}/`)
  return (
    <Link
      to={to}
      // The router's own aria-current must agree with the highlight above:
      // without this, "/tasks/<id>" would announce the Tasks list as the
      // current page while the Board is the surface actually showing.
      activeOptions={exact ? { exact: true } : undefined}
      className={`flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm transition-colors ${
        active ? 'bg-raised font-medium text-foreground' : 'text-muted hover:bg-raised/60 hover:text-foreground'
      }`}
    >
      <Icon className="size-4" />
      <span className="flex-1">{label}</span>
      {children}
    </Link>
  )
}
