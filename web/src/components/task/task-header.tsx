import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import {
  Check,
  ChevronDown,
  ChevronUp,
  CircleDot,
  ExternalLink,
  GitBranch,
  GitPullRequest,
  Link2,
  Link2Off,
  RotateCcw,
  Terminal,
  Trash2,
  UserRound,
} from 'lucide-react'
import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import {
  dependencyRelationLabel,
  failedTriage,
  pullRequestURL,
  restartPullRequestOutcome,
  taskCreator,
  taskRestartLinks,
} from '../../lib/activity'
import { addTaskDependency, cancelTask, fetchTasks, removeTaskDependency, setTaskAssignee } from '../../lib/api'
import { findBlueprint } from '../../lib/blueprint'
import { taskStateLabels } from '../../lib/contracts'
import { errorMessage } from '../../lib/errors'
import { relatedTaskRoute } from '../../lib/task-route'
import type { ActivityItem } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { useBlueprints, useWorkspaceCapability, useWorkspaceMembers, useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'
import { Dialog } from '../ui/dialog'
import { Disclosure } from '../ui/disclosure'
import { DropdownMenu, DropdownMenuItem } from '../ui/dropdown-menu'
import { Input, Textarea } from '../ui/input'
import { MarkdownProse } from '../ui/markdown-prose'
import { AssigneeChip } from './assignee-chip'
import { SectionHeading } from './section-heading'
import { TaskRestartControl, TaskRestartNotice } from './task-restart-dialog'

// The task-header facts: state badges,
// the facts a reviewer actually references — where the work lives, where it
// came from, where to read it — and the task-run command
// (component-web-task-surfaces). Anything the specification card or the timeline already states is
// deliberately absent: the header introduces the task, it does not summarize
// the whole page.
export function TaskHeader({ item, variant }: { item: ActivityItem; variant: 'sheet' | 'full' }) {
  const members = useWorkspaceMembers()
  const creator = taskCreator(item.summaries?.creator, members.data)
  const restartLinks = taskRestartLinks(item.task)
  const closeOutcome = restartPullRequestOutcome(item.task)
  const canOperate = useWorkspaceCapability('operate_gates')
  const prURL = pullRequestURL(item.summaries)
  const Heading = variant === 'full' ? 'h1' : 'h2'
  const relatedRoute = relatedTaskRoute(variant)
  // The parent is a blueprint anchor, which no longer rides the activity feed;
  // the blueprint projection is where its title now lives.
  const { data: blueprints } = useBlueprints()
  const parent = findBlueprint(blueprints, item.task.parent_task_id ?? '')?.task
  const blockingIDs = new Set(item.task.blocking_task_ids ?? [])
  const unsatisfiableIDs = new Set(item.stalled?.unsatisfiable_edge ? (item.stalled.blocking_task_ids ?? []) : [])
  const triageFailure = failedTriage(item)
  const stateLabel = triageFailure
    ? 'Triage failed'
    : item.stalled?.needed
      ? 'Stalled'
      : (taskStateLabels[item.task.state] ?? item.task.state)
  const mergedChildren = item.task.children?.filter((child) => child.state === 'merged').length ?? 0
  const closedChildren = item.task.children?.filter((child) => child.state === 'closed').length ?? 0
  const openChildren = (item.task.children?.length ?? 0) - mergedChildren - closedChildren
  const childRollup = [
    mergedChildren > 0 ? `${mergedChildren} merged` : '',
    closedChildren > 0 ? `${closedChildren} closed` : '',
    openChildren > 0 ? `${openChildren} open` : '',
  ]
    .filter(Boolean)
    .join(' · ')

  return (
    <div>
      <div className="mb-2 flex flex-wrap items-center gap-1.5">
        {/* Approved reads as good news even while it waits at the gate —
            amber stays reserved for states that are genuinely stuck. */}
        <Disclosure
          label={`Task status: ${stateLabel}`}
          triggerClassName="rounded-md focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
          content={triageFailure?.reason ?? item.stalled?.reason ?? `Current task status: ${stateLabel}.`}
          contentClassName="pointer-events-none bottom-full top-auto left-0 z-10 mb-1.5 w-56 border-0 bg-foreground px-2.5 py-1.5 text-[11px] leading-4 text-background shadow-md transition-opacity after:absolute after:left-3 after:top-full after:border-4 after:border-transparent after:border-t-foreground"
        >
          <Badge
            variant={
              item.task.state === 'approved' || item.task.state === 'merged'
                ? 'positive'
                : item.needs_attention
                  ? 'attention'
                  : 'outline'
            }
          >
            {stateLabel}
          </Badge>
        </Disclosure>
        {restartLinks.to && (
          <Link to={relatedRoute} params={{ taskId: restartLinks.to }} className="text-sm text-primary underline">
            Started over as {restartLinks.to}
          </Link>
        )}
        {unsatisfiableIDs.size > 0 ? (
          <Badge variant="attention">Dependency needs attention</Badge>
        ) : (
          blockingIDs.size > 0 && <Badge variant="mono">Waiting on dependencies</Badge>
        )}
        {item.task.class && <Badge>{sentenceCase(item.task.class)}</Badge>}
        <span className="min-w-0 font-mono text-[11px] text-faint [overflow-wrap:anywhere]">{item.task.id}</span>
        {/* Controls sit apart from the status chips: a destructive action
            never belongs in the row the eye reads for state. */}
        {canOperate && <TaskActions key={item.task.id} item={item} variant={variant} />}
      </div>
      <TaskRestartNotice taskId={item.task.id} />
      {restartLinks.from && (
        <p className="my-2 text-sm text-muted">
          Restarted from{' '}
          <Link to={relatedRoute} params={{ taskId: restartLinks.from }} className="text-primary underline">
            {restartLinks.from}
          </Link>
        </p>
      )}
      {closeOutcome && <p className="my-2 text-sm text-muted">{closeOutcome}</p>}
      {item.task.intake_operator_direction?.trim() && (
        <section aria-label="Operator direction" className="my-3 rounded-md border border-border p-3 text-sm">
          <p className="font-medium">Operator direction</p>
          <p className="mt-1 whitespace-pre-wrap">{item.task.intake_operator_direction}</p>
        </section>
      )}
      <Heading className="text-xl font-semibold leading-snug tracking-tight text-balance">{item.task.title}</Heading>
      {item.task.body && <TaskBody body={item.task.body} />}

      <dl
        className={cn(
          'mt-5 grid grid-cols-[minmax(6.5rem,max-content)_minmax(0,1fr)] items-center gap-x-6 gap-y-2 border-y border-border py-3.5 text-[13px] leading-5',
          variant === 'full' &&
            'lg:grid-cols-[minmax(6.5rem,max-content)_minmax(0,1fr)_minmax(6.5rem,max-content)_minmax(0,1fr)] lg:gap-x-10',
        )}
      >
        <Fact label="Repository" value={item.task.repo} />
        <Fact
          label="Branch"
          value={
            <span className="inline-flex max-w-full items-center gap-1.5 font-mono text-xs">
              <GitBranch aria-hidden className="size-3 shrink-0 text-faint" />
              <span className="truncate">{item.task.branch}</span>
              <span className="shrink-0 text-faint">← {item.task.base_branch}</span>
            </span>
          }
        />
        {prURL && (
          <Fact
            label="Pull request"
            value={
              <a
                href={prURL}
                target="_blank"
                rel="noreferrer"
                className="inline-flex max-w-full items-center gap-1.5 hover:text-primary hover:underline"
              >
                <GitPullRequest aria-hidden className="size-3 shrink-0 text-faint" />
                <span className="truncate">{forgeReference(prURL)}</span>
                <ExternalLink aria-hidden className="size-3 shrink-0 text-faint" />
              </a>
            }
          />
        )}
        {item.task.github && (
          <Fact
            label="Issue"
            value={
              item.task.github.issue_url ? (
                <a
                  href={item.task.github.issue_url}
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex max-w-full items-center gap-1.5 hover:text-primary hover:underline"
                >
                  <CircleDot aria-hidden className="size-3 shrink-0 text-faint" />
                  <span className="truncate">
                    {item.task.github.repository}#{item.task.github.issue_number}
                  </span>
                  <ExternalLink aria-hidden className="size-3 shrink-0 text-faint" />
                </a>
              ) : (
                `${item.task.github.state} · plan v${item.task.github.spec_version}`
              )
            }
          />
        )}
        {(item.task.dependencies?.length ?? 0) > 0 && (
          <Fact
            label="Depends on"
            value={
              <span className="flex min-w-0 flex-col gap-1">
                {item.task.dependencies!.map((dependency) => {
                  const relation = dependencyRelationLabel(
                    dependency.state,
                    blockingIDs.has(dependency.id),
                    unsatisfiableIDs.has(dependency.id),
                  )
                  return (
                    // The title truncates on its own line; the relation is a
                    // separate chip so it is never clipped with the title.
                    <span key={dependency.id} className="flex min-w-0 items-center gap-2">
                      <Link
                        to={relatedRoute}
                        params={{ taskId: dependency.id }}
                        aria-label={`${dependency.title || dependency.id} · ${relation}`}
                        className="min-w-0 truncate hover:text-primary hover:underline"
                      >
                        {dependency.title || dependency.id}
                      </Link>
                      <Badge
                        variant={
                          unsatisfiableIDs.has(dependency.id)
                            ? 'attention'
                            : dependency.state === 'merged'
                              ? 'positive'
                              : 'default'
                        }
                        className="shrink-0"
                      >
                        {relation}
                      </Badge>
                    </span>
                  )
                })}
              </span>
            }
          />
        )}
        {/* A labelled fact answers "who holds this?", so it says Unassigned
            rather than going blank — unlike the list rows and board cards,
            where absence is the answer. */}
        <Fact
          label="Assignee"
          value={
            item.task.assignee ? (
              <AssigneeChip assignee={item.task.assignee} />
            ) : (
              <span className="text-muted">Unassigned</span>
            )
          }
        />
        <Fact
          label="Created"
          startRow
          value={
            <span className="inline-flex min-w-0 max-w-full items-baseline gap-1.5">
              <span className="shrink-0">{absoluteTime(item.task.created_at)}</span>
              <span className="min-w-0 truncate text-muted" data-task-creator>
                by <span title={creator.detail}>{creator.label}</span>
                {/* left-0 pins the absolutely positioned text inside the row so a
                    long identity never widens the page. */}
                <span className="sr-only left-0"> ({creator.detail})</span>
              </span>
            </span>
          }
        />
        {item.task.parent_task_id && (
          <Fact
            label="Parent blueprint"
            value={
              // The parent is an intent artifact, not work, so this reference
              // leaves the task routes for the blueprint's canonical home,
              // unlike dependencies above, which are tasks.
              <span className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
                <Link
                  to="/blueprints/$taskId"
                  params={{ taskId: item.task.parent_task_id }}
                  className="text-primary hover:underline"
                >
                  {parent?.title ?? item.task.parent_task_id}
                  {item.task.origin_spec_version ? ` · plan v${item.task.origin_spec_version}` : ''}
                </Link>
                {/* Blueprint history left the sidebar with the rest of the
                    parked presentation (AC-4.1), so the records reach through
                    the tasks that came from them instead. */}
                <Link to="/blueprints" className="text-faint hover:text-primary hover:underline">
                  Blueprint history
                </Link>
              </span>
            }
          />
        )}
      </dl>

      {canOperate && unsatisfiableIDs.size > 0 && (
        <UnlinkDependencyControl item={item} dependencyIDs={[...unsatisfiableIDs]} />
      )}

      {(item.task.children?.length ?? 0) > 0 && (
        <section className="mt-5 rounded-md border border-border">
          <div className="flex items-baseline justify-between gap-3 border-b border-border px-3 py-2">
            <SectionHeading count={item.task.children!.length}>Blueprint tasks</SectionHeading>
            <span className="text-[11px] text-faint">{childRollup}</span>
          </div>
          <ul className="space-y-1.5 px-3 py-2.5">
            {item.task.children!.map((child) => (
              <li key={child.id} className="flex items-center gap-2 text-xs">
                <Badge variant="mono">{child.origin_sub_id}</Badge>
                <Link
                  to={relatedRoute}
                  params={{ taskId: child.id }}
                  className="min-w-0 flex-1 truncate text-primary hover:underline"
                >
                  {child.title}
                </Link>
                <span className="text-faint">{taskStateLabels[child.state] ?? child.state}</span>
              </li>
            ))}
          </ul>
        </section>
      )}

      <div className="mt-4">
        <Checkout item={item} />
      </div>
    </div>
  )
}

function TaskActions({ item, variant }: { item: ActivityItem; variant: 'sheet' | 'full' }) {
  const [action, setAction] = useState<'dependency' | 'assign' | 'cancel' | 'restart' | null>(null)
  const canAssign = useWorkspaceCapability('set_assignee')
  const { workspace } = useWorkspaceSelection()
  const close = () => setAction(null)
  if (item.task.state === 'merged' || item.task.state === 'closed') return null
  return (
    <div className="ml-auto flex items-center gap-0.5">
      {/* The operations live behind the overflow menu so the header stays
          quieter than the title. Each confirms before acting; the
          confirmation returns focus to whichever control opened it. A task
          carries no hold, so there is no hold action (DEC-74(3)). */}
      <DropdownMenu label="Task actions" trigger="overflow">
        <DropdownMenuItem onSelect={() => setAction('dependency')}>
          <Link2 /> Link dependency
        </DropdownMenuItem>
        {canAssign && Boolean(workspace) && (
          <DropdownMenuItem onSelect={() => setAction('assign')}>
            <UserRound /> {item.task.assignee ? 'Reassign' : 'Assign'}
          </DropdownMenuItem>
        )}
        <hr className="my-1 border-border" />
        <DropdownMenuItem destructive onSelect={() => setAction('cancel')}>
          <Trash2 /> Cancel task
        </DropdownMenuItem>
        <DropdownMenuItem destructive onSelect={() => setAction('restart')}>
          <RotateCcw /> Start over
        </DropdownMenuItem>
      </DropdownMenu>
      {action === 'dependency' && <LinkDependencyControl item={item} onClose={close} />}
      {action === 'assign' && <AssigneeControl item={item} onClose={close} />}
      {action === 'cancel' && <CancelControl item={item} onClose={close} />}
      {action === 'restart' && <TaskRestartControl item={item} variant={variant} onClose={close} />}
    </div>
  )
}

function LinkDependencyControl({ item, onClose }: { item: ActivityItem; onClose: () => void }) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const [search, setSearch] = useState('')
  const [selectedID, setSelectedID] = useState('')
  const [reason, setReason] = useState('')
  const requestID = useRef(crypto.randomUUID())
  const tasks = useQuery({
    queryKey: ['tasks', workspace, 'dependency-candidates'],
    queryFn: fetchTasks,
    enabled: Boolean(workspace),
  })
  const existing = new Set(item.task.dependencies?.map((dependency) => dependency.id) ?? [])
  const query = search.trim().toLowerCase()
  const candidates = (tasks.data ?? []).filter(
    (task) =>
      task.id !== item.task.id &&
      !existing.has(task.id) &&
      task.state !== 'merged' &&
      task.state !== 'closed' &&
      (!query || task.id.toLowerCase().includes(query) || task.title.toLowerCase().includes(query)),
  )
  const selected =
    candidates.find((task) => task.id === selectedID) ?? tasks.data?.find((task) => task.id === selectedID)
  const mutation = useMutation({
    mutationFn: () => addTaskDependency(item.task.id, selectedID, reason.trim(), requestID.current),
    onSuccess: () => {
      onClose()
      setSearch('')
      setSelectedID('')
      setReason('')
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.workspace, item.task.id] })
    },
  })
  const closeDialog = () => {
    mutation.reset()
    onClose()
    setSearch('')
    setSelectedID('')
    setReason('')
  }
  if (item.task.state === 'merged' || item.task.state === 'closed') return null
  return (
    <>
      <Dialog label="Link dependency" onClose={() => !mutation.isPending && closeDialog()}>
        <div className="border-b border-border px-5 py-4">
          <h2 className="font-semibold">What should this task depend on?</h2>
          <p className="mt-1 text-sm leading-6 text-muted">
            Choose an open task. This task will wait for it to merge before future implementation work can start.
          </p>
        </div>
        <div className="space-y-4 px-5 py-4">
          <label htmlFor="link-dependency-search" className="block text-sm font-medium">
            Search open tasks
            <Input
              id="link-dependency-search"
              autoFocus
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              placeholder="Task title or ID"
              className="mt-1.5"
            />
          </label>
          <div className="max-h-48 overflow-y-auto rounded-md border border-border">
            {tasks.isPending && <p className="p-3 text-sm text-muted">Loading open tasks…</p>}
            {tasks.error != null && <p className="p-3 text-sm text-failure">Could not load open tasks.</p>}
            {!tasks.isPending && tasks.error == null && candidates.length === 0 && (
              <p className="p-3 text-sm text-muted">No matching open tasks.</p>
            )}
            {candidates.map((task) => (
              <button
                key={task.id}
                type="button"
                aria-pressed={selectedID === task.id}
                onClick={() => setSelectedID(task.id)}
                className={cn(
                  'block w-full border-b border-border px-3 py-2 text-left last:border-0 hover:bg-surface',
                  selectedID === task.id && 'bg-primary-soft',
                )}
              >
                <span className="block text-sm font-medium">{task.title || task.id}</span>
                <span className="block font-mono text-[11px] text-faint">{task.id}</span>
              </button>
            ))}
          </div>
          {selected && <p className="text-xs text-muted">This task will depend on {selected.title || selected.id}.</p>}
          <label htmlFor="link-dependency-reason" className="block text-sm font-medium">
            Reason
            <Textarea
              id="link-dependency-reason"
              value={reason}
              maxLength={200}
              onChange={(event) => setReason(event.target.value)}
              placeholder="Why should this task wait for the selected task?"
              className="mt-1.5"
            />
          </label>
          {mutation.error != null && <p className="text-sm text-failure">{errorMessage(mutation.error)}</p>}
          <div className="flex justify-end gap-2">
            <Button variant="secondary" onClick={closeDialog} disabled={mutation.isPending}>
              Cancel
            </Button>
            <Button onClick={() => mutation.mutate()} disabled={mutation.isPending || !selectedID || !reason.trim()}>
              {mutation.isPending ? 'Linking…' : 'Add dependency'}
            </Button>
          </div>
        </div>
      </Dialog>
    </>
  )
}

