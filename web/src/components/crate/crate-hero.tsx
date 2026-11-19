// The top of a crate's page (req-crates AC-5.2): the name, the shipped or
// closed statement, the open, merged, and total counts, the target date, and
// one bar showing where the member tasks stand. The counts and `shipped` are
// the server's (component-crates invariant 9); the bar groups the member rows
// the same read returns.
import { Archive, CircleCheck } from 'lucide-react'
import { useEffect, useState } from 'react'
import { taskStateLabels } from '../../lib/contracts'
import type { CrateDetail, TaskState } from '../../lib/types'
import { cn } from '../../lib/utils'
import { daysUntil, formatTargetDate } from './crate-parts'

// Furthest along first, so the bar fills left to right as work lands.
const segments: { states: TaskState[]; fill: string }[] = [
  { states: ['merged'], fill: 'bg-positive' },
  { states: ['approved'], fill: 'bg-positive/50' },
  { states: ['awaiting_human'], fill: 'bg-attention' },
  { states: ['running', 'claiming'], fill: 'bg-info' },
  { states: ['queued'], fill: 'bg-edge' },
  { states: ['parked'], fill: 'bg-failure/70' },
  { states: ['closed'], fill: 'bg-edge/50' },
]

export function CrateHero({
  crate,
  canAct,
  onSetDate,
}: {
  crate: CrateDetail
  canAct: boolean
  onSetDate: () => void
}) {
  // The bar grows in once on first paint; reduced motion skips it.
  const [shown, setShown] = useState(false)
  useEffect(() => {
    const frame = requestAnimationFrame(() => setShown(true))
    return () => cancelAnimationFrame(frame)
  }, [])

  const groups = segments
    .map((segment) => ({
      ...segment,
      label: taskStateLabels[segment.states[0]] ?? segment.states[0],
      count: crate.tasks.filter((task) => segment.states.includes(task.state)).length,
    }))
    .filter((group) => group.count > 0)

  return (
    <section aria-label="Crate summary" className="rounded-xl border border-border bg-card px-5 py-5 sm:px-6">
      <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
        <div className="min-w-0">
          <h1 className="truncate text-xl font-semibold tracking-tight sm:text-2xl">{crate.name}</h1>
          <Summary crate={crate} />
        </div>
        <Due crate={crate} canAct={canAct} onSetDate={onSetDate} />
      </div>

      <div
        role="img"
        aria-label={
          groups.length > 0 ? groups.map((group) => `${group.count} ${group.label}`).join(', ') : 'No tasks yet'
        }
        className="mt-5 flex h-2 gap-0.5 overflow-hidden rounded-full bg-raised"
      >
        {groups.map((group, index) => (
          <span
            key={group.fill}
            className={cn(
              'h-full transition-[flex-grow] duration-700 ease-out motion-reduce:transition-none',
              group.fill,
            )}
            style={{ flexGrow: shown ? group.count : 0, flexBasis: 0, transitionDelay: `${index * 70}ms` }}
          />
        ))}
      </div>

      {groups.length > 0 && (
        <ul className="mt-3 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
          {groups.map((group) => (
            <li key={group.fill} className="flex items-center gap-1.5">
              <span aria-hidden className={cn('size-2 rounded-full', group.fill)} />
              {group.label}
              <span className="tabular-nums text-foreground">{group.count}</span>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function Summary({ crate }: { crate: CrateDetail }) {
  if (crate.state === 'closed')
    return (
      <p className="mt-1 flex items-center gap-1.5 text-sm text-muted">
        <Archive aria-hidden className="size-3.5 shrink-0" />
        Closed{crate.closed_at ? ` on ${formatDay(crate.closed_at)}` : ''}
      </p>
    )
  if (crate.shipped)
    return (
      <p data-shipped-statement className="mt-1 flex items-center gap-1.5 text-sm font-medium text-positive">
        <CircleCheck aria-hidden className="size-4 shrink-0" />
        Shipped. Every task in this crate is merged or closed.
      </p>
    )
  return (
    <p className="mt-1 text-sm text-muted">
      <span data-stat="merged" className="font-medium tabular-nums text-foreground">
        {crate.merged_task_count}
      </span>{' '}
      of{' '}
      <span data-stat="total" className="tabular-nums">
        {crate.total_task_count}
      </span>{' '}
      merged
      <span aria-hidden className="mx-1.5 text-faint">
        ·
      </span>
      <span data-stat="open" className="tabular-nums">
        {crate.open_task_count}
      </span>{' '}
      open
    </p>
  )
}

// The target date and how far off it is. Late shows only on an open crate
// that has not shipped.
function Due({ crate, canAct, onSetDate }: { crate: CrateDetail; canAct: boolean; onSetDate: () => void }) {
  if (!crate.target_date) {
    if (!canAct || crate.state !== 'open') return null
    return (
      <button type="button" onClick={onSetDate} className="text-sm text-muted hover:text-foreground">
        Set a target date
      </button>
    )
  }
  const days = daysUntil(crate.target_date)
  const active = crate.state === 'open' && !crate.shipped
  const late = active && days < 0
  const hint = !active
    ? null
    : days === 0
      ? 'Due today'
      : days > 0
        ? `${days} ${days === 1 ? 'day' : 'days'} left`
        : `${-days} ${days === -1 ? 'day' : 'days'} late`
  return (
    <div className="text-sm sm:text-right">
      <p data-target-date className="font-medium">
        {formatTargetDate(crate.target_date)}
      </p>
      {hint && <p className={cn('text-xs', late ? 'font-medium text-attention' : 'text-muted')}>{hint}</p>}
    </div>
  )
}

function formatDay(value: string) {
  return new Date(value).toLocaleDateString('en', { month: 'short', day: 'numeric' })
}
