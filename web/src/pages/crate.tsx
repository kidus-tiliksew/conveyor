import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import {
  Archive,
  ArchiveRestore,
  CircleDot,
  CircleSlash,
  ExternalLink,
  GitBranch,
  GitMerge,
  GitPullRequest,
  MessageSquareText,
  Package,
  Pencil,
  Plus,
  Search,
  Undo2,
  X,
} from 'lucide-react'
import { type ReactNode, useState } from 'react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
import { CrateHero } from '../components/crate/crate-hero'
import { CrateFormDialog } from '../components/crate/crate-parts'
import { TaskRow } from '../components/insights/shared'
import { Button } from '../components/ui/button'
import { Dialog } from '../components/ui/dialog'
import { DropdownMenu, DropdownMenuItem } from '../components/ui/dropdown-menu'
import { ErrorState } from '../components/ui/error-state'
import { Input } from '../components/ui/input'
import { PageHeader } from '../components/ui/page-header'
import { Skeleton } from '../components/ui/skeleton'
import {
  addTaskToCrate,
  fetchCrate,
  fetchCrateActivity,
  fetchOpenTasks,
  removeTaskFromCrate,
  updateCrate,
} from '../lib/api'
import { taskStateLabels } from '../lib/contracts'
import { crateActivityQueryKey, crateQueryKey } from '../lib/query-keys'
import type { CrateActivityEvent, CrateDetail, CrateRequirement, CrateTask } from '../lib/types'
import { absoluteTime, cn, relativeTime } from '../lib/utils'

// One crate's page (component-crates invariant 17; req-crates REQ-5): the
// header with its counts and shipped statement, its declared requirements and
// the ones its tasks carry beyond them, its member tasks, and their activity.
// Every act renders only for a user with create_tasks; the server checks it
// again.
export function CratePage() {
  const { crateId } = useParams({ strict: false }) as { crateId: string }
  const { workspace } = useWorkspaceSelection()
  const crate = useQuery({
    queryKey: crateQueryKey(workspace, crateId),
    queryFn: () => fetchCrate(workspace, crateId),
    enabled: Boolean(workspace && crateId),
  })

  if (crate.isPending) return <CratePageSkeleton />
  if (crate.error != null || !crate.data)
    return (
      <div className="flex h-full flex-col">
        <PageHeader title={<CratesCrumb />} />
        <ErrorState
          title="This crate did not load"
          error={crate.error}
          onRetry={() => void crate.refetch()}
          retrying={crate.isFetching}
        />
      </div>
    )
  return <CrateView crate={crate.data} />
}

function CratesCrumb({ name }: { name?: string }) {
  const { workspace } = useWorkspaceSelection()
  return (
    <span className="flex min-w-0 items-baseline gap-1.5">
      <Link
        to="/w/$workspaceId/crates"
        params={{ workspaceId: workspace }}
        className={cn('shrink-0', name ? 'font-normal text-muted hover:text-foreground' : '')}
      >
        Crates
      </Link>
      {name && (
        <>
          <span aria-hidden className="font-normal text-faint">
            /
          </span>
          <span className="min-w-0 truncate" data-crate-title>
            {name}
          </span>
        </>
      )}
    </span>
  )
}

type Panel = 'edit' | 'date' | 'add' | { remove: CrateTask } | null

