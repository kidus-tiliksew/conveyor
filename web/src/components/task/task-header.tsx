import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import {
  Check,
  ChevronDown,
  ChevronUp,
  ExternalLink,
  GitBranch,
  GitPullRequest,
  Hand,
  Link2Off,
  Terminal,
  Trash2,
} from 'lucide-react'
import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import { assigneeName, dependencyRelationLabel, pullRequestURL } from '../../lib/activity'
import { cancelTask, removeTaskDependency, setTaskAssignee, setTaskHold } from '../../lib/api'
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
import { Textarea } from '../ui/input'
import { MarkdownProse } from '../ui/markdown-prose'
import { AssigneeChip } from './assignee-chip'

// The task-header facts: state badges,
// the facts a reviewer actually references — where the work lives, where it
// came from, where to read it — and the task-run command
// (§21.8). Anything the specification card or the timeline already states is
// deliberately absent: the header introduces the task, it does not summarize
// the whole page.
export function TaskHeader({ item, variant }: { item: ActivityItem; variant: 'sheet' | 'full' }) {
  const canOperate = useWorkspaceCapability('operate_gates')
  const prURL = pullRequestURL(item.events)
  const Heading = variant === 'full' ? 'h1' : 'h2'
  const relatedRoute = relatedTaskRoute(variant)
  // The parent is a blueprint anchor, which no longer rides the activity feed;
  // the blueprint projection is where its title now lives.
  const { data: blueprints } = useBlueprints()
  const parent = findBlueprint(blueprints, item.task.parent_task_id ?? '')?.task
  const blockingIDs = new Set(item.task.blocking_task_ids ?? [])
  const unsatisfiableIDs = new Set(item.stalled?.unsatisfiable_edge ? (item.stalled.blocking_task_ids ?? []) : [])
  const stateLabel = item.stalled?.needed ? 'Stalled' : (taskStateLabels[item.task.state] ?? item.task.state)
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
        <span
          role="img"
          className="group/status relative inline-flex rounded-md focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
          aria-label={`Task status: ${stateLabel}`}
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
          <span
            role="tooltip"
            className="pointer-events-none absolute bottom-full left-0 z-10 mb-1.5 w-56 rounded-md bg-foreground px-2.5 py-1.5 text-[11px] leading-4 text-background opacity-0 shadow-md transition-opacity after:absolute after:left-3 after:top-full after:border-4 after:border-transparent after:border-t-foreground group-hover/status:opacity-100 group-focus/status:opacity-100"
          >
            {item.stalled?.reason ?? `Current task status: ${stateLabel}.`}
          </span>
        </span>
        {item.task.hold && <Badge variant="mono">Held</Badge>}
        {unsatisfiableIDs.size > 0 ? (
          <Badge variant="attention">Dependency needs attention</Badge>
        ) : (
          blockingIDs.size > 0 && <Badge variant="mono">Waiting on dependencies</Badge>
        )}
        {item.task.class && <Badge>{item.task.class}</Badge>}
        {/* Controls sit apart from the status chips: a destructive action
            never belongs in the row the eye reads for state. */}
        {canOperate && (
          <span className="ml-auto flex items-center gap-1">
            <HoldControl item={item} />
            <AssigneeControl item={item} />
            <CancelControl item={item} />
          </span>
        )}
      </div>
      <Heading className={cn('font-semibold leading-snug tracking-tight', variant === 'full' ? 'text-xl' : 'text-lg')}>
        {item.task.title}
      </Heading>
      {item.task.body && <TaskBody body={item.task.body} />}

      <dl
        className={cn(
          'mt-5 grid grid-cols-[minmax(5.5rem,max-content)_minmax(0,1fr)] items-baseline gap-x-6 gap-y-2 text-[13px] leading-5',
          variant === 'full' &&
            'lg:grid-cols-[minmax(5.5rem,max-content)_minmax(0,1fr)_minmax(5.5rem,max-content)_minmax(0,1fr)] lg:gap-x-10',
        )}
      >
        <Fact label="Repo" value={item.task.repo} />
        <Fact
          label="Branch"
          value={
            <span className="inline-flex max-w-full items-baseline gap-1 font-mono">
              <GitBranch className="size-3 shrink-0 translate-y-0.5 text-faint" />
              <span className="truncate">{item.task.branch}</span>
              <span className="shrink-0 text-faint">← {item.task.base_branch}</span>
            </span>
          }
        />
        <Fact label="Created" value={absoluteTime(item.task.created_at)} />
        {/* A labelled fact answers "who holds this?", so it says Unassigned
            rather than going blank — unlike the list rows and board cards,
            where absence is the answer. */}
        <Fact
          label="Assignee"
          value={item.task.assignee ? <AssigneeChip assignee={item.task.assignee} /> : 'Unassigned'}
        />
        {item.task.parent_task_id && (
          <Fact
            label="Parent blueprint"
            value={
              // The parent is an intent artifact, not work, so this reference
              // leaves the task routes for the blueprint's canonical home,
              // unlike dependencies below, which are tasks.
              <span className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
                <Link
                  to="/blueprints/$taskId"
                  params={{ taskId: item.task.parent_task_id }}
                  className="text-primary hover:underline"
                >
                  {parent?.title ?? item.task.parent_task_id}
                  {item.task.origin_spec_version ? ` · spec v${item.task.origin_spec_version}` : ''}
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
        {(item.task.dependencies?.length ?? 0) > 0 && (
          <Fact
            label="Dependencies"
            value={
              <span className="flex flex-wrap gap-x-2 gap-y-1">
                {item.task.dependencies!.map((dependency) => (
                  <Link
                    key={dependency.id}
                    to={relatedRoute}
                    params={{ taskId: dependency.id }}
                    className="text-primary hover:underline"
                  >
                    {dependency.title || dependency.id} ·{' '}
                    {dependencyRelationLabel(
                      dependency.state,
                      blockingIDs.has(dependency.id),
                      unsatisfiableIDs.has(dependency.id),
                    )}
                  </Link>
                ))}
              </span>
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
                  className="inline-flex max-w-full items-center gap-1 text-primary hover:underline"
                >
                  <span className="truncate">
                    {item.task.github.repository}#{item.task.github.issue_number}
                  </span>
                  <ExternalLink className="size-3 shrink-0" />
                </a>
              ) : (
                `${item.task.github.state} · spec v${item.task.github.spec_version}`
              )
            }
          />
        )}
        {prURL && (
          <Fact
            label="Pull request"
            value={
              <a
                href={prURL}
                target="_blank"
                rel="noreferrer"
                className="inline-flex max-w-full items-center gap-1 text-primary hover:underline"
              >
                <GitPullRequest className="size-3 shrink-0" />
                <span className="truncate">{prURL.replace(/^https:\/\/github\.com\//, '')}</span>
                <ExternalLink className="size-3 shrink-0" />
              </a>
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
            <h3 className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">Blueprint tasks</h3>
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
// keeps it while suppressing checkout, branch, and hold. Its
// consequences for children are whatever the backend does today.
export function CancelControl({ item }: { item: ActivityItem }) {
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
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
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs font-medium leading-4 text-muted transition-colors hover:bg-failure-soft hover:text-failure [&_svg]:size-3.5"
      >
        <Trash2 /> Cancel task
      </button>
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
        <div ref={viewportRef} id={viewportID} className={cn(!expanded && 'max-h-40 overflow-hidden')}>
          <MarkdownProse className="markdown-demoted text-sm text-muted">{body}</MarkdownProse>
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
          className="mt-1 inline-flex items-center gap-1 rounded text-xs font-medium text-primary hover:underline focus-visible:outline-2 focus-visible:outline-primary"
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

// Per-task hold toggle: while held, the worker daemon never
// claims this task's work orders — you attach an agent and claim explicitly.
function HoldControl({ item }: { item: ActivityItem }) {
  const queryClient = useQueryClient()
  const toggle = useMutation({
    mutationFn: () => setTaskHold(item.task.id, !item.task.hold),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ['activity'] }),
  })
  const terminal = item.task.state === 'merged' || item.task.state === 'closed'
  if (terminal) return null
  // The badge row already says "Held"; this control says what pressing it does.
  return (
    <button
      type="button"
      disabled={toggle.isPending}
      onClick={() => toggle.mutate()}
      title={
        item.task.hold
          ? 'Held — your worker won’t claim this task. Release it back to the queue.'
          : 'Hold this task so your worker won’t claim it; you attach an agent and claim it yourself.'
      }
      className="inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs font-medium leading-4 text-muted transition-colors hover:bg-raised hover:text-foreground disabled:opacity-40 [&_svg]:size-3.5"
    >
      <Hand />
      {item.task.hold ? 'Release' : 'Hold'}
    </button>
  )
}

/**
 * Set, reassign, or clear the task's assignee from the workspace's own member
 * list (REQ-4). Assigning constrains who may claim the task's work orders; it
 * never touches queue order (DEC-18).
 *
 * Assignment is an operator act, carried by the `set_assignee` capability. The
 * caller's role for this workspace comes from the server's own self-identity
 * read rather than being guessed in the browser, so a member simply has no
 * control. Every member still reads the assignee wherever it is rendered — the
 * members list itself is a co-member read, never a user directory (AC-3.2).
 */
function AssigneeControl({ item }: { item: ActivityItem }) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  const terminal = item.task.state === 'merged' || item.task.state === 'closed'
  const enabled = Boolean(workspace) && !terminal
  const canAssign = useWorkspaceCapability('set_assignee')
  const members = useWorkspaceMembers()
  const mutation = useMutation({
    mutationFn: (userId: string) => setTaskAssignee(item.task.id, userId),
    onSuccess: () => {
      setOpen(false)
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.workspace, item.task.id] })
      // The Tasks rows read their own projection, so they must be told too or
      // the chip there disagrees with the header that just changed it.
      void queryClient.invalidateQueries({ queryKey: ['task-operations'] })
    },
  })
  useEffect(() => {
    const close = (event: MouseEvent) => {
      if (!ref.current?.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', close)
    return () => document.removeEventListener('mousedown', close)
  }, [])
  if (!enabled || !canAssign) return null
  const roster = members.data ?? []
  return (
    <div className="relative" ref={ref}>
      <button
        type="button"
        aria-haspopup="listbox"
        aria-expanded={open}
        disabled={mutation.isPending}
        onClick={() => setOpen(!open)}
        title={item.task.assignee ? `Assigned to ${assigneeName(item.task.assignee)}` : 'Assign this task to a member'}
        className="inline-flex items-center rounded-md px-2 py-1 text-xs font-medium leading-4 text-muted transition-colors hover:bg-raised hover:text-foreground disabled:opacity-40"
      >
        {item.task.assignee ? 'Reassign' : 'Assign'}
      </button>
      {open && (
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
      )}
    </div>
  )
}

// One aligned row of the facts grid: dt and dd are sibling grid cells, so
// every label shares a column and the values line up.
function Fact({ label, value }: { label: React.ReactNode; value: React.ReactNode }) {
  return (
    <>
      <dt className="text-xs leading-5 text-faint">{label}</dt>
      <dd className="min-w-0 truncate text-foreground/90">{value}</dd>
    </>
  )
}

// The task-run command, inline rather than behind a disclosure: it stays
// content-sized on roomy screens and wraps as one usable group when constrained.
function Checkout({ item }: { item: ActivityItem }) {
  if (item.checkout_available && item.checkout_command) {
    return (
      <div className="inline-flex w-fit max-w-full flex-wrap items-center gap-x-2 gap-y-1 rounded-md border border-border bg-surface py-0.5 pl-2.5 pr-0.5">
        <span className="inline-flex min-w-0 items-center gap-2">
          <Terminal className="size-3.5 shrink-0 text-faint" aria-hidden="true" />
          <span className="min-w-0 text-[11px] font-medium text-muted">Work on this locally</span>
        </span>
        <code className="min-w-0 break-all font-mono text-[11px] text-faint">{item.checkout_command}</code>
        <CopyButton value={item.checkout_command} label="Copy task run command" />
      </div>
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
