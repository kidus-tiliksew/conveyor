import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { FileCode2, ListChecks, Network, Plus, Workflow } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useWorkspaceSelection } from '../components/app-shell'
import { TaskCreateSheet } from '../components/task/task-create-sheet'
import {
  TaskFilters,
  emptyTaskFilter,
  taskFilterActive,
  taskFilterParams,
  taskFilterRangeError,
  useTaskFilters,
} from '../components/task/task-filters'
import { TaskSheet } from '../components/task/task-sheet'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent } from '../components/ui/card'
import { Skeleton } from '../components/ui/skeleton'
import { fetchTaskOperations } from '../lib/api'
import { stageLabels, taskStateLabels } from '../lib/contracts'
import { errorMessage } from '../lib/errors'
import { relativeTime } from '../lib/utils'
import type { TaskOperationsItem, TaskPlanStatus, TaskRelation } from '../lib/types'

const PAGE_SIZE = 25

// The list-first Tasks view (spec §21.58, REQ-1 and REQ-2). It reads one
// paginated projection and renders only what that projection carries: state,
// repository, dependencies and blockers, child rollups, attached context, and
// plan status. There is no priority, assignee, or declared-phase control
// anywhere on this surface, and none may be added to support it (AC-1.5).
//
// This is also where tasks are created and inspected (REQ-2): intake opens over
// the list, and selecting a row opens the task's own detail composition in a
// right panel without leaving it.
export function TasksPage() {
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  // The open panel lives in the address, so the view an operator is looking at
  // is the one a colleague opens from the link they paste (AC-2.2).
  const { task: selectedId, create } = useSearch({ strict: false }) as { task?: string; create?: boolean }
  const [filter, setFilter] = useTaskFilters('tasks')
  const [offset, setOffset] = useState(0)
  const params = taskFilterParams(filter)
  // A range no task can satisfy is a half-typed date, not a question worth
  // asking the server: the filter says so in place and the last good page stays.
  const rangeError = taskFilterRangeError(filter)
  // Every filter is applied by the server against the whole workspace, and the
  // browser holds one page of what already matched (AC-2.3).
  const {
    data: page,
    isLoading,
    error,
  } = useQuery({
    queryKey: ['task-operations', workspace, params, offset],
    queryFn: () => fetchTaskOperations({ limit: PAGE_SIZE, offset, filter: params }),
    enabled: Boolean(workspace) && !rangeError,
    refetchInterval: 15_000,
  })
  const items = page?.items
  const filtersActive = taskFilterActive(filter)

  // Narrowing the workspace changes what page one is, so paging restarts there
  // rather than stranding the operator past the end of a shorter result.
  const filterKey = JSON.stringify(params)
  useEffect(() => {
    setOffset(0)
  }, [filterKey])

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-5xl px-6 py-8">
        <header>
          <div className="flex items-center gap-2">
            <h1 className="text-xl font-semibold tracking-tight">Tasks</h1>
            {/* The server counts what the filters match, so the badge reports
                the whole matching set rather than the page in hand. */}
            <Badge variant="mono">{page?.total ?? 0}</Badge>
            {/* Intake belongs to the surface where delivery is managed
                (AC-2.1); it opens over this list, which stays behind it. */}
            <Link to="/tasks" search={{ create: true }} className="ml-auto">
              <Button size="sm" tabIndex={-1}>
                <Plus />
                New task
              </Button>
            </Link>
          </div>
          <p className="mt-1 max-w-2xl text-sm text-muted">
            Every task in this workspace with its ordering, attached context, and plan status.
          </p>
        </header>

        <TaskFilters value={filter} onChange={setFilter} fallback={emptyTaskFilter} className="mt-6" />

        {!workspace && <EmptyMessage>Choose a workspace to open its tasks.</EmptyMessage>}
        {isLoading && workspace && (
          <div className="mt-7 space-y-3">
            {['one', 'two', 'three'].map((key) => (
              <Skeleton key={key} className="h-24 w-full rounded-lg" />
            ))}
          </div>
        )}
        {error != null && <EmptyMessage tone="failure">{errorMessage(error, 'Could not load tasks.')}</EmptyMessage>}

        {items && items.length === 0 && (
          <Card className="mt-8 border-dashed">
            <CardContent className="flex min-h-56 flex-col items-center justify-center text-center">
              <ListChecks className="size-7 text-primary" />
              {/* Every filter is applied by the server, so an empty page no
                  longer means an empty workspace — whether any filter is active
                  is what separates the two messages. */}
              <h2 className="mt-4 text-base font-semibold">
                {filtersActive ? 'No tasks match these filters' : 'No tasks yet'}
              </h2>
              <p className="mt-2 max-w-md text-sm leading-6 text-muted">
                {filtersActive
                  ? 'Reset the filters to see the rest of the workspace.'
                  : 'Delivery work arrives here once planning files a task set.'}
              </p>
            </CardContent>
          </Card>
        )}

        {items && items.length > 0 && (
          <ul aria-label="Tasks" className="mt-7 space-y-3">
            {items.map((item) => (
              <li key={item.task.id}>
                <TaskRow item={item} selected={item.task.id === selectedId} />
              </li>
            ))}
          </ul>
        )}

        {page && page.total > page.limit && (
          <nav className="mt-5 flex items-center justify-end gap-2" aria-label="Task pages">
            <button
              type="button"
              className="rounded-md border border-border px-3 py-1.5 text-xs disabled:opacity-40"
              disabled={page.offset === 0}
              onClick={() => setOffset(Math.max(0, page.offset - page.limit))}
            >
              Previous
            </button>
            <span className="text-xs text-muted">
              {page.offset + 1}–{Math.min(page.offset + page.items.length, page.total)} of {page.total}
            </span>
            <button
              type="button"
              className="rounded-md border border-border px-3 py-1.5 text-xs disabled:opacity-40"
              disabled={page.offset + page.limit >= page.total}
              onClick={() => setOffset(page.offset + page.limit)}
            >
              Next
            </button>
          </nav>
        )}
      </div>

      {/* Task intake, opened over the list it files into (AC-2.1). */}
      {create && <TaskCreateSheet />}

      {/* The task's own detail composition, mounted as this surface's panel
          rather than reimplemented on it (AC-2.2). A blueprint anchor opened
          here still redirects to its canonical route — that rule belongs to the
          composition, so hosting it here inherits it (spec §21.49). */}
      {!create && selectedId && (
        <TaskSheet
          taskId={selectedId}
          panel={{
            order: (items ?? []).map((item) => item.task.id),
            permalink: taskPermalink(selectedId),
            close: () => void navigate({ to: '/tasks', search: {} }),
            select: (taskId) => void navigate({ to: '/tasks', search: { task: taskId } }),
          }}
        />
      )}
    </div>
  )
}

