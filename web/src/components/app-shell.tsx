import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, Outlet, useNavigate, useRouterState } from '@tanstack/react-router'
import {
  Activity,
  BellRing,
  FileCode2,
  FolderGit2,
  Kanban,
  KeyRound,
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
  fetchWorkspaces,
  SESSION_AUTH,
  signOutDashboardSession,
} from '../lib/api'
import { isBlueprintAnchor } from '../lib/blueprint'
import { cn } from '../lib/utils'
import { ThemeProvider, useTheme } from './theme-provider'
import { Badge } from './ui/badge'
import { Button } from './ui/button'
import { Input } from './ui/input'

export type WorkspaceCapability =
  | 'view_workspace'
  | 'claim_work'
  | 'request_changes'
  | 'propose_documents'
  | 'confirm_documents'
  | 'manage_membership'
  | 'set_assignee'
  | 'operate_gates'
  | 'recover_work'
  | 'manage_workspace'

const roleCapabilities: Record<import('../lib/types').WorkspaceRole, readonly WorkspaceCapability[]> = {
  viewer: ['view_workspace'],
  executor: ['view_workspace', 'claim_work', 'request_changes'],
  contributor: ['view_workspace', 'claim_work', 'request_changes', 'propose_documents'],
  maintainer: [
    'view_workspace',
    'claim_work',
    'request_changes',
    'propose_documents',
    'set_assignee',
    'operate_gates',
    'recover_work',
  ],
  operator: [
    'view_workspace',
    'claim_work',
    'request_changes',
    'propose_documents',
    'confirm_documents',
    'manage_membership',
    'set_assignee',
    'operate_gates',
    'recover_work',
    'manage_workspace',
  ],
}

// The context carries either a tab-scoped operator bearer token or an opaque
// marker indicating that the browser's HttpOnly session cookie is active.
const TokenContext = createContext<{ token: string; operatorToken: string; setToken: (value: string) => void }>({
  token: '',
  operatorToken: '',
  setToken: () => {},
})

const WorkspaceContext = createContext<{ workspace: string; setWorkspace: (value: string) => void }>({
  workspace: '',
  setWorkspace: () => {},
})

export function useWorkspaceSelection() {
  return useContext(WorkspaceContext)
}

export function useOperatorToken() {
  return useContext(TokenContext).token
}

export function useTokenState() {
  const { operatorToken, setToken } = useContext(TokenContext)
  return { token: operatorToken, setToken }
}

// Dashboard affordances consume the same fixed role bundles as the server.
// This is presentation only: every request still crosses the server capability
// boundary, while a viewer never sees a control that can only be refused.
export function useWorkspaceCapability(capability: WorkspaceCapability) {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const identity = useQuery({
    queryKey: ['caller-identity', token, workspace],
    queryFn: () => fetchCallerIdentity(token),
    enabled: Boolean(token && workspace),
    retry: false,
  })
  return Boolean(identity.data?.role && roleCapabilities[identity.data.role]?.includes(capability))
}

// The Board passes the shared filter family so the server returns what it will
// show (AC-2.4). Callers that speak for the whole workspace — the attention
// badge below, the sheet's prev/next order — pass nothing and keep reading the
// unfiltered feed: an operator narrowing the board must not also narrow what is
// waiting on them.
export function useActivity(filter?: Record<string, string | string[] | undefined>, enabled = true) {
  const { workspace } = useWorkspaceSelection()
  return useQuery({
    queryKey: ['activity', workspace, filter ?? null],
    queryFn: () => fetchActivity(filter),
    enabled: enabled && !!workspace,
    refetchInterval: 15_000,
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
    refetchInterval: () => (document.visibilityState === 'visible' ? 15_000 : false),
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    refetchOnReconnect: true,
  })
}

export function AppShell() {
  const pathname = useRouterState({ select: (state) => state.location.pathname })
  if (pathname === '/sign-in') {
    return (
      <ThemeProvider>
        <Outlet />
      </ThemeProvider>
    )
  }
  return <AuthenticatedAppShell />
}

