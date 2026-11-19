// PROTOTYPE. Where the crate's work is, as a branch map on a time axis: one
// lane per long-lived branch, and one spur per member task that leaves its
// base branch when the task is created and returns where it merges. A spur
// that has not returned ends at "now" with the task's name. A release of one
// branch into another crosses the lanes.
import { ArrowRight, GitBranch } from 'lucide-react'
import { useLayoutEffect, useMemo, useRef, useState } from 'react'
import { cn } from '../../../lib/utils'
import { type CrateInsight, DAY, phaseMeta, shortTitle, type TaskTrack } from './model'

const ROW = 22
const LANE_HEAD = 20
const LANE_GAP = 26
const TOP = 30

interface Spur {
  track: TaskTrack
  row: number
  xs: number
  xe: number
  y0: number
  y1: number
  open: boolean
  path: string
}

function useWidth() {
  const ref = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(0)
  useLayoutEffect(() => {
    if (!ref.current) return
    const observer = new ResizeObserver(([entry]) => setWidth(Math.round(entry.contentRect.width)))
    observer.observe(ref.current)
    return () => observer.disconnect()
  }, [])
  return [ref, width] as const
}

const dayLabel = new Intl.DateTimeFormat('en', { month: 'short', day: 'numeric', timeZone: 'UTC' })
const timeLabel = new Intl.DateTimeFormat('en', { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })

