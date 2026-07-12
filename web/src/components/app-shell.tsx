import { createContext, useContext, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, Outlet, useRouterState } from '@tanstack/react-router'
import { Activity, FolderGit2, Home, Play, Plus, Settings, type LucideIcon } from 'lucide-react'
import { fetchActivity, fetchWorkspace } from '../lib/api'
import { Badge } from './ui/badge'

// The operator bearer token authenticates mutations (spec §17.3).
// Session-scoped on purpose: closing the tab forgets it.
const TokenContext = createContext<{ token: string; setToken: (value: string) => void }>({
  token: '',
  setToken: () => {},
})

export function useOperatorToken() {
  return useContext(TokenContext).token
}

export function useTokenState() {
  return useContext(TokenContext)
}

export function useActivity() {
  return useQuery({ queryKey: ['activity'], queryFn: fetchActivity, refetchInterval: 15_000 })
}

export function useWorkspace() {
  return useQuery({ queryKey: ['workspace'], queryFn: fetchWorkspace, staleTime: 60_000, retry: 1 })
}

export function AppShell() {
  const [token, setToken] = useState(() => sessionStorage.getItem('conveyor-token') ?? '')
  const saveToken = (value: string) => {
    setToken(value)
    sessionStorage.setItem('conveyor-token', value)
  }

  return (
    <TokenContext.Provider value={{ token, setToken: saveToken }}>
      <div className="flex h-screen overflow-hidden">
        <IconRail />
        <NavSidebar />
        <main className="min-w-0 flex-1 overflow-hidden bg-background">
          <Outlet />
        </main>
      </div>
    </TokenContext.Provider>
  )
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
  const { data: workspace } = useWorkspace()
  const { data: activity } = useActivity()
  const attention = (activity ?? []).filter((item) => item.needs_attention).length

  return (
    <nav className="flex w-56 shrink-0 flex-col border-r border-border bg-surface/60" aria-label="Primary">
      <div className="px-4 py-4">
        <p className="truncate text-sm font-semibold tracking-tight">
          {workspace?.workspace ?? 'Conveyor'}
        </p>
        <p className="mt-0.5 text-[11px] text-faint">Conveyor · software factory</p>
      </div>
      <div className="flex-1 space-y-0.5 px-2">
        <NavItem to="/" icon={Home} label="Home" exact />
        <NavItem to="/activity" icon={Activity} label="Activity">
          {attention > 0 && <Badge variant="attention">{attention}</Badge>}
        </NavItem>
        <NavItem to="/workspace" icon={FolderGit2} label="Workspace" />
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