function AuthenticatedAppShell() {
  const [token, setToken] = useState(() => sessionStorage.getItem('conveyor-token') ?? '')
  const saveToken = (value: string) => {
    setToken(value)
    if (value) sessionStorage.setItem('conveyor-token', value)
    else sessionStorage.removeItem('conveyor-token')
  }
  const workspaces = useQuery({
    queryKey: ['workspaces', token],
    queryFn: () => fetchWorkspaces(token),
    retry: false,
  })
  const credential = token || (workspaces.isSuccess ? SESSION_AUTH : '')

  return (
    <ThemeProvider>
      <TokenContext.Provider value={{ token: credential, operatorToken: token, setToken: saveToken }}>
        {workspaces.isPending ? (
          <div className="grid h-screen place-items-center bg-background text-sm text-muted">Opening Conveyor…</div>
        ) : workspaces.isError ? (
          <SignInRequired token={token} setToken={saveToken} />
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
      </TokenContext.Provider>
    </ThemeProvider>
  )
}

function SignInRequired({ token, setToken }: { token: string; setToken: (value: string) => void }) {
  const [value, setValue] = useState(token)
  const queryClient = useQueryClient()
  return (
    <main className="grid min-h-screen place-items-center bg-background px-6 py-12">
      <div className="w-full max-w-md rounded-xl border border-border bg-card p-7 shadow-sm">
        <div className="mb-5 grid size-10 place-items-center rounded-lg bg-primary-soft text-primary">
          <KeyRound className="size-5" />
        </div>
        <h1 className="text-xl font-semibold tracking-tight">Sign in to Conveyor</h1>
        <p className="mt-2 text-sm leading-6 text-muted">
          Open the invitation link sent by your operator. Each link can be used once.
        </p>
        <div className="my-6 flex items-center gap-3 text-xs text-faint">
          <span className="h-px flex-1 bg-border" />
          Operator fallback
          <span className="h-px flex-1 bg-border" />
        </div>
        <form
          className="space-y-3"
          onSubmit={(event) => {
            event.preventDefault()
            setToken(value.trim())
            void queryClient.invalidateQueries({ queryKey: ['workspaces'] })
          }}
        >
          <Input
            type="password"
            aria-label="Operator token"
            placeholder="Paste the operator token"
            value={value}
            onChange={(event) => setValue(event.target.value)}
          />
          <Button type="submit" className="w-full" disabled={!value.trim()}>
            Continue as operator
          </Button>
        </form>
        {token && <p className="mt-3 text-xs text-failure">That operator token was not accepted.</p>}
      </div>
    </main>
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
  return <WorkspaceContext.Provider value={{ workspace, setWorkspace }}>{children}</WorkspaceContext.Provider>
}

// The rail is the workspace switcher (§21.10: workspace context is explicit
// everywhere): one initials tile per workspace, "+" creates a new one.
function IconRail() {
  const token = useOperatorToken()
  const { token: operatorToken } = useTokenState()
  const navigate = useNavigate()
  const { workspace: selected, setWorkspace } = useWorkspaceSelection()
  const { data: workspaces } = useQuery({
    queryKey: ['workspaces', token],
    queryFn: () => fetchWorkspaces(token),
    enabled: !!token,
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
        {operatorToken && (
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
  const token = useOperatorToken()
  const { setToken } = useTokenState()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const { workspace: selected } = useWorkspaceSelection()
  const { data: workspaces } = useQuery({
    queryKey: ['workspaces', token],
    queryFn: () => fetchWorkspaces(token),
    enabled: !!token,
  })
  const { data: workspace } = useWorkspace()
  const { data: activity } = useActivity()
  const { data: proposals } = usePendingProposals()
  // Until the combined projection arrives, preserve the Board's existing
  // task-only fallback and keep blueprint anchors out of that count.
  const taskAttention = (activity ?? []).filter((item) => !isBlueprintAnchor(item.task) && item.needs_attention).length
  // The server owns the combined derivation (AC-1.1); fall back to the
  // already-loaded task count only while the protected projection is loading.
  // Older fixtures and briefly mixed-version deployments can return the
  // previous empty collection shape; degrade to the task signal instead of
  // taking down the app shell while the projection catches up.
  const attention = proposals?.attention?.total ?? taskAttention
  const requirementAttention = new Set(
    (proposals?.items ?? []).filter((item) => item.tier === 'requirement').map((item) => item.id),
  ).size

  const currentName = workspaces?.find((item) => item.id === selected)?.name ?? workspace?.workspace ?? 'Conveyor'
  const signOut = useMutation({
    mutationFn: signOutDashboardSession,
    onSettled: () => {
      setToken('')
      localStorage.removeItem('conveyor-workspace')
      queryClient.clear()
      void navigate({ to: '/' })
    },
  })

  return (
    <nav className="flex w-56 shrink-0 flex-col border-r border-border bg-rail" aria-label="Primary">
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
        <NavItem to="/pending-proposals" icon={BellRing} label="Pending proposals" />
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