export function BranchMap({
  insight,
  hoveredId,
  onHover,
  onSelect,
}: {
  insight: CrateInsight
  hoveredId: string | null
  onHover: (id: string | null) => void
  onSelect: (id: string) => void
}) {
  const [wrapper, measured] = useWidth()
  const [group, setGroup] = useState<string | null>(null)
  const width = Math.max(measured, 680)
  const gutterLeft = 92
  const gutterRight = width >= 900 ? 250 : 190

  const model = useMemo(() => {
    const { tracks, branches, promotions, now } = insight
    const span = Math.max(now - insight.start, DAY / 4)
    const t0 = insight.start - span * 0.05
    const plot = width - gutterLeft - gutterRight
    const x = (time: number) => gutterLeft + ((Math.min(time, now) - t0) / (now - t0)) * plot
    const xNow = x(now)

    let y = TOP
    const lanes: Array<{ branch: string; y: number; spurs: Spur[]; bottom: number }> = []
    for (const branch of branches) {
      const members = tracks.filter((track) => track.baseBranch === branch)
      const finished = members
        .filter((track) => track.mergedAt !== undefined || track.closedAt !== undefined)
        .sort((a, b) => a.createdAt - b.createdAt)
      const order = ['gate', 'review', 'build', 'plan', 'intake']
      const flying = members
        .filter((track) => track.mergedAt === undefined && track.closedAt === undefined)
        .sort((a, b) => order.indexOf(a.station) - order.indexOf(b.station) || a.createdAt - b.createdAt)
      const rowEnds: number[] = []
      const laneY = y
      const spurs: Spur[] = []
      const add = (track: TaskTrack, open: boolean) => {
        const xe = open ? xNow : x((track.mergedAt ?? track.closedAt) as number)
        const xs = Math.min(x(track.createdAt), xe - 30)
        // A finished spur shares a row when it clears the one before it. An
        // open spur keeps its own row, where its name is written.
        let row = open ? rowEnds.length : rowEnds.findIndex((end) => end + 12 < xs)
        if (row < 0) row = rowEnds.length
        rowEnds[row] = open ? Number.POSITIVE_INFINITY : xe
        const y1 = laneY + LANE_HEAD + row * ROW
        const bend = Math.min(16, (xe - xs) / 2)
        const out = `M${xs},${laneY} C${xs + bend * 0.7},${laneY} ${xs + bend * 0.3},${y1} ${xs + bend},${y1}`
        const path =
          track.mergedAt !== undefined
            ? `${out} L${xe - bend},${y1} C${xe - bend * 0.3},${y1} ${xe - bend * 0.7},${laneY} ${xe},${laneY}`
            : `${out} L${xe},${y1}`
        spurs.push({ track, row, xs, xe, y0: laneY, y1, open, path })
      }
      for (const track of finished) add(track, false)
      for (const track of flying) add(track, true)
      const bottom = laneY + LANE_HEAD + Math.max(rowEnds.length, 1) * ROW - 6
      lanes.push({ branch, y: laneY, spurs, bottom })
      y = bottom + LANE_GAP
    }
    const height = y - LANE_GAP + 34

    // Day ticks, thinned to at most eight labels.
    const firstDay = Math.ceil(t0 / DAY) * DAY
    const days: number[] = []
    for (let day = firstDay; day <= now; day += DAY) days.push(day)
    const step = Math.max(1, Math.ceil(days.length / 8))
    const ticks = days.filter((_, index) => index % step === 0)

    const laneOf = (branch: string) => lanes.find((lane) => lane.branch === branch)
    const releases = promotions
      .map((promotion) => ({ promotion, from: laneOf(promotion.from), to: laneOf(promotion.to), x: x(promotion.at) }))
      .filter((release) => release.from && release.to)
    return { lanes, height, x, xNow, ticks, releases, spurs: lanes.flatMap((lane) => lane.spurs) }
  }, [insight, width, gutterRight])

  const groupIds = useMemo(() => {
    const found = insight.whereabouts.find((item) => item.key === group)
    return found ? new Set(found.tracks.map((track) => track.id)) : null
  }, [insight, group])
  const lit = (id: string) => (hoveredId ? hoveredId === id : groupIds ? groupIds.has(id) : true)
  const hovered = hoveredId ? model.spurs.find((spur) => spur.track.id === hoveredId) : undefined
  const labelChars = Math.floor((gutterRight - 26) / 6.4)

  return (
    <section aria-label="Where the work is" className="min-w-0 rounded-lg border border-border bg-card">
      <header className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-b border-border px-4 py-2.5">
        <h2 className="text-sm font-medium">Where the work is</h2>
        {/* The furthest place each task's work has reached, left to right. */}
        <ol className="flex flex-wrap items-center gap-1" aria-label="Tasks by where their work is">
          {insight.whereabouts.map((item, index) => (
            <li key={item.key} className="flex items-center gap-1">
              {index > 0 && <ArrowRight aria-hidden className="size-3 text-faint" />}
              <button
                type="button"
                onMouseEnter={() => setGroup(item.key)}
                onMouseLeave={() => setGroup(null)}
                onFocus={() => setGroup(item.key)}
                onBlur={() => setGroup(null)}
                className={cn(
                  'flex items-baseline gap-1.5 rounded-md border px-2 py-1 text-xs transition-colors',
                  group === item.key ? 'border-edge bg-raised' : 'border-border bg-card hover:bg-raised',
                  item.tracks.length === 0 && 'text-faint',
                )}
              >
                <span
                  className={cn('font-semibold tabular-nums', item.branch && item.tracks.length > 0 && 'text-positive')}
                >
                  {item.tracks.length}
                </span>
                <span className={cn(item.branch ? 'font-mono text-[11px]' : '', 'text-muted')}>
                  {item.branch ? `in ${item.branch}` : item.label.toLowerCase()}
                </span>
              </button>
            </li>
          ))}
        </ol>
      </header>

      <div ref={wrapper} className="relative overflow-x-auto">
        <svg
          width={width}
          height={model.height}
          viewBox={`0 0 ${width} ${model.height}`}
          role="img"
          aria-label={`Branch map of ${insight.tracks.length} tasks on ${insight.branches.join(' and ')}`}
          className="block"
          onMouseLeave={() => onHover(null)}
        >
          {model.ticks.map((tick) => (
            <g key={tick}>
              <line
                x1={model.x(tick)}
                x2={model.x(tick)}
                y1={TOP - 14}
                y2={model.height - 24}
                stroke="var(--color-border)"
                strokeWidth={1}
              />
              <text x={model.x(tick)} y={model.height - 9} textAnchor="middle" className="fill-faint text-[10px]">
                {dayLabel.format(new Date(tick))}
              </text>
            </g>
          ))}
          <line
            x1={model.xNow}
            x2={model.xNow}
            y1={TOP - 14}
            y2={model.height - 24}
            stroke="var(--color-edge)"
            strokeDasharray="3 3"
          />
          <text x={model.xNow} y={model.height - 9} textAnchor="middle" className="fill-muted text-[10px] font-medium">
            Now
          </text>

          {/* Releases sit behind the tasks. */}
          {model.releases.map(({ promotion, from, to, x }) => {
            if (!from || !to) return null
            const up = to.y < from.y
            return (
              <g key={`${promotion.at}:${promotion.to}`} className="proto-fade">
                <line
                  x1={x}
                  x2={x}
                  y1={from.y}
                  y2={to.y + (up ? 7 : -7)}
                  stroke="var(--color-foreground)"
                  strokeWidth={1.5}
                  strokeDasharray="2 4"
                  strokeLinecap="round"
                  opacity={0.55}
                />
                <path
                  d={
                    up
                      ? `M${x - 4},${to.y + 10} L${x},${to.y + 4} L${x + 4},${to.y + 10}`
                      : `M${x - 4},${to.y - 10} L${x},${to.y - 4} L${x + 4},${to.y - 10}`
                  }
                  fill="none"
                  stroke="var(--color-foreground)"
                  strokeWidth={1.5}
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  opacity={0.7}
                />
                <circle
                  cx={x}
                  cy={from.y}
                  r={3}
                  fill="var(--color-card)"
                  stroke="var(--color-foreground)"
                  strokeWidth={1.5}
                />
                <text x={x + 7} y={Math.min(from.y, to.y) - 8} className="fill-muted text-[10px]">
                  Released to {promotion.to}
                </text>
              </g>
            )
          })}

          {model.lanes.map((lane) => (
            <g key={lane.branch}>
              <line
                x1={gutterLeft - 8}
                x2={model.xNow + 10}
                y1={lane.y}
                y2={lane.y}
                stroke="var(--color-foreground)"
                strokeWidth={2.5}
                strokeLinecap="round"
                opacity={0.72}
              />
              <foreignObject x={0} y={lane.y - 11} width={gutterLeft - 12} height={22}>
                <div className="flex h-full items-center justify-end">
                  <span className="inline-flex max-w-full items-center gap-1 rounded-md bg-foreground px-1.5 py-0.5 font-mono text-[11px] font-medium leading-4 text-card">
                    <GitBranch aria-hidden className="size-3 shrink-0" />
                    <span className="truncate">{lane.branch}</span>
                  </span>
                </div>
              </foreignObject>
            </g>
          ))}

          {model.spurs.map((spur, index) => {
            const { track } = spur
            const color = phaseMeta[track.phase].color
            const on = lit(track.id)
            const focused = hoveredId === track.id
            const notStarted = track.station === 'intake'
            const working = track.phase === 'building' || track.phase === 'planning' || track.phase === 'review'
            return (
              // biome-ignore lint/a11y/useSemanticElements: an SVG group cannot be a button element
              <g
                key={track.id}
                role="button"
                tabIndex={0}
                aria-label={`${track.title}: ${track.task.next_action.label}, base ${track.baseBranch}`}
                opacity={on ? 1 : 0.22}
                className="cursor-pointer outline-none transition-opacity"
                onMouseEnter={() => onHover(track.id)}
                onFocus={() => onHover(track.id)}
                onBlur={() => onHover(null)}
                onClick={() => onSelect(track.id)}
                onKeyDown={(event) => {
                  if (event.key === 'Enter' || event.key === ' ') onSelect(track.id)
                }}
                data-branch-task={track.id}
              >
                <path
                  d={spur.path}
                  fill="none"
                  stroke={color}
                  strokeWidth={focused ? 3.5 : 2.25}
                  strokeLinecap="round"
                  strokeDasharray={notStarted || track.closedAt !== undefined ? '2 5' : undefined}
                  pathLength={notStarted || track.closedAt !== undefined ? undefined : 1}
                  className={notStarted || track.closedAt !== undefined ? 'proto-fade' : 'proto-draw'}
                  style={{ animationDelay: `${120 + index * 55}ms` }}
                />
                {/* A wide, unseen stroke makes the thin line easy to point at. */}
                <path d={spur.path} fill="none" stroke="transparent" strokeWidth={16} />
                {track.reviews
                  .filter((review) => review.verdict === 'changes_requested')
                  .map((review) => (
                    <circle
                      key={review.at}
                      cx={Math.max(spur.xs + 12, Math.min(model.x(review.at), spur.xe - 10))}
                      cy={spur.y1}
                      r={3.25}
                      fill="var(--color-card)"
                      stroke="var(--color-attention-dot)"
                      strokeWidth={2}
                    />
                  ))}
                {track.mergedAt !== undefined && (
                  <circle
                    cx={spur.xe}
                    cy={spur.y0}
                    r={4.25}
                    fill={color}
                    stroke="var(--color-card)"
                    strokeWidth={1.75}
                  />
                )}
                {track.closedAt !== undefined && (
                  <path
                    d={`M${spur.xe - 3.5},${spur.y1 - 3.5} l7,7 m0,-7 l-7,7`}
                    stroke="var(--color-faint)"
                    strokeWidth={1.75}
                    strokeLinecap="round"
                  />
                )}
                {spur.open && (
                  <>
                    {working && (
                      <circle r={2.4} fill={color} className="proto-motion">
                        <animateMotion dur="2.6s" repeatCount="indefinite" path={spur.path} begin={`${index * 0.3}s`} />
                      </circle>
                    )}
                    {track.phase === 'needs_you' && (
                      <circle cx={spur.xe} cy={spur.y1} r={5} fill="none" stroke={color} className="proto-ping" />
                    )}
                    <circle
                      cx={spur.xe}
                      cy={spur.y1}
                      r={4.75}
                      fill={notStarted ? 'var(--color-card)' : color}
                      stroke={notStarted ? color : 'var(--color-card)'}
                      strokeWidth={1.75}
                    />
                    <text x={spur.xe + 12} y={spur.y1 + 3.5} className="text-[11px]">
                      <tspan className={cn('fill-foreground', focused && 'font-medium')}>
                        {shortTitle(track.title, Math.max(10, labelChars - phaseMeta[track.phase].label.length - 3))}
                      </tspan>
                      <tspan className="fill-muted"> · {phaseMeta[track.phase].label}</tspan>
                    </text>
                  </>
                )}
              </g>
            )
          })}
        </svg>

        {hovered && (
          <div
            className="pointer-events-none absolute z-10 w-max max-w-64 rounded-md border border-border bg-card px-2.5 py-1.5 shadow-md"
            style={{
              left: Math.min(Math.max((hovered.xs + Math.min(hovered.xe, model.xNow)) / 2, 120), width - 140),
              top: hovered.y1 + 12,
              transform: 'translateX(-50%)',
            }}
          >
            <p className="text-xs font-medium leading-snug text-foreground">{shortTitle(hovered.track.title, 64)}</p>
            <p className="mt-0.5 text-[11px] leading-4 text-muted">
              {hovered.track.task.next_action.label}
              {hovered.track.task.pull_request?.number ? ` · #${hovered.track.task.pull_request.number}` : ''}
              {hovered.track.reviews.length > 0
                ? ` · ${hovered.track.reviews.length} review ${hovered.track.reviews.length === 1 ? 'round' : 'rounds'}`
                : ''}
            </p>
            <p className="font-mono text-[11px] leading-4 text-faint">
              {hovered.track.mergedAt !== undefined
                ? `in ${hovered.track.landedOn.join(' → ')} since ${timeLabel.format(new Date(hovered.track.mergedAt))}`
                : `${hovered.track.task.branch} → ${hovered.track.baseBranch}`}
            </p>
          </div>
        )}
      </div>

      <footer className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-border px-4 py-2 text-[11px] text-muted">
        <span className="inline-flex items-center gap-1.5">
          <svg width="22" height="10" aria-hidden="true">
            <circle cx="16" cy="5" r="4" fill="var(--color-positive)" />
            <line x1="0" x2="12" y1="5" y2="5" stroke="var(--color-positive)" strokeWidth="2" />
          </svg>
          Merged into the branch
        </span>
        <span className="inline-flex items-center gap-1.5">
          <svg width="10" height="10" aria-hidden="true">
            <circle cx="5" cy="5" r="3.25" fill="none" stroke="var(--color-attention-dot)" strokeWidth="2" />
          </svg>
          Review asked for changes
        </span>
        <span className="inline-flex items-center gap-1.5">
          <svg width="22" height="10" aria-hidden="true">
            <line
              x1="1"
              x2="21"
              y1="5"
              y2="5"
              stroke="var(--color-faint)"
              strokeWidth="2"
              strokeDasharray="2 5"
              strokeLinecap="round"
            />
          </svg>
          Not started
        </span>
        {insight.promotions.length > 0 && (
          <span className="inline-flex items-center gap-1.5">
            <svg width="10" height="14" aria-hidden="true">
              <line
                x1="5"
                x2="5"
                y1="1"
                y2="13"
                stroke="var(--color-foreground)"
                strokeWidth="1.5"
                strokeDasharray="2 4"
                strokeLinecap="round"
              />
            </svg>
            One branch released into another
          </span>
        )}
      </footer>
    </section>
  )
}
