import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { Plus } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { groupForSummary } from '../../lib/activity'
import { fetchWorkspaces } from '../../lib/api'
import { isBlueprintAnchor } from '../../lib/blueprint'
import { type GroupKey, stageGroups } from '../../lib/contracts'
import type { ActivitySummary } from '../../lib/types'
import { useActivity, useTokenState, useWorkspaceSelection } from '../app-shell'
import { MCPSetup } from '../mcp/mcp-setup-dialog'
import { TaskCreateSheet } from '../task/task-create-sheet'
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

// The kanban board: the distribution of work across
// stages is the factory's health made visible. Read-only on purpose — tasks
// move between columns via the pipeline, never by hand.
export function Board() {
  const navigate = useNavigate()
  const [creating, setCreating] = useState(false)
  // The board opens on the last month of activity and remembers whatever the
  // operator changes it to, per workspace (AC-2.4).
  const [filter, setFilter] = useTaskFilters('board', boardDefaultTaskFilter)
  const params = taskFilterParams(filter)
  const [offset, setOffset] = useState(0)
  const filterKey = JSON.stringify(params)
  // A range no task can satisfy is a half-typed date; the filter says so in
  // place rather than emptying the board behind a request the server refuses.
  const rangeError = taskFilterRangeError(filter)
  const { data, isLoading, isFetching, error } = useActivity(params, !rangeError, offset)
  const { workspace } = useWorkspaceSelection()
  // Highlight the card whose sheet is open (child route /tasks/$taskId).
  const { taskId: selectedId } = useParams({ strict: false }) as { taskId?: string }

  const grouped = useMemo(() => {
    const byGroup = new Map<GroupKey, ActivitySummary[]>()
    for (const item of data?.items ?? []) {
      // The board represents claimable, executable work. A blueprint anchor
      // takes no orders and moves through no stage, so it lives on the
      // Blueprints surface instead. The feed already excludes
      // anchors; applying the same predicate here keeps the column counts
      // honest for any caller that hands the board a wider list.
      if (isBlueprintAnchor(item.task)) continue
      const key = groupForSummary(item)
      byGroup.set(key, [...(byGroup.get(key) ?? []), item])
    }
    for (const items of byGroup.values()) {
      items.sort((a, b) => {
        const createdOrder = new Date(b.task.created_at).getTime() - new Date(a.task.created_at).getTime()
        return createdOrder || a.task.id.localeCompare(b.task.id)
      })
    }
    return byGroup
  }, [data])

  useEffect(() => setOffset(0), [filterKey])
  useEffect(() => {
    if (!data || data.total === 0 || offset < data.total) return
    setOffset(Math.floor((data.total - 1) / data.limit) * data.limit)
  }, [data, offset])

  if (!workspace) return <Onboarding />

  return (
    <div className="flex h-full flex-col">
      <header className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-6 py-3.5">
        <h1 className="text-lg font-semibold tracking-tight">Board</h1>
        <TaskFilters
          value={filter}
          onChange={(next) => {
            setOffset(0)
            setFilter(next)
          }}
          fallback={boardDefaultTaskFilter}
          className="ml-auto"
        />
        <MCPSetup />
        <Button size="sm" onClick={() => setCreating(true)}>
          <Plus />
          New task
        </Button>
      </header>
      {error != null && (
        <p className="mx-6 mt-4 rounded-lg bg-failure-soft p-3 text-sm text-failure">
          Activity feed unavailable: {String(error)}
        </p>
      )}
      {data && !isLoading && (
        <div className="flex shrink-0 items-center justify-end gap-3 border-b border-border px-6 py-2 text-xs text-muted">
          <span>
            Showing {data.total === 0 ? 0 : data.offset + 1}–{Math.min(data.offset + data.items.length, data.total)} of{' '}
            {data.total}
          </span>
          <Button
            size="sm"
            variant="secondary"
            disabled={data.offset === 0 || isFetching}
            onClick={() => setOffset(Math.max(0, data.offset - data.limit))}
          >
            Previous
          </Button>
          <Button
            size="sm"
            variant="secondary"
            disabled={data.offset + data.items.length >= data.total || isFetching}
            onClick={() => setOffset(data.offset + data.limit)}
          >
            Next
          </Button>
        </div>
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
      {creating && (
        <TaskCreateSheet
          onClose={() => setCreating(false)}
          onCreated={(taskId) => {
            setCreating(false)
            void navigate({ to: '/tasks/$taskId', params: { taskId } })
          }}
        />
      )}
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
