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

const groupStyle: Record<GroupKey, { icon: LucideIcon; header: string; title: string }> = {
  triage: { icon: ListFilter, header: 'bg-raised', title: 'text-foreground' },
  spec: { icon: FileText, header: 'bg-[#f5f0fe]', title: 'text-[#6d28d9]' },
  implement: { icon: Code2, header: 'bg-[#eef4fe]', title: 'text-[#2563eb]' },
  review: { icon: GitPullRequest, header: 'bg-[#f7f0fb]', title: 'text-[#7e22ce]' },
  verify: { icon: ShieldCheck, header: 'bg-[#ecfaf5]', title: 'text-[#0f766e]' },
  human: { icon: UserRound, header: 'bg-raised', title: 'text-foreground' },
  done: { icon: Archive, header: 'bg-raised', title: 'text-muted' },
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
  const style = groupStyle[groupKey]
  const Icon = style.icon
  const attention = items.filter((item) => item.needs_attention).length
  const visible = groupKey === 'done' ? items.slice(0, DONE_CAP) : items
  return (
    <section aria-label={label} className="flex h-full w-72 shrink-0 flex-col rounded-xl bg-surface">
      <header className={cn('flex shrink-0 items-center gap-2 rounded-t-xl px-3 py-2.5', style.header)}>
        <Icon className={cn('size-3.5', style.title)} />
        <h2 className={cn('text-sm font-semibold', style.title)}>{label}</h2>
        <span className="text-sm text-faint">{items.length}</span>
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
