import { useQueries, useQuery } from '@tanstack/react-query'
import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { Check, ChevronDown, ChevronRight, GitBranch, ListChecks, UserRound, X } from 'lucide-react'
import { useEffect, useState } from 'react'
import { fetchCallerIdentity, fetchTaskOperations } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type {
  TaskOperationsGroup,
  TaskOperationsGroupCounts,
  TaskOperationsItem,
  TaskOperationsPage,
} from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { AssigneeChip } from '../task/assignee-chip'
import { ReturnedForChangesAttention } from '../task/returned-for-changes'
import type { TaskFilterState } from '../task/task-filters'
import {
  emptyTaskFilter,
  TaskFilters,
  taskFilterActive,
  taskFilterParams,
  taskFilterRangeError,
  useTaskFilters,
} from '../task/task-filters'
import { Button } from '../ui/button'
import { Card, CardContent } from '../ui/card'
import { Disclosure } from '../ui/disclosure'
import { Skeleton } from '../ui/skeleton'
import { BoardListContext } from './board-list-context'
import { BoardListProgress } from './board-list-progress'
import { type BoardSearch, boardSearchFilter } from './board-search'
import { BoardToolbar } from './board-toolbar'

const PAGE_SIZE = 25

// The states an assignment is still asking something of its holder. Terminal
// and approved work is done being routed, so My tasks leaves it out; a
// bounced-back review lands in awaiting_human, which is why it is here.
const MY_TASK_STATES = ['queued', 'running', 'awaiting_human']

// Who acts next, in the order the list presents it (req-operator-next-action
// AC-6.5). The server assigns every task to exactly one group from its stated
// next action; the browser never sorts tasks into groups itself.
const GROUPS: { group: TaskOperationsGroup; title: string; open: boolean }[] = [
  { group: 'operator', title: 'Needs you', open: true },
  { group: 'agent', title: 'In flight', open: true },
  { group: 'waiting', title: 'Waiting', open: true },
  // Finished work stays folded away until asked for, and asks the server for
  // nothing while it is.
  { group: 'terminal', title: 'Done', open: false },
]

type GroupRecord<T> = Record<TaskOperationsGroup, T>

const initialOpen: GroupRecord<boolean> = { operator: true, agent: true, waiting: true, terminal: false }
const zeroOffsets: GroupRecord<number> = { operator: 0, agent: 0, waiting: 0, terminal: 0 }

// Every row is its own grid, so the header and the rows line up only because
// they read one track definition. Name, Progress, and Context shorten
// gracefully; the assignee and Updated tracks hold the width their short
// content needs. Below md the cells stack into one card-like row and the
// header hides, because there are no columns to head.
const TASK_COLUMNS =
  'grid-cols-1 gap-y-2 md:grid-cols-[minmax(220px,2.2fr)_minmax(150px,1.3fr)_minmax(140px,1fr)_24px_44px] md:gap-x-4 md:gap-y-0'

