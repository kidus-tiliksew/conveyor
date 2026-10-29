import { type QueryClient, queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, Outlet, useNavigate, useRouterState } from '@tanstack/react-router'
import {
  Activity,
  FileCode2,
  FolderGit2,
  Inbox,
  Kanban,
  LogOut,
  type LucideIcon,
  Menu,
  Plus,
  Settings,
  SunMoon,
  Workflow,
} from 'lucide-react'
import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useState,
  useSyncExternalStore,
} from 'react'
import {
  fetchActivity,
  fetchBlueprints,
  fetchCallerIdentity,
  fetchPendingProposals,
  fetchSystemDesigns,
  fetchWorkspace,
  fetchWorkspaceMembers,
  fetchWorkspaces,
  signOutDashboardSession,
  WorkspacesError,
} from '../lib/api'
import { sameWorkspacePlaceholder } from '../lib/query-keys'
import type { ActivityPage, WorkspaceCapability, WorkspaceRole } from '../lib/types'
import { useMediaQuery, wideLayoutQuery } from '../lib/use-media-query'
import { cn } from '../lib/utils'
import { ThemeProvider, useTheme } from './theme-provider'
import { Badge } from './ui/badge'
import { Button } from './ui/button'
import { Sheet } from './ui/sheet'

const WorkspaceContext = createContext<{ workspace: string; setWorkspace: (value: string) => void }>({
  workspace: '',
  setWorkspace: () => {},
})

export function useWorkspaceSelection() {
  return useContext(WorkspaceContext)
}

// Slow-changing shared reads stay fresh for five minutes: identity, members,
// blueprints, and the task sheet's System Design collection. Their existing
// mutation invalidations still refresh them at once, and a workspace switch or
// sign-out still drops them (component-web-dashboard, Server-state discipline).
export const slowReadStaleTime = 5 * 60_000

// Registered in main.tsx as the family defaults of caller-identity,
// workspace-members, and blueprints, so a reader outside the shell that sets
// no stale time of its own shares the same freshness.
export const slowReadQueryDefaults = { staleTime: slowReadStaleTime }

// One option factory per slow read: the shell's hooks and the task sheet's
// mount-time prefetch build from these, so a prefetched entry and its later
// observer share one key, one request, and one stale time.
//
// The caller's identity in the workspace the key names; '' is the unscoped
// creation-right read. The workspace captured in the query key is the one the
// request names, so every entry holds exactly that workspace's projection.
export function callerIdentityQueryOptions(workspace: string) {
  return queryOptions({
    queryKey: ['caller-identity', workspace],
    queryFn: () => fetchCallerIdentity(workspace),
    retry: false,
    staleTime: slowReadStaleTime,
  })
}

export function workspaceMembersQueryOptions(workspace: string) {
  return queryOptions({
    queryKey: ['workspace-members', workspace],
    queryFn: () => fetchWorkspaceMembers(workspace),
    enabled: Boolean(workspace),
    staleTime: slowReadStaleTime,
    retry: false,
  })
}

// No interval here: only the Blueprints routes poll (useBlueprints).
export function blueprintsQueryOptions(workspace: string) {
  return queryOptions({
    queryKey: ['blueprints', workspace],
    queryFn: fetchBlueprints,
    enabled: Boolean(workspace),
    staleTime: slowReadStaleTime,
  })
}

// The non-archived System Design collection as the task sheet reads it;
// useSystemDesignProposals keeps the same key and request with this stale time.
export function systemDesignsQueryOptions(workspace: string) {
  return queryOptions({
    queryKey: ['system-designs', workspace],
    queryFn: () => fetchSystemDesigns(),
    enabled: Boolean(workspace),
    staleTime: slowReadStaleTime,
  })
}

function useCallerIdentity() {
  const { workspace } = useWorkspaceSelection()
  return useQuery({ ...callerIdentityQueryOptions(workspace), enabled: Boolean(workspace) })
}

// Dashboard affordances follow the capability list the server serves for the
// selected workspace; the browser holds no role-to-capability table
// (req-accounts-and-membership AC-5.1). This is presentation only: every
// request still crosses the server capability boundary, while a viewer never
// sees a control that can only be refused. No workspace, a pending or failed
// read, and a missing or empty list all deny, including a failed refetch over
// an earlier successful projection.
export function useWorkspaceCapability(capability: WorkspaceCapability) {
  const { workspace } = useWorkspaceSelection()
  const identity = useCallerIdentity()
  if (!workspace || identity.status !== 'success') return false
  return identity.data.capabilities?.includes(capability) ?? false
}

