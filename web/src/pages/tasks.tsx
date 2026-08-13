import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import {
  CalendarDays,
  ChevronsUpDown,
  CircleAlert,
  FileCode2,
  GitBranch,
  ListChecks,
  Plus,
  UserRound,
  Workflow,
} from 'lucide-react'
import { useEffect, useState } from 'react'
import { useOperatorToken, useWorkspaceSelection } from '../components/app-shell'
import { AssigneeChip } from '../components/task/assignee-chip'
import { ReturnedForChangesAttention } from '../components/task/returned-for-changes'
import { TaskCreateSheet } from '../components/task/task-create-sheet'
import type { TaskFilterState } from '../components/task/task-filters'
import {
  emptyTaskFilter,
  TaskFilters,
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
import { humanizeClaimRefusal } from '../lib/activity'
import { fetchCallerIdentity, fetchTaskOperations } from '../lib/api'
import { stageLabels, taskStateLabels } from '../lib/contracts'
import { errorMessage } from '../lib/errors'
import type { TaskOperationsItem, TaskPlanStatus } from '../lib/types'
import { relativeTime } from '../lib/utils'

const PAGE_SIZE = 25

// The states an assignment is still asking something of its holder. Terminal
// and approved work is done being routed, so My tasks leaves it out; a
// bounced-back review lands in awaiting_human, which is why it is here.
const MY_TASK_STATES = ['queued', 'running', 'awaiting_human']

// Every row is its own grid, so the header and the rows line up only because
// they read one track definition — hence the shared constant rather than a
// string repeated in two places. State, Stage and Updated are fixed: their
// content is badges and a short phrase that cannot be reflowed, so each is held
// at the width its widest value needs and nothing spills into the neighbour.
// Name and Context take what is left, and they are the two that shorten
// gracefully. The total is narrower than the table area a 1280px window leaves
// beside the navigation, so the horizontal scroll below is the fallback for
// smaller windows rather than the ordinary way this list is read.
const TASK_COLUMNS = 'grid grid-cols-[minmax(200px,2.2fr)_116px_192px_minmax(150px,1.4fr)_168px]'

// The list-first Tasks view. It reads one
// paginated projection and renders only what that projection carries: state,
// repository, dependencies and blockers, child rollups, attached context, and
// plan status. There is no priority or declared-phase control
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
  const token = useOperatorToken()
  // Who "my" is. Without an identity the preset has no referent, so it is not
  // offered rather than guessing (REQ-2).
  const { data: me } = useQuery({
    queryKey: ['caller-identity', token, workspace],
    queryFn: () => fetchCallerIdentity(token),
    enabled: Boolean(workspace && token),
    retry: false,
  })
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

        {/* What is waiting on me personally leads the surface, above the list
            it is drawn from and above the filters — a return that only showed
            up once the right filter was chosen would not be attention (REQ-6).
            Without an identity there is no "me" to scope it to, so it is
            absent rather than guessed at. */}
        {me && <ReturnedForChangesAttention me={me.id} />}

        <div className="mt-6 flex flex-wrap items-center gap-2">
          <TaskFilters value={filter} onChange={setFilter} fallback={emptyTaskFilter} />
          {me && <MyTasksPreset me={me.id} value={filter} onChange={setFilter} />}
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
          <section className="mt-7 overflow-hidden rounded-xl border border-border bg-card" aria-label="Tasks table">
            {/* The scroll lives on the outer element and the width floor on the
                content inside it: a floor on the scroller itself pushes the
                table past its own viewport and clips the last column instead of
                offering the scroll it was meant to. */}
            <div className="overflow-x-auto">
              <div className="min-w-[860px]">
                <div
                  className={`${TASK_COLUMNS} border-b border-border bg-background/70 px-4 py-3 text-[11px] font-medium uppercase tracking-[0.12em] text-muted`}
                >
                  <SortHeader label="Name" />
                  <SortHeader label="State" />
                  <SortHeader label="Stage" />
                  <SortHeader label="Context" />
                  <SortHeader label="Updated" />
                </div>
                {/* One static group over the whole page: it labels and counts
                    what is below it, and carries no expand affordance, because
                    there is nothing here to collapse it into. */}
                <div className="flex items-center gap-2 border-b border-border bg-raised/40 px-4 py-3 text-sm font-medium">
                  <ListChecks className="size-4 text-primary" aria-hidden="true" />
                  <span>All tasks</span>
                  <span className="text-xs text-muted">{page?.total ?? items.length}</span>
                </div>
                <ul aria-label="Tasks">
                  {items.map((item) => (
                    <TaskRow key={item.task.id} item={item} selected={item.task.id === selectedId} />
                  ))}
                </ul>
              </div>
            </div>
            <p className="border-t border-border px-4 py-3 text-xs text-muted">
              {page?.total ?? items.length} {page?.total === 1 ? 'task' : 'tasks'}
            </p>
          </section>
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
          composition, so hosting it here inherits it. */}
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

