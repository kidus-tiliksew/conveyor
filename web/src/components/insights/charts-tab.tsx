// The Charts tab (req-requirement-insights REQ-3; feature-requirement-insights
// RI-7): four numbers, the burn-down with its burn-up toggle and forecast,
// the status mix, and delivered per week, all from one progress read.
import { useQuery } from '@tanstack/react-query'
import { type ReactNode, useState } from 'react'
import { fetchRequirementInsightProgress } from '../../lib/api'
import { cn } from '../../lib/utils'
import { Skeleton } from '../ui/skeleton'
import {
  BurnChart,
  BurnLegend,
  type BurnMode,
  BurnTable,
  ChartCard,
  DataTable,
  StatusMix,
  statusMixParts,
  WeeklyBars,
  WeeklyTable,
} from './progress-charts'
import { formatDuration, formatNumber, insightErrorText, insightKey, insightRetry, Notice } from './shared'

export function ChartsTab({ workspace, requirementId }: { workspace: string; requirementId: string }) {
  const progress = useQuery({
    queryKey: insightKey(workspace, requirementId, 'progress'),
    queryFn: ({ signal }) => fetchRequirementInsightProgress(workspace, requirementId, signal),
    retry: insightRetry,
  })
  const [mode, setMode] = useState<BurnMode>('down')
  if (progress.isPending)
    return (
      <div className="space-y-2.5" aria-busy="true">
        <Skeleton className="h-14 w-full" />
        <Skeleton className="h-52 w-full" />
        <Skeleton className="h-24 w-full" />
      </div>
    )
  const data = progress.data
  if (progress.error || !data)
    return <Notice tone="failure">Charts unavailable: {insightErrorText(progress.error)}</Notice>

  const cycle = data.median_cycle_seconds === null ? undefined : formatDuration(data.median_cycle_seconds)
  const mixTotal = statusMixParts(data).reduce((sum, part) => sum + part.count, 0)
  return (
    <div>
      {/* AC-3.5: delivered of total, open, throughput, and median cycle. */}
      <dl className="mb-3 grid grid-cols-4 gap-1.5">
        <Figure label="Delivered" value={data.delivered} unit={`/${data.total}`} />
        <Figure label="Open" value={data.open} />
        <Figure label="Throughput" value={formatNumber(data.throughput_per_week)} unit="/wk" />
        <Figure label="Median cycle" value={cycle?.value ?? 'none'} unit={cycle?.unit} muted={!cycle} />
      </dl>

      <ChartCard
        title={mode === 'down' ? 'Burn-down' : 'Burn-up'}
        subtitle={mode === 'down' ? 'open tasks vs. scope' : 'delivered tasks vs. scope'}
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
        {!data.forecast && (
          <p className="mt-1 text-[11px] text-faint">
            No forecast exists: no serving task was delivered in the last 14 days.
          </p>
        )}
      </ChartCard>

      <ChartCard
        title="Status mix"
        subtitle={`${mixTotal} ${mixTotal === 1 ? 'task' : 'tasks'}`}
        table={
          <DataTable head={['Status', 'Tasks']} rows={statusMixParts(data).map((part) => [part.label, part.count])} />
        }
      >
        <StatusMix progress={data} />
      </ChartCard>

      <ChartCard title="Delivered per week" subtitle="merged, last eight weeks" table={<WeeklyTable progress={data} />}>
        <WeeklyBars progress={data} />
      </ChartCard>
    </div>
  )
}

function Figure({ label, value, unit, muted }: { label: string; value: ReactNode; unit?: string; muted?: boolean }) {
  return (
    // The label precedes its value for assistive technology; the value reads
    // first on screen.
    <div className="flex min-w-0 flex-col-reverse rounded-lg border border-border bg-card px-2.5 py-2">
      <dt className="mt-0.5 truncate text-[11px] text-muted">{label}</dt>
      <dd
        className={cn(
          'm-0 truncate text-[17px] font-semibold tabular-nums leading-tight',
          muted ? 'text-muted' : 'text-foreground',
        )}
      >
        {value}
        {unit && <small className="ml-px text-[11px] font-normal text-faint">{unit}</small>}
      </dd>
    </div>
  )
}
