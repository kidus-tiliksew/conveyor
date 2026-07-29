import { useId, useLayoutEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ChevronDown, ChevronUp, ExternalLink, GitBranch, GitPullRequest, Hand, Link2Off, Trash2 } from 'lucide-react'
import { parseProvenance, pullRequestURL } from '../../lib/activity'
import { cancelTask, changeTaskSetup, fetchWorkspaceConfig, removeTaskDependency, setTaskHold } from '../../lib/api'
import { taskStateLabels } from '../../lib/contracts'
import { relatedTaskRoute } from '../../lib/task-route'
import type { ActivityItem } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { useActivity, useOperatorToken } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'
import { Dialog } from '../ui/dialog'
import { Textarea } from '../ui/input'
import { MarkdownProse } from '../ui/markdown-prose'

// The task-header facts (spec §13.3, amended by §§21.6–21.7): state badges,
// the verification chip fed by the §4.1 acceptance block, the PR deep link,
// provenance, and the dedicated-worktree checkout command (§21.8).
export function TaskHeader({ item, variant }: { item: ActivityItem; variant: 'sheet' | 'full' }) {
  const provenance = parseProvenance(item.task.source)
  const prURL = pullRequestURL(item.events)
  const spec = item.spec
  const Heading = variant === 'full' ? 'h1' : 'h2'
  const relatedRoute = relatedTaskRoute(variant)
  const { data: activity } = useActivity()
  const parent = activity?.find((entry) => entry.task.id === item.task.parent_task_id)?.task
  const blockingIDs = new Set(item.task.blocking_task_ids ?? [])
  const unsatisfiableIDs = new Set(item.stalled?.unsatisfiable_edge ? item.stalled.blocking_task_ids ?? [] : [])
  const stateLabel = item.stalled?.needed ? 'Stalled' : (taskStateLabels[item.task.state] ?? item.task.state)
  const mergedChildren = item.task.children?.filter((child) => child.state === 'merged').length ?? 0
  const closedChildren = item.task.children?.filter((child) => child.state === 'closed').length ?? 0
  const openChildren = (item.task.children?.length ?? 0) - mergedChildren - closedChildren
  const childRollup = [
    mergedChildren > 0 ? `${mergedChildren} merged` : '',
    closedChildren > 0 ? `${closedChildren} closed` : '',
    openChildren > 0 ? `${openChildren} open` : '',
  ].filter(Boolean).join(' · ')

  return (
    <div>
      <div className="mb-2 flex flex-wrap items-center gap-1.5">
        {/* Approved reads as good news even while it waits at the gate —
            amber stays reserved for states that are genuinely stuck. */}
        <span
          className="group/status relative inline-flex rounded-md focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
          tabIndex={0}
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
        <HoldControl item={item} />
        {unsatisfiableIDs.size > 0
          ? <Badge variant="attention">Dependency needs attention</Badge>
          : blockingIDs.size > 0 && <Badge variant="mono">Waiting on dependencies</Badge>}
        <CancelControl item={item} />
        {item.task.setup && <Badge variant="mono">setup: {item.task.setup}</Badge>}
        {item.task.class && <Badge>{item.task.class}</Badge>}
        <Badge variant="accent">{provenance.label}</Badge>
      </div>
      <Heading className={cn('font-semibold leading-snug tracking-tight', variant === 'full' ? 'text-xl' : 'text-base')}>
        {item.task.title}
      </Heading>
      {item.task.body && <TaskBody body={item.task.body} />}

      <dl className={cn('mt-4 grid gap-x-8 gap-y-2.5 text-xs', variant === 'full' ? 'grid-cols-2 lg:grid-cols-4' : 'grid-cols-2')}>
        <Fact label="Repo" value={item.task.repo} />
        <Fact
          label="Assigned branch"
          value={
            <span className="inline-flex max-w-full items-center gap-1 font-mono">
              <GitBranch className="size-3 shrink-0 text-faint" />
              <span className="truncate">{item.task.branch}</span>
            </span>
          }
        />
        <Fact label="Base" value={<span className="font-mono">{item.task.base_branch}</span>} />
        <Fact
          label="Source"
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
        <Fact label="Created" value={absoluteTime(item.task.created_at)} />
        <Fact label="Gates" value={`spec ${item.task.spec_approval ? 'human' : 'auto'} · merge ${item.task.merge_approval ? 'human' : 'auto'}`} />
        {item.task.parent_task_id && (
          <Fact
            label="Parent blueprint"
            value={(
              <Link to={relatedRoute} params={{ taskId: item.task.parent_task_id }} className="text-primary hover:underline">
                {parent?.title ?? item.task.parent_task_id}
                {item.task.origin_spec_version ? ` · spec v${item.task.origin_spec_version}` : ''}
              </Link>
            )}
          />
        )}
        {(item.task.dependencies?.length ?? 0) > 0 && (
          <Fact
            label="Dependencies"
            value={(
              <span className="flex flex-wrap gap-x-2 gap-y-1">
                {item.task.dependencies!.map((dependency) => (
                  <Link key={dependency.id} to={relatedRoute} params={{ taskId: dependency.id }} className="text-primary hover:underline">
                    {dependency.title || dependency.id} · {dependencyRelationLabel(dependency.state, blockingIDs.has(dependency.id), unsatisfiableIDs.has(dependency.id))}
                  </Link>
                ))}
              </span>
            )}
          />
        )}
        {item.task.setup_contract?.name && <Fact label="Frozen setup" value={<details><summary className="cursor-pointer font-mono">{item.task.setup_contract.name}</summary><span className="block font-mono text-[11px]">Implement: {item.task.setup_contract.execution_settings.implementation.harness} · {item.task.setup_contract.execution_settings.implementation.model || 'harness default'}</span><span className="block font-mono text-[11px]">Review: {item.task.setup_contract.review.seats.map((seat) => `${seat.harness || item.task.setup_contract.execution_settings.review.fallback_harness || 'in-process'} / ${seat.model}`).join(', ')}</span></details>} />}
        <Fact
          label="Verification"
          value={
            spec
              ? `${spec.acceptance_count} criteria · v${spec.version} ${spec.approved ? 'approved' : 'draft'}`
              : 'No spec yet'
          }
        />
        {item.task.github && (
          <Fact
            label="GitHub issue"
            value={item.task.github.issue_url ? (
              <a href={item.task.github.issue_url} target="_blank" rel="noreferrer" className="inline-flex max-w-full items-center gap-1 text-primary hover:underline">
                <span className="truncate">{item.task.github.repository}#{item.task.github.issue_number}</span>
                <ExternalLink className="size-3 shrink-0" />
              </a>
            ) : `${item.task.github.state} · spec v${item.task.github.spec_version}`}
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

      {unsatisfiableIDs.size > 0 && (
        <UnlinkDependencyControl item={item} dependencyIDs={[...unsatisfiableIDs]} />
      )}

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
                <Link to={relatedRoute} params={{ taskId: child.id }} className="min-w-0 flex-1 truncate text-primary hover:underline">{child.title}</Link>
                <span className="text-faint">{taskStateLabels[child.state] ?? child.state}</span>
              </li>
            ))}
          </ul>
        </section>
      )}

      <SetupChangeControl item={item} variant={variant} />

      <Checkout item={item} variant={variant} />
    </div>
  )
}

