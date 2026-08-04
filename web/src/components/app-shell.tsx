import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, Outlet, useNavigate, useRouterState } from '@tanstack/react-router'
import {
  Activity,
  Blocks,
  FolderGit2,
  Kanban,
  MessageSquare,
  Plus,
  Settings,
  SunMoon,
  Workflow,
  type LucideIcon,
} from 'lucide-react'
import { fetchActivity, fetchBlueprints, fetchWorkspace, fetchWorkspaces } from '../lib/api'
import { isBlueprintAnchor } from '../lib/blueprint'
import { cn } from '../lib/utils'
import { Badge } from './ui/badge'
import { ThemeProvider, useTheme } from './theme-provider'

// The operator bearer token authenticates mutations (spec §17.3).
// Session-scoped on purpose: closing the tab forgets it.
const TokenContext = createContext<{ token: string; setToken: (value: string) => void }>({
  token: '',
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
  return useContext(TokenContext)
}

export function useActivity() {
  const { workspace } = useWorkspaceSelection()
  return useQuery({
    queryKey: ['activity', workspace],
    queryFn: fetchActivity,
    enabled: !!workspace,
    refetchInterval: 15_000,
  })
}

// Blueprint anchors left the activity feed (spec §21.49), so this projection
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

export function AppShell() {
  const [token, setToken] = useState(() => sessionStorage.getItem('conveyor-token') ?? '')
  const saveToken = (value: string) => {
    setToken(value)
    sessionStorage.setItem('conveyor-token', value)
  }

  return (
    <ThemeProvider>
      <TokenContext.Provider value={{ token, setToken: saveToken }}>
        <WorkspaceProvider token={token}>
          <div className="flex h-screen overflow-hidden">
            <IconRail />
            <NavSidebar />
            <main className="min-w-0 flex-1 overflow-hidden bg-background">
              <Outlet />
            </main>
          </div>
        </WorkspaceProvider>
      </TokenContext.Provider>
    </ThemeProvider>
  )
}

function WorkspaceProvider({ token, children }: { token: string; children: ReactNode }) {
  const queryClient = useQueryClient()
  const [workspace, setWorkspaceState] = useState(() => localStorage.getItem('conveyor-workspace') ?? '')
  const { data: workspaces } = useQuery({
    queryKey: ['workspaces', token],
    queryFn: () => fetchWorkspaces(token),
    enabled: !!token,
  })
  const setWorkspace = (value: string) => {
    void queryClient.cancelQueries()
    if (value) localStorage.setItem('conveyor-workspace', value)
    else localStorage.removeItem('conveyor-workspace')
    setWorkspaceState(value)
    queryClient.removeQueries({ predicate: (query) => query.queryKey[0] !== 'workspaces' })
  }
  useEffect(() => {
    if (!workspaces) return
    if (workspaces.length === 1 && !workspaces.some((item) => item.id === workspace)) setWorkspace(workspaces[0].id)
    else if (!workspaces.some((item) => item.id === workspace) && workspace) setWorkspace('')
  }, [workspaces, workspace])
  return <WorkspaceContext.Provider value={{ workspace, setWorkspace }}>{children}</WorkspaceContext.Provider>
}

// The rail is the workspace switcher (§21.10: workspace context is explicit
// everywhere): one initials tile per workspace, "+" creates a new one.
function IconRail() {
  const token = useOperatorToken()
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
        <Link
          to="/workspaces/new"
          title="Add workspace"
          aria-label="Add workspace"
          className="grid size-8 shrink-0 place-items-center rounded-[9px] text-workspace-tile-foreground transition-colors hover:bg-workspace-tile hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-[5px] focus-visible:outline-primary"
        >
          <Plus className="size-5" aria-hidden="true" />
        </Link>
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
  const { workspace: selected } = useWorkspaceSelection()
  const { data: workspaces } = useQuery({
    queryKey: ['workspaces', token],
    queryFn: () => fetchWorkspaces(token),
    enabled: !!token,
  })
  const { data: workspace } = useWorkspace()
  const { data: activity } = useActivity()
  // This badge counts what the Board will actually show, so it applies the
  // board's own predicate (spec §21.49): an anchor lives on the Blueprints
  // surface, and counting one here would send the operator to a board with
  // nothing on it to resolve.
  const attention = (activity ?? []).filter((item) => !isBlueprintAnchor(item.task) && item.needs_attention).length

  const currentName = workspaces?.find((item) => item.id === selected)?.name ?? workspace?.workspace ?? 'Conveyor'

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
        <NavItem to="/workspace" icon={FolderGit2} label="Workspace" />
        <NavItem to="/requirements" icon={Workflow} label="Requirements" />
        <NavItem to="/blueprints" icon={Blocks} label="Blueprints" />
        <NavItem to="/planning" icon={MessageSquare} label="Planning" />
        <NavItem to="/monitor" icon={Activity} label="Monitor" />
        <NavItem to="/settings" icon={Settings} label="Settings" />
      </div>
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
  children,
}: {
  to: string
  icon: LucideIcon
  label: string
  children?: ReactNode
}) {
  const pathname = useRouterState({ select: (state) => state.location.pathname })
  // The board stays highlighted while any of its overlays (task sheet, full
  // page, task intake, workspace modal) is open.
  const active =
    to === '/'
      ? pathname === '/' || pathname.startsWith('/tasks') || pathname === '/new' || pathname === '/workspaces/new'
      : pathname === to || pathname.startsWith(`${to}/`)
  return (
    <Link
      to={to}
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
