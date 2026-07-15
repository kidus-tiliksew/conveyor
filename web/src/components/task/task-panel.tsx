import { useMemo } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { ChevronDown, ChevronUp, RotateCcw, X } from 'lucide-react'
import { fetchTaskActivity, redispatchTask } from '../../lib/api'
import { groupForSummary, parseProvenance } from '../../lib/activity'
import { taskStateLabels } from '../../lib/contracts'
import { useTaskStream } from '../../lib/use-task-stream'
import type { ActivityItem } from '../../lib/types'
import { stageGroups } from '../../lib/contracts'
import { useActivity, useOperatorToken } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Skeleton } from '../ui/skeleton'
import { ReviewPanel } from './review-panel'
import { SpecCard } from './spec-card'
import { SummaryRail } from './summary-rail'
import { Timeline } from './timeline'

// The task detail panel (spec §13.3): the costed event history plus review
// actions, opened beside the feed so the reviewer never loses list context.
export function TaskPanel({ taskId }: { taskId: string }) {
  const navigate = useNavigate()
  const { data: item, isLoading, error } = useQuery({
    queryKey: ['task', taskId],
    queryFn: () => fetchTaskActivity(taskId),
  })
  useTaskStream(taskId)

  // Prev/next follow the feed's visual order (stage groups, then recency).
  const { data: activity } = useActivity()
  const order = useMemo(() => {
    const byGroup = new Map<string, string[]>()
    for (const summary of activity ?? []) {
      const key = groupForSummary(summary)
      byGroup.set(key, [...(byGroup.get(key) ?? []), summary.task.id])
    }
    return stageGroups.flatMap(({ key }) => byGroup.get(key) ?? [])
  }, [activity])
  const index = order.indexOf(taskId)
  const previousId = index > 0 ? order[index - 1] : undefined
  const nextId = index >= 0 && index < order.length - 1 ? order[index + 1] : undefined

  return (
    <aside
      aria-label="Task detail"
      className="flex w-full shrink-0 flex-col border-l border-border bg-background md:w-[480px]"
    >
      <header className="flex shrink-0 items-center gap-1 border-b border-border px-4 py-2.5">
        <span className="mr-auto font-mono text-xs text-muted">{taskId}</span>
        <PanelNavButton targetId={previousId} label="Previous task" icon={<ChevronUp />} />
        <PanelNavButton targetId={nextId} label="Next task" icon={<ChevronDown />} />
        <Button variant="ghost" size="icon" aria-label="Close panel" onClick={() => void navigate({ to: '/activity' })}>
          <X />
        </Button>
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">
        {isLoading && (
          <div className="space-y-3">
            <Skeleton className="h-16" />
            <Skeleton className="h-40" />
            <Skeleton className="h-64" />
          </div>
        )}
        {error != null && <p className="text-sm text-failure">{String(error)}</p>}
        {item && <PanelBody item={item} />}
      </div>
    </aside>
  )
}

function PanelNavButton({ targetId, label, icon }: { targetId?: string; label: string; icon: React.ReactNode }) {
  if (!targetId) {
    return (
      <Button variant="ghost" size="icon" aria-label={label} disabled>
        {icon}
      </Button>
    )
  }
  return (
    <Link to="/tasks/$taskId" params={{ taskId: targetId }} aria-label={label}>
      <Button variant="ghost" size="icon" tabIndex={-1}>
        {icon}
      </Button>
    </Link>
  )
}

function PanelBody({ item }: { item: ActivityItem }) {
  const provenance = parseProvenance(item.task.source)
  const reviewable = item.needs_attention || item.task.state === 'approved'
  return (
    <div className="space-y-4">
      <div>
        <div className="mb-2 flex flex-wrap items-center gap-1.5">
          <Badge variant={item.needs_attention ? 'attention' : item.task.state === 'merged' ? 'positive' : 'outline'}>
            {taskStateLabels[item.task.state] ?? item.task.state}
          </Badge>
          <Badge variant="mono">{item.task.level || 'L2'}</Badge>
          {item.task.class && <Badge>{item.task.class}</Badge>}
          <Badge variant="accent">{provenance.label}</Badge>
        </div>
        <h2 className="text-base font-semibold leading-snug tracking-tight">{item.task.title}</h2>
        {item.task.body && <p className="mt-1.5 text-sm leading-6 text-muted">{item.task.body}</p>}
      </div>
      {reviewable && <ReviewPanel item={item} />}
      {item.work_orders?.map((order) => (
        <div key={order.id} className="rounded-lg border border-border bg-surface p-3">
          <div className="flex items-center gap-2">
            <Badge variant="accent">{order.stage} work order</Badge>
            <Badge variant={order.state === 'claimed' ? 'positive' : order.state === 'timed_out' ? 'failure' : order.state === 'stale' ? 'attention' : 'mono'}>{order.state.replaceAll('_', ' ')}</Badge>
            <Badge variant="outline">{order.claimable ? 'claimable' : 'not claimable'}</Badge>
          </div>
          <p className="mt-2 text-xs text-muted">
            {order.state === 'stale'
              ? 'Queue retention elapsed; redispatch is required.'
              : order.state === 'timed_out'
                ? 'Execution deadline elapsed; use the existing retry policy.'
                : order.claimed_by
                  ? `Claimed by ${order.claimed_by}${order.agent ? ` · ${order.agent}` : ''}${order.model ? ` · ${order.model}` : ''}`
                  : 'Waiting for an operator-owned agent over MCP.'}
          </p>
          <p className="mt-1 text-[11px] text-faint">Queued {new Date(order.queue_entered_at).toLocaleString()} · queue deadline {new Date(order.queue_deadline).toLocaleString()}</p>
          {order.execution_deadline && <p className="mt-1 text-[11px] text-faint">Execution deadline {new Date(order.execution_deadline).toLocaleString()}</p>}
          {order.lease_expires_at && <p className="mt-1 text-[11px] text-faint">Lease expires {new Date(order.lease_expires_at).toLocaleString()} · usage self-reported</p>}
          {order.progress && <p className="mt-2 text-xs">{order.progress}</p>}
        </div>
      ))}
      {(item.task.state === 'queued' || item.task.state === 'closed') && <RedispatchCard item={item} />}
      <Timeline item={item} />
      {item.spec && <SpecCard spec={item.spec} />}
      <SummaryRail item={item} />
    </div>
  )
}

// Task management beyond the gate: nudge a stuck queued task (or reopen a
// closed one with a decided stage) back into dispatch.
function RedispatchCard({ item }: { item: ActivityItem }) {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => redispatchTask(item.task.id, token),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.id] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  return (
    <div className="flex items-center justify-between gap-3 rounded-lg border border-border bg-surface px-3 py-2.5">
      <p className="text-xs text-muted">
        {item.task.state === 'queued' ? 'Queued — re-enqueue if dispatch stalled.' : 'Closed — redispatch resumes at the decided stage.'}
      </p>
      <Button variant="secondary" size="sm" disabled={!token || mutation.isPending} onClick={() => mutation.mutate()}>
        <RotateCcw />
        {mutation.isPending ? 'Dispatching…' : 'Redispatch'}
      </Button>
      {mutation.error != null && <p className="text-xs text-failure">{String(mutation.error)}</p>}
    </div>
  )
}
