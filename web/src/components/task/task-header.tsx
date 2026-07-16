import { ExternalLink, GitBranch, GitPullRequest } from 'lucide-react'
import { parseProvenance, pullRequestURL } from '../../lib/activity'
import { taskStateLabels } from '../../lib/contracts'
import type { ActivityItem } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { Badge } from '../ui/badge'
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
        <Badge variant="mono" className="capitalize">{item.task.mode || 'manual'}</Badge>
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
        <Fact
          label="Verification"
          value={
            spec
              ? `${spec.acceptance_count} criteria · v${spec.version} ${spec.approved ? 'approved' : 'draft'}`
              : 'No spec yet'
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

      <Checkout item={item} variant={variant} />
    </div>
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
