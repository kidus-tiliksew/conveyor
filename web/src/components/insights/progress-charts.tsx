// Hand-drawn SVG charts for the Charts tab (req-requirement-insights REQ-3;
// feature-requirement-insights RI-7). They plot the server's values as
// served, on UTC days and ISO weeks, with no chart dependency, and colour
// marks with the dashboard's own tokens. Every chart has a Table disclosure
// listing its values (AC-3.6).
import { type PointerEvent, type ReactNode, useRef, useState } from 'react'
import type { RequirementInsightProgress } from '../../lib/types'
import { cn } from '../../lib/utils'
import { actorMeta, formatDate, formatDay, formatNumber } from './shared'

const DAY = 86_400_000

export function ChartCard({
  title,
  subtitle,
  action,
  children,
  table,
}: {
  title: string
  subtitle?: ReactNode
  action?: ReactNode
  children: ReactNode
  table: ReactNode
}) {
  return (
    <section aria-label={title} className="mb-2.5 rounded-lg border border-border bg-card px-3 pb-2 pt-2.5">
      <div className="mb-1.5 flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <h3 className="text-xs font-semibold text-foreground">{title}</h3>
        {subtitle && <span className="text-[11px] text-faint">{subtitle}</span>}
        {action && <div className="ml-auto">{action}</div>}
      </div>
      {children}
      <details className="group mt-1.5 text-[11px]">
        <summary className="w-max cursor-pointer select-none rounded px-1 py-0.5 text-muted hover:text-foreground">
          Table
        </summary>
        <div className="mt-1 max-h-56 overflow-auto rounded-md border border-border">{table}</div>
      </details>
    </section>
  )
}