function UnlinkDependencyControl({ item, dependencyIDs }: { item: ActivityItem; dependencyIDs: string[] }) {
  const queryClient = useQueryClient()
  const [selectedID, setSelectedID] = useState('')
  const [reason, setReason] = useState('')
  const requestID = useRef(crypto.randomUUID())
  const selected = item.task.dependencies?.find((dependency) => dependency.id === selectedID)
  const mutation = useMutation({
    mutationFn: () => removeTaskDependency(item.task.id, selectedID, reason.trim(), requestID.current),
    onSuccess: () => {
      setSelectedID('')
      setReason('')
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.workspace, item.task.id] })
    },
  })
  const openDialog = (id: string) => {
    requestID.current = crypto.randomUUID()
    mutation.reset()
    setReason('')
    setSelectedID(id)
  }
  const closeDialog = () => {
    mutation.reset()
    setReason('')
    setSelectedID('')
  }
  return (
    <section
      className="mt-4 rounded-md border border-attention/40 bg-attention-soft p-3"
      aria-label="Dependency needs attention"
    >
      <p className="text-sm font-medium text-attention">A dependency closed without merging</p>
      <p className="mt-1 text-xs leading-5 text-muted">
        Remove the dependency with an audit reason so this task can continue, or cancel the task above.
      </p>
      <div className="mt-2 flex flex-wrap gap-2">
        {dependencyIDs.map((id) => {
          const dependency = item.task.dependencies?.find((entry) => entry.id === id)
          return (
            <Button key={id} variant="secondary" size="sm" onClick={() => openDialog(id)}>
              <Link2Off /> Unlink dependency {dependency?.title ?? id}
            </Button>
          )
        })}
      </div>
      {selectedID && (
        <Dialog label="Unlink dependency" onClose={() => !mutation.isPending && closeDialog()}>
          <div className="border-b border-border px-5 py-4">
            <h2 className="font-semibold">Remove this dependency?</h2>
            <p className="mt-1 text-sm leading-6 text-muted">
              {selected?.title ?? selectedID} closed without merging. Removing it is audited and may let this task
              continue.
            </p>
          </div>
          <div className="space-y-4 px-5 py-4">
            <label htmlFor="unlink-dependency-reason" className="block text-sm font-medium">
              Reason
              <Textarea
                id="unlink-dependency-reason"
                autoFocus
                value={reason}
                maxLength={200}
                onChange={(event) => setReason(event.target.value)}
                placeholder="Why should this dependency be removed?"
                className="mt-1.5"
              />
            </label>
            {mutation.error != null && <p className="text-sm text-failure">{String(mutation.error)}</p>}
            <div className="flex justify-end gap-2">
              <Button variant="secondary" onClick={closeDialog} disabled={mutation.isPending}>
                Keep dependency
              </Button>
              <Button onClick={() => mutation.mutate()} disabled={mutation.isPending || !reason.trim()}>
                {mutation.isPending ? 'Removing…' : 'Remove dependency'}
              </Button>
            </div>
          </div>
        </Dialog>
      )}
    </section>
  )
}