function CrateView({ crate }: { crate: CrateDetail }) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const canAct = useWorkspaceCapability('create_tasks')
  const [panel, setPanel] = useState<Panel>(null)
  const open = crate.state === 'open'
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['crates', workspace] })
  const setState = useMutation({
    mutationFn: (state: 'open' | 'closed') => updateCrate(workspace, crate.id, { state }),
    onSuccess: refresh,
  })

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader title={<CratesCrumb name={crate.name} />}>
        {canAct && open && (
          <Link
            to="/w/$workspaceId/"
            params={{ workspaceId: workspace }}
            search={{ create: true, crate: crate.id }}
            data-crate-create-task
          >
            <Button size="sm" tabIndex={-1}>
              <Plus />
              New task
            </Button>
          </Link>
        )}
        {canAct && (
          <DropdownMenu label="Crate actions" trigger="overflow">
            {open && (
              <DropdownMenuItem onSelect={() => setPanel('add')}>
                <Plus aria-hidden />
                Add an existing task
              </DropdownMenuItem>
            )}
            <DropdownMenuItem onSelect={() => setPanel('edit')}>
              <Pencil aria-hidden />
              Edit crate
            </DropdownMenuItem>
            {open ? (
              <DropdownMenuItem onSelect={() => setState.mutate('closed')}>
                <Archive aria-hidden />
                Close crate
              </DropdownMenuItem>
            ) : (
              <DropdownMenuItem onSelect={() => setState.mutate('open')}>
                <ArchiveRestore aria-hidden />
                Reopen crate
              </DropdownMenuItem>
            )}
          </DropdownMenu>
        )}
      </PageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto w-full max-w-6xl space-y-5 px-4 py-5 lg:px-6">
          {setState.error != null && (
            <p role="alert" className="rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">
              {setState.error instanceof Error ? setState.error.message : String(setState.error)}
            </p>
          )}
          <CrateHero crate={crate} canAct={canAct} onSetDate={() => setPanel('date')} />
          {/* Requirements sit beside the tasks from lg up, and between the
              tasks and the activity on a narrow screen. */}
          <div className="grid items-start gap-5 lg:grid-cols-[minmax(0,1fr)_20rem]">
            <div className="min-w-0 lg:col-start-1">
              <MemberTasks
                crate={crate}
                canAct={canAct}
                onAdd={() => setPanel('add')}
                onRemove={(task) => setPanel({ remove: task })}
              />
            </div>
            <div className="min-w-0 lg:col-start-2 lg:row-span-2 lg:row-start-1">
              <Requirements crate={crate} />
            </div>
            <div className="min-w-0 lg:col-start-1">
              <Activity crate={crate} />
            </div>
          </div>
        </div>
      </div>
      {(panel === 'edit' || panel === 'date') && (
        <CrateFormDialog
          crate={crate}
          focus={panel === 'date' ? 'date' : 'name'}
          declared={crate.requirements}
          onClose={() => setPanel(null)}
          onSaved={() => setPanel(null)}
        />
      )}
      {panel === 'add' && <AddTaskDialog crate={crate} onClose={() => setPanel(null)} />}
      {panel && typeof panel === 'object' && (
        <RemoveTaskDialog crate={crate} task={panel.remove} onClose={() => setPanel(null)} />
      )}
    </div>
  )
}

function SectionCard({
  title,
  count,
  action,
  children,
  label,
}: {
  title: string
  count?: number
  action?: ReactNode
  children: ReactNode
  label?: string
}) {
  return (
    <section aria-label={label ?? title} className="min-w-0 rounded-lg border border-border bg-card">
      <header className="flex min-h-11 items-center justify-between gap-3 border-b border-border px-4 py-2">
        <h2 className="flex items-center gap-1.5 text-sm font-medium">
          {title}
          {count !== undefined && (
            <span className="rounded-md bg-raised px-1.5 font-mono text-[11px] font-normal text-muted">{count}</span>
          )}
        </h2>
        {action}
      </header>
      {children}
    </section>
  )
}

