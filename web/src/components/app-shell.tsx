import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, Outlet, useNavigate, useRouterState } from '@tanstack/react-router'
import { FolderGit2, Kanban, Plus, Settings, Workflow, type LucideIcon } from 'lucide-react'
import { fetchActivity, fetchWorkspace, fetchWorkspaces } from '../lib/api'
import { cn } from '../lib/utils'
import { Badge } from './ui/badge'

// The operator bearer token authenticates mutations (spec §17.3).
// Session-scoped on purpose: closing the tab forgets it.
const TokenContext = createContext<{ token: string; setToken: (value: string) => void }>({
  token: '',
  setToken: () => {},
})

const WorkspaceContext = createContext<{ workspace: string; setWorkspace: (value: string) => void }>({ workspace: '', setWorkspace: () => {} })

export function useWorkspaceSelection() { return useContext(WorkspaceContext) }

export function useOperatorToken() {
  return useContext(TokenContext).token
}

export function useTokenState() {
  return useContext(TokenContext)
}

export function useActivity() {
	const { workspace } = useWorkspaceSelection()
	return useQuery({ queryKey: ['activity', workspace], queryFn: fetchActivity, enabled: !!workspace, refetchInterval: 15_000 })
}

export function useWorkspace() {
	const { workspace } = useWorkspaceSelection()
	return useQuery({ queryKey: ['workspace', workspace], queryFn: fetchWorkspace, enabled: !!workspace, staleTime: 60_000, retry: 1 })
}

export function AppShell() {
  const [token, setToken] = useState(() => sessionStorage.getItem('conveyor-token') ?? '')
  const saveToken = (value: string) => {
    setToken(value)
    sessionStorage.setItem('conveyor-token', value)
  }

	return (
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
	)
}

function WorkspaceProvider({ token, children }: { token: string; children: ReactNode }) {
	const queryClient = useQueryClient()
	const [workspace, setWorkspaceState] = useState(() => localStorage.getItem('conveyor-workspace') ?? '')
	const { data: workspaces } = useQuery({ queryKey: ['workspaces', token], queryFn: () => fetchWorkspaces(token), enabled: !!token })
	const setWorkspace = (value: string) => {
		void queryClient.cancelQueries()
		if (value) localStorage.setItem('conveyor-workspace', value); else localStorage.removeItem('conveyor-workspace')
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
  const { data: workspaces } = useQuery({ queryKey: ['workspaces', token], queryFn: () => fetchWorkspaces(token), enabled: !!token })
  // Land on the board after a switch so an open task sheet from the previous
  // workspace can't linger pointing at a task the new context can't resolve.
  const switchTo = (id: string) => {
    if (id !== selected) setWorkspace(id)
    void navigate({ to: '/' })
  }
  return (
    <div className="flex w-14 shrink-0 flex-col items-center gap-3 bg-rail py-3">
      <Link to="/" aria-label="Conveyor home" className="grid size-9 shrink-0 place-items-center rounded-lg bg-primary font-mono text-xs font-black text-primary-foreground">
        CV
      </Link>
      <span className="h-px w-8 shrink-0 bg-rail-raised" />
      <div className="flex min-h-0 flex-1 flex-col items-center gap-2 overflow-y-auto">
        {(workspaces ?? []).map((item) => (
          <button
            key={item.id}
            type="button"
            title={item.name}
            aria-label={`Switch to ${item.name}`}
            aria-current={item.id === selected ? 'true' : undefined}
            onClick={() => switchTo(item.id)}
            className={cn(
              'grid size-9 shrink-0 place-items-center rounded-lg text-xs font-bold transition-colors',
              item.id === selected
                ? 'bg-rail-raised text-white ring-2 ring-primary'
                : 'text-white/60 hover:bg-rail-raised hover:text-white',
            )}
          >
            {initials(item.name || item.id)}
          </button>
        ))}
      </div>
      <Link
        to="/workspaces/new"
        aria-label="Create workspace"
        className="grid size-9 shrink-0 place-items-center rounded-lg text-white/60 transition-colors hover:bg-rail-raised hover:text-white"
      >
        <Plus className="size-5" />
      </Link>
    </div>
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
	const { data: workspaces } = useQuery({ queryKey: ['workspaces', token], queryFn: () => fetchWorkspaces(token), enabled: !!token })
	const { data: workspace } = useWorkspace()
  const { data: activity } = useActivity()
  const attention = (activity ?? []).filter((item) => item.needs_attention).length

  const currentName = workspaces?.find((item) => item.id === selected)?.name ?? workspace?.workspace ?? 'Conveyor'

  return (
    <nav className="flex w-56 shrink-0 flex-col border-r border-border bg-surface/60" aria-label="Primary">
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
        <NavItem to="/settings" icon={Settings} label="Settings" />
      </div>
    </nav>
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
  // The board stays highlighted while a task sheet or full page is open.
  const active = to === '/' ? pathname === '/' || pathname.startsWith('/tasks') : pathname === to || pathname.startsWith(`${to}/`)
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
