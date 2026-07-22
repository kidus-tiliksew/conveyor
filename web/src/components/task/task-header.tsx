import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ExternalLink, GitBranch, GitPullRequest, Hand } from 'lucide-react'
import { parseProvenance, pullRequestURL } from '../../lib/activity'
import { changeTaskSetup, fetchWorkspaceConfig, setTaskHold } from '../../lib/api'
import { taskStateLabels } from '../../lib/contracts'
import type { ActivityItem } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { useOperatorToken } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { CopyButton } from '../ui/copy-button'

// The task-header facts (spec §13.3, amended by §§21.6–21.7): state badges,
// the verification chip fed by the §4.1 acceptance block, the PR deep link,
// provenance, and the dedicated-worktree checkout command (§21.8).
export function TaskHeader({ item, variant }: { item: ActivityItem; variant: 'sheet' | 'full' }) {
  const provenance = parseProvenance(item.task.source)
  const prURL = pullRequestURL(item.events)
  const spec = item.spec
  const Heading = variant === 'full' ? 'h1' : 'h2'

  return (
    <div>
      <div className="mb-2 flex flex-wrap items-center gap-1.5">
        {/* Approved reads as good news even while it waits at the gate —
            amber stays reserved for states that are genuinely stuck. */}
        <Badge
          variant={
            item.task.state === 'approved' || item.task.state === 'merged'
              ? 'positive'
              : item.needs_attention
                ? 'attention'
                : 'outline'
          }
        >
          {taskStateLabels[item.task.state] ?? item.task.state}
        </Badge>
        <HoldControl item={item} />
        {item.task.setup && <Badge variant="mono">setup: {item.task.setup}</Badge>}
        {item.task.class && <Badge>{item.task.class}</Badge>}
        <Badge variant="accent">{provenance.label}</Badge>
      </div>
      <Heading className={cn('font-semibold leading-snug tracking-tight', variant === 'full' ? 'text-xl' : 'text-base')}>
        {item.task.title}
      </Heading>
      {item.task.body && <p className="mt-1.5 max-w-3xl text-sm leading-6 text-muted">{item.task.body}</p>}

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

      {variant === 'full' && <SetupChangeControl item={item} />}

      <Checkout item={item} variant={variant} />
    </div>
  )
}

function SetupChangeControl({ item }: { item: ActivityItem }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const config = useQuery({ queryKey: ['workspace-config', item.task.workspace, token], queryFn: () => fetchWorkspaceConfig(token), enabled: Boolean(token) })
  const [selected, setSelected] = useState(item.task.setup)
  const [reason, setReason] = useState('')
  const active = (item.work_orders ?? []).some((order) => order.state === 'claimed' || order.state === 'submitted')
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
  const disabledReason = terminal ? 'Terminal tasks cannot change setup.' : active ? 'A work order is claimed or executing.' : ''
  const oldSeats = current?.review.seats ?? []
  const newSeats = next?.review.seats ?? []
  const interruptedOutcome = item.interrupted_review_recovery
    ? oldSeats.length !== newSeats.length
      ? 'Interrupted review: panel size changes, so a whole new round will run.'
      : 'Interrupted review: identical seat assignments are retained; changed seats re-run.'
    : ''
  return (
    <section className="mt-4 rounded-lg border border-border bg-surface p-3" aria-label="Change execution setup">
      <div className="flex flex-wrap items-center gap-2">
        <strong className="text-xs font-semibold">Change execution setup</strong>
        <span className="text-xs text-attention">affects future work only</span>
      </div>
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
    </section>
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