// The caller's own workspace-creation right. It is read unscoped:
// fetchCallerIdentity('') sends neither workspace_id nor X-Workspace-ID,
// whatever the stored selection, and the query runs with no selection and no
// memberships. Only a successful read whose can_create_workspaces is exactly
// true allows; pending, failed, missing, or malformed reads deny, including a
// failed refetch over an earlier successful read. The entry follows the
// shell's cache rules, so a workspace switch or sign-out drops it
// (component-web-dashboard). Presentation only: the server authorizes
// POST /v1/workspaces again.
export function useWorkspaceCreationStatus(): 'pending' | 'allowed' | 'denied' {
  const identity = useQuery(callerIdentityQueryOptions(''))
  if (identity.status === 'pending') return 'pending'
  if (identity.status === 'success' && identity.data.can_create_workspaces === true) return 'allowed'
  return 'denied'
}

export function useCanCreateWorkspaces() {
  return useWorkspaceCreationStatus() === 'allowed'
}

// The server's ascending role chain for the selected workspace, used for the
// membership picker's options and highest-role emphasis. Empty until a
// successful read serves it.
export function useWorkspaceRoles(): WorkspaceRole[] {
  const { workspace } = useWorkspaceSelection()
  const identity = useCallerIdentity()
  if (!workspace || identity.status !== 'success') return []
  return identity.data.roles ?? []
}

type ActivityFilter = Record<string, string | string[] | undefined>