// The panel's address, absolute so it survives being pasted somewhere else.
function taskPermalink(taskId: string) {
  const path = `/tasks?task=${encodeURIComponent(taskId)}`
  return typeof window === 'undefined' ? path : new URL(path, window.location.origin).toString()
}

function TaskRow({ item, selected }: { item: TaskOperationsItem; selected: boolean }) {
  const { task } = item
  const stage =
    task.state === 'queued' ? (task.next_stage ?? item.latest_stage) : (item.latest_stage ?? task.next_stage)
  return (
    <div className={`rounded-lg border bg-card p-4 transition-colors ${selected ? 'border-primary' : 'border-border'}`}>
      {/* Title first, then how the task stands, then the quieter facts: the
          row is read top-down, and its own name is the loudest thing on it. */}
      <div className="flex flex-wrap items-center gap-2">
        {/* Selecting a row opens the task's detail beside the list rather than
            navigating away from it (AC-2.2). It stays a link, so the row is
            still openable in a new tab and the address is still shareable. */}
        <Link
          to="/tasks"
          search={{ task: task.id }}
          className="truncate text-sm font-medium text-foreground hover:underline"
        >
          {task.title || task.id}
        </Link>
        <Badge variant="outline">{taskStateLabels[task.state] ?? task.state}</Badge>
        {stage && <Badge variant="accent">{stageLabels[stage] ?? stage}</Badge>}
        <PlanBadge plan={item.plan} />
        {item.needs_attention && <Badge variant="attention">Needs operator</Badge>}
        {task.hold && <Badge variant="attention">On hold</Badge>}
        {/* Identity and repository are how a row is looked up, not how it is
            read, so they sit last and name themselves on hover. */}
        <Badge variant="mono" title="Task ID">
          {task.id}
        </Badge>
        {task.repo && (
          <Badge variant="mono" title="Repository">
            {task.repo}
          </Badge>
        )}
      </div>

      {/* One wrapping line of facts, so a row with nothing blocking it and
          nothing attached collapses to a single quiet statement of that. */}
      <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1.5 text-xs">
        <Dependencies item={item} />
        <Children item={item} />
        <AttachedContext item={item} />
      </div>

      {/* Staleness reads as the reason a row cannot move on its own, beside
          the badge that says a human is wanted rather than instead of it: a
          task can hold at a gate and carry a stalled order at once. The state
          itself is the derived §21.34 projection the board and task detail
          already render — the list neither stores nor re-derives one. */}
      {item.stalled?.needed && (
        <p className="mt-2 line-clamp-2 text-xs text-failure">
          Stalled — {item.stalled.reason}
          {item.stalled.last_failure ? `: ${item.stalled.last_failure}` : ''}
        </p>
      )}

      {/* The instant the updated-at filter keys on, so a row narrowed away by
          that filter can be explained by the value the row itself shows. */}
      <p
        className="mt-2 text-[11px] text-faint"
        title={new Date(item.last_event_at || task.created_at).toLocaleString()}
      >
        Updated {relativeTime(item.last_event_at || task.created_at)}
      </p>
    </div>
  )
}

