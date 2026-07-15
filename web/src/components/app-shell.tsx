import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, Outlet, useRouterState } from '@tanstack/react-router'
import { Activity, FolderGit2, Home, Play, Plus, Settings, Workflow, type LucideIcon } from 'lucide-react'
import { fetchActivity, fetchWorkspace, fetchWorkspaces } from '../lib/api'
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

function IconRail() {
  return (
    <div className="flex w-14 shrink-0 flex-col items-center gap-3 bg-rail py-3">
      <Link to="/" aria-label="Conveyor home" className="grid size-9 place-items-center rounded-lg bg-primary font-mono text-xs font-black text-primary-foreground">
        CV
      </Link>
      <span className="h-px w-8 bg-rail-raised" />
      <Link
        to="/new"
        aria-label="New task"
        className="grid size-9 place-items-center rounded-lg text-white/60 transition-colors hover:bg-rail-raised hover:text-white"
      >
        <Plus className="size-5" />
      </Link>
    </div>
  )
}

function NavSidebar() {
	const token = useOperatorToken()
	const { workspace: selected, setWorkspace } = useWorkspaceSelection()
	const { data: workspaces } = useQuery({ queryKey: ['workspaces', token], queryFn: () => fetchWorkspaces(token), enabled: !!token })
	const { data: workspace } = useWorkspace()
  const { data: activity } = useActivity()
  const attention = (activity ?? []).filter((item) => item.needs_attention).length

  return (
    <nav className="flex w-56 shrink-0 flex-col border-r border-border bg-surface/60" aria-label="Primary">
		<div className="px-4 py-4">
			{workspaces && workspaces.length > 0 ? <select aria-label="Current workspace" className="w-full rounded border border-border bg-background px-2 py-1.5 text-sm font-semibold" value={selected} onChange={(event) => setWorkspace(event.target.value)}><option value="" disabled>Select workspace</option>{workspaces.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select> : <p className="truncate text-sm font-semibold tracking-tight">{workspace?.workspace ?? 'Conveyor'}</p>}
			<p className="mt-0.5 text-[11px] text-faint">Conveyor · software factory</p>
      </div>
      <div className="flex-1 space-y-0.5 px-2">
        <NavItem to="/" icon={Home} label="Home" exact />
        <NavItem to="/activity" icon={Activity} label="Activity">
          {attention > 0 && <Badge variant="attention">{attention}</Badge>}
        </NavItem>
        <NavItem to="/workspace" icon={FolderGit2} label="Workspace" />
        <NavItem to="/requirements" icon={Workflow} label="Requirements" />
        <NavItem to="/new" icon={Play} label="New task" />
        <NavItem to="/settings" icon={Settings} label="Settings" />
      </div>
    </nav>
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
  // Activity stays highlighted while a task panel is open.
  const active = exact ? pathname === to : pathname.startsWith(to) || (to === '/activity' && pathname.startsWith('/tasks'))
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
