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
import { cn } from '../../lib/utils'
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
  // An idle stage narrows rather than reserving a full lane. Seven stages
  // never fit a desktop at full width, so the columns that hold work earn the
  // space — and the ones that need a decision stay on screen more often.
  const idle = items.length === 0
  return (
    <section
      aria-label={label}
      className={cn(
        'flex h-full shrink-0 flex-col rounded-lg border border-border transition-[width]',
        idle ? 'w-44 bg-surface/50' : 'w-72 bg-surface',
      )}
    >
      <header className="flex shrink-0 items-center gap-2 border-b border-border px-3 py-2.5">
        <Icon className="size-3.5 shrink-0 text-muted" />
        <h2 className="truncate text-[11px] font-semibold uppercase tracking-[0.08em] text-muted">{label}</h2>
        <span className="font-mono text-[11px] text-faint">{items.length}</span>
        {attention > 0 && groupKey !== 'human' && <Badge variant="attention" className="ml-auto">{attention}</Badge>}
      </header>
      {!idle && (
        <div className="min-h-0 flex-1 space-y-2 overflow-y-auto p-2">
          {visible.map((item) => (
            <TaskCard key={item.task.id} item={item} selected={item.task.id === selectedId} />
          ))}
          {items.length > visible.length && (
            <p className="px-2 py-1.5 text-center text-xs text-faint">+{items.length - visible.length} more completed</p>
          )}
        </div>
      )}
    </section>
  )
}
