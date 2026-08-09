import { useId, useLayoutEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import {
  ChevronDown,
  ChevronRight,
  ChevronUp,
  ExternalLink,
  GitBranch,
  GitPullRequest,
  Hand,
  Link2Off,
  Trash2,
} from 'lucide-react'
import { dependencyRelationLabel, parseProvenance, pullRequestURL } from '../../lib/activity'
import { cancelTask, changeTaskSetup, fetchWorkspaceConfig, removeTaskDependency, setTaskHold } from '../../lib/api'
import { findBlueprint } from '../../lib/blueprint'
import { taskStateLabels } from '../../lib/contracts'
import { relatedTaskRoute } from '../../lib/task-route'
import type { ActivityItem } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { useBlueprints, useOperatorToken } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'
import { Dialog } from '../ui/dialog'
import { Textarea } from '../ui/input'
import { MarkdownProse } from '../ui/markdown-prose'

// The task-header facts: state badges,
// the facts a reviewer actually references — where the work lives, where it
// came from, where to read it — and the dedicated-worktree checkout command
// (§21.8). Anything the specification card or the timeline already states is
// deliberately absent: the header introduces the task, it does not summarize
// the whole page.
export function TaskHeader({ item, variant }: { item: ActivityItem; variant: 'sheet' | 'full' }) {
  const provenance = parseProvenance(item.task.source)
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
  const issueLabel = item.task.github?.issue_number
    ? `${item.task.github.repository}#${item.task.github.issue_number}`
    : ''
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
        <span className="ml-auto flex items-center gap-1">
          <HoldControl item={item} />
          <CancelControl item={item} />
        </span>
      </div>
      <Heading
        className={cn('font-semibold leading-snug tracking-tight', variant === 'full' ? 'text-xl' : 'text-base')}
      >
        {item.task.title}
      </Heading>
      {item.task.body && <TaskBody body={item.task.body} />}

      <dl
        className={cn(
          'mt-4 grid gap-x-8 gap-y-2.5 text-xs',
          variant === 'full' ? 'grid-cols-2 lg:grid-cols-4' : 'grid-cols-2',
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
        <Fact label="Human approval" value={approvalLabel(item.task.spec_approval, item.task.merge_approval)} />
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
        {item.task.setup_contract?.name && (
          <Fact label="Setup" value={<span className="font-mono">{item.task.setup_contract.name}</span>} />
        )}
        {/* Suppressed when the task was raised from the very issue Conveyor
            went on to adopt — the Issue fact below already links it. */}
        {provenance.label !== issueLabel && (
          <Fact
            label="Raised by"
            value={
              provenance.href ? (
                <a href={provenance.href} target="_blank" rel="noreferrer" className="text-primary hover:underline">
                  {provenance.label}
                </a>
              ) : (
                provenance.label
              )
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

      {unsatisfiableIDs.size > 0 && <UnlinkDependencyControl item={item} dependencyIDs={[...unsatisfiableIDs]} />}

      {(item.task.children?.length ?? 0) > 0 && (
        <section className="mt-4 rounded-md border border-border p-3">
          <div className="flex items-center justify-between gap-3">
            <h3 className="text-sm font-semibold">Blueprint tasks</h3>
            <span className="text-xs text-muted">{childRollup}</span>
          </div>
          <ul className="mt-2 space-y-1.5">
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

      {/* Two disclosures, one row: the local-checkout helper and the rarely
          used setup change. Both stay collapsed so the page opens on the work
          rather than on configuration. */}
      <div className="mt-3 flex flex-wrap gap-x-6 gap-y-2">
        <Checkout item={item} />
        <SetupChangeControl item={item} />
      </div>
    </div>
  )
}

// Human approval stated as what a reviewer will be asked to do, not as the
// pair of policy flags behind it.
function approvalLabel(specApproval: boolean, mergeApproval: boolean) {
  if (specApproval && mergeApproval) return 'Spec and merge'
  if (specApproval) return 'Spec only'
  if (mergeApproval) return 'Merge only'
  return 'None — runs to merge'
}

function UnlinkDependencyControl({ item, dependencyIDs }: { item: ActivityItem; dependencyIDs: string[] }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const [selectedID, setSelectedID] = useState('')
  const [reason, setReason] = useState('')
  const requestID = useRef(crypto.randomUUID())
  const selected = item.task.dependencies?.find((dependency) => dependency.id === selectedID)
  const mutation = useMutation({
    mutationFn: () => removeTaskDependency(item.task.id, selectedID, token, reason.trim(), requestID.current),
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
  if (!token) return null
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
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const [reason, setReason] = useState('')
  const mutation = useMutation({
    mutationFn: () => cancelTask(item.task.id, token, reason.trim()),
    onSuccess: () => {
      setOpen(false)
      setReason('')
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void queryClient.invalidateQueries({ queryKey: ['task'] })
    },
  })
  const terminal = item.task.state === 'merged' || item.task.state === 'closed'
  if (!token || terminal) return null
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
          <MarkdownProse className="text-sm text-muted">{body}</MarkdownProse>
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

function SetupChangeControl({ item }: { item: ActivityItem }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const config = useQuery({
    queryKey: ['workspace-config', item.task.workspace, token],
    queryFn: () => fetchWorkspaceConfig(token),
    enabled: Boolean(token),
  })
  const [selected, setSelected] = useState(item.task.setup)
  const [reason, setReason] = useState('')
  // Submitted spec/implement attempts are delivered, not executing; only
  // claimed attempts and in-flight review verdicts block.
  const claimed = (item.work_orders ?? []).some((order) => order.state === 'claimed')
  const verdictInFlight = (item.work_orders ?? []).some(
    (order) => order.stage === 'review' && order.state === 'submitted',
  )
  const terminal = item.task.state === 'merged' || item.task.state === 'closed'
  const next = config.data?.document.setups.find((setup) => setup.name === selected)
  const current = item.task.setup_contract
  const mutation = useMutation({
    mutationFn: () =>
      changeTaskSetup(
        item.task.id,
        token,
        selected === item.task.setup
          ? { apply_latest: true, reason, request_id: crypto.randomUUID() }
          : { setup: selected, reason, request_id: crypto.randomUUID() },
      ),
    onSuccess: () => {
      setReason('')
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.workspace, item.task.id] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  if (!token || !current) return null
  const disabledReason = terminal
    ? 'Terminal tasks cannot change setup.'
    : claimed
      ? 'An attempt is claimed and executing.'
      : verdictInFlight
        ? 'A review verdict is in flight.'
        : ''
  const oldSeats = current?.review.seats ?? []
  const newSeats = next?.review.seats ?? []
  const interruptedOutcome = item.interrupted_review_recovery
    ? oldSeats.length !== newSeats.length
      ? 'Interrupted review: panel size changes, so a whole new round will run.'
      : 'Interrupted review: identical seat assignments are retained; changed seats re-run.'
    : ''
  return (
    <Disclosure summary="Change execution setup" note="affects future work only">
      <div className="mt-2 grid gap-2 sm:grid-cols-[minmax(10rem,14rem)_1fr_auto]">
        <select
          aria-label="Named execution setup"
          value={selected}
          onChange={(event) => setSelected(event.target.value)}
          disabled={Boolean(disabledReason) || mutation.isPending}
          className="rounded-md border border-border bg-background px-2 py-1.5 text-xs"
        >
          {(config.data?.document.setups ?? []).map((setup) => (
            <option key={setup.name} value={setup.name}>
              {setup.name}
            </option>
          ))}
        </select>
        <input
          aria-label="Setup change reason"
          value={reason}
          onChange={(event) => setReason(event.target.value)}
          placeholder="Reason (optional)"
          disabled={Boolean(disabledReason) || mutation.isPending}
          className="rounded-md border border-border bg-background px-2 py-1.5 text-xs"
        />
        <Button
          size="sm"
          disabled={Boolean(disabledReason) || !next || mutation.isPending}
          onClick={() => mutation.mutate()}
        >
          {selected === item.task.setup ? 'Apply latest setup' : 'Change setup'}
        </Button>
      </div>
      {next && (
        <div className="mt-2 grid gap-2 text-[11px] text-muted md:grid-cols-2">
          <p>
            <span className="font-medium text-foreground/80">Before:</span> implement{' '}
            {current.execution_settings.implementation.harness} /{' '}
            {current.execution_settings.implementation.model_policy} /{' '}
            {current.execution_settings.implementation.effort || 'default'} /{' '}
            {current.execution_settings.implementation.timeout}; review {current.execution_settings.review.execution},{' '}
            {oldSeats
              .map((seat) => `${seat.harness || 'fallback'}/${seat.model}/${seat.effort || 'default'}`)
              .join(', ')}
          </p>
          <p>
            <span className="font-medium text-foreground/80">After:</span> implement{' '}
            {next.execution_settings.implementation.harness} / {next.execution_settings.implementation.model_policy} /{' '}
            {next.execution_settings.implementation.effort || 'default'} /{' '}
            {next.execution_settings.implementation.timeout}; review {next.execution_settings.review.execution},{' '}
            {newSeats
              .map((seat) => `${seat.harness || 'fallback'}/${seat.model}/${seat.effort || 'default'}`)
              .join(', ')}
          </p>
        </div>
      )}
      {(disabledReason || interruptedOutcome) && (
        <p className="mt-2 text-xs text-muted">{disabledReason || interruptedOutcome}</p>
      )}
      {mutation.error != null && <p className="mt-2 text-xs text-failure">{String(mutation.error)}</p>}
      {mutation.data && (
        <p className="mt-2 text-xs text-positive">
          Setup changed: {mutation.data.review_transition.replaceAll('_', ' ')}.
        </p>
      )}
    </Disclosure>
  )
}

// A quiet inline disclosure: secondary controls stay one click away instead
// of occupying the space above the work they configure.
function Disclosure({ summary, note, children }: { summary: string; note?: string; children: React.ReactNode }) {
  return (
    <details className="group/disclosure min-w-0 basis-full sm:basis-auto">
      <summary className="inline-flex cursor-pointer list-none items-center gap-1.5 text-xs text-muted hover:text-foreground">
        <ChevronRight className="size-3 shrink-0 transition-transform group-open/disclosure:rotate-90" />
        {summary}
        {note && <span className="text-faint">{note}</span>}
      </summary>
      <div className="mt-2 rounded-lg border border-border bg-surface p-3">{children}</div>
    </details>
  )
}

// Per-task hold toggle: while held, the worker daemon never
// claims this task's work orders — you attach an agent and claim explicitly.
function HoldControl({ item }: { item: ActivityItem }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const toggle = useMutation({
    mutationFn: () => setTaskHold(item.task.id, token, !item.task.hold),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ['activity'] }),
  })
  const terminal = item.task.state === 'merged' || item.task.state === 'closed'
  if (!token || terminal) return null
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

function Fact({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-faint">{label}</dt>
      <dd className="mt-0.5 truncate text-foreground/85">{value}</dd>
    </div>
  )
}

function Checkout({ item }: { item: ActivityItem }) {
  return (
    <Disclosure summary="Work on this locally">
      {item.checkout_available && item.checkout_command ? (
        <div className="flex max-w-xl items-center gap-1">
          <code className="min-w-0 flex-1 truncate font-mono text-[11px] text-muted">{item.checkout_command}</code>
          <CopyButton value={item.checkout_command} label="Copy dedicated worktree command" />
        </div>
      ) : (
        <p className="max-w-xl text-[11px] leading-5 text-muted">{item.checkout_guidance}</p>
      )}
    </Disclosure>
  )
}
