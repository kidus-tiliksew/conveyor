// The crate's delivery numbers in a sheet beside the page
// (req-crate-insights REQ-4; component-crates invariant 33): the figures, the
// first-review approvals, the forecast statement, the burn chart, and the
// tasks merged by week. Every number is the insight view's `progress`,
// `first_review`, and `forecast_days_before_target`. The sheet formats them
// and computes none (component-crates invariant 21). The charts are the
// requirement insight panel's own components, used as they stand
// (component-requirement-insights). Ported from the approved prototype
// (feature-crate-insights CI-4).
import { RotateCw, X } from 'lucide-react'
import { type ReactNode, useState } from 'react'
import type { CrateDetail, CrateInsight } from '../../lib/types'
import { cn } from '../../lib/utils'
import {
  BurnChart,
  BurnLegend,
  type BurnMode,
  BurnTable,
  ChartCard,
  WeeklyBars,
  WeeklyTable,
} from '../insights/progress-charts'
import { formatDate, formatDuration, formatNumber } from '../insights/shared'
import { Button } from '../ui/button'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'

// The sheet. It opens while the insight view loads or after the read fails,
// and then says so with a way to try again.
export function DeliveryStatsSheet({
  crate,
  insight,
  insightError,
  retryingInsight,
  onRetryInsight,
  onClose,
}: {
  crate: CrateDetail
  insight: CrateInsight | undefined
  insightError: unknown
  retryingInsight: boolean
  onRetryInsight: () => void
  onClose: () => void
}) {
  return (
    <Sheet onClose={onClose} label="Delivery stats" width="w-full sm:w-[27rem]">
      <header className="flex items-start justify-between gap-3 border-b border-border px-4 py-3">
        <div className="min-w-0">
          <h2 className="text-base font-semibold tracking-tight">Delivery stats</h2>
          <p className="mt-0.5 truncate text-xs text-muted">{crate.name}</p>
        </div>
        <Button variant="ghost" size="icon" aria-label="Close" onClick={onClose}>
          <X />
        </Button>
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">
        {insight ? (
          <DeliveryStats insight={insight} />
        ) : insightError != null ? (
          <div data-stats-error className="flex flex-wrap items-center gap-2 text-sm text-muted">
            <span>
              The delivery numbers did not load.
              {insightError instanceof Error && insightError.message ? ` ${insightError.message}` : ''}
            </span>
            <Button variant="outline" size="sm" onClick={onRetryInsight} disabled={retryingInsight}>
              <RotateCw aria-hidden />
              Try again
            </Button>
          </div>
        ) : (
          <div data-stats-loading className="grid gap-2.5">
            <Skeleton className="h-32 w-full" />
            <Skeleton className="h-48 w-full" />
          </div>
        )}
      </div>
    </Sheet>
  )
}

// The forecast statement (AC-4.3). With no open task it says that every task
// is merged or closed. With open tasks and no forecast it says that no
// forecast exists. Otherwise it names the served finish day and, when the
// view serves the signed day count against the target date, how far before
// or after that date it falls.
function outlookOf(insight: CrateInsight): { text: ReactNode; tone: 'positive' | 'attention' | 'muted' } {
  const { progress } = insight
  if (progress.open === 0) return { text: 'Every task is merged or closed.', tone: 'positive' }
  if (!progress.forecast)
    return { text: 'No forecast: no task in this crate merged in the last 14 days.', tone: 'muted' }
  const finish = (
    <b data-forecast-day className="font-semibold text-foreground">
      {formatDate(progress.forecast.day)}
    </b>
  )
  const days = insight.forecast_days_before_target
  if (days === null) return { text: <>At this rate the open tasks finish around {finish}.</>, tone: 'muted' }
  if (days === 0)
    return { text: <>At this rate the open tasks finish around {finish}, on the target date.</>, tone: 'positive' }
  const count = Math.abs(days)
  const unit = count === 1 ? 'day' : 'days'
  if (days > 0)
    return {
      text: (
        <>
          At this rate the open tasks finish around {finish}, {count} {unit} before the target date.
        </>
      ),
      tone: 'positive',
    }
  return {
    text: (
      <>
        At this rate the open tasks finish around {finish}, {count} {unit} after the target date.
      </>
    ),
    tone: 'attention',
  }
}

