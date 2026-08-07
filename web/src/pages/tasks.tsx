import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { FileCode2, ListChecks, Network, Search, Workflow } from 'lucide-react'
import { useMemo, useState } from 'react'
import { useWorkspace, useWorkspaceSelection } from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Card, CardContent } from '../components/ui/card'
import { Input, Select } from '../components/ui/input'
import { Skeleton } from '../components/ui/skeleton'
import { fetchTaskOperations } from '../lib/api'
import { stageLabels, taskStateLabels } from '../lib/contracts'
import { errorMessage } from '../lib/errors'
import { relativeTime } from '../lib/utils'
import type { TaskOperationsItem, TaskPlanStatus, TaskRelation } from '../lib/types'

// The list-first Tasks view (spec §21.58, REQ-1). It reads one projection and
// renders only what that projection carries: state, repository, dependencies
// and blockers, child rollups, attached context, and plan status. There is no
// priority, assignee, or declared-phase control anywhere on this surface, and
// none may be added to support it (AC-1.5).
export function TasksPage() {
  const { workspace } = useWorkspaceSelection()
  const { data: workspaceInfo } = useWorkspace()
  const [query, setQuery] = useState('')
  const [state, setState] = useState('')
  const [repo, setRepo] = useState('')
  const [offset, setOffset] = useState(0)
  const limit = 100
  const {
    data: page,
    isLoading,
    error,
  } = useQuery({
    queryKey: ['task-operations', workspace, state, repo, offset],
    queryFn: () => fetchTaskOperations({ limit, offset, state, repository: repo }),
    enabled: Boolean(workspace),
    refetchInterval: 15_000,
  })
  const items = page?.items

  // State and repository are server-side filters now, so the returned page no
  // longer contains the values the operator has filtered away. The option lists
  // come from workspace-stable sources instead: the declared task states and
  // the configured repositories.
  const states = useMemo(() => Object.keys(taskStateLabels).sort(), [])
  const repos = useMemo(() => (workspaceInfo?.repos ?? []).map((entry) => entry.name).sort(), [workspaceInfo])
  // Free text stays a client-side narrowing of the fetched page; state and
  // repository are already applied by the server.
  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase()
    if (!needle) return items ?? []
    return (items ?? []).filter((item) =>
      [item.task.title, item.task.id, item.task.source, item.task.branch]
        .filter(Boolean)
        .some((field) => field.toLowerCase().includes(needle)),
    )
  }, [items, query])
  const filtersActive = Boolean(query.trim() || state || repo)

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-5xl px-6 py-8">
        <header>
          <div className="flex items-center gap-2">
            <h1 className="text-xl font-semibold tracking-tight">Tasks</h1>
            {/* The server total counts what the state/repository filters match;
                a free-text needle narrows the fetched page, so it counts rows. */}
            <Badge variant="mono">{query.trim() ? filtered.length : (page?.total ?? filtered.length)}</Badge>
          </div>
          <p className="mt-1 max-w-2xl text-sm text-muted">
            Every task in this workspace with its ordering, attached context, and plan status.
          </p>
        </header>

        <div className="mt-6 flex flex-wrap items-center gap-2">
          <label className="relative" htmlFor="task-search">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-faint" />
            <Input
              id="task-search"
              aria-label="Search tasks"
              type="search"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="Search tasks"
              className="h-8 w-56 pl-8 text-xs"
            />
          </label>
          <Select
            aria-label="Filter by state"
            value={state}
            onChange={(event) => {
              setState(event.target.value)
              setOffset(0)
            }}
            className="h-8 w-44 text-xs"
          >
            <option value="">All states</option>
            {states.map((value) => (
              <option key={value} value={value}>
                {taskStateLabels[value] ?? value}
              </option>
            ))}
          </Select>
          <Select
            aria-label="Filter by repository"
            value={repo}
            onChange={(event) => {
              setRepo(event.target.value)
              setOffset(0)
            }}
            className="h-8 w-44 text-xs"
          >
            <option value="">All repositories</option>
            {repos.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </Select>
        </div>

        {!workspace && <EmptyMessage>Choose a workspace to open its tasks.</EmptyMessage>}
        {isLoading && workspace && (
          <div className="mt-7 space-y-3">
            {['one', 'two', 'three'].map((key) => (
              <Skeleton key={key} className="h-24 w-full rounded-lg" />
            ))}
          </div>
        )}
        {error != null && <EmptyMessage tone="failure">{errorMessage(error, 'Could not load tasks.')}</EmptyMessage>}

        {items && filtered.length === 0 && (
          <Card className="mt-8 border-dashed">
            <CardContent className="flex min-h-56 flex-col items-center justify-center text-center">
              <ListChecks className="size-7 text-primary" />
              {/* The state and repository filters are applied by the server, so
                  an empty page no longer means an empty workspace — whether any
                  filter is active is what separates the two messages. */}
              <h2 className="mt-4 text-base font-semibold">
                {filtersActive ? 'No tasks match these filters' : 'No tasks yet'}
              </h2>
              <p className="mt-2 max-w-md text-sm leading-6 text-muted">
                {filtersActive
                  ? 'Clear the search, state, or repository filter to see the rest of the workspace.'
                  : 'Delivery work arrives here once planning files a task set.'}
              </p>
            </CardContent>
          </Card>
        )}

        {filtered.length > 0 && (
          <ul aria-label="Tasks" className="mt-7 space-y-3">
            {filtered.map((item) => (
              <li key={item.task.id}>
                <TaskRow item={item} />
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
    </div>
  )
}

function TaskRow({ item }: { item: TaskOperationsItem }) {
  const { task } = item
  const stage =
    task.state === 'queued' ? (task.next_stage ?? item.latest_stage) : (item.latest_stage ?? task.next_stage)
  return (
    <div className="rounded-lg border border-border bg-card p-4">
      <div className="flex flex-wrap items-center gap-2">
        {/* The row reuses the existing task detail route rather than
            re-rendering the task's own surface (spec §13.3). */}
        <Link
          to="/tasks/$taskId/full"
          params={{ taskId: task.id }}
          className="truncate text-sm font-medium text-foreground hover:underline"
        >
          {task.title || task.id}
        </Link>
        <Badge variant="mono">{task.id}</Badge>
        <Badge variant="outline">{taskStateLabels[task.state] ?? task.state}</Badge>
        {task.repo && <Badge variant="mono">{task.repo}</Badge>}
        {stage && <Badge variant="accent">{stageLabels[stage] ?? stage}</Badge>}
        <PlanBadge plan={item.plan} />
        {item.needs_attention && <Badge variant="attention">Needs operator</Badge>}
        {task.hold && <Badge variant="attention">On hold</Badge>}
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

      <p className="mt-2 text-[11px] text-faint">Updated {relativeTime(item.last_event_at || task.created_at)}</p>
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
    <Link to="/tasks/$taskId/full" params={{ taskId: dependency.id }}>
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
