import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { ChevronLeft, ChevronRight, Plus } from 'lucide-react'
import { lazy, type RefObject, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { boardColumnFor } from '../../lib/activity'
import { isBlueprintAnchor } from '../../lib/blueprint'
import { type GroupKey, stageGroups } from '../../lib/contracts'
import type { ActivitySummary } from '../../lib/types'
import { useMediaQuery } from '../../lib/use-media-query'
import { cn } from '../../lib/utils'
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
import { ErrorState, readableError } from '../ui/error-state'
import { PageHeader } from '../ui/page-header'
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
      <PageHeader title="Board">
        <MCPSetup iconOnly />
        {canCreateTasks && (
          <Link to="." search={(prev: BoardSearch) => ({ ...prev, create: true as const })}>
            <Button size="sm" tabIndex={-1}>
              <Plus />
              New task
            </Button>
          </Link>
        )}
      </PageHeader>
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

// The width from which the toolbar holds the lane strip on one line beside
// search, filters, pager, and the view switch.
const laneStripQuery = '(min-width: 88rem)'
// A phone shows one lane at a time, so the strip gets its own scrolling row.
const phoneQuery = '(max-width: 47.99rem)'

// Brings a lane into view with the lane strip's button, moving only the lanes.
function scrollToLane(scroller: HTMLElement | null, key: GroupKey) {
  const lane = scroller?.querySelector<HTMLElement>(`[data-lane="${key}"]`)
  if (!scroller || !lane) return
  const reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches
  const left = lane.getBoundingClientRect().left - scroller.getBoundingClientRect().left + scroller.scrollLeft
  const gutter = Number.parseFloat(getComputedStyle(scroller).paddingLeft) || 0
  scroller.scrollTo({ left: left - gutter, behavior: reduced ? 'auto' : 'smooth' })
}

// Which lanes the scroller shows, and which of its edges hide more.
function useLaneScroll(scroller: RefObject<HTMLElement | null>) {
  const [state, setState] = useState({ inView: new Set<string>(), fadeStart: false, fadeEnd: false })
  const measure = useCallback(() => {
    const element = scroller.current
    if (!element) return
    const box = element.getBoundingClientRect()
    const inView = new Set<string>()
    for (const lane of element.querySelectorAll<HTMLElement>('[data-lane]')) {
      const rect = lane.getBoundingClientRect()
      // Most of the lane shows, so a sliver at the edge does not count.
      const seen = Math.min(rect.right, box.right) - Math.max(rect.left, box.left)
      if (seen >= rect.width * 0.6 && lane.dataset.lane) inView.add(lane.dataset.lane)
    }
    const fadeStart = element.scrollLeft > 1
    const fadeEnd = element.scrollLeft + element.clientWidth < element.scrollWidth - 1
    setState((prev) =>
      prev.fadeStart === fadeStart &&
      prev.fadeEnd === fadeEnd &&
      prev.inView.size === inView.size &&
      [...inView].every((key) => prev.inView.has(key))
        ? prev
        : { inView, fadeStart, fadeEnd },
    )
  }, [scroller])
  useEffect(() => {
    const element = scroller.current
    if (!element) return
    measure()
    let frame = 0
    const schedule = () => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(measure)
    }
    element.addEventListener('scroll', schedule, { passive: true })
    const observer = new ResizeObserver(schedule)
    observer.observe(element)
    return () => {
      cancelAnimationFrame(frame)
      element.removeEventListener('scroll', schedule)
      observer.disconnect()
    }
  }, [measure, scroller])
  return { ...state, measure }
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
  const { data, isLoading, isFetching, error, refetch } = useActivity(params, !rangeError && !arrivalKey, offset)
  // With nothing loaded, empty lanes would claim the board has no work.
  const loadFailed = error != null && !data
  const filtered = filterKey !== JSON.stringify(taskFilterParams(boardDefaultTaskFilter))
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

  // The feed's total counts blueprint anchors, which it drops from each page
  // after reading that page. So a short page does not end the list, but a page
  // whose window reaches the total does. A page holding only anchors comes back
  // empty: the pager steps back from it and remembers that the list ends there.
  const [emptyPage, setEmptyPage] = useState<{ offset: number; total: number }>()
  useEffect(() => setEmptyPage(undefined), [filterKey])
  useEffect(() => {
    if (!data || data.items.length > 0 || data.offset === 0 || data.offset >= data.total) return
    setEmptyPage({ offset: data.offset, total: data.total })
    setOffset(Math.max(0, data.offset - data.limit))
  }, [data])
  const end = data && (emptyPage?.total === data.total ? emptyPage.offset : data.total)
  const lastPage = Boolean(data && end !== undefined && data.offset + data.limit >= end)
  const singlePage = lastPage && data?.offset === 0
  const shown = data ? data.offset + data.items.length : 0

  const scroller = useRef<HTMLElement>(null)
  const laneStrip = useMediaQuery(laneStripQuery)
  const phone = useMediaQuery(phoneQuery)
  const { inView, fadeStart, fadeEnd, measure } = useLaneScroll(scroller)
  // The lanes mount with the first read.
  useEffect(measure, [isLoading, measure])

  const laneButtons = stageGroups.map(({ key, label }) => {
    const items = grouped.get(key) ?? []
    const operator = items.filter((item) => item.next_action?.actor === 'operator').length
    return (
      <button
        key={key}
        type="button"
        title={`Scroll to ${label}`}
        onClick={() => scrollToLane(scroller.current, key)}
        className={cn(
          'inline-flex h-7 shrink-0 items-center gap-1 whitespace-nowrap rounded-md px-1 text-xs transition-colors hover:bg-surface hover:text-foreground pointer-coarse:h-9 pointer-coarse:px-2',
          inView.has(key) ? 'text-foreground' : 'text-muted',
        )}
      >
        {label}
        <span className="tabular-nums text-faint">{items.length}</span>
        {operator > 0 && (
          <span className="ml-0.5 inline-flex items-center gap-1 font-medium tabular-nums text-attention before:size-1 before:rounded-full before:bg-attention-dot">
            {operator}
            <span className="sr-only">waiting on an operator</span>
          </span>
        )}
      </button>
    )
  })

  return (
    <>
      <BoardToolbar
        lanes={
          !isLoading &&
          !loadFailed &&
          laneStrip && (
            <nav aria-label="Board lanes" className="flex items-center gap-0.5">
              {laneButtons}
            </nav>
          )
        }
        pager={
          data &&
          !isLoading &&
          !singlePage && (
            <nav aria-label="Board pages" className="flex h-8 items-center rounded-md border border-border bg-surface">
              <span className="whitespace-nowrap px-2.5">
                <span className="font-medium text-foreground">
                  {data.total === 0 ? 0 : data.offset + 1}–{Math.min(shown, data.total)}
                </span>{' '}
                of {lastPage ? Math.min(shown, data.total) : data.total}
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
                disabled={lastPage || isFetching}
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
      {phone && !isLoading && !loadFailed && (
        <nav
          aria-label="Board lanes"
          className="relative flex shrink-0 items-center gap-1 overflow-x-auto border-b border-border px-3 py-0.5 [scrollbar-width:none]"
        >
          {laneButtons}
        </nav>
      )}
      {error != null && !loadFailed && (
        <p className="mx-4 mt-4 rounded-lg lg:mx-6 bg-failure-soft p-3 text-sm text-failure">
          Activity feed unavailable: {readableError(error, 'Check your connection and try again.')}
        </p>
      )}
      {loadFailed ? (
        <ErrorState
          title="Could not load the board"
          error={error}
          onRetry={() => void refetch()}
          retrying={isFetching}
        />
      ) : (
        <div className="relative flex min-h-0 flex-1">
          <section
            ref={scroller}
            aria-label="Task board"
            className="relative flex min-h-0 min-w-0 flex-1 gap-3 overflow-x-auto px-4 py-4 lg:px-6"
          >
            {isLoading
              ? stageGroups.map(({ key }) => <Skeleton key={key} className="h-full w-72 shrink-0 rounded-xl" />)
              : stageGroups.map(({ key, label }) => (
                  <BoardColumn
                    key={key}
                    groupKey={key}
                    label={label}
                    items={grouped.get(key) ?? []}
                    selectedId={selectedId}
                    filtered={filtered}
                  />
                ))}
          </section>
          {/* A quiet sign that more lanes sit past the edge. */}
          <span
            aria-hidden
            data-lane-fade="start"
            className={cn(
              'pointer-events-none absolute inset-y-0 left-0 w-10 bg-gradient-to-r from-background to-transparent transition-opacity duration-150',
              fadeStart ? 'opacity-100' : 'opacity-0',
            )}
          />
          <span
            aria-hidden
            data-lane-fade="end"
            className={cn(
              'pointer-events-none absolute inset-y-0 right-0 w-14 bg-gradient-to-l from-background to-transparent transition-opacity duration-150',
              fadeEnd ? 'opacity-100' : 'opacity-0',
            )}
          />
        </div>
      )}
    </>
  )
}