export function DeliveryStats({ insight }: { insight: CrateInsight }) {
  const [mode, setMode] = useState<BurnMode>('down')
  const data = insight.progress
  const cycle = data.median_cycle_seconds === null ? undefined : formatDuration(data.median_cycle_seconds)
  // The share of reviewed tasks whose first review round approved, written
  // from the two served counts. With no reviewed task there is no share.
  const { approved, reviewed } = insight.first_review
  const firstPass = reviewed > 0 ? Math.round((approved / reviewed) * 100) : undefined
  const outlook = outlookOf(insight)

  return (
    <section aria-label="Delivery" data-delivery-stats className="min-w-0">
      <div className="grid gap-2.5">
        <div>
          <dl className="grid grid-cols-2 gap-1.5">
            <Figure stat="merged" label="Merged" value={data.delivered} unit={`/${data.total}`} />
            <Figure stat="open" label="Open" value={data.open} />
            <Figure stat="throughput" label="Throughput" value={formatNumber(data.throughput_per_week)} unit="/wk" />
            <Figure
              stat="cycle"
              label="Median cycle"
              value={cycle?.value ?? 'none'}
              unit={cycle?.unit}
              muted={!cycle}
            />
          </dl>
          <div data-first-review className="mt-1.5 rounded-lg border border-border bg-card px-2.5 py-2">
            <div className="flex items-baseline justify-between gap-2">
              <span className="text-xs text-muted">Approved on first review</span>
              <span
                data-first-review-share
                className={cn('text-sm font-semibold tabular-nums', firstPass === undefined && 'text-muted')}
              >
                {firstPass === undefined ? 'none' : `${firstPass}%`}
              </span>
            </div>
            <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-raised">
              <div
                className="h-full rounded-full"
                style={{ width: `${firstPass ?? 0}%`, background: 'var(--color-document)' }}
              />
            </div>
            <p data-first-review-count className="mt-1 text-[11px] text-faint">
              {reviewed} {reviewed === 1 ? 'task has' : 'tasks have'} been reviewed.
            </p>
          </div>
          <p
            data-forecast={outlook.tone}
            className={cn(
              'mt-1.5 rounded-lg border px-2.5 py-2 text-xs leading-relaxed',
              outlook.tone === 'positive' && 'border-transparent bg-positive-soft text-positive',
              outlook.tone === 'attention' && 'border-transparent bg-attention-soft text-attention',
              outlook.tone === 'muted' && 'border-border bg-card text-muted',
            )}
          >
            {outlook.text}
          </p>
        </div>

        <ChartCard
          title={mode === 'down' ? 'Burn-down' : 'Burn-up'}
          subtitle={mode === 'down' ? 'open tasks vs. scope' : 'merged tasks vs. scope'}
          action={
            <fieldset aria-label="Series" className="m-0 flex rounded-md bg-raised p-0.5">
              {(
                [
                  ['down', 'Burn-down'],
                  ['up', 'Burn-up'],
                ] as const
              ).map(([value, label]) => (
                <button
                  key={value}
                  type="button"
                  aria-pressed={mode === value}
                  onClick={() => setMode(value)}
                  className={cn(
                    'rounded px-2 py-0.5 text-[11px] transition-colors',
                    mode === value ? 'bg-card text-foreground shadow-sm' : 'text-muted hover:text-foreground',
                  )}
                >
                  {label}
                </button>
              ))}
            </fieldset>
          }
          table={<BurnTable progress={data} />}
        >
          <BurnLegend mode={mode} forecast={Boolean(data.forecast)} />
          <BurnChart progress={data} mode={mode} />
        </ChartCard>

        <ChartCard title="Merged per week" subtitle="last eight weeks" table={<WeeklyTable progress={data} />}>
          <WeeklyBars progress={data} />
        </ChartCard>
      </div>
    </section>
  )
}

function Figure({
  stat,
  label,
  value,
  unit,
  muted,
}: {
  stat: string
  label: string
  value: ReactNode
  unit?: string
  muted?: boolean
}) {
  return (
    <div
      data-figure={stat}
      className="flex min-w-0 flex-col-reverse justify-end rounded-lg border border-border bg-card px-2.5 py-2"
    >
      <dt className="mt-0.5 text-xs leading-4 text-muted">{label}</dt>
      <dd
        className={cn(
          'm-0 truncate text-lg font-semibold tabular-nums leading-tight',
          muted ? 'text-muted' : 'text-foreground',
        )}
      >
        {value}
        {unit && <small className="ml-px text-xs font-normal text-faint">{unit}</small>}
      </dd>
    </div>
  )
}
