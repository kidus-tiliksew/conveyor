import { Link } from '@tanstack/react-router'
import { gateBadge, parseProvenance } from '../../lib/activity'
import type { ActivitySummary } from '../../lib/types'
import { cn, relativeTime } from '../../lib/utils'
import { Badge } from '../ui/badge'

// Deterministic repo hue so a repo reads consistently across the board.
const repoHues = ['#6d28d9', '#1a7f37', '#2563eb', '#c2410c', '#0f766e', '#be185d']

function repoColor(seed: string) {
  let hash = 0
  for (const char of seed) hash = (hash * 31 + char.charCodeAt(0)) | 0
  return repoHues[Math.abs(hash) % repoHues.length]
}

// One board card (spec §13.3): ID, title, escalation-level badge, provenance
// chip, recency — "Needs attention" is the only alarm on the page.
export function TaskCard({ item, selected }: { item: ActivitySummary; selected: boolean }) {
  const provenance = parseProvenance(item.task.source)
  const lastAt = item.last_event_at || item.task.created_at
  const gate = gateBadge(item)
  return (
    <Link
      to="/tasks/$taskId"
      params={{ taskId: item.task.id }}
      className={cn(
        'block rounded-lg border bg-card p-3 transition-colors',
        selected ? 'border-primary bg-primary-soft/40' : 'border-border hover:border-edge',
      )}
    >
      <p className="line-clamp-2 text-sm font-medium leading-snug">{item.task.title}</p>
      <div className="mt-1.5 flex items-baseline gap-2 font-mono text-[11px] text-faint">
        <span className="truncate">{item.task.id}</span>
        <span className="ml-auto shrink-0 whitespace-nowrap">{relativeTime(lastAt)}</span>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-1.5">
        {gate && <Badge variant={gate.variant}>{gate.label}</Badge>}
        <Badge variant="mono">{item.task.level || 'L2'}</Badge>
        {item.task.class && <Badge>{item.task.class}</Badge>}
        <Badge variant="accent" className="max-w-36 truncate">{provenance.label}</Badge>
      </div>
      {item.task.repo && (
        <div className="mt-2 flex items-center gap-1.5 text-[11px] text-faint">
          <span aria-hidden className="size-2 shrink-0 rounded-full" style={{ backgroundColor: repoColor(item.task.repo) }} />
          <span className="truncate">{item.task.repo}</span>
        </div>
      )}
    </Link>
  )
}