// Cancel is lifecycle, not an execution affordance, so the blueprint detail
// keeps it while suppressing checkout and branch. Its
// consequences for children are whatever the backend does today.
export function CancelControl({ item, onClose }: { item: ActivityItem; onClose?: () => void }) {
  const queryClient = useQueryClient()
  const [open, setLocalOpen] = useState(Boolean(onClose))
  const setOpen = (value: boolean) => {
    setLocalOpen(value)
    if (!value) onClose?.()
  }
  const [reason, setReason] = useState('')
  const mutation = useMutation({
    mutationFn: () => cancelTask(item.task.id, reason.trim()),
    onSuccess: () => {
      setOpen(false)
      setReason('')
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void queryClient.invalidateQueries({ queryKey: ['task'] })
    },
  })
  const terminal = item.task.state === 'merged' || item.task.state === 'closed'
  if (terminal) return null
  return (
    <>
      {!onClose && (
        <button
          type="button"
          onClick={() => setOpen(true)}
          className="inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs font-medium leading-4 text-muted transition-colors hover:bg-failure-soft hover:text-failure [&_svg]:size-3.5"
        >
          <Trash2 /> Cancel task
        </button>
      )}
      {open && (
        <Dialog label="Cancel task" onClose={() => !mutation.isPending && setOpen(false)}>
          <div className="border-b border-border px-5 py-4">
            <h2 className="font-semibold">Cancel this task?</h2>
            <p className="mt-1 text-sm leading-6 text-muted">
              This closes the pipeline and cancels active work orders. It does not delete the branch, worktree, issue,
              or pull request.
            </p>
          </div>
          <div className="space-y-4 px-5 py-4">
            <label htmlFor="cancel-task-reason" className="block text-sm font-medium">
              Reason
              <Textarea
                id="cancel-task-reason"
                autoFocus
                value={reason}
                maxLength={64}
                onChange={(event) => setReason(event.target.value)}
                placeholder="Why is this task being cancelled?"
                className="mt-1.5"
              />
            </label>
            {mutation.error != null && <p className="text-sm text-failure">{String(mutation.error)}</p>}
            <div className="flex justify-end gap-2">
              <Button variant="secondary" onClick={() => setOpen(false)} disabled={mutation.isPending}>
                Keep task
              </Button>
              <Button
                variant="destructive"
                onClick={() => mutation.mutate()}
                disabled={mutation.isPending || !reason.trim()}
              >
                {mutation.isPending ? 'Cancelling…' : 'Cancel task'}
              </Button>
            </div>
          </div>
        </Dialog>
      )}
    </>
  )
}