// One click onto the work that is actually mine and still moving. It is a
// preset over the existing filter row, not a view of its own: the row shows
// exactly what it selected, and pressing it again hands the surface back
// (REQ-4, DEC-18 — routing, never ordering).
function MyTasksPreset({
  me,
  value,
  onChange,
}: {
  me: string
  value: TaskFilterState
  onChange: (next: TaskFilterState) => void
}) {
  const active =
    value.assignee === me &&
    value.states.length === MY_TASK_STATES.length &&
    MY_TASK_STATES.every((state) => value.states.includes(state))
  return (
    <Button
      type="button"
      variant={active ? 'secondary' : 'outline'}
      size="sm"
      aria-pressed={active}
      onClick={() => onChange(active ? emptyTaskFilter : { ...value, assignee: me, states: [...MY_TASK_STATES] })}
    >
      <UserRound />
      My tasks
    </Button>
  )
}

// The panel's address, absolute so it survives being pasted somewhere else.
function taskPermalink(taskId: string) {
  const path = `/tasks?task=${encodeURIComponent(taskId)}`
  return typeof window === 'undefined' ? path : new URL(path, window.location.origin).toString()
}

function SortHeader({ label }: { label: string }) {
  return (
    <div className="flex items-center gap-1">
      <span>{label}</span>
      <ChevronsUpDown className="size-3 text-faint" aria-hidden="true" />
    </div>
  )
}

