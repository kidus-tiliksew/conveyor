import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, useParams } from '@tanstack/react-router'
import { Plus, Search } from 'lucide-react'
import { groupForSummary } from '../../lib/activity'
import { fetchWorkspaces } from '../../lib/api'
import { stageGroups, type GroupKey } from '../../lib/contracts'
import type { ActivitySummary } from '../../lib/types'
import { useActivity, useTokenState, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Input } from '../ui/input'
import { Skeleton } from '../ui/skeleton'
import { BoardColumn } from './board-column'

// The kanban board (spec §13.3 element 1): the distribution of work across
// stages is the factory's health made visible. Read-only on purpose — tasks
// move between columns via the pipeline, never by hand.
export function Board() {
  const { data, isLoading, error } = useActivity()
  const { workspace } = useWorkspaceSelection()
  const [query, setQuery] = useState('')
  // Highlight the card whose sheet is open (child route /tasks/$taskId).
  const { taskId: selectedId } = useParams({ strict: false }) as { taskId?: string }

  const grouped = useMemo(() => {
    const needle = query.trim().toLowerCase()
    const byGroup = new Map<GroupKey, ActivitySummary[]>()
    for (const item of data ?? []) {
      if (
        needle &&
        !item.task.title.toLowerCase().includes(needle) &&
        !item.task.id.includes(needle) &&
        !item.task.source.toLowerCase().includes(needle)
      ) {
        continue
      }
      const key = groupForSummary(item)
      byGroup.set(key, [...(byGroup.get(key) ?? []), item])
    }
    for (const items of byGroup.values()) {
      items.sort(
        (a, b) =>
          new Date(b.last_event_at || b.task.created_at).getTime() -
          new Date(a.last_event_at || a.task.created_at).getTime(),
      )
    }
    return byGroup
  }, [data, query])

  if (!workspace) return <Onboarding />

  return (
    <div className="flex h-full flex-col">
      <header className="flex shrink-0 items-center justify-between gap-4 border-b border-border px-6 py-3.5">
        <h1 className="text-lg font-semibold tracking-tight">Board</h1>
        <div className="flex items-center gap-2">
          <label className="relative">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-faint" />
            <Input
              type="search"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="Search tasks"
              className="h-8 w-56 pl-8 text-xs"
            />
          </label>
          <Link to="/new">
            <Button size="sm" tabIndex={-1}>
              <Plus />
              New task
            </Button>
          </Link>
        </div>
      </header>
      {error != null && (
        <p className="mx-6 mt-4 rounded-lg bg-failure-soft p-3 text-sm text-failure">Activity feed unavailable: {String(error)}</p>
      )}
      <div className="flex min-h-0 flex-1 gap-3 overflow-x-auto px-4 py-4">
        {isLoading
          ? stageGroups.map(({ key }) => <Skeleton key={key} className="h-full w-72 shrink-0 rounded-xl" />)
          : stageGroups.map(({ key, label }) => (
              <BoardColumn key={key} groupKey={key} label={label} items={grouped.get(key) ?? []} selectedId={selectedId} />
            ))}
      </div>
    </div>
  )
}

// The board is the landing page, so it also owns first-run guidance: no
// token → Settings; token but no workspace → create one; several → pick one.
function Onboarding() {
  const { token } = useTokenState()
  const { data: workspaces } = useQuery({ queryKey: ['workspaces', token], queryFn: () => fetchWorkspaces(token), enabled: !!token })
  return (
    <div className="grid h-full place-items-center px-6">
      <div className="max-w-md text-center">
        <h1 className="text-lg font-semibold tracking-tight">Welcome to Conveyor</h1>
        {!token ? (
          <>
            <p className="mt-2 text-sm leading-6 text-muted">
              Set the operator token to load your workspaces and the factory board.
            </p>
            <Link to="/settings" className="mt-4 inline-block">
              <Button tabIndex={-1}>Open Settings</Button>
            </Link>
          </>
        ) : (workspaces?.length ?? 0) === 0 ? (
          <>
            <p className="mt-2 text-sm leading-6 text-muted">No workspaces yet. Create one to start the line.</p>
            <Link to="/workspaces/new" className="mt-4 inline-block">
              <Button tabIndex={-1}>
                <Plus />
                Create workspace
              </Button>
            </Link>
          </>
        ) : (
          <p className="mt-2 text-sm leading-6 text-muted">Pick a workspace from the left rail to see its board.</p>
        )}
      </div>
    </div>
  )
}
