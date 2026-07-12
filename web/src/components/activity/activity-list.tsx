import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import {
  Archive,
  ChevronDown,
  Code2,
  FileText,
  GitPullRequest,
  ListFilter,
  Search,
  ShieldCheck,
  UserRound,
  type LucideIcon,
} from 'lucide-react'
import { groupForSummary, parseProvenance } from '../../lib/activity'
import { stageGroups, type GroupKey } from '../../lib/contracts'
import type { ActivitySummary } from '../../lib/types'
import { cn, relativeTime } from '../../lib/utils'
import { useActivity } from '../app-shell'
import { Badge } from '../ui/badge'
import { Input } from '../ui/input'
import { Skeleton } from '../ui/skeleton'

const groupStyle: Record<GroupKey, { icon: LucideIcon; header: string; title: string }> = {
  triage: { icon: ListFilter, header: 'bg-surface', title: 'text-foreground' },
  spec: { icon: FileText, header: 'bg-[#f5f0fe]', title: 'text-[#6d28d9]' },
  implement: { icon: Code2, header: 'bg-[#eef4fe]', title: 'text-[#2563eb]' },
  review: { icon: GitPullRequest, header: 'bg-[#f7f0fb]', title: 'text-[#7e22ce]' },
  verify: { icon: ShieldCheck, header: 'bg-[#ecfaf5]', title: 'text-[#0f766e]' },
  human: { icon: UserRound, header: 'bg-surface', title: 'text-foreground' },
  done: { icon: Archive, header: 'bg-surface', title: 'text-muted' },
}

// The stage-grouped feed (spec §13.3 element 1): the distribution of work
// across stages is the factory's health made visible.
export function ActivityList({ selectedId }: { selectedId?: string }) {
  const { data, isLoading, error } = useActivity()
  const [collapsed, setCollapsed] = useState<Partial<Record<GroupKey, boolean>>>({ done: true })
  const [query, setQuery] = useState('')

  const grouped = useMemo(() => {
    const needle = query.trim().toLowerCase()
    const byGroup = new Map<GroupKey, ActivitySummary[]>()
    for (const item of data ?? []) {
      if (
        needle &&
        !item.task.title.toLowerCase().includes(needle) &&
        !item.task.id.includes(needle) &&
        !item.task.source.toLowerCase().includes(needle)
      ) {
        continue
      }
      const key = groupForSummary(item)
      byGroup.set(key, [...(byGroup.get(key) ?? []), item])
    }
    for (const items of byGroup.values()) {
      items.sort(
        (a, b) =>
          new Date(b.last_event_at || b.task.created_at).getTime() -
          new Date(a.last_event_at || a.task.created_at).getTime(),
      )
    }
    return byGroup
  }, [data, query])

  return (
    <div className="flex h-full flex-col">
      <header className="flex shrink-0 items-center justify-between gap-4 border-b border-border px-6 py-3.5">
        <h1 className="text-lg font-semibold tracking-tight">Activity</h1>
        <label className="relative">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-faint" />
          <Input
            type="search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Search tasks"
            className="h-8 w-56 pl-8 text-xs"
          />
        </label>
      </header>
      <div className="@container min-h-0 flex-1 overflow-y-auto px-4 py-4">
        {isLoading && (
          <div className="space-y-3">
            {Array.from({ length: 6 }, (_, i) => (
              <Skeleton key={i} className="h-14" />
            ))}
          </div>
        )}
        {error != null && (
          <p className="rounded-lg bg-failure-soft p-3 text-sm text-failure">Activity feed unavailable: {String(error)}</p>
        )}
        {stageGroups.map(({ key, label }) => {
          const items = grouped.get(key) ?? []
          if (isLoading || (items.length === 0 && (key === 'done' || query))) return null
          const style = groupStyle[key]
          const Icon = style.icon
          const attention = items.filter((item) => item.needs_attention).length
          const isCollapsed = collapsed[key] ?? false
          return (
            <section key={key} className="mb-2">
              <button
                type="button"
                onClick={() => setCollapsed((prev) => ({ ...prev, [key]: !isCollapsed }))}
                aria-expanded={!isCollapsed}
                className={cn('flex w-full items-center gap-2 rounded-lg px-3 py-2 text-left', style.header)}
              >
                <ChevronDown className={cn('size-3.5 text-faint transition-transform', isCollapsed && '-rotate-90')} />
                <Icon className={cn('size-3.5', style.title)} />
                <span className={cn('text-sm font-semibold', style.title)}>{label}</span>
                <span className="text-sm text-faint">{items.length}</span>
                {attention > 0 && key !== 'human' && <Badge variant="attention">{attention}</Badge>}
              </button>
              {!isCollapsed && (
                <div className="py-1">
                  {items.map((item) => (
                    <TaskRow key={item.task.id} item={item} selected={item.task.id === selectedId} />
                  ))}
                  {items.length === 0 && <p className="px-4 py-2 text-xs text-faint">Nothing in this stage.</p>}
                </div>
              )}
            </section>
          )
        })}
      </div>
    </div>
  )
}

// Deterministic avatar hue so a repo reads consistently across the feed.
const avatarHues = ['#6d28d9', '#1a7f37', '#2563eb', '#c2410c', '#0f766e', '#be185d']

function avatarColor(seed: string) {
  let hash = 0
  for (const char of seed) hash = (hash * 31 + char.charCodeAt(0)) | 0
  return avatarHues[Math.abs(hash) % avatarHues.length]
}

// One feed row (spec §13.3): ID, title, escalation-level badge, provenance
// chips, recency — "Needs attention" is the only alarm on the page.
function TaskRow({ item, selected }: { item: ActivitySummary; selected: boolean }) {
  const provenance = parseProvenance(item.task.source)
  const lastAt = item.last_event_at || item.task.created_at
  const group = groupForSummary(item)
  const Icon = groupStyle[group].icon
  return (
    <Link
      to="/tasks/$taskId"
      params={{ taskId: item.task.id }}
      className={cn(
        'flex items-center gap-3 rounded-lg border px-3 py-2.5 transition-colors',
        selected ? 'border-primary bg-primary-soft/40' : 'border-transparent hover:bg-surface',
      )}
    >
      <span className="grid size-7 shrink-0 place-items-center rounded-full border border-dashed border-edge text-muted">
        <Icon className="size-3.5" />
      </span>
      <span className="hidden w-24 shrink-0 font-mono text-xs text-faint @lg:block">{item.task.id}</span>
      <span className="min-w-0 flex-1 truncate text-sm font-medium">{item.task.title}</span>
      {item.needs_attention && <Badge variant="attention">Needs attention</Badge>}
      {item.task.class && <Badge className="hidden @2xl:inline-flex">{item.task.class}</Badge>}
      <Badge variant="mono" className="hidden @xl:inline-flex">{item.task.level || 'L2'}</Badge>
      <Badge variant="accent" className="hidden max-w-44 truncate @3xl:inline-flex">{provenance.label}</Badge>
      <span
        aria-hidden
        className="hidden size-6 shrink-0 rounded-full @2xl:block"
        style={{ backgroundColor: avatarColor(item.task.repo || item.task.workspace) }}
      />
      <span className="w-24 shrink-0 whitespace-nowrap text-right text-xs text-faint">{relativeTime(lastAt)}</span>
    </Link>
  )
}