function TaskBody({ body }: { body: string }) {
  const [expanded, setExpanded] = useState(false)
  const [hasOverflow, setHasOverflow] = useState(false)
  const viewportRef = useRef<HTMLDivElement>(null)
  const viewportID = useId()

  useLayoutEffect(() => {
    const viewport = viewportRef.current
    if (!viewport || expanded) return
    const measure = () => setHasOverflow(viewport.scrollHeight > viewport.clientHeight + 1)
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(viewport)
    if (viewport.firstElementChild) observer.observe(viewport.firstElementChild)
    return () => observer.disconnect()
  }, [body, expanded])

  return (
    <div className="mt-2 max-w-3xl">
      <div className="relative">
        <div ref={viewportRef} id={viewportID} className={cn(!expanded && 'max-h-32 overflow-hidden')}>
          <MarkdownProse className="markdown-demoted text-sm">{body}</MarkdownProse>
        </div>
        {hasOverflow && !expanded && (
          <div
            aria-hidden="true"
            className="spec-overflow-shadow pointer-events-none absolute inset-x-0 bottom-0 h-16"
            data-task-body-overflow-shadow
          />
        )}
      </div>
      {hasOverflow && (
        <button
          type="button"
          aria-controls={viewportID}
          aria-expanded={expanded}
          onClick={() => setExpanded((value) => !value)}
          className="mt-1 inline-flex items-center gap-1 rounded text-xs font-medium text-muted hover:text-foreground focus-visible:outline-2 focus-visible:outline-primary"
        >
          {expanded ? (
            <ChevronUp aria-hidden="true" className="size-3.5" />
          ) : (
            <ChevronDown aria-hidden="true" className="size-3.5" />
          )}
          {expanded ? 'Show less description' : 'Show full description'}
        </button>
      )}
    </div>
  )
}