function TaskRow({ item, selected }: { item: TaskOperationsItem; selected: boolean }) {
  const { task } = item
  const stage =
    task.state === 'queued' ? (task.next_stage ?? item.latest_stage) : (item.latest_stage ?? task.next_stage)
  return (
    <li
      className={`${TASK_COLUMNS} items-center border-b border-border px-4 py-3 transition-colors hover:bg-raised/50 ${selected ? 'bg-primary/5 ring-1 ring-inset ring-primary' : ''}`}
    >
      <div className="flex min-w-0 items-center gap-3 pr-4">
        <span className="text-faint" aria-hidden="true">
          ⠿
        </span>
        <div className="min-w-0">
          <Link
            to="/tasks"
            search={{ task: task.id }}
            aria-current={selected ? 'true' : undefined}
            // A title long enough to be cut by this column stays readable in
            // place rather than only inside the panel it opens.
            title={task.title || task.id}
            className="block truncate text-sm font-medium text-foreground hover:underline"
          >
            {task.title || task.id}
          </Link>
          <div className="mt-1 flex items-center gap-2 text-[11px] text-faint">
            <span className="font-mono">{task.id}</span>
            {task.repo && (
              <span className="inline-flex items-center gap-1">
                <GitBranch className="size-3" aria-hidden="true" />
                {task.repo}
              </span>
            )}
            {/* Assignment is claim-eligibility routing, never ordering, so it
                rides the row's identity line rather than claiming a column of
                its own beside State and Stage (REQ-4, DEC-18). */}
            <AssigneeChip assignee={task.assignee} />
          </div>
        </div>
      </div>
      <div className="flex flex-wrap items-center gap-1.5 pr-3">
        <Badge variant="outline">{taskStateLabels[task.state] ?? task.state}</Badge>
        {task.hold && <Badge variant="attention">On hold</Badge>}
        {item.needs_attention && <CircleAlert className="size-4 text-attention" aria-label="Needs operator" />}
      </div>
      <div className="flex min-w-0 flex-wrap items-center gap-1.5 pr-3">
        {stage ? (
          <Badge variant="accent">{stageLabels[stage] ?? stage}</Badge>
        ) : (
          <span className="text-xs text-muted">No stage</span>
        )}
        <PlanBadge plan={item.plan} />
      </div>
      <div className="flex min-w-0 flex-wrap items-center gap-1.5 pr-3 text-xs text-muted">
        <AttachedContext item={item} />
        {item.child_rollup && <Children item={item} />}
      </div>
      {/* The timestamp is one short phrase, so it holds its line rather than
          breaking mid-phrase in the narrowest supported column. */}
      <div
        className="flex items-center gap-2 whitespace-nowrap text-xs text-muted"
        title={new Date(item.last_event_at || task.created_at).toLocaleString()}
      >
        <CalendarDays className="size-4 shrink-0 text-faint" aria-hidden="true" />
        <span>Updated {relativeTime(item.last_event_at || task.created_at)}</span>
      </div>
      {item.stalled?.needed && (
        <p className="col-span-full mt-2 border-t border-failure/20 pt-2 text-xs text-failure">
          Stalled — {item.stalled.reason}
          {item.stalled.last_failure ? `: ${humanizeClaimRefusal(item.stalled.last_failure, task.assignee)}` : ''}
        </p>
      )}
    </li>
  )
}

// Plan status is shown where it is actionable (AC-1.4). An approved plan is the
// state every task passes through on its way to being implemented, so naming it
// beside the stage that already implies it says nothing an operator can act on;
// the outcomes that still want a decision, and the absence of a plan, keep their
// badge. The historical-gate marker is orthogonal to the outcome and stays.
function PlanBadge({ plan }: { plan: TaskPlanStatus }) {
  const version = plan.version ? ` v${plan.version}` : ''
  switch (plan.state) {
    case 'approved':
      return plan.legacy ? <Badge variant="mono">Historical spec gate</Badge> : null
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
//
// Document titles are written for the document, not for this column, so a badge
// that refused to shrink would run under the Updated cell. Each attachment takes
// a line of its own with its kind icon beside it — a badge that shrank away from
// a wrapped icon would leave the icon stranded on a line by itself.
function AttachedContext({ item }: { item: TaskOperationsItem }) {
  const requirements = item.task.context?.requirements ?? []
  const designs = item.task.context?.designs ?? []
  if (requirements.length === 0 && designs.length === 0) {
    return <p className="text-muted">No attached context</p>
  }
  return (
    <div className="flex min-w-0 flex-col gap-1 text-muted">
      {requirements.map((requirement) => (
        <div key={requirement.id} className="flex min-w-0 items-center gap-1.5">
          <Workflow className="size-3 shrink-0" aria-hidden="true" />
          <Link to="/requirements" search={{ requirement: requirement.id }} className="min-w-0">
            <ContextBadge label={`${requirement.title} v${requirement.version}`} />
          </Link>
        </div>
      ))}
      {designs.map((design) => (
        <div key={design.id} className="flex min-w-0 items-center gap-1.5">
          <FileCode2 className="size-3 shrink-0" aria-hidden="true" />
          <Link to="/system-design" search={{ document: design.id }} className="min-w-0">
            <ContextBadge label={`${design.title} v${design.version}`} />
          </Link>
        </div>
      ))}
    </div>
  )
}

// Held to the width its column leaves and ellipsised there, with the whole title
// in the tooltip and the link it opens still carrying the rest.
function ContextBadge({ label }: { label: string }) {
  return (
    <Badge variant="accent" className="max-w-full" title={label}>
      <span className="truncate">{label}</span>
    </Badge>
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