// The Board's list view (REQ-6): the task-operations projection grouped by who
// acts next. Each open group reads its own server page and the headings read
// the server's per-group counts. There is no priority or declared-phase
// control anywhere on this surface, and none may be added to support it
// (AC-1.5). A row opens the Board's task sheet over the list.
export function BoardList() {
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const search = useSearch({ strict: false }) as BoardSearch
  const { taskId: selectedId } = useParams({ strict: false }) as { taskId?: string }
  // Saved list filters keep their storage key across the move from /tasks.
  const [filter, setFilter] = useTaskFilters('tasks')
  // A filter named in the address (a redirected /tasks link) wins on arrival,
  // then leaves the address so the saved filter is the one source of truth.
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
  // Who "my" is. Without an identity the preset has no referent, so it is not
  // offered rather than guessing (REQ-2).
  const { data: me } = useQuery({
    queryKey: ['caller-identity', workspace],
    queryFn: () => fetchCallerIdentity(workspace),
    enabled: Boolean(workspace),
    retry: false,
  })
  const params = taskFilterParams(filter)
  // A range no task can satisfy is a half-typed date, not a question worth
  // asking the server: the filter says so in place and nothing is sent.
  const rangeError = taskFilterRangeError(filter)
  const filtersActive = taskFilterActive(filter)

  // Collapse lives in memory only; a reload opens the default groups again.
  const [open, setOpen] = useState(initialOpen)
  // Offsets belong to one workspace and filter. Deriving them against the
  // current scope resets every group to its first page in the same render the
  // scope changes, so no request for the new scope carries an old offset.
  const scope = JSON.stringify([workspace, params])
  const [paging, setPaging] = useState({ scope, offsets: zeroOffsets })
  const offsets = paging.scope === scope ? paging.offsets : zeroOffsets
  const setOffset = (group: TaskOperationsGroup, offset: number) =>
    setPaging({ scope, offsets: { ...offsets, [group]: offset } })

  // One query per open group. Every filter, including My tasks, is applied by
  // the server across the whole workspace history before it groups and pages
  // (AC-2.3). A collapsed group mounts no request and no refresh.
  const enabled = Boolean(workspace) && !rangeError && !arrivalKey
  const queries = useQueries({
    queries: GROUPS.map(({ group }) => ({
      queryKey: ['task-operations', workspace, params, group, offsets[group]],
      queryFn: () => fetchTaskOperations({ limit: PAGE_SIZE, offset: offsets[group], filter: params, group }),
      enabled: enabled && open[group],
      refetchInterval: 15_000,
    })),
  })
  const pages = Object.fromEntries(GROUPS.map(({ group }, index) => [group, queries[index]])) as GroupRecord<
    (typeof queries)[number]
  >

  // The headings read one count snapshot: the grouped response for this scope
  // that arrived last. Every query key carries the scope, so a response for an
  // earlier workspace or filter can never supply it, and counts are never
  // summed from pages or mixed across responses.
  let counts: TaskOperationsGroupCounts | undefined
  let countsAt = -1
  for (const query of queries) {
    if (query.data?.groupCounts && query.dataUpdatedAt > countsAt) {
      counts = query.data.groupCounts
      countsAt = query.dataUpdatedAt
    }
  }

  // A refresh can shrink a group under its open page; move it to the last page
  // that still exists rather than stranding it past the end.
  const overflowed = GROUPS.map(({ group }) => {
    const page = pages[group].data
    if (!page || page.offset !== offsets[group] || offsets[group] === 0) return null
    if (page.offset < page.total) return null
    return [group, page.total === 0 ? 0 : Math.floor((page.total - 1) / PAGE_SIZE) * PAGE_SIZE] as const
  }).find(Boolean)
  const overflowGroup = overflowed?.[0]
  const overflowOffset = overflowed?.[1]
  useEffect(() => {
    if (overflowGroup === undefined || overflowOffset === undefined) return
    setPaging((current) =>
      current.scope === scope ? { scope, offsets: { ...current.offsets, [overflowGroup]: overflowOffset } } : current,
    )
  }, [overflowGroup, overflowOffset, scope])

  const empty = counts !== undefined && GROUPS.every(({ group }) => counts?.[group] === 0)

  return (
    <>
      {/* The list keeps its filters with its content; the shared toolbar holds
          only the view switch so it stays where the other views put it. */}
      <BoardToolbar />
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto max-w-5xl px-4 py-6 sm:px-6">
          <header>
            <p className="max-w-2xl text-sm text-muted">
              Every task in this workspace, grouped by who acts next, with its progress and attached context.
            </p>
          </header>

          {/* What is waiting on me personally leads the surface, above the list
            it is drawn from and above the filters — a return that only showed
            up once the right filter was chosen would not be attention (REQ-6).
            Without an identity there is no "me" to scope it to, so it is
            absent rather than guessed at. */}
          {me && <ReturnedForChangesAttention />}

          <div className="mt-6 flex flex-wrap items-center gap-2">
            <TaskFilters value={filter} onChange={setFilter} fallback={emptyTaskFilter} />
            {me && <MyTasksPreset me={me.id} value={filter} onChange={setFilter} />}
          </div>

          {!workspace && <EmptyMessage>Choose a workspace to open its tasks.</EmptyMessage>}

          {workspace && (
            <section className="mt-7 overflow-hidden rounded-xl border border-border bg-card" aria-label="Tasks table">
              {/* The scroll lives on the outer element and the width floor on the
                content inside it: a floor on the scroller itself pushes the
                table past its own viewport and clips the last column instead of
                offering the scroll it was meant to. */}
              <div className="overflow-x-auto">
                <div className="md:min-w-[720px]">
                  {/* The order is fixed, newest created first inside each group,
                    so no header offers a sort. */}
                  <div
                    data-testid="tasks-table-header"
                    className={`hidden md:grid ${TASK_COLUMNS} border-b border-border bg-background/70 px-4 py-2.5 text-[11px] font-medium uppercase tracking-[0.12em] text-muted`}
                  >
                    <span>Name</span>
                    <span>Progress</span>
                    <span>Context</span>
                    <span aria-hidden="true" />
                    <span>Updated</span>
                  </div>
                  {GROUPS.map(({ group, title }) => (
                    <TaskGroup
                      key={group}
                      group={group}
                      title={title}
                      count={counts?.[group]}
                      open={open[group]}
                      onToggle={() => setOpen((current) => ({ ...current, [group]: !current[group] }))}
                      query={pages[group]}
                      offset={offsets[group]}
                      onOffset={(offset) => setOffset(group, offset)}
                      selectedId={selectedId}
                    />
                  ))}
                </div>
              </div>
            </section>
          )}

          {empty && (
            <Card className="mt-8 border-dashed">
              <CardContent className="flex min-h-56 flex-col items-center justify-center text-center">
                <ListChecks className="size-7 text-faint" />
                {/* Every filter is applied by the server, so an empty result
                  no longer means an empty workspace — whether any filter is
                  active is what separates the two messages. */}
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
        </div>
      </div>
    </>
  )
}

type GroupQuery = {
  data?: TaskOperationsPage
  isLoading: boolean
  error: unknown
}

// One collapsible group (AC-6.5). The heading carries the server's count for
// the whole filtered workspace history; a collapsed group renders no rows and
// its query is disabled, so it reads nothing and runs no refresh.
function TaskGroup({
  group,
  title,
  count,
  open,
  onToggle,
  query,
  offset,
  onOffset,
  selectedId,
}: {
  group: TaskOperationsGroup
  title: string
  count: number | undefined
  open: boolean
  onToggle: () => void
  query: GroupQuery
  offset: number
  onOffset: (offset: number) => void
  selectedId?: string
}) {
  const page = query.data
  const items = open && page && page.offset === offset ? page.items : undefined
  const bodyId = `task-group-${group}`
  const Chevron = open ? ChevronDown : ChevronRight
  return (
    <section aria-label={title} data-group={group} className="border-b border-border last:border-b-0">
      <h3 className="m-0">
        <button
          type="button"
          aria-expanded={open}
          aria-controls={bodyId}
          onClick={onToggle}
          className="flex w-full items-center gap-2 bg-raised/40 px-4 py-2.5 text-left text-sm font-medium hover:bg-raised/70"
        >
          <Chevron className="size-4 shrink-0 text-faint" aria-hidden="true" />
          <span className={group === 'operator' ? 'text-attention' : 'text-foreground'}>{title}</span>
          {/* A count the server has not stated yet is unknown, never zero. */}
          <span
            data-group-count
            className="text-xs font-normal text-muted"
            title={count === undefined ? 'Count unavailable' : undefined}
          >
            {count === undefined ? '–' : count}
          </span>
        </button>
      </h3>
      {open && (
        <div id={bodyId}>
          {query.isLoading && (
            <div className="space-y-2 px-4 py-3">
              {['one', 'two'].map((key) => (
                <Skeleton key={key} className="h-9 w-full rounded-md" />
              ))}
            </div>
          )}
          {query.error != null && (
            <p className="px-4 py-3 text-sm text-failure">{errorMessage(query.error, 'Could not load tasks.')}</p>
          )}
          {items && items.length > 0 && (
            <ul aria-label="Tasks">
              {items.map((item) => (
                <TaskRow key={item.task.id} item={item} group={group} selected={item.task.id === selectedId} />
              ))}
            </ul>
          )}
          {page && page.total > page.limit && (
            <nav className="flex items-center justify-end gap-2 px-4 py-2" aria-label="Task pages">
              <button
                type="button"
                className="rounded-md border border-border px-3 py-1 text-xs disabled:opacity-40"
                disabled={offset === 0}
                onClick={() => onOffset(Math.max(0, offset - page.limit))}
              >
                Previous
              </button>
              <span className="text-xs text-muted">
                {page.items.length === 0
                  ? `0 of ${page.total}`
                  : `${page.offset + 1}–${Math.min(page.offset + page.items.length, page.total)} of ${page.total}`}
              </span>
              <button
                type="button"
                className="rounded-md border border-border px-3 py-1 text-xs disabled:opacity-40"
                disabled={offset + page.limit >= page.total}
                onClick={() => onOffset(offset + page.limit)}
              >
                Next
              </button>
            </nav>
          )}
        </div>
      )}
    </section>
  )
}

// One click onto the work that is actually mine and still moving. It is a
// preset over the existing filter row, not a view of its own: the row shows
// exactly what it selected, and pressing it again hands the surface back
// (req-task-lifecycle-and-queue AC-2.3, AC-2.7; DEC-74(4) — routing, never
// ordering).
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
      onClick={() =>
        onChange(
          active ? { ...value, assignee: '', states: [] } : { ...value, assignee: me, states: [...MY_TASK_STATES] },
        )
      }
    >
      <UserRound />
      My tasks
    </Button>
  )
}

