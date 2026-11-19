// PROTOTYPE. The crate's delivery numbers, shown in a side sheet: the same four figures, burn chart,
// and weekly bars a requirement's insight panel shows, drawn by the same
// components, plus the review first-pass rate and the forecast set against
// the crate's target date.
import { type ReactNode, useState } from 'react'
import type { CrateDetail } from '../../../lib/types'
import { cn } from '../../../lib/utils'
import {
  BurnChart,
  BurnLegend,
  type BurnMode,
  BurnTable,
  ChartCard,
  WeeklyBars,
  WeeklyTable,
} from '../../insights/progress-charts'
import { formatDate, formatDuration, formatNumber } from '../../insights/shared'
import { type CrateInsight, DAY } from './model'

export function DeliveryStats({ crate, insight }: { crate: CrateDetail; insight: CrateInsight }) {
  const [mode, setMode] = useState<BurnMode>('down')
  const data = insight.progress
  const cycle = data.median_cycle_seconds === null ? undefined : formatDuration(data.median_cycle_seconds)
  const firstPass = insight.firstPassRate === null ? undefined : Math.round(insight.firstPassRate * 100)

  // The forecast against the target date, in whole days.
  let outlook: { text: ReactNode; tone: 'positive' | 'attention' | 'muted' } | undefined
  if (data.open === 0 && data.total > 0) outlook = { text: 'Every task is merged or closed.', tone: 'positive' }
  else if (!data.forecast)
    outlook = { text: 'No forecast: no task in this crate merged in the last 14 days.', tone: 'muted' }
  else {
    const finish = <b className="font-semibold text-foreground">{formatDate(data.forecast.day)}</b>
    if (!crate.target_date) outlook = { text: <>At this rate the open tasks finish around {finish}.</>, tone: 'muted' }
    else {
      const days = Math.round((Date.parse(`${crate.target_date}T00:00:00Z`) - Date.parse(data.forecast.day)) / DAY)
      outlook =
        days >= 0
          ? {
              text: (
                <>
                  At this rate the open tasks finish around {finish},{' '}
                  {days === 0 ? 'on the target date' : `${days} ${days === 1 ? 'day' : 'days'} before the target date`}.
                </>
              ),
              tone: 'positive',
            }
          : {
              text: (
                <>
                  At this rate the open tasks finish around {finish}, {-days} {days === -1 ? 'day' : 'days'} after the
                  target date.
                </>
              ),
              tone: 'attention',
            }
    }
  }

  return (
    <section aria-label="Delivery" className="min-w-0">
      <div className="grid gap-2.5">
        <div>
          <dl className="grid grid-cols-2 gap-1.5">
            <Figure label="Merged" value={data.delivered} unit={`/${data.total}`} />
            <Figure label="Open" value={data.open} />
            <Figure label="Throughput" value={formatNumber(data.throughput_per_week)} unit="/wk" />
            <Figure label="Median cycle" value={cycle?.value ?? 'none'} unit={cycle?.unit} muted={!cycle} />
          </dl>
          <div className="mt-1.5 rounded-lg border border-border bg-card px-2.5 py-2">
            <div className="flex items-baseline justify-between gap-2">
              <span className="text-xs text-muted">Approved on first review</span>
              <span className={cn('text-sm font-semibold tabular-nums', firstPass === undefined && 'text-muted')}>
                {firstPass === undefined ? 'none' : `${firstPass}%`}
              </span>
            </div>
            <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-raised">
              <div
                className="proto-grow h-full rounded-full bg-document"
                style={{ width: `${firstPass ?? 0}%`, background: 'var(--color-document)' }}
              />
            </div>
            <p className="mt-1 text-[11px] text-faint">
              {insight.reviewedTasks} {insight.reviewedTasks === 1 ? 'task has' : 'tasks have'} been reviewed.
            </p>
          </div>
          {outlook && (
            <p
              className={cn(
                'mt-1.5 rounded-lg border px-2.5 py-2 text-xs leading-relaxed',
                outlook.tone === 'positive' && 'border-transparent bg-positive-soft text-positive',
                outlook.tone === 'attention' && 'border-transparent bg-attention-soft text-attention',
                outlook.tone === 'muted' && 'border-border bg-card text-muted',
              )}
            >
              {outlook.text}
            </p>
          )}
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

function Figure({ label, value, unit, muted }: { label: string; value: ReactNode; unit?: string; muted?: boolean }) {
  return (
    <div className="flex min-w-0 flex-col-reverse justify-end rounded-lg border border-border bg-card px-2.5 py-2">
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