// Plan status is shown for every task, including the ones with no plan record,
// so a blank cell never stands in for an unknown state (AC-1.4).
function PlanBadge({ plan }: { plan: TaskPlanStatus }) {
  const version = plan.version ? ` v${plan.version}` : ''
  switch (plan.state) {
    case 'approved':
      return (
        <>
          <Badge variant="positive">Plan approved{version}</Badge>
          {plan.legacy && <Badge variant="mono">Historical spec gate</Badge>}
        </>
      )
    case 'pending_gate':
      return (
        <>
          <Badge variant="attention">Plan awaiting approval{version}</Badge>
          {plan.legacy && <Badge variant="mono">Historical spec gate</Badge>}
        </>
      )
    case 'redirected':
      return (
        <>
          <Badge variant="attention">Plan changes requested{version}</Badge>
          {plan.legacy && <Badge variant="mono">Historical spec gate</Badge>}
        </>
      )
    default:
      return <Badge variant="outline">No plan</Badge>
  }
}

// Dependencies and blocking state come from the task's own relations and the
// projection's unsatisfiable edges; the view derives neither itself (AC-1.2).
function Dependencies({ item }: { item: TaskOperationsItem }) {
  const dependencies = item.task.dependencies ?? []
  if (dependencies.length === 0) {
    return <p className="text-muted">No dependencies</p>
  }
  const blocking = new Set(item.task.blocking_task_ids ?? [])
  const unsatisfiable = new Set(item.unsatisfiable_task_ids ?? [])
  return (
    <div className="flex flex-wrap items-center gap-1.5 text-muted">
      <Network className="size-3 shrink-0" aria-hidden="true" />
      <span>{blocking.size > 0 ? `Blocked by ${blocking.size} of ${dependencies.length}` : 'Depends on'}</span>
      {dependencies.map((dependency) => (
        <DependencyChip
          key={dependency.id}
          dependency={dependency}
          blocking={blocking.has(dependency.id)}
          unsatisfiable={unsatisfiable.has(dependency.id)}
        />
      ))}
    </div>
  )
}

function DependencyChip({
  dependency,
  blocking,
  unsatisfiable,
}: {
  dependency: TaskRelation
  blocking: boolean
  unsatisfiable: boolean
}) {
  const suffix = unsatisfiable ? ' · unsatisfiable' : blocking ? ' · blocking' : ''
  return (
    <Link to="/tasks" search={{ task: dependency.id }}>
      <Badge variant={unsatisfiable ? 'failure' : blocking ? 'attention' : 'outline'}>
        {dependency.title || dependency.id}
        {suffix}
      </Badge>
    </Link>
  )
}

// A rollup renders only where the projection reports children, so a task
// without them says so instead of showing an empty tally (AC-1.2).
function Children({ item }: { item: TaskOperationsItem }) {
  const rollup = item.child_rollup
  if (!rollup) return null
  return (
    <p className="text-muted">
      {rollup.total} child {rollup.total === 1 ? 'task' : 'tasks'} · {rollup.merged} merged · {rollup.closed} closed ·{' '}
      {rollup.open} open
    </p>
  )
}

// Attached context links through the existing Requirements and System Design
// routes. A task with nothing attached says so rather than implying authority
// it does not carry (AC-1.3).
function AttachedContext({ item }: { item: TaskOperationsItem }) {
  const requirements = item.task.context?.requirements ?? []
  const designs = item.task.context?.designs ?? []
  if (requirements.length === 0 && designs.length === 0) {
    return <p className="text-muted">No attached context</p>
  }
  return (
    <div className="flex flex-wrap items-center gap-1.5 text-muted">
      {requirements.length > 0 && <Workflow className="size-3 shrink-0" aria-hidden="true" />}
      {requirements.map((requirement) => (
        <Link key={requirement.id} to="/requirements" search={{ requirement: requirement.id }}>
          <Badge variant="accent">
            {requirement.title} v{requirement.version}
          </Badge>
        </Link>
      ))}
      {designs.length > 0 && <FileCode2 className="size-3 shrink-0" aria-hidden="true" />}
      {designs.map((design) => (
        <Link key={design.id} to="/system-design" search={{ document: design.id }}>
          <Badge variant="accent">
            {design.title} v{design.version}
          </Badge>
        </Link>
      ))}
    </div>
  )
}

function EmptyMessage({ children, tone = 'muted' }: { children: string; tone?: 'muted' | 'failure' }) {
  return (
    <p
      className={`mt-8 rounded-md border border-border p-4 text-sm ${tone === 'failure' ? 'text-failure' : 'text-muted'}`}
    >
      {children}
    </p>
  )
}
