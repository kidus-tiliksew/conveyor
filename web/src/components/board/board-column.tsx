import {
  Archive,
  Code2,
  FileText,
  GitPullRequest,
  ListFilter,
  ShieldCheck,
  UserRound,
  type LucideIcon,
} from 'lucide-react'
import type { GroupKey } from '../../lib/contracts'
import type { ActivitySummary } from '../../lib/types'
import { Badge } from '../ui/badge'
import { TaskCard } from './task-card'

// Neutral column identity: position and icon carry the stage; color is
// reserved for state (attention/positive) per the visual-economy rule.
const groupIcons: Record<GroupKey, LucideIcon> = {
  triage: ListFilter,
  spec: FileText,
  implement: Code2,
  review: GitPullRequest,
  verify: ShieldCheck,
  human: UserRound,
  done: Archive,
}

// Terminal work archives under "Completed"; keep the column light so the
// board stays a WIP view rather than a history dump.
const DONE_CAP = 30

export function BoardColumn({
  groupKey,
  label,
  items,
  selectedId,
}: {
  groupKey: GroupKey
  label: string
  items: ActivitySummary[]
  selectedId?: string
}) {
  const Icon = groupIcons[groupKey]
  const attention = items.filter((item) => item.needs_attention).length
  const visible = groupKey === 'done' ? items.slice(0, DONE_CAP) : items
  return (
    <section aria-label={label} className="flex h-full w-72 shrink-0 flex-col rounded-lg border border-border bg-surface">
      <header className="flex shrink-0 items-center gap-2 border-b border-border px-3 py-2.5">
        <Icon className="size-3.5 text-muted" />
        <h2 className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted">{label}</h2>
        <span className="font-mono text-[11px] text-faint">{items.length}</span>
        {attention > 0 && groupKey !== 'human' && <Badge variant="attention" className="ml-auto">{attention}</Badge>}
      </header>
      <div className="min-h-0 flex-1 space-y-2 overflow-y-auto p-2">
        {visible.map((item) => (
          <TaskCard key={item.task.id} item={item} selected={item.task.id === selectedId} />
        ))}
        {items.length === 0 && <p className="px-2 py-1.5 text-xs text-faint">Nothing in this stage.</p>}
        {items.length > visible.length && (
          <p className="px-2 py-1.5 text-center text-xs text-faint">+{items.length - visible.length} more completed</p>
        )}
      </div>
    </section>
  )
}