export function DataTable({ head, rows }: { head: string[]; rows: ReactNode[][] }) {
  return (
    <table className="w-full border-collapse text-left text-[11px] tabular-nums">
      <thead className="sticky top-0 bg-surface text-faint">
        <tr>
          {head.map((cell) => (
            <th key={cell} scope="col" className="px-2 py-1 font-medium">
              {cell}
            </th>
          ))}
        </tr>
      </thead>
      <tbody className="text-muted">
        {rows.map((row, index) => (
          <tr key={index} className="border-t border-border">
            {row.map((cell, column) => (
              <td key={column} className="px-2 py-1">
                {cell}
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// A tick step of 1, 2, or 5 times a power of ten, giving about three ticks.
function tickStep(max: number) {
  const raw = Math.max(1, max / 3)
  const power = 10 ** Math.floor(Math.log10(raw))
  const step = [1, 2, 5, 10].map((factor) => factor * power).find((candidate) => candidate >= raw) ?? raw
  return Math.max(1, Math.round(step))
}

export type BurnMode = 'down' | 'up'

// The daily series (AC-3.1) as steps: scope with open (burn-down) or with
// delivered (burn-up), and the served forecast (AC-3.2) as a dashed
// projection from today, distinct from the recorded series.
export function BurnChart({ progress, mode }: { progress: RequirementInsightProgress; mode: BurnMode }) {
  const svg = useRef<SVGSVGElement>(null)
  const [hover, setHover] = useState<number>()
  const { daily, forecast } = progress
  if (daily.length === 0) return <p className="py-6 text-center text-xs text-muted">No task serves it yet.</p>

  const W = 360
  const H = 176
  const ml = 28
  const mr = 14
  const mt = 14
  const mb = 22
  const iw = W - ml - mr
  const ih = H - mt - mb
  const times = daily.map((point) => Date.parse(point.day))
  const start = times[0]
  const today = times[times.length - 1]
  const forecastAt = forecast ? Date.parse(forecast.day) : undefined
  const end = Math.max(today, forecastAt ?? today, start + DAY)
  const x = (time: number) => ml + ((time - start) / (end - start)) * iw
  const maxValue = Math.max(1, ...daily.map((point) => point.scope))
  const step = tickStep(maxValue)
  const top = Math.ceil(maxValue / step) * step
  const y = (value: number) => mt + ih - (value / top) * ih
  const ticks = Array.from({ length: top / step + 1 }, (_, index) => index * step)
  const series = mode === 'down' ? ('open' as const) : ('delivered' as const)
  const stroke = mode === 'down' ? 'var(--color-primary)' : 'var(--color-positive)'
  const stepped = (key: 'scope' | 'open' | 'delivered') =>
    daily
      .map((point, index) => {
        const px = x(times[index])
        const py = y(point[key])
        return index === 0 ? `M${px},${py}` : `H${px} V${py}`
      })
      .join(' ')
  const last = daily[daily.length - 1]
  const projectedEnd = mode === 'down' ? 0 : last.scope

  const onMove = (event: PointerEvent<SVGRectElement>) => {
    const box = svg.current?.getBoundingClientRect()
    if (!box) return
    const at = start + (((event.clientX - box.left) * (W / box.width) - ml) / iw) * (end - start)
    const index = Math.round((at - start) / DAY)
    setHover(Math.max(0, Math.min(Math.round((end - start) / DAY), index)))
  }
  const hoverTime = hover === undefined ? undefined : start + hover * DAY
  const hoverPoint = hover !== undefined && hover < daily.length ? daily[hover] : undefined
  const hoverX = hoverTime === undefined ? 0 : x(Math.min(hoverTime, end))

  const labels: Array<{ at: number; text: string; anchor: 'start' | 'middle' | 'end' }> = [
    { at: start, text: formatDay(daily[0].day), anchor: 'start' },
  ]
  if (x(today) - x(start) > 48) labels.push({ at: today, text: 'Today', anchor: forecastAt ? 'middle' : 'end' })
  if (forecast && forecastAt && x(forecastAt) - x(today) > 44)
    labels.push({ at: forecastAt, text: formatDay(forecast.day), anchor: 'end' })

  return (
    <div className="relative">
      <svg
        ref={svg}
        viewBox={`0 0 ${W} ${H}`}
        className="block h-auto w-full"
        role="img"
        aria-label={`${mode === 'down' ? 'Burn-down' : 'Burn-up'}: ${last[series]} ${series} of ${last.scope} in scope today`}
      >
        {ticks.map((tick) => (
          <g key={tick}>
            <line
              x1={ml}
              x2={W - mr}
              y1={y(tick)}
              y2={y(tick)}
              stroke={tick === 0 ? 'var(--color-edge)' : 'var(--color-border)'}
            />
            <text x={ml - 6} y={y(tick) + 3} textAnchor="end" className="fill-faint text-[9.5px]">
              {tick}
            </text>
          </g>
        ))}
        {labels.map((label) => (
          <text
            key={label.text}
            x={x(label.at)}
            y={H - 6}
            textAnchor={label.anchor}
            className="fill-faint text-[9.5px]"
          >
            {label.text}
          </text>
        ))}
        <line x1={x(today)} x2={x(today)} y1={mt} y2={mt + ih} stroke="var(--color-edge)" strokeDasharray="2 3" />
        <path d={stepped('scope')} fill="none" stroke="var(--color-faint)" strokeWidth={1.5} />
        <path d={stepped(series)} fill="none" stroke={stroke} strokeWidth={2} data-series={series} />
        {forecast && forecastAt && (
          <path
            d={`M${x(today)},${y(last[series])} L${x(forecastAt)},${y(projectedEnd)}`}
            fill="none"
            stroke={stroke}
            strokeWidth={2}
            strokeDasharray="4 4"
            opacity={0.75}
            data-series="forecast"
          />
        )}
        <circle cx={x(today)} cy={y(last[series])} r={4} fill={stroke} stroke="var(--color-card)" strokeWidth={2} />
        <text
          x={x(today) + (forecast ? 7 : -7)}
          y={y(last[series]) - 7}
          textAnchor={forecast ? 'start' : 'end'}
          className="fill-foreground text-[10px] font-semibold"
        >
          {last[series]} {series}
        </text>
        {hoverTime !== undefined && (
          <line x1={hoverX} x2={hoverX} y1={mt} y2={mt + ih} stroke="var(--color-muted)" opacity={0.5} />
        )}
        {/* The hit area is the whole plot, larger than any mark. */}
        <rect
          x={ml}
          y={mt}
          width={iw}
          height={ih}
          fill="transparent"
          onPointerMove={onMove}
          onPointerLeave={() => setHover(undefined)}
        />
      </svg>
      {hoverTime !== undefined && (
        <div
          role="presentation"
          className="pointer-events-none absolute top-1 z-10 whitespace-nowrap rounded-md border border-edge bg-raised px-2 py-1 text-[10.5px] leading-snug text-muted shadow-md"
          style={
            hoverX > W / 2 ? { right: `${((W - hoverX + 8) / W) * 100}%` } : { left: `${((hoverX + 8) / W) * 100}%` }
          }
        >
          {hoverPoint ? (
            <>
              <b className="font-semibold text-foreground">{formatDay(hoverPoint.day)}</b>
              <br />
              {mode === 'down' ? 'Open' : 'Delivered'}{' '}
              <b className="font-semibold text-foreground">{hoverPoint[series]}</b> · Scope{' '}
              <b className="font-semibold text-foreground">{hoverPoint.scope}</b>
            </>
          ) : forecast ? (
            <>
              <b className="font-semibold text-foreground">Forecast</b>
              <br />
              Open tasks finish around <b className="font-semibold text-foreground">{formatDate(forecast.day)}</b>
            </>
          ) : null}
        </div>
      )}
    </div>
  )
}

export function BurnLegend({ mode, forecast }: { mode: BurnMode; forecast: boolean }) {
  const colour = mode === 'down' ? 'bg-primary' : 'bg-positive'
  return (
    <div className="mb-1 flex flex-wrap gap-x-3 text-[11px] text-muted">
      <span className="inline-flex items-center gap-1.5">
        <i className={cn('inline-block h-0.5 w-3 rounded-full', colour)} />
        {mode === 'down' ? 'Open' : 'Delivered'}
      </span>
      <span className="inline-flex items-center gap-1.5">
        <i className="inline-block h-0.5 w-3 rounded-full bg-faint" />
        Scope
      </span>
      {forecast && (
        <span className="inline-flex items-center gap-1.5">
          <i
            className={cn('inline-block h-0.5 w-3', mode === 'down' ? 'text-primary' : 'text-positive')}
            style={{ background: 'repeating-linear-gradient(90deg, currentColor 0 3px, transparent 3px 6px)' }}
          />
          Forecast
        </span>
      )}
    </div>
  )
}

export function BurnTable({ progress }: { progress: RequirementInsightProgress }) {
  const rows: ReactNode[][] = progress.daily.map((point) => [
    formatDate(point.day),
    point.scope,
    point.open,
    point.delivered,
  ])
  if (progress.forecast)
    rows.push([
      `Forecast: ${formatDate(progress.forecast.day)}`,
      '—',
      '0 (projected)',
      `${formatNumber(progress.forecast.rate_per_day)} a day`,
    ])
  return <DataTable head={['Day (UTC)', 'Scope', 'Open', 'Delivered']} rows={rows} />
}

// The status mix (AC-3.3): delivered, closed, and open by next-action
// actor, summing to the serving tasks.
export function statusMixParts(progress: RequirementInsightProgress) {
  return [
    { key: 'delivered', label: 'Delivered', count: progress.status_mix.delivered, fill: 'bg-positive' },
    { key: 'closed', label: 'Closed', count: progress.status_mix.closed, fill: 'bg-faint' },
    ...progress.status_mix.open.map((entry) => {
      const meta = actorMeta[entry.actor] ?? actorMeta.none
      return { key: entry.actor, label: `Open · ${meta.label}`, count: entry.count, fill: meta.fill }
    }),
  ]
}

export function StatusMix({ progress }: { progress: RequirementInsightProgress }) {
  const parts = statusMixParts(progress)
  const total = parts.reduce((sum, part) => sum + part.count, 0)
  return (
    <div>
      <div aria-hidden className="mb-2.5 mt-1 flex h-3 gap-[2px] overflow-hidden rounded-[4px] bg-raised">
        {parts
          .filter((part) => part.count > 0)
          .map((part) => (
            <i
              key={part.key}
              title={`${part.label}: ${part.count}`}
              className={cn('block h-full', part.fill)}
              style={{ flexGrow: part.count, flexBasis: 0 }}
            />
          ))}
      </div>
      <ul
        aria-label={`Status mix of ${total} tasks`}
        className="grid grid-cols-2 gap-x-4 gap-y-1 text-[11px] text-muted"
      >
        {parts.map((part) => (
          <li key={part.key} className="flex items-center gap-1.5">
            <i
              className={cn(
                'size-2 shrink-0 rounded-[2px]',
                part.fill,
                part.fill === 'bg-raised' && 'border border-edge',
              )}
            />
            {part.label}
            <b className="ml-auto font-medium tabular-nums text-foreground">{part.count}</b>
          </li>
        ))}
      </ul>
    </div>
  )
}

// Delivered per week (AC-3.4): the server's eight ISO weeks, oldest first,
// the current one faded and marked in progress.
export function WeeklyBars({ progress }: { progress: RequirementInsightProgress }) {
  const weeks = progress.weekly
  const W = 360
  const H = 132
  const ml = 8
  const mr = 8
  const mt = 16
  const mb = 30
  const iw = W - ml - mr
  const ih = H - mt - mb
  const max = Math.max(1, ...weeks.map((week) => week.delivered))
  const slot = iw / Math.max(1, weeks.length)
  const bar = Math.min(30, slot - 10)
  const base = mt + ih
  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="block h-auto w-full" role="img" aria-label="Delivered per week">
      <line x1={ml} x2={W - mr} y1={base} y2={base} stroke="var(--color-edge)" />
      {weeks.map((week, index) => {
        const cx = ml + slot * index + slot / 2
        const height = (week.delivered / max) * ih
        const r = Math.min(4, height, bar / 2)
        const left = cx - bar / 2
        const top = base - height
        return (
          <g
            key={week.week_start}
            data-week={`${week.iso_year}-W${week.iso_week}`}
            data-in-progress={week.in_progress || undefined}
            opacity={week.in_progress ? 0.45 : 1}
          >
            <title>
              {`Week of ${formatDate(week.week_start)} (${week.iso_year}-W${week.iso_week}): ${week.delivered} delivered${week.in_progress ? ', in progress' : ''}`}
            </title>
            <rect x={cx - slot / 2} y={mt} width={slot} height={ih + mb} fill="transparent" />
            {height > 0 && (
              <path
                d={`M${left},${base} V${top + r} Q${left},${top} ${left + r},${top} H${left + bar - r} Q${left + bar},${top} ${left + bar},${top + r} V${base} Z`}
                fill="var(--color-positive)"
              />
            )}
            <text x={cx} y={top - 4} textAnchor="middle" className="fill-muted text-[9.5px] tabular-nums">
              {week.delivered}
            </text>
            <text x={cx} y={base + 12} textAnchor="middle" className="fill-faint text-[9.5px]">
              {formatDay(week.week_start)}
            </text>
            {week.in_progress && (
              <text x={cx} y={base + 23} textAnchor="middle" className="fill-faint text-[8.5px] italic">
                in progress
              </text>
            )}
          </g>
        )
      })}
    </svg>
  )
}

export function WeeklyTable({ progress }: { progress: RequirementInsightProgress }) {
  return (
    <DataTable
      head={['ISO week', 'Starts (UTC)', 'Delivered']}
      rows={progress.weekly.map((week) => [
        `${week.iso_year}-W${String(week.iso_week).padStart(2, '0')}`,
        formatDate(week.week_start),
        week.in_progress ? `${week.delivered} (in progress)` : week.delivered,
      ])}
    />
  )
}