// Member tasks (req-crates AC-5.3): each row prints the task's next action
// label verbatim from the server's projection (invariant 12; DEC-61) and
// links to the task's detail.
function MemberTasks({
  crate,
  canAct,
  onAdd,
  onRemove,
}: {
  crate: CrateDetail
  canAct: boolean
  onAdd: () => void
  onRemove: (task: CrateTask) => void
}) {
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const open = crate.state === 'open'
  return (
    <SectionCard
      title="Tasks"
      count={crate.tasks.length}
      action={
        canAct && open && crate.tasks.length > 0 ? (
          <Button variant="ghost" size="sm" onClick={onAdd}>
            <Plus />
            Add task
          </Button>
        ) : undefined
      }
    >
      {crate.tasks.length === 0 ? (
        <div className="flex flex-col items-center px-6 py-10 text-center">
          <span className="grid size-9 place-items-center rounded-lg bg-raised text-muted">
            <Package aria-hidden className="size-4" />
          </span>
          <p className="mt-3 text-sm font-medium">No tasks in this crate yet</p>
          <p className="mt-1 max-w-sm text-sm text-muted">
            {open
              ? 'Create a task here, or add one that already exists. The crate counts each task toward its progress.'
              : 'Reopen this crate to add tasks.'}
          </p>
          {canAct && open && (
            <div className="mt-4 flex flex-wrap justify-center gap-2">
              <Link
                to="/w/$workspaceId/"
                params={{ workspaceId: workspace }}
                search={{ create: true, crate: crate.id }}
              >
                <Button size="sm" tabIndex={-1}>
                  <Plus />
                  New task
                </Button>
              </Link>
              <Button size="sm" variant="secondary" onClick={onAdd}>
                Add an existing task
              </Button>
            </div>
          )}
        </div>
      ) : (
        <ul className="divide-y divide-border">
          {crate.tasks.map((task) => (
            <li key={task.id} data-crate-task={task.id} className="flex items-start gap-1 px-2 py-1.5">
              <div className="min-w-0 flex-1">
                <TaskRow
                  task={task}
                  onActivate={(id) =>
                    void navigate({
                      to: '/w/$workspaceId/tasks/$taskId',
                      params: { workspaceId: workspace, taskId: id },
                    })
                  }
                  details={
                    <span className="mt-1.5 flex min-w-0 items-center gap-2 text-[11px] text-faint">
                      <span
                        data-task-state={task.state}
                        className={cn(
                          'shrink-0 rounded-[4px] px-1.5 font-medium leading-[18px]',
                          stateTone(task.state),
                        )}
                      >
                        {taskStateLabels[task.state] ?? task.state}
                      </span>
                      <span className="flex min-w-0 items-center gap-1">
                        <GitBranch aria-hidden className="size-3 shrink-0" />
                        <span className="min-w-0 truncate font-mono" data-task-branch>
                          {task.branch}
                        </span>
                      </span>
                    </span>
                  }
                />
              </div>
              <div className="flex shrink-0 items-center gap-0.5 pt-1">
                {task.pull_request?.url && (
                  <a
                    href={task.pull_request.url}
                    target="_blank"
                    rel="noreferrer"
                    title={`Pull request ${pullRequestLabel(task.pull_request)}`}
                    className="inline-flex h-7 items-center gap-1 rounded-md px-1.5 text-xs text-muted hover:bg-raised hover:text-foreground"
                  >
                    <GitPullRequest aria-hidden className="size-3.5" />
                    <span className="tabular-nums">{pullRequestLabel(task.pull_request)}</span>
                  </a>
                )}
                {canAct && (
                  <Button
                    variant="ghost"
                    size="icon"
                    className="size-7"
                    aria-label={`Remove ${task.title} from this crate`}
                    title="Remove from crate"
                    onClick={() => onRemove(task)}
                  >
                    <X />
                  </Button>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
    </SectionCard>
  )
}

function stateTone(state: CrateTask['state']) {
  if (state === 'merged') return 'bg-positive-soft text-positive'
  if (state === 'awaiting_human') return 'bg-attention-soft text-attention'
  if (state === 'running' || state === 'claiming') return 'bg-primary-soft text-primary'
  return 'bg-raised text-muted'
}

function pullRequestLabel(pull: { number?: number; url?: string }) {
  return pull.number ? `#${pull.number}` : 'Open'
}

// Declared requirements with their task counts and the uncovered mark, and
// apart from them the requirements a member carries that the crate does not
// declare (req-crates AC-5.4; component-crates invariant 11).
function Requirements({ crate }: { crate: CrateDetail }) {
  const uncovered = crate.requirements.filter((item) => item.task_count === 0).length
  return (
    <SectionCard title="Requirements" count={crate.requirements.length}>
      {crate.requirements.length === 0 ? (
        <p className="px-4 py-4 text-sm text-muted">This crate declares no requirements.</p>
      ) : (
        <>
          {uncovered > 0 && (
            <p className="mx-3 mt-3 rounded-md bg-attention-soft px-3 py-2 text-xs leading-5 text-attention">
              {uncovered === 1
                ? '1 declared requirement has no task yet.'
                : `${uncovered} declared requirements have no task yet.`}
            </p>
          )}
          <ul aria-label="Declared requirements" className="p-1.5">
            {crate.requirements.map((item) => (
              <RequirementRow key={item.id} requirement={item} declared />
            ))}
          </ul>
        </>
      )}
      {crate.undeclared_requirements.length > 0 && (
        <div className="border-t border-border">
          <h3 className="px-4 pt-3 text-xs font-medium text-muted">Also carried by tasks</h3>
          <p className="px-4 pt-0.5 text-xs text-faint">
            Tasks in this crate carry these, but the crate does not declare them.
          </p>
          <ul aria-label="Undeclared requirements" className="p-1.5">
            {crate.undeclared_requirements.map((item) => (
              <RequirementRow key={item.id} requirement={item} declared={false} />
            ))}
          </ul>
        </div>
      )}
    </SectionCard>
  )
}

function RequirementRow({ requirement, declared }: { requirement: CrateRequirement; declared: boolean }) {
  const { workspace } = useWorkspaceSelection()
  const uncovered = declared && requirement.task_count === 0
  return (
    <li data-requirement={requirement.id} data-uncovered={uncovered || undefined}>
      <Link
        to="/w/$workspaceId/requirements"
        params={{ workspaceId: workspace }}
        search={{ requirement: requirement.id }}
        className="flex items-start gap-2 rounded-md px-2.5 py-2 hover:bg-raised"
      >
        <span className="min-w-0 flex-1">
          <span className="block truncate text-sm text-foreground">{requirement.title}</span>
          <span className="block truncate font-mono text-[11px] text-faint">{requirement.id}</span>
        </span>
        {uncovered ? (
          <span
            data-uncovered-mark
            className="mt-0.5 inline-flex shrink-0 items-center gap-1 rounded-[5px] bg-attention-soft px-1.5 text-xs font-medium leading-[18px] text-attention"
          >
            <CircleSlash aria-hidden className="size-3" />
            Uncovered
          </span>
        ) : (
          <span className="mt-0.5 shrink-0 rounded-[5px] bg-raised px-1.5 text-xs leading-[18px] tabular-nums text-muted">
            {requirement.task_count} {requirement.task_count === 1 ? 'task' : 'tasks'}
          </span>
        )}
      </Link>
    </li>
  )
}

// Recorded events of member tasks, newest first (req-crates AC-5.5;
// component-crates invariant 13). A pull request entry links to it.
function Activity({ crate }: { crate: CrateDetail }) {
  const { workspace } = useWorkspaceSelection()
  const activity = useInfiniteQuery({
    queryKey: crateActivityQueryKey(workspace, crate.id),
    queryFn: ({ pageParam }) => fetchCrateActivity(workspace, crate.id, pageParam),
    initialPageParam: '',
    getNextPageParam: (page) => page.next_cursor || undefined,
    enabled: Boolean(workspace),
  })
  const events = activity.data?.pages.flatMap((page) => page.events) ?? []
  return (
    <SectionCard title="Activity">
      {activity.isPending && (
        <div className="space-y-3 px-4 py-4">
          <Skeleton className="h-4 w-2/3" />
          <Skeleton className="h-4 w-1/2" />
        </div>
      )}
      {activity.error != null && (
        <p className="px-4 py-4 text-sm text-failure">
          {activity.error instanceof Error ? activity.error.message : 'Activity did not load.'}
        </p>
      )}
      {activity.data && events.length === 0 && (
        <p className="px-4 py-4 text-sm text-muted">Nothing has happened in this crate yet.</p>
      )}
      {events.length > 0 && (
        <ol className="relative px-4 py-3">
          {events.map((event, index) => (
            <ActivityEntry key={event.id} event={event} last={index === events.length - 1} />
          ))}
        </ol>
      )}
      {activity.hasNextPage && (
        <div className="border-t border-border px-4 py-2">
          <Button
            variant="ghost"
            size="sm"
            disabled={activity.isFetchingNextPage}
            onClick={() => void activity.fetchNextPage()}
          >
            {activity.isFetchingNextPage ? 'Loading…' : 'Show older'}
          </Button>
        </div>
      )}
    </SectionCard>
  )
}

const activityMeta: Record<string, { Icon: typeof Plus; tone: string }> = {
  'task.created': { Icon: CircleDot, tone: 'text-muted bg-raised' },
  'task.crate_set': { Icon: Package, tone: 'text-primary bg-primary-soft' },
  'pull_request.opened': { Icon: GitPullRequest, tone: 'text-primary bg-primary-soft' },
  'review.round_completed': { Icon: MessageSquareText, tone: 'text-muted bg-raised' },
  'merge.confirmed': { Icon: GitMerge, tone: 'text-positive bg-positive-soft' },
  'merge.reconciled': { Icon: GitMerge, tone: 'text-positive bg-positive-soft' },
  'task.cancelled': { Icon: Undo2, tone: 'text-faint bg-raised' },
}

function activityText(event: CrateActivityEvent): ReactNode {
  switch (event.kind) {
    case 'task.created':
      return 'Task created'
    case 'task.crate_set':
      return 'Joined this crate'
    case 'pull_request.opened':
      return `Pull request ${event.pull_request?.number ? `#${event.pull_request.number} ` : ''}opened`
    case 'review.round_completed':
      return event.verdict ? `Review finished: ${verdictText(event.verdict)}` : 'Review finished'
    case 'merge.confirmed':
    case 'merge.reconciled':
      return 'Merged'
    case 'task.cancelled':
      return 'Task cancelled'
    default:
      return event.kind
  }
}

function verdictText(verdict: string) {
  if (verdict === 'approve') return 'approved'
  if (verdict === 'request_changes') return 'changes requested'
  return verdict.replaceAll('_', ' ')
}

function ActivityEntry({ event, last }: { event: CrateActivityEvent; last: boolean }) {
  const { workspace } = useWorkspaceSelection()
  const meta = activityMeta[event.kind] ?? activityMeta['task.created']
  const Icon = meta.Icon
  return (
    <li data-activity-kind={event.kind} className="relative flex gap-3 pb-4 last:pb-1">
      {!last && <span aria-hidden className="absolute top-7 bottom-0 left-[13px] w-px bg-border" />}
      <span className={cn('relative grid size-7 shrink-0 place-items-center rounded-full', meta.tone)}>
        <Icon aria-hidden className="size-3.5" />
      </span>
      <div className="min-w-0 flex-1 pt-0.5">
        <p className="flex flex-wrap items-baseline gap-x-2 text-sm">
          {event.pull_request?.url ? (
            <a
              href={event.pull_request.url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 font-medium hover:text-primary hover:underline"
            >
              {activityText(event)}
              <ExternalLink aria-hidden className="size-3 text-faint" />
            </a>
          ) : (
            <span className="font-medium">{activityText(event)}</span>
          )}
          <time dateTime={event.at} title={absoluteTime(event.at)} className="text-xs text-faint">
            {relativeTime(event.at)}
          </time>
        </p>
        <Link
          to="/w/$workspaceId/tasks/$taskId"
          params={{ workspaceId: workspace, taskId: event.task_id }}
          className="mt-0.5 block truncate text-xs text-muted hover:text-foreground hover:underline"
        >
          {event.task_title || event.task_id}
        </Link>
      </div>
    </li>
  )
}

// Adds an existing task by ID. A task in another crate leaves it when it
// joins this one (req-crates AC-2.2).
function AddTaskDialog({ crate, onClose }: { crate: CrateDetail; onClose: () => void }) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const [taskId, setTaskId] = useState('')
  const candidates = useQuery({
    queryKey: ['tasks', workspace, 'crate-candidates'],
    queryFn: () => fetchOpenTasks(workspace),
    enabled: Boolean(workspace),
    staleTime: 30_000,
  })
  const members = new Set(crate.tasks.map((task) => task.id))
  const query = taskId.trim().toLowerCase()
  const matches = (candidates.data ?? [])
    .filter((task) => !members.has(task.id))
    .filter((task) => !query || task.id.toLowerCase().includes(query) || task.title.toLowerCase().includes(query))
    .slice(0, 6)
  const chosen = (candidates.data ?? []).find((task) => task.id === taskId.trim())
  const add = useMutation({
    mutationFn: () => addTaskToCrate(workspace, taskId.trim(), crate.id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['crates', workspace] })
      void queryClient.invalidateQueries({ queryKey: ['task', workspace, taskId.trim()] })
      onClose()
    },
  })
  return (
    <Dialog onClose={onClose} label="Add a task to this crate">
      <form
        onSubmit={(event) => {
          event.preventDefault()
          if (taskId.trim() && !add.isPending) add.mutate()
        }}
      >
        <header className="flex items-start justify-between gap-3 border-b border-border px-5 py-3.5">
          <div className="min-w-0">
            <h2 className="text-base font-semibold tracking-tight">Add a task</h2>
            <p className="mt-0.5 text-xs text-muted">
              The task joins {crate.name}. Its state, plan, and context stay as they are.
            </p>
          </div>
          <Button type="button" variant="ghost" size="icon" aria-label="Close" onClick={onClose}>
            <X />
          </Button>
        </header>
        <div className="px-5 py-4">
          <div>
            <span className="mb-1 block text-xs font-medium text-muted">Task ID</span>
            <span className="relative block">
              <Search
                aria-hidden
                className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-faint"
              />
              <Input
                aria-label="Task ID"
                autoFocus
                value={taskId}
                placeholder="Task ID, or search open tasks by title"
                onChange={(event) => setTaskId(event.target.value)}
                className="pl-8 font-mono"
              />
            </span>
          </div>
          <ul aria-label="Matching open tasks" className="mt-2 max-h-60 space-y-0.5 overflow-y-auto">
            {matches.map((task) => (
              <li key={task.id}>
                <button
                  type="button"
                  aria-pressed={task.id === taskId.trim()}
                  onClick={() => setTaskId(task.id)}
                  className={cn(
                    'flex w-full items-start gap-2 rounded-md px-2 py-1.5 text-left hover:bg-raised',
                    task.id === taskId.trim() && 'bg-primary-soft hover:bg-primary-soft',
                  )}
                >
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-sm">{task.title}</span>
                    <span className="block truncate text-[11px] text-faint">
                      <span className="font-mono">{task.id}</span>
                      {task.crate && <> · in {task.crate.name}</>}
                    </span>
                  </span>
                </button>
              </li>
            ))}
            {candidates.data && matches.length === 0 && (
              <li className="px-2 py-1.5 text-xs text-faint">
                No open task matches. You can still add a task by its ID.
              </li>
            )}
          </ul>
          {chosen?.crate && chosen.crate.id !== crate.id && (
            <p className="mt-3 rounded-md bg-attention-soft px-3 py-2 text-xs text-attention">
              This task is in {chosen.crate.name}. Adding it here takes it out of that crate.
            </p>
          )}
          {add.error != null && (
            <p role="alert" className="mt-3 rounded-md bg-failure-soft px-3 py-2 text-xs text-failure">
              {add.error instanceof Error ? add.error.message : String(add.error)}
            </p>
          )}
        </div>
        <footer className="flex items-center justify-end gap-2 border-t border-border px-5 py-3">
          <Button type="button" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={!taskId.trim() || add.isPending}>
            {add.isPending ? 'Adding…' : 'Add to crate'}
          </Button>
        </footer>
      </form>
    </Dialog>
  )
}

function RemoveTaskDialog({ crate, task, onClose }: { crate: CrateDetail; task: CrateTask; onClose: () => void }) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const remove = useMutation({
    mutationFn: () => removeTaskFromCrate(workspace, task.id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['crates', workspace] })
      void queryClient.invalidateQueries({ queryKey: ['task', workspace, task.id] })
      onClose()
    },
  })
  return (
    <Dialog onClose={onClose} label="Remove a task from this crate">
      <div className="px-5 py-4">
        <h2 className="text-base font-semibold tracking-tight">Remove this task from {crate.name}?</h2>
        <p className="mt-2 text-sm text-muted">
          <span className="font-medium text-foreground">{task.title}</span> leaves the crate. Its state, plan, and
          context stay as they are, and you can add it back later.
        </p>
        {remove.error != null && (
          <p role="alert" className="mt-3 rounded-md bg-failure-soft px-3 py-2 text-xs text-failure">
            {remove.error instanceof Error ? remove.error.message : String(remove.error)}
          </p>
        )}
      </div>
      <footer className="flex items-center justify-end gap-2 border-t border-border px-5 py-3">
        <Button variant="ghost" onClick={onClose}>
          Cancel
        </Button>
        <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
          {remove.isPending ? 'Removing…' : 'Remove from crate'}
        </Button>
      </footer>
    </Dialog>
  )
}

function CratePageSkeleton() {
  return (
    <div className="flex h-full flex-col">
      <PageHeader title={<CratesCrumb />} />
      <div className="mx-auto w-full max-w-6xl space-y-5 px-4 py-5 lg:px-6">
        <Skeleton className="h-36 w-full" />
        <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_20rem]">
          <Skeleton className="h-72" />
          <Skeleton className="h-48" />
        </div>
      </div>
    </div>
  )
}
