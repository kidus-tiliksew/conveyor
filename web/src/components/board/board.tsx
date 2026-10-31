import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { ChevronLeft, ChevronRight, Plus } from 'lucide-react'
import { lazy, Suspense, useEffect, useMemo, useState } from 'react'
import { boardColumnFor } from '../../lib/activity'
import { isBlueprintAnchor } from '../../lib/blueprint'
import { type GroupKey, stageGroups } from '../../lib/contracts'
import type { ActivitySummary } from '../../lib/types'
import { useActivity, useWorkspaceCapability } from '../app-shell'
import { MCPSetup } from '../mcp/mcp-setup-dialog'
import { TaskCreateSheet } from '../task/task-create-sheet'
import {
  boardDefaultTaskFilter,
  type TaskFilterState,
  TaskFilters,
  taskFilterParams,
  taskFilterRangeError,
  useTaskFilters,
} from '../task/task-filters'
import { Button } from '../ui/button'
import { Skeleton } from '../ui/skeleton'
import { EmptyRepositoryNotice } from '../workspace/empty-repository-notice'
import { BoardColumn } from './board-column'
import { type BoardSearch, boardSearchFilter } from './board-search'
import { BoardToolbar } from './board-toolbar'
import { startLayoutWorker } from './map/layout-worker'

// The map's graph libraries load with the view, never with the Board (DEC-76).
// Asking for the map's chunk also starts the ELK worker, so it compiles while
// the chunk and the map's read load (req-board-map AC-3.2).
const BoardMap = lazy(() => {
  startLayoutWorker()
  return import('./map/board-map').then((module) => ({ default: module.BoardMap }))
})

// The Board: kanban columns or the map of the same workspace, and no list
// view (REQ-6, AC-6.1; req-board-map), with task intake over either. The view,
// intake, and any arriving kanban filter live in the address so a reload or a
// shared link lands on the same view.
export function Board() {
  const search = useSearch({ strict: false }) as BoardSearch
  // Intake follows its own capability (DEC-60); the server checks it again.
  const canCreateTasks = useWorkspaceCapability('create_tasks')
  const map = search.view === 'map'
  return (
    <div className="flex h-full flex-col">
      <header className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-4 py-3 lg:px-6 lg:py-3.5">
        <h1 className="text-lg font-semibold tracking-tight">Board</h1>
        <div className="ml-auto flex items-center gap-2">
          <MCPSetup iconOnly />
          {canCreateTasks && (
            <Link to="." search={(prev: BoardSearch) => ({ ...prev, create: true as const })}>
              <Button size="sm" tabIndex={-1}>
                <Plus />
                New task
              </Button>
            </Link>
          )}
        </div>
      </header>
      <div className="mx-4 lg:mx-6">
        <EmptyRepositoryNotice />
      </div>
      {map ? (
        <Suspense
          fallback={
            <>
              <BoardToolbar />
              <Skeleton className="m-6 h-96" />
            </>
          }
        >
          <BoardMap />
        </Suspense>
      ) : (
        <Kanban />
      )}
      {search.create && canCreateTasks && <TaskCreateSheet />}
    </div>
  )
}

// The kanban view: the distribution of work across stages is the factory's
// health made visible. Read-only on purpose — tasks move between columns via
// the pipeline, never by hand.
function Kanban() {
  // The board opens on the last month of activity and remembers whatever the
  // operator changes it to, per workspace (AC-6.13).
  const [filter, setFilter] = useTaskFilters('board', boardDefaultTaskFilter)
  // A filter named in the address, such as a redirected /tasks link carries,
  // wins on arrival and then leaves the address, so the saved filter stays the
  // one source of truth (AC-6.3). No read is sent for the replaced filter.
  const navigate = useNavigate()
  const search = useSearch({ strict: false }) as BoardSearch
  const arrival = boardSearchFilter(search)
  const arrivalKey = arrival ? JSON.stringify(arrival) : ''
  useEffect(() => {
    if (!arrivalKey) return
    setFilter(JSON.parse(arrivalKey) as TaskFilterState)
    void navigate({
      to: '.',
      replace: true,
      search: (prev: BoardSearch) => ({ view: prev.view, create: prev.create }),
    })
  }, [arrivalKey, setFilter, navigate])
  const params = taskFilterParams(filter)
  const [offset, setOffset] = useState(0)
  const filterKey = JSON.stringify(params)
  // A range no task can satisfy is a half-typed date; the filter says so in
  // place rather than emptying the board behind a request the server refuses.
  const rangeError = taskFilterRangeError(filter)
  const { data, isLoading, isFetching, error } = useActivity(params, !rangeError && !arrivalKey, offset)
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
      const key = boardColumnFor(item)
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

  return (
    <>
      <BoardToolbar
        pager={
          data &&
          !isLoading && (
            <nav aria-label="Board pages" className="flex h-8 items-center rounded-md border border-border bg-surface">
              <span className="whitespace-nowrap px-2.5">
                <span className="font-medium text-foreground">
                  {data.total === 0 ? 0 : data.offset + 1}–{Math.min(data.offset + data.items.length, data.total)}
                </span>{' '}
                of {data.total}
              </span>
              <Button
                size="icon"
                variant="ghost"
                aria-label="Previous"
                title="Previous"
                className="h-full rounded-none border-l border-border"
                disabled={data.offset === 0 || isFetching}
                onClick={() => setOffset(Math.max(0, data.offset - data.limit))}
              >
                <ChevronLeft />
              </Button>
              <Button
                size="icon"
                variant="ghost"
                aria-label="Next"
                title="Next"
                className="h-full rounded-r-md rounded-l-none border-l border-border"
                disabled={data.offset + data.items.length >= data.total || isFetching}
                onClick={() => setOffset(data.offset + data.limit)}
              >
                <ChevronRight />
              </Button>
            </nav>
          )
        }
      >
        <TaskFilters
          value={filter}
          onChange={(next) => {
            setOffset(0)
            setFilter(next)
          }}
          fallback={boardDefaultTaskFilter}
          className="min-w-0 flex-1"
          iconOnly
        />
      </BoardToolbar>
      {error != null && (
        <p className="mx-4 mt-4 rounded-lg lg:mx-6 bg-failure-soft p-3 text-sm text-failure">
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
    </>
  )
}