// One task on one line (AC-6.2, AC-6.6). The group and the glyph carry the
// state, so the row has no state or stage badge. Only an operator row carries
// the attention tone, and only progress and selection carry primary.
function TaskRow({
  item,
  group,
  selected,
}: {
  item: TaskOperationsItem
  group: TaskOperationsGroup
  selected: boolean
}) {
  const { task } = item
  const action = item.next_action
  const operator = action?.actor === 'operator'
  const updated = item.last_event_at || task.created_at
  return (
    <li
      data-task-row={task.id}
      data-operator={operator ? 'true' : undefined}
      className={cn(
        `grid ${TASK_COLUMNS} items-center border-t border-border px-4 py-2.5 transition-colors first:border-t-0`,
        selected
          ? 'bg-primary/5 shadow-[inset_2px_0_0_var(--color-primary)]'
          : cn('hover:bg-raised/50', operator && 'shadow-[inset_2px_0_0_var(--color-attention)]'),
      )}
    >
      <div className="flex min-w-0 items-start gap-2.5">
        <StateGlyph group={group} state={task.state} />
        <div className="min-w-0 flex-1">
          <Link
            to="/tasks/$taskId"
            params={{ taskId: task.id }}
            search={{ view: 'list' }}
            aria-current={selected ? 'true' : undefined}
            // A title long enough to be cut by this column stays readable in
            // place rather than only inside the panel it opens.
            title={task.title || task.id}
            className="block truncate text-sm font-medium leading-5 text-foreground hover:underline"
          >
            {task.title || task.id}
          </Link>
          {operator && action ? (
            // The server's own words for what the operator is asked (AC-1.8).
            // This line replaces the stalled and agent-question footers.
            <p
              className="truncate text-xs leading-5"
              data-operator-reason
              title={`${action.label} — ${action.summary}`}
            >
              <span className="font-medium text-attention">{action.label}</span>
              <span className="text-faint"> — {action.summary}</span>
            </p>
          ) : (
            <p className="flex min-w-0 items-center gap-2 truncate text-[11px] leading-5 text-faint">
              <span className="font-mono">{task.id}</span>
              {task.repo && (
                <span className="inline-flex items-center gap-1">
                  <GitBranch className="size-3" aria-hidden="true" />
                  {task.repo}
                </span>
              )}
              {item.child_rollup && <ChildRollup item={item} />}
            </p>
          )}
        </div>
      </div>
      <BoardListProgress item={item} operator={operator} />
      <BoardListContext task={task} />
      {/* Assignment is claim-eligibility routing, never ordering
          (req-task-lifecycle-and-queue AC-2.3, AC-2.7; DEC-74(4)). */}
      <div className="flex min-w-0 items-center md:justify-center">
        <AssigneeChip assignee={task.assignee} compact tone="neutral" />
      </div>
      <Disclosure
        triggerClassName="whitespace-nowrap text-xs text-muted"
        content={new Date(updated).toLocaleString()}
        contentClassName="left-auto right-0"
        label={shortElapsed(updated) === 'now' ? 'Updated just now' : `Updated ${shortElapsed(updated)} ago`}
      >
        {shortElapsed(updated)}
      </Disclosure>
    </li>
  )
}