/**
 * Set, reassign, or clear the task's assignee from the workspace's own member
 * list (req-task-lifecycle-and-queue AC-2.4, AC-2.5). Assigning constrains who
 * may claim the task's work orders; it never touches queue order
 * (req-task-lifecycle-and-queue AC-2.3, AC-2.7; DEC-74(4)).
 *
 * Assignment is an operator act, carried by the `set_assignee` capability. The
 * caller's role for this workspace comes from the server's own self-identity
 * read rather than being guessed in the browser, so a member simply has no
 * control. Every member still reads the assignee wherever it is rendered — the
 * members list itself is a co-member read, never a user directory (AC-3.2).
 */
function AssigneeControl({ item, onClose }: { item: ActivityItem; onClose: () => void }) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const ref = useRef<HTMLDivElement>(null)
  const terminal = item.task.state === 'merged' || item.task.state === 'closed'
  const enabled = Boolean(workspace) && !terminal
  const canAssign = useWorkspaceCapability('set_assignee')
  const members = useWorkspaceMembers()
  const mutation = useMutation({
    mutationFn: (userId: string) => setTaskAssignee(item.task.id, userId),
    onSuccess: () => {
      onClose()
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.workspace, item.task.id] })
      // The Tasks rows read their own projection, so they must be told too or
      // the chip there disagrees with the header that just changed it.
      void queryClient.invalidateQueries({ queryKey: ['task-operations'] })
    },
  })
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose
  useEffect(() => {
    const previous = document.activeElement
    ref.current?.querySelector<HTMLButtonElement>('button')?.focus()
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.stopImmediatePropagation()
        onCloseRef.current()
      }
    }
    const close = (event: MouseEvent) => {
      if (!ref.current?.contains(event.target as Node)) onCloseRef.current()
    }
    document.addEventListener('mousedown', close)
    document.addEventListener('keydown', onKeyDown, true)
    return () => {
      document.removeEventListener('mousedown', close)
      document.removeEventListener('keydown', onKeyDown, true)
      if (previous instanceof HTMLElement) previous.focus()
    }
  }, [])
  if (!enabled || !canAssign) return null
  const roster = members.data ?? []
  return (
    <div className="relative" ref={ref}>
      <div className="absolute right-0 z-30 mt-1 w-64 overflow-hidden rounded-xl border border-border bg-surface shadow-2xl">
        <div role="listbox" aria-label="Workspace members" className="max-h-56 overflow-y-auto p-1">
          {roster.map((member) => {
            const name = member.display_name || member.email || member.user_id
            const current = item.task.assignee?.user_id === member.user_id
            return (
              <button
                key={member.user_id}
                type="button"
                role="option"
                aria-selected={current}
                disabled={mutation.isPending}
                onClick={() => mutation.mutate(member.user_id)}
                className="flex w-full items-center gap-2 rounded-md px-2 py-2 text-left text-xs hover:bg-raised aria-selected:bg-raised disabled:opacity-40"
              >
                <span className="min-w-0 flex-1 truncate">{name}</span>
                {current && <Check className="size-3 shrink-0 text-primary" aria-hidden="true" />}
              </button>
            )
          })}
          {members.isSuccess && roster.length === 0 && (
            <p className="px-2.5 py-3 text-xs text-muted">No workspace members to assign.</p>
          )}
          {members.isLoading && <p className="px-2.5 py-3 text-xs text-muted">Loading members…</p>}
        </div>
        {item.task.assignee && (
          <button
            type="button"
            disabled={mutation.isPending}
            onClick={() => mutation.mutate('')}
            className="w-full border-t border-border px-3 py-2 text-left text-xs text-muted transition-colors hover:bg-raised hover:text-foreground disabled:opacity-40"
          >
            Clear assignee
          </button>
        )}
        {mutation.error != null && (
          <p role="alert" className="border-t border-border px-3 py-2 text-xs text-failure">
            {errorMessage(mutation.error, 'Could not update the assignee.')}
          </p>
        )}
      </div>
    </div>
  )
}

