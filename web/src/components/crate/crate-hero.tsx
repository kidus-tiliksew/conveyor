// The top of a crate's page (req-crates AC-5.2): the name, the open, merged,
// and total counts, the shipped or closed statement, the target date, and a
// bar of merged work. Every number and `shipped` is the server's
// (component-crates invariant 9).
import { Archive, CalendarPlus, CircleCheck } from 'lucide-react'
import { useEffect, useState } from 'react'
import type { CrateDetail } from '../../lib/types'
import { cn } from '../../lib/utils'
import { Button } from '../ui/button'
import { formatTargetDate, targetDateHint } from './crate-parts'

export function CrateHero({
  crate,
  canAct,
  onSetDate,
}: {
  crate: CrateDetail
  canAct: boolean
  onSetDate: () => void
}) {
  const open = crate.state === 'open'
  const total = crate.total_task_count
  return (
    <section aria-label="Crate summary" className="rounded-xl border border-border bg-card px-5 py-4 sm:px-6">
      <div className="grid grid-cols-[minmax(0,1fr)_auto] items-start gap-x-4">
        <div className="min-w-0">
          <h1 className="line-clamp-2 break-words text-xl font-semibold tracking-tight sm:text-2xl">{crate.name}</h1>
          <p className="mt-1 text-sm text-muted">
            <span data-stat="merged" className="font-medium tabular-nums text-foreground">
              {crate.merged_task_count}
            </span>{' '}
            of{' '}
            <span data-stat="total" className="tabular-nums">
              {total}
            </span>{' '}
            merged
            <span aria-hidden className="mx-1.5 text-faint">
              ·
            </span>
            <span className="sr-only">, </span>
            <span data-stat="open" className="tabular-nums">
              {crate.open_task_count}
            </span>{' '}
            open
          </p>
        </div>
        {open && <Due crate={crate} canAct={canAct} onSetDate={onSetDate} />}
      </div>

      {open && total > 0 && !crate.shipped && <MergedBar merged={crate.merged_task_count} total={total} />}

      {crate.shipped ? (
        <p data-shipped-statement className="mt-3 flex items-center gap-1.5 text-sm font-medium text-positive">
          <CircleCheck aria-hidden className="size-4 shrink-0" />
          Shipped. Every task in this crate is merged or closed.
        </p>
      ) : (
        !open && (
          <p className="mt-3 flex items-center gap-1.5 text-sm text-muted">
            <Archive aria-hidden className="size-3.5 shrink-0" />
            Closed{crate.closed_at ? ` on ${formatDay(crate.closed_at)}` : ''}
          </p>
        )
      )}
    </section>
  )
}

// The merged share, growing in from the left once when the page opens. Later
// refreshes move it without replaying, and reduced motion shows it at once.
function MergedBar({ merged, total }: { merged: number; total: number }) {
  const [revealed, setRevealed] = useState(
    () => typeof window !== 'undefined' && window.matchMedia('(prefers-reduced-motion: reduce)').matches,
  )
  useEffect(() => {
    if (revealed) return
    let second = 0
    // Two frames, so the empty state is painted before the wipe starts.
    const first = requestAnimationFrame(() => {
      second = requestAnimationFrame(() => setRevealed(true))
    })
    return () => {
      cancelAnimationFrame(first)
      cancelAnimationFrame(second)
    }
  }, [revealed])

  return (
    <div aria-hidden className="mt-4 h-1.5 overflow-hidden rounded-full bg-raised">
      <div
        className="h-full rounded-full bg-positive transition-[width] duration-500 ease-out motion-reduce:transition-none"
        style={{ width: revealed ? `${(merged / total) * 100}%` : '0%' }}
      />
    </div>
  )
}

// The target date of an open crate and how far off it is, worded as on the
// crates list.
function Due({ crate, canAct, onSetDate }: { crate: CrateDetail; canAct: boolean; onSetDate: () => void }) {
  if (!crate.target_date)
    return canAct ? (
      <Button variant="ghost" size="sm" onClick={onSetDate} className="-mr-2.5">
        <CalendarPlus aria-hidden />
        Set a target date
      </Button>
    ) : null
  const hint = targetDateHint(crate)
  return (
    <div className="text-right">
      <p className="text-xs text-faint">Target date</p>
      <p data-target-date className="text-sm font-medium">
        {formatTargetDate(crate.target_date)}
      </p>
      {hint && <p className={cn('text-xs', hint.late ? 'font-medium text-attention' : 'text-muted')}>{hint.text}</p>}
    </div>
  )
}

function formatDay(value: string) {
  return new Date(value).toLocaleDateString('en', { month: 'short', day: 'numeric', year: 'numeric' })
}