// TanStack keeps each server representation independently addressable, while
// this queue keeps simultaneous consumers from issuing overlapping activity
// reads. WorkspaceProvider owns the only refresh clock below; queries merely
// describe the page they need (component-web-dashboard). Each read carries its
// query's abort signal, so a cancelled read, such as one a workspace switch
// cancels, aborts its request and releases the queue for the next workspace.
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
    queryFn: ({ signal }) => {
      const previous = queryClient.getQueryData<import('../lib/types').ActivityPage>(queryKey)
      return enqueueActivityRequest(queryClient, () =>
        fetchActivity({
          workspace,
          signal,
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
    // A filter or page change keeps the previous page on screen; a workspace
    // switch shows the first-load state instead.
    placeholderData: sameWorkspacePlaceholder(workspace),
    refetchInterval: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
}

export function useWorkspaceMembers() {
  const { workspace } = useWorkspaceSelection()
  return useQuery(workspaceMembersQueryOptions(workspace))
}

// Blueprint anchors left the activity feed, so this projection
// is the only place the dashboard can resolve one — the Blueprints surface,
// the anchor's own detail, and a child's parent reference all read it. It
// polls every 15 s only on the Blueprints routes; a task sheet or full task
// route reading a parent's title never polls (component-web-dashboard).
export function useBlueprints() {
  const { workspace } = useWorkspaceSelection()
  const onBlueprints = useRouterState({ select: (state) => isBlueprintsPath(state.location.pathname) })
  return useQuery({ ...blueprintsQueryOptions(workspace), refetchInterval: onBlueprints ? 15_000 : false })
}

function isBlueprintsPath(pathname: string) {
  return pathname === '/blueprints' || pathname.startsWith('/blueprints/')
}

// The Board's current activity page, read from the Query cache without
// mounting an activity observer or sending a request (component-web-dashboard,
// Server-state discipline). The Kanban view is the only activity observer, so
// its page is the one active member of the workspace's activity family, and
// that member's key already carries the Board's canonical filter, limit, and
// offset. No active member, more than one, or one without data yields nothing:
// an inactive cached page is never read, and nothing pages or fetches.
export function useBoardActivityPage(): ActivityPage | undefined {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const subscribe = useCallback(
    (onChange: () => void) => {
      let active = true
      // Cache events can fire while another component renders; read after it.
      const unsubscribe = queryClient.getQueryCache().subscribe(() => queueMicrotask(() => active && onChange()))
      return () => {
        active = false
        unsubscribe()
      }
    },
    [queryClient],
  )
  const snapshot = useCallback(() => {
    if (!workspace) return undefined
    const pages = queryClient.getQueryCache().findAll({ queryKey: ['activity', workspace], type: 'active' })
    return pages.length === 1 ? (pages[0].state.data as ActivityPage | undefined) : undefined
  }, [queryClient, workspace])
  return useSyncExternalStore(subscribe, snapshot)
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

// Only a 401 means the dashboard session is gone (req-invitations-and-sign-in
// AC-4.3). A network rejection, a 5xx, or another 4xx is a failed read of a
// session that may still be valid.
function isSignedOut(error: unknown) {
  return error instanceof WorkspacesError && error.status === 401
}

const workspacesRetryDelaysMs = [1_000, 2_000] as const

// The retry policy of the workspace list, registered as the `['workspaces']`
// query defaults so every reader of the list fetches with it, whichever one
// triggers a focus or reconnect refetch (component-web-dashboard). A non-401
// failure is retried twice, after 1 s and then 2 s; a 401 is never retried.
// A reader that mounts does not refetch: the shell's own reader stays mounted
// for the session, and a page the router renders briefly while leaving for
// `/sign-in` must send no read after a 401. A failed read is not repeated on
// mount either. Focus, reconnect, and Retry still refetch.
export const workspacesQueryDefaults = {
  refetchOnMount: false,
  retryOnMount: false,
  retry: (failureCount: number, error: unknown) => !isSignedOut(error) && failureCount < workspacesRetryDelaysMs.length,
  retryDelay: (failureCount: number) =>
    workspacesRetryDelaysMs[Math.min(failureCount, workspacesRetryDelaysMs.length - 1)],
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
  const wide = useMediaQuery(wideLayoutQuery)
  const workspaces = useQuery({
    queryKey: ['workspaces'],
    queryFn: fetchWorkspaces,
  })
  // A 401 leaves for sign-in even over a cached list. Any other failure keeps
  // the cached list mounted, or offers Retry when no list has arrived yet.
  const signedOut = isSignedOut(workspaces.error)
  useEffect(() => {
    if (signedOut) void navigate({ to: '/sign-in', replace: true })
  }, [navigate, signedOut])

  return (
    <ThemeProvider>
      {signedOut || (workspaces.data === undefined && !workspaces.isError) ? (
        <div className="grid h-dvh place-items-center bg-background text-sm text-muted">Opening Conveyor…</div>
      ) : workspaces.data === undefined ? (
        <div className="grid h-dvh place-items-center bg-background px-6">
          <div role="alert" className="max-w-sm text-center">
            <h1 className="text-base font-semibold">Could not reach Conveyor</h1>
            <p className="mt-2 text-sm text-muted">Your session is still signed in. Check your connection and retry.</p>
            {/* A refetch without a cached list returns the shell to "Opening
                Conveyor…" until it settles, so one click starts one read. */}
            <Button className="mt-4" disabled={workspaces.isFetching} onClick={() => void workspaces.refetch()}>
              Retry
            </Button>
          </div>
        </div>
      ) : (
        <WorkspaceProvider workspaces={workspaces.data}>
          {/* Desktop keeps the two fixed navigation columns beside the
              content. Below the lg breakpoint they would leave a phone with
              no room to read, so the same rail and sidebar move into a
              drawer behind a top bar (component-web-dashboard). The dynamic
              viewport unit keeps the shell inside the visible area while a
              mobile browser shows or hides its own chrome. */}
          <div className="flex h-dvh flex-col overflow-hidden lg:flex-row">
            {wide ? (
              <>
                <IconRail />
                <NavSidebar />
              </>
            ) : (
              <MobileNav />
            )}
            <main className="min-w-0 min-h-0 flex-1 overflow-hidden bg-background">
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
    // request loops (component-web-dashboard).
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

// Navigation inside the narrow-viewport drawer closes the drawer once it has
// done its job; on desktop the same items render in place and this is a no-op.
const ShellNavContext = createContext<{ closeDrawer: () => void }>({ closeDrawer: () => {} })

// The narrow-viewport shell: a slim bar carrying the workspace name, the
// operator-decision count linking to the Inbox, and the menu that opens the
// rail and sidebar as a drawer.
function MobileNav() {
  const [open, setOpen] = useState(false)
  const { workspace: selected } = useWorkspaceSelection()
  const { data: workspaces } = useQuery({
    queryKey: ['workspaces'],
    queryFn: fetchWorkspaces,
  })
  const { data: proposals } = usePendingProposals()
  const operatorDecisions = proposals?.attention?.operator_decisions ?? 0
  const currentName = workspaces?.find((item) => item.id === selected)?.name ?? 'Conveyor'
  const close = () => setOpen(false)
  return (
    <>
      <header className="flex h-12 shrink-0 items-center gap-2 border-b border-border bg-rail px-2">
        <Button variant="ghost" size="icon" aria-label="Open navigation" onClick={() => setOpen(true)}>
          <Menu />
        </Button>
        <p className="min-w-0 flex-1 truncate text-sm font-semibold tracking-tight">{currentName}</p>
        {operatorDecisions > 0 && (
          <Link to="/inbox" aria-label={`${operatorDecisions} decisions waiting in the Inbox`}>
            <Badge variant="attention">{operatorDecisions}</Badge>
          </Link>
        )}
      </header>
      {open && (
        <ShellNavContext.Provider value={{ closeDrawer: close }}>
          {/* Rail plus sidebar at their desktop widths, so the drawer
              reads exactly like the columns it stands in for. */}
          <Sheet onClose={close} label="Navigation" side="left" width="w-auto max-w-[90vw]">
            <div className="flex min-h-0 flex-1">
              <IconRail />
              <NavSidebar className="min-w-0 shrink border-r-0" />
            </div>
          </Sheet>
        </ShellNavContext.Provider>
      )}
    </>
  )
}

// The rail is the workspace switcher (component-web-dashboard: workspace context is explicit
// everywhere): one initials tile per workspace, "+" creates a new one when the
// caller's own creation right allows it.
function IconRail() {
  const { closeDrawer } = useContext(ShellNavContext)
  const canCreateWorkspaces = useCanCreateWorkspaces()
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
    closeDrawer()
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
        {canCreateWorkspaces && (
          <Link
            to="/workspaces/new"
            title="Add workspace"
            aria-label="Add workspace"
            onClick={closeDrawer}
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

function NavSidebar({ className }: { className?: string }) {
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const { workspace: selected } = useWorkspaceSelection()
  const { data: workspaces } = useQuery({
    queryKey: ['workspaces'],
    queryFn: fetchWorkspaces,
  })
  const { data: workspace } = useWorkspace()
  const { data: proposals } = usePendingProposals()
  // The bounded pending-proposals projection owns the workspace's count of
  // operator decisions (AC-2.7). Reading it here avoids mounting a second
  // unfiltered query beside the Board's filter-keyed activity request.
  const operatorDecisions = proposals?.attention?.operator_decisions ?? 0
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
    <nav className={cn('flex w-64 shrink-0 flex-col border-r border-border bg-rail', className)} aria-label="Primary">
      <div className="px-4 py-4">
        <p className="truncate text-sm font-semibold tracking-tight">{currentName}</p>
        <p className="mt-0.5 text-[11px] text-faint">Conveyor · software factory</p>
      </div>
      <div className="flex-1 space-y-0.5 px-2">
        <NavItem to="/" icon={Kanban} label="Board" />
        <NavItem to="/inbox" icon={Inbox} label="Inbox">
          {operatorDecisions > 0 && <Badge variant="attention">{operatorDecisions}</Badge>}
        </NavItem>
        <NavItem to="/workspace" icon={FolderGit2} label="Workspace" />
        <NavItem to="/requirements" icon={Workflow} label="Requirements">
          {requirementAttention > 0 && <Badge variant="attention">{requirementAttention}</Badge>}
        </NavItem>
        <NavItem to="/system-design" icon={FileCode2} label="System Design" />
        {/* Exactly the accepted operating surfaces, and no others
            (req-document-operating-surfaces REQ-4, AC-4.1). Planning and
            Blueprint history are parked, not retired: their routes stay
            mounted for deep links, the blueprint anchor redirect
            (component-web-task-surfaces) is untouched, and blueprint history reaches through
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
  const { closeDrawer } = useContext(ShellNavContext)
  // The board stays highlighted while any of its overlays (task sheet, full
  // page, workspace modal) is open (req-operator-next-action AC-6.10). `/tasks`
  // itself only redirects to the Board (AC-6.3).
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
      // without this, a prefix match could announce another entry as the
      // current page while the Board is the surface actually showing.
      activeOptions={exact ? { exact: true } : undefined}
      onClick={closeDrawer}
      className={`flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm transition-colors pointer-coarse:py-2.5 ${
        active ? 'bg-raised font-medium text-foreground' : 'text-muted hover:bg-raised/60 hover:text-foreground'
      }`}
    >
      <Icon className="size-4" />
      <span className="flex-1">{label}</span>
      {children}
    </Link>
  )
}
