import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, useParams } from '@tanstack/react-router'
import { Plus } from 'lucide-react'
import { groupForSummary } from '../../lib/activity'
import { fetchWorkspaces } from '../../lib/api'
import { isBlueprintAnchor } from '../../lib/blueprint'
import { stageGroups, type GroupKey } from '../../lib/contracts'
import type { ActivitySummary } from '../../lib/types'
import { useActivity, useTokenState, useWorkspaceSelection } from '../app-shell'
import {
  boardDefaultTaskFilter,
  TaskFilters,
  taskFilterParams,
  taskFilterRangeError,
  useTaskFilters,
} from '../task/task-filters'
import { Button } from '../ui/button'
import { Skeleton } from '../ui/skeleton'
import { BoardColumn } from './board-column'

// The kanban board (spec §13.3 element 1): the distribution of work across
// stages is the factory's health made visible. Read-only on purpose — tasks
// move between columns via the pipeline, never by hand.
export function Board() {
  // The board opens on the last month of activity and remembers whatever the
  // operator changes it to, per workspace (AC-2.4).
  const [filter, setFilter] = useTaskFilters('board', boardDefaultTaskFilter)
  const params = taskFilterParams(filter)
  // A range no task can satisfy is a half-typed date; the filter says so in
  // place rather than emptying the board behind a request the server refuses.
  const rangeError = taskFilterRangeError(filter)
  const { data, isLoading, error } = useActivity(params, !rangeError)
  const { workspace } = useWorkspaceSelection()
  // Highlight the card whose sheet is open (child route /tasks/$taskId).
  const { taskId: selectedId } = useParams({ strict: false }) as { taskId?: string }

  const grouped = useMemo(() => {
    const byGroup = new Map<GroupKey, ActivitySummary[]>()
    for (const item of data ?? []) {
      // The board represents claimable, executable work. A blueprint anchor
      // takes no orders and moves through no stage, so it lives on the
      // Blueprints surface instead (spec §21.49). The feed already excludes
      // anchors; applying the same predicate here keeps the column counts
      // honest for any caller that hands the board a wider list.
      if (isBlueprintAnchor(item.task)) continue
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
  }, [data])

  if (!workspace) return <Onboarding />

  return (
    <div className="flex h-full flex-col">
      {/* Task intake left this header for the Tasks view, which is where
          operators manage delivery (AC-2.1). The board is the pipeline lens
          over work that already exists. */}
      <header className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-6 py-3.5">
        <h1 className="text-lg font-semibold tracking-tight">Board</h1>
        <TaskFilters value={filter} onChange={setFilter} fallback={boardDefaultTaskFilter} className="ml-auto" />
      </header>
      {error != null && (
        <p className="mx-6 mt-4 rounded-lg bg-failure-soft p-3 text-sm text-failure">
          Activity feed unavailable: {String(error)}
        </p>
      )}
      <section aria-label="Task board" className="flex min-h-0 flex-1 gap-3 overflow-x-auto px-4 py-4">
        {isLoading
          ? stageGroups.map(({ key }) => <Skeleton key={key} className="h-full w-72 shrink-0 rounded-xl" />)
          : stageGroups.map(({ key, label }) => (
              <BoardColumn
                key={key}
                groupKey={key}
                label={label}
                items={grouped.get(key) ?? []}
                selectedId={selectedId}
              />
            ))}
      </section>
    </div>
  )
}

// The board is the landing page, so it also owns first-run guidance: no
// token → Settings; token but no workspace → create one; several → pick one.
function Onboarding() {
  const { token } = useTokenState()
  const { data: workspaces } = useQuery({
    queryKey: ['workspaces', token],
    queryFn: () => fetchWorkspaces(token),
    enabled: !!token,
  })
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