const glyphBase = 'mt-[3px] flex size-3.5 shrink-0 items-center justify-center rounded-full'

// The 14 px mark that leads every row: who acts next, or how a finished task
// ended. Only the operator's mark carries a colour.
function StateGlyph({ group, state }: { group: TaskOperationsGroup; state: string }) {
  switch (group) {
    case 'operator':
      return (
        <span
          role="img"
          aria-label="Needs you"
          className={cn(glyphBase, 'bg-attention-dot text-[9px] font-bold leading-none text-white')}
        >
          !
        </span>
      )
    case 'agent':
      return (
        <span
          role="img"
          aria-label="In flight"
          className={cn(glyphBase, 'border-[1.5px] border-muted')}
          style={{ background: 'linear-gradient(90deg, var(--color-muted) 50%, transparent 50%)' }}
        />
      )
    case 'waiting':
      return (
        <span role="img" aria-label="Waiting" className={cn(glyphBase, 'border-[1.5px] border-dashed border-faint')} />
      )
    default:
      return state === 'closed' ? (
        <X role="img" aria-label="Closed" className="mt-[3px] size-3.5 shrink-0 text-muted" strokeWidth={2.5} />
      ) : (
        <Check role="img" aria-label="Merged" className="mt-[3px] size-3.5 shrink-0 text-muted" strokeWidth={2.5} />
      )
  }
}

// A rollup renders only where the projection reports children, so a task
// without them says so by its absence (AC-1.2).
function ChildRollup({ item }: { item: TaskOperationsItem }) {
  const rollup = item.child_rollup
  if (!rollup) return null
  return (
    <span className="truncate">
      {rollup.total} child {rollup.total === 1 ? 'task' : 'tasks'} · {rollup.merged} merged · {rollup.closed} closed ·{' '}
      {rollup.open} open
    </span>
  )
}

// "3m", "16h", "2d": the Updated column's short elapsed time. The disclosure
// beside it carries the full timestamp.
export function shortElapsed(value: string, now = Date.now()): string {
  const at = new Date(value).getTime()
  if (!Number.isFinite(at) || at <= 0) return '—'
  const minutes = Math.floor(Math.max(0, now - at) / 60_000)
  if (minutes < 1) return 'now'
  if (minutes < 60) return `${minutes}m`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h`
  return `${Math.floor(hours / 24)}d`
}

function EmptyMessage({ children }: { children: string }) {
  return <p className="mt-8 rounded-md border border-border p-4 text-sm text-muted">{children}</p>
}
