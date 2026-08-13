import { Link } from '@tanstack/react-router'
import { gateBadge, humanizeClaimRefusal, reviewDiagnosticBadge } from '../../lib/activity'
import type { ActivitySummary } from '../../lib/types'
import { cn, relativeTime } from '../../lib/utils'
import { AssigneeChip } from '../task/assignee-chip'
import { Badge } from '../ui/badge'

// Deterministic repo hue so a repo reads consistently across the board.
const repoHues = ['#3355f5', '#0f766e', '#1a7f37', '#b3540e', '#7c3aed', '#be185d']

function repoColor(seed: string) {
  let hash = 0
  for (const char of seed) hash = (hash * 31 + char.charCodeAt(0)) | 0
  return repoHues[Math.abs(hash) % repoHues.length]
}

// One board card: title, a single quiet meta
// line, and chips only for state that changes what the operator does next.
// Class and provenance are metadata, not signal — they live in the task
// header, so a healthy card carries no chips at all and an exception stands
// out on an otherwise calm board.
export function TaskCard({ item, selected }: { item: ActivitySummary; selected: boolean }) {
  const lastAt = item.last_event_at || item.task.created_at
  const gate = gateBadge(item)
  const reviewDiagnostic = reviewDiagnosticBadge(item)
  const unsatisfiable = item.stalled?.unsatisfiable_edge === true
  const blockingIDs = item.stalled?.blocking_task_ids ?? item.task.blocking_task_ids ?? []
  const blockingTitles = blockingIDs.map(
    (id) => item.task.dependencies?.find((dependency) => dependency.id === id)?.title ?? id,
  )
  const dependencyExplanation = unsatisfiable
    ? `Needs attention: ${blockingTitles.join(', ')} closed without merging`
    : `Waiting for ${blockingTitles.join(', ')}`
  const chips = Boolean(gate || reviewDiagnostic || item.task.hold || blockingIDs.length > 0)
  return (
    <Link
      to="/tasks/$taskId"
      params={{ taskId: item.task.id }}
      className={cn(
        'group/card block rounded-lg border bg-card p-3 transition-colors',
        selected ? 'border-primary bg-primary-soft/40' : 'border-border hover:border-edge',
      )}
    >
      <p className="line-clamp-2 text-sm font-medium leading-snug">{item.task.title}</p>
      <div className="mt-1.5 flex items-baseline gap-2 font-mono text-[11px] text-faint">
        {item.task.repo && (
          <span
            aria-hidden
            title={item.task.repo}
            className="size-2 shrink-0 self-center rounded-full"
            style={{ backgroundColor: repoColor(item.task.repo) }}
          />
        )}
        <span className="truncate">{item.task.id}</span>
        <span className="ml-auto shrink-0 whitespace-nowrap">{relativeTime(lastAt)}</span>
      </div>
      {/* The same chip the Tasks rows carry, so a colleague's assignment reads
          identically on both surfaces (REQ-4, AC-4.3). */}
      {item.task.assignee && <AssigneeChip assignee={item.task.assignee} className="mt-1 text-[11px] text-muted" />}
      {chips && (
        <div className="mt-2 flex flex-wrap items-center gap-1.5">
          {gate && <Badge variant={gate.variant}>{gate.label}</Badge>}
          {reviewDiagnostic && <Badge variant={reviewDiagnostic.variant}>{reviewDiagnostic.label}</Badge>}
          {item.task.hold && <Badge variant="mono">Held</Badge>}
          {blockingIDs.length > 0 && (
            <span role="img" className="group/dependency relative inline-flex" aria-label={dependencyExplanation}>
              <Badge variant={unsatisfiable ? 'attention' : 'mono'}>
                {unsatisfiable ? 'Dependency needs attention' : 'Waiting on dependencies'}
              </Badge>
              <span
                role="tooltip"
                className="pointer-events-none absolute bottom-full left-0 z-10 mb-1.5 w-60 rounded-md bg-foreground px-2.5 py-1.5 text-[11px] leading-4 text-background opacity-0 shadow-md transition-opacity after:absolute after:left-3 after:top-full after:border-4 after:border-transparent after:border-t-foreground group-hover/dependency:opacity-100 group-focus-visible/card:opacity-100"
              >
                {dependencyExplanation}
              </span>
            </span>
          )}
        </div>
      )}
      {item.stalled?.last_failure && (
        <p className="mt-2 line-clamp-2 text-[11px] leading-4 text-failure">
          {humanizeClaimRefusal(item.stalled.last_failure, item.task.assignee)}
        </p>
      )}
      {item.forge_failure && (
        <p className="mt-2 line-clamp-2 text-[11px] leading-4 text-failure">
          {item.forge_failure.category && (
            <>
              <span className="font-mono">{item.forge_failure.category}</span>
              {' · '}
            </>
          )}
          {item.forge_failure.surface}: {item.forge_failure.detail}
        </p>
      )}
    </Link>
  )
}