function dependencyRelationLabel(state: string, blocking: boolean, unsatisfiable: boolean) {
  if (unsatisfiable) return 'Needs attention'
  if (blocking) return 'Waiting'
  if (state === 'merged') return 'Satisfied'
  return taskStateLabels[state as keyof typeof taskStateLabels] ?? state.replaceAll('_', ' ')
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
    <section className="mt-4 rounded-md border border-attention/40 bg-attention-soft p-3" aria-label="Dependency needs attention">
      <p className="text-sm font-medium text-attention">A dependency closed without merging</p>
      <p className="mt-1 text-xs leading-5 text-muted">Remove the dependency with an audit reason so this task can continue, or cancel the task above.</p>
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
              {selected?.title ?? selectedID} closed without merging. Removing it is audited and may let this task continue.
            </p>
          </div>
          <div className="space-y-4 px-5 py-4">
            <label className="block text-sm font-medium">Reason
              <Textarea
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
              <Button variant="secondary" onClick={closeDialog} disabled={mutation.isPending}>Keep dependency</Button>
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

function CancelControl({ item }: { item: ActivityItem }) {
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
        className="inline-flex items-center gap-1 rounded-md border border-failure/30 bg-failure-soft px-2 py-0.5 text-[11px] font-medium leading-4 text-failure transition-colors hover:bg-failure/15 [&_svg]:size-3"
      >
        <Trash2 /> Cancel task
      </button>
      {open && (
        <Dialog label="Cancel task" onClose={() => !mutation.isPending && setOpen(false)}>
          <div className="border-b border-border px-5 py-4">
            <h2 className="font-semibold">Cancel this task?</h2>
            <p className="mt-1 text-sm leading-6 text-muted">This closes the pipeline and cancels active work orders. It does not delete the branch, worktree, issue, or pull request.</p>
          </div>
          <div className="space-y-4 px-5 py-4">
            <label className="block text-sm font-medium">Reason
              <Textarea autoFocus value={reason} maxLength={64} onChange={(event) => setReason(event.target.value)} placeholder="Why is this task being cancelled?" className="mt-1.5" />
            </label>
            {mutation.error != null && <p className="text-sm text-failure">{String(mutation.error)}</p>}
            <div className="flex justify-end gap-2">
              <Button variant="secondary" onClick={() => setOpen(false)} disabled={mutation.isPending}>Keep task</Button>
              <Button variant="destructive" onClick={() => mutation.mutate()} disabled={mutation.isPending || !reason.trim()}>
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
        <div
          ref={viewportRef}
          id={viewportID}
          className={cn(!expanded && 'max-h-40 overflow-hidden')}
        >
          <MarkdownProse className="text-sm text-muted">{body}</MarkdownProse>
        </div>
        {hasOverflow && !expanded && (
          <div aria-hidden="true" className="spec-overflow-shadow pointer-events-none absolute inset-x-0 bottom-0 h-16" data-task-body-overflow-shadow />
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
          {expanded ? <ChevronUp aria-hidden="true" className="size-3.5" /> : <ChevronDown aria-hidden="true" className="size-3.5" />}
          {expanded ? 'Show less description' : 'Show full description'}
        </button>
      )}
    </div>
  )
}

function SetupChangeControl({ item, variant }: { item: ActivityItem; variant: 'sheet' | 'full' }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const config = useQuery({ queryKey: ['workspace-config', item.task.workspace, token], queryFn: () => fetchWorkspaceConfig(token), enabled: Boolean(token) })
  const [selected, setSelected] = useState(item.task.setup)
  const [reason, setReason] = useState('')
  // Submitted spec/implement attempts are delivered, not executing; only
  // claimed attempts and in-flight review verdicts block (spec §21.36).
  const claimed = (item.work_orders ?? []).some((order) => order.state === 'claimed')
  const verdictInFlight = (item.work_orders ?? []).some((order) => order.stage === 'review' && order.state === 'submitted')
  const terminal = item.task.state === 'merged' || item.task.state === 'closed'
  const next = config.data?.document.setups.find((setup) => setup.name === selected)
  const current = item.task.setup_contract
  const mutation = useMutation({
    mutationFn: () => changeTaskSetup(item.task.id, token, selected === item.task.setup
      ? { apply_latest: true, reason, request_id: crypto.randomUUID() }
      : { setup: selected, reason, request_id: crypto.randomUUID() }),
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
  const body = (
    <>
      <div className="mt-2 grid gap-2 sm:grid-cols-[minmax(10rem,14rem)_1fr_auto]">
        <select aria-label="Named execution setup" value={selected} onChange={(event) => setSelected(event.target.value)} disabled={Boolean(disabledReason) || mutation.isPending} className="rounded-md border border-border bg-background px-2 py-1.5 text-xs">
          {(config.data?.document.setups ?? []).map((setup) => <option key={setup.name} value={setup.name}>{setup.name}</option>)}
        </select>
        <input aria-label="Setup change reason" value={reason} onChange={(event) => setReason(event.target.value)} placeholder="Reason (required)" disabled={Boolean(disabledReason) || mutation.isPending} className="rounded-md border border-border bg-background px-2 py-1.5 text-xs" />
        <Button size="sm" disabled={Boolean(disabledReason) || !next || !reason.trim() || mutation.isPending} onClick={() => mutation.mutate()}>
          {selected === item.task.setup ? 'Apply latest setup' : 'Change setup'}
        </Button>
      </div>
      {next && <div className="mt-2 grid gap-2 text-[11px] text-muted md:grid-cols-2">
        <p><span className="font-medium text-foreground/80">Before:</span> implement {current.execution_settings.implementation.harness} / {current.execution_settings.implementation.model_policy} / {current.execution_settings.implementation.effort || 'default'} / {current.execution_settings.implementation.timeout}; review {current.execution_settings.review.execution}, {oldSeats.map((seat) => `${seat.harness || 'fallback'}/${seat.model}/${seat.effort || 'default'}`).join(', ')}</p>
        <p><span className="font-medium text-foreground/80">After:</span> implement {next.execution_settings.implementation.harness} / {next.execution_settings.implementation.model_policy} / {next.execution_settings.implementation.effort || 'default'} / {next.execution_settings.implementation.timeout}; review {next.execution_settings.review.execution}, {newSeats.map((seat) => `${seat.harness || 'fallback'}/${seat.model}/${seat.effort || 'default'}`).join(', ')}</p>
      </div>}
      {(disabledReason || interruptedOutcome) && <p className="mt-2 text-xs text-muted">{disabledReason || interruptedOutcome}</p>}
      {mutation.error != null && <p className="mt-2 text-xs text-failure">{String(mutation.error)}</p>}
      {mutation.data && <p className="mt-2 text-xs text-positive">Setup changed: {mutation.data.review_transition.replaceAll('_', ' ')}.</p>}
    </>
  )
  if (variant === 'full') {
    return (
      <section className="mt-4 rounded-lg border border-border bg-surface p-3" aria-label="Change execution setup">
        <div className="flex flex-wrap items-center gap-2">
          <strong className="text-xs font-semibold">Change execution setup</strong>
          <span className="text-xs text-attention">affects future work only</span>
        </div>
        {body}
      </section>
    )
  }
  return (
    <details className="mt-3 rounded-lg border border-border bg-surface p-3">
      <summary className="cursor-pointer text-xs font-medium text-muted hover:text-foreground">
        Change execution setup <span className="ml-1 font-normal text-attention">affects future work only</span>
      </summary>
	  {body}
	</details>
  )
}

// Per-task hold toggle (spec §21.31): while held, the worker daemon never
// claims this task's work orders — you attach an agent and claim explicitly.
function HoldControl({ item }: { item: ActivityItem }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const toggle = useMutation({
    mutationFn: () => setTaskHold(item.task.id, token, !item.task.hold),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ['activity'] }),
  })
  const terminal = item.task.state === 'merged' || item.task.state === 'closed'
  if (!token || terminal) return item.task.hold ? <Badge variant="mono">Held</Badge> : null
  return (
    <button
      type="button"
      disabled={toggle.isPending}
      onClick={() => toggle.mutate()}
      title={item.task.hold ? 'Held — your worker won’t claim this task. Click to release it back to the queue.' : 'Hold this task so your worker won’t claim it; you attach an agent and claim it yourself.'}
      className={cn(
        'inline-flex items-center gap-1 rounded-md border px-2 py-0.5 text-[11px] font-medium leading-4 transition-colors disabled:opacity-40 [&_svg]:size-3',
        item.task.hold ? 'border-primary/40 bg-primary-soft text-primary' : 'border-border bg-surface text-muted hover:text-foreground',
      )}
    >
      <Hand />
      {item.task.hold ? 'Held' : 'Hold'}
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

function Checkout({ item, variant }: { item: ActivityItem; variant: 'sheet' | 'full' }) {
  const block =
    item.checkout_available && item.checkout_command ? (
      <div className="flex max-w-xl items-center gap-1 rounded-md border border-border bg-surface px-2.5 py-1">
        <code className="min-w-0 flex-1 truncate font-mono text-[11px] text-muted">{item.checkout_command}</code>
        <CopyButton value={item.checkout_command} label="Copy dedicated worktree command" />
      </div>
    ) : (
      <p className="max-w-xl rounded-md border border-border bg-surface px-2.5 py-2 text-[11px] leading-5 text-muted">
        {item.checkout_guidance}
      </p>
    )
  if (variant === 'full') return <div className="mt-3">{block}</div>
  return (
    <details className="mt-3">
      <summary className="cursor-pointer text-xs font-medium text-muted hover:text-foreground">Checkout</summary>
      <div className="mt-2">{block}</div>
    </details>
  )
}