// One aligned row of the facts grid: dt and dd are sibling grid cells, so
// every label shares a column and the values line up.
function Fact({ label, value, startRow }: { label: React.ReactNode; value: React.ReactNode; startRow?: boolean }) {
  return (
    <>
      <dt className={cn('text-muted', startRow && 'col-start-1')}>{label}</dt>
      <dd className="min-w-0 truncate text-foreground">{value}</dd>
    </>
  )
}

// "feature" reads as "Feature": badges speak in sentence case.
function sentenceCase(value: string) {
  return value.charAt(0).toUpperCase() + value.slice(1).replaceAll('_', ' ')
}

// A GitHub pull-request URL reads as owner/repo#number; anything else keeps
// its path.
function forgeReference(url: string) {
  const match = /^https:\/\/github\.com\/([^/]+\/[^/]+)\/pull\/(\d+)/.exec(url)
  return match ? `${match[1]}#${match[2]}` : url.replace(/^https:\/\/github\.com\//, '')
}

// The agent prompt for the conveyor-work skill (req-agent-skills REQ-3) is the
// task title and the instruction, one blank line apart. It leaves the task body
// out: the agent reads it from its claimed work order. The instruction carries
// only what an agent cannot infer: the task, the selected workspace, and this
// dashboard's origin as the server.
function agentPrompt(item: ActivityItem, workspace: string): string {
  return [
    item.task.title,
    `Use the conveyor-work skill to work Conveyor task ${item.task.id} in workspace \`${workspace}\` on ${window.location.origin}.`,
  ].join('\n\n')
}

// One row: the label, a one-line preview of the prompt, a toggle, and the copy
// button. The toggle reveals the complete prompt the button copies, wrapped
// with its blank line kept, and a long unbroken token breaks anywhere rather
// than widening the row (component-web-task-surfaces, Agent prompt). It
// renders nothing until the workspace selection resolves.
function Checkout({ item }: { item: ActivityItem }) {
  const { workspace } = useWorkspaceSelection()
  const [open, setOpen] = useState(false)
  const promptID = useId()
  if (item.checkout_available) {
    if (!workspace) return null
    const prompt = agentPrompt(item, workspace)
    // The instruction is the informative half: the title already heads the page.
    const preview = prompt.slice(prompt.indexOf('\n\n') + 2)
    return (
      <section aria-label="Agent prompt" className="w-full min-w-0 rounded-lg border border-border bg-surface">
        <div className="flex min-w-0 items-center gap-2 py-1 pr-1 pl-3">
          <Terminal className="size-3.5 shrink-0 text-faint" aria-hidden="true" />
          <span className="shrink-0 text-[13px] font-medium">Agent prompt</span>
          <span className="min-w-0 flex-1 truncate text-[13px] text-muted" data-agent-prompt-preview>
            {preview}
          </span>
          <button
            type="button"
            aria-expanded={open}
            aria-controls={promptID}
            aria-label={open ? 'Hide agent prompt' : 'Show agent prompt'}
            onClick={() => setOpen((value) => !value)}
            className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-faint hover:bg-raised hover:text-foreground focus-visible:outline-2 focus-visible:outline-primary"
          >
            {open ? (
              <ChevronUp className="size-3.5" aria-hidden="true" />
            ) : (
              <ChevronDown className="size-3.5" aria-hidden="true" />
            )}
          </button>
          <CopyButton value={prompt} label="Copy agent prompt" />
        </div>
        <code
          id={promptID}
          hidden={!open}
          className="block border-t border-border px-3 py-2 font-mono text-[11px] leading-4 whitespace-pre-wrap wrap-anywhere text-muted"
        >
          {prompt}
        </code>
      </section>
    )
  }
  if (!item.checkout_guidance) return null
  return (
    <div className="flex max-w-xl items-start gap-2 rounded-md border border-border bg-surface px-2.5 py-1.5">
      <Terminal className="mt-0.5 size-3.5 shrink-0 text-faint" aria-hidden="true" />
      <p className="min-w-0 text-[11px] leading-5 text-muted">
        <span className="font-medium">Work on this locally</span> — {item.checkout_guidance}
      </p>
    </div>
  )
}
