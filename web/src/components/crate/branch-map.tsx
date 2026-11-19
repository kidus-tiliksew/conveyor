// The branch map (req-crate-insights REQ-2; component-crates invariants 26
// and 33): one lane for each branch a member task merges into, in the served
// lane order, and one line for each member task on a time axis. A line leaves
// its lane at the task's creation and returns at its merge. An open line ends
// at the present with the task's title and what it is doing. Every lane,
// time, count, and group is the insight view's; titles, task branches, pull
// requests, and next-action labels come from the crate read, joined by task
// ID. The page places each mark at its served time and derives no value
// (component-crates invariant 21; DEC-61). Ported from the approved prototype
// without its release marks (feature-crate-insights CI-4).
import { ArrowRight, GitBranch, RotateCw } from 'lucide-react'
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { CrateDetail, CrateInsight, CrateInsightTask, CrateTask, CrateWhereabouts } from '../../lib/types'
import { useMediaQuery } from '../../lib/use-media-query'
import { cn } from '../../lib/utils'
import { Button } from '../ui/button'
import { Skeleton } from '../ui/skeleton'
import type { CrateEmphasis } from './crate-emphasis'
import { phaseMeta } from './line-model'

const DAY = 86_400_000
const ROW = 22
const LANE_HEAD = 20
const LANE_GAP = 26
const TOP = 30
const GUTTER_LEFT = 92
// Below this width the map keeps it and scrolls sideways inside its card.
const MIN_WIDTH = 680
// The hover card's height, used to keep the card inside the map.
const CARD_ROOM = 64
// The order in which a lane's open lines take rows, nearest the merge first.
const OPEN_ORDER = ['gate', 'review', 'build', 'plan', 'intake', 'crate', 'aside']

interface Spur {
  task: CrateInsightTask
  row: CrateTask | undefined
  // The x of the task's creation and of its line's end, and the lane's y and
  // the line's own y.
  xs: number
  xe: number
  y0: number
  y1: number
  ends: 'merged' | 'closed' | 'open'
  notStarted: boolean
  path: string
  changes: number[]
}

// A served time in milliseconds, or null when it is absent or unreadable.
function served(value: string | null | undefined) {
  if (!value) return null
  const time = Date.parse(value)
  return Number.isFinite(time) ? time : null
}

function shortTitle(title: string, max: number) {
  const clean = title.replace(/\s+/g, ' ').trim()
  return clean.length <= max ? clean : `${clean.slice(0, max - 1).trimEnd()}…`
}

// The key that names one whereabouts group. Merged groups share their kind,
// so the branch is part of the key.
export function whereaboutsKey(item: CrateWhereabouts) {
  return `${item.key}:${item.branch ?? ''}`
}

// The words after a group's count. A merged group's branch is written
// apart, in the branch's own type.
function whereaboutsLabel(item: CrateWhereabouts) {
  return item.key === 'not_started' ? 'not started' : 'on task branches'
}

function useWidth() {
  const ref = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(0)
  useLayoutEffect(() => {
    if (!ref.current || typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver(([entry]) => setWidth(Math.round(entry.contentRect.width)))
    observer.observe(ref.current)
    return () => observer.disconnect()
  }, [])
  return [ref, width] as const
}

const dayLabel = new Intl.DateTimeFormat('en', { month: 'short', day: 'numeric', timeZone: 'UTC' })
const timeLabel = new Intl.DateTimeFormat('en', { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })

// The map's place on the page. While the insight view loads it holds a
// placeholder, and when the read fails it names the failure with a way to
// try again. A crate with no member task shows no map (feature-crate-insights
// Rollout and closure).
export function BranchMap({
  crate,
  insight,
  insightError,
  retryingInsight,
  onRetryInsight,
  emphasis,
  onSelect,
}: {
  crate: CrateDetail
  insight: CrateInsight | undefined
  insightError: unknown
  retryingInsight: boolean
  onRetryInsight: () => void
  emphasis: CrateEmphasis
  onSelect: (id: string) => void
}) {
  if (!insight) {
    if (insightError == null)
      return (
        <div data-branch-map-loading>
          <Skeleton className="h-56 w-full" />
        </div>
      )
    return (
      <section aria-label="Where the work is" data-branch-map-error className="rounded-lg border border-border bg-card">
        <header className="border-b border-border px-4 py-2.5">
          <h2 className="text-sm font-medium">Where the work is</h2>
        </header>
        <div className="flex flex-wrap items-center gap-2 px-4 py-4 text-sm text-muted">
          <span>
            The branch map did not load.
            {insightError instanceof Error && insightError.message ? ` ${insightError.message}` : ''}
          </span>
          <Button variant="outline" size="sm" onClick={onRetryInsight} disabled={retryingInsight}>
            <RotateCw aria-hidden />
            Try again
          </Button>
        </div>
      </section>
    )
  }
  if (insight.tasks.length === 0) return null
  return <BranchMapChart crate={crate} insight={insight} emphasis={emphasis} onSelect={onSelect} />
}

// What the pointer rests on and what holds keyboard focus. When one of them
// leaves, the emphasis goes back to the other, so a line that still has
// focus keeps its emphasis after the pointer moves away.
type Target = { task: string } | { group: string } | null

function BranchMapChart({
  crate,
  insight,
  emphasis,
  onSelect,
}: {
  crate: CrateDetail
  insight: CrateInsight
  emphasis: CrateEmphasis
  onSelect: (id: string) => void
}) {
  const [wrapper, measured] = useWidth()
  const reducedMotion = useMediaQuery('(prefers-reduced-motion: reduce)')
  const width = Math.max(measured, MIN_WIDTH)
  const gutterRight = width >= 900 ? 250 : 190
  const rows = useMemo(() => new Map(crate.tasks.map((task) => [task.id, task])), [crate.tasks])

  const model = useMemo(() => {
    // The time axis runs over the served replay span, from the first member
    // task's creation to the read instant (AC-2.1). The replay slider does
    // not move it.
    const end = served(insight.replay.end)
    const startServed = served(insight.replay.start)
    const now = end ?? startServed ?? 0
    const first = startServed === null ? now : Math.min(startServed, now)
    const span = Math.max(now - first, DAY / 4)
    const t0 = first - span * 0.05
    const plot = width - GUTTER_LEFT - gutterRight
    const x = (time: number) => GUTTER_LEFT + ((Math.min(Math.max(time, t0), now) - t0) / (now - t0)) * plot
    const xNow = x(now)

    let y = TOP
    const lanes: Array<{ branch: string; y: number; spurs: Spur[] }> = []
    for (const branch of insight.branches) {
      const members = insight.tasks.filter((task) => task.base_branch === branch)
      const endOf = (task: CrateInsightTask) => served(task.merged_at) ?? served(task.closed_at)
      const finished = members
        .filter((task) => endOf(task) !== null)
        .sort((a, b) => (served(a.created_at) ?? 0) - (served(b.created_at) ?? 0))
      const open = members
        .filter((task) => endOf(task) === null)
        .sort(
          (a, b) =>
            OPEN_ORDER.indexOf(a.station) - OPEN_ORDER.indexOf(b.station) ||
            (served(a.created_at) ?? 0) - (served(b.created_at) ?? 0),
        )
      const rowEnds: number[] = []
      const laneY = y
      const spurs: Spur[] = []
      const add = (task: CrateInsightTask) => {
        const merged = served(task.merged_at)
        const closed = merged === null ? served(task.closed_at) : null
        const ends = merged !== null ? 'merged' : closed !== null ? 'closed' : 'open'
        const xe = ends === 'open' ? xNow : x((merged ?? closed) as number)
        // The line leaves its lane at the served creation time. Only a
        // creation after the line's end, which the view never serves, is
        // held back to the end.
        const xs = Math.min(x(served(task.created_at) ?? first), xe)
        // A finished line shares a row when it clears the one before it. An
        // open line keeps its own row, where its name is written.
        let row = ends === 'open' ? rowEnds.length : rowEnds.findIndex((rowEnd) => rowEnd + 12 < xs)
        if (row < 0) row = rowEnds.length
        rowEnds[row] = ends === 'open' ? Number.POSITIVE_INFINITY : xe
        const y1 = laneY + LANE_HEAD + row * ROW
        const bend = Math.min(16, (xe - xs) / 2)
        const out = `M${xs},${laneY} C${xs + bend * 0.7},${laneY} ${xs + bend * 0.3},${y1} ${xs + bend},${y1}`
        const path =
          ends === 'merged'
            ? `${out} L${xe - bend},${y1} C${xe - bend * 0.3},${y1} ${xe - bend * 0.7},${laneY} ${xe},${laneY}`
            : `${out} L${xe},${y1}`
        // Each round that asked for changes, at its served time (AC-2.3).
        const changes = task.changes_requested_at.map(served).flatMap((time) => (time === null ? [] : [x(time)]))
        spurs.push({
          task,
          row: rows.get(task.id),
          xs,
          xe,
          y0: laneY,
          y1,
          ends,
          notStarted: ends === 'open' && task.station === 'intake',
          path,
          changes,
        })
      }
      for (const task of finished) add(task)
      for (const task of open) add(task)
      lanes.push({ branch, y: laneY, spurs })
      y = laneY + LANE_HEAD + Math.max(rowEnds.length, 1) * ROW - 6 + LANE_GAP
    }
    const height = y - LANE_GAP + 34

    // Day ticks at UTC midnight, thinned to at most eight labels.
    const days: number[] = []
    for (let day = Math.ceil(t0 / DAY) * DAY; day <= now; day += DAY) days.push(day)
    const step = Math.max(1, Math.ceil(days.length / 8))
    const ticks = days.filter((_, index) => index % step === 0)
    return { lanes, height, x, xNow, ticks, spurs: lanes.flatMap((lane) => lane.spurs) }
  }, [insight, width, gutterRight, rows])

  // Pointing at a count emphasizes the lines of its served task IDs (AC-2.4).
  const groupIds = useMemo(() => {
    const found = insight.whereabouts.find((item) => whereaboutsKey(item) === emphasis.emphasizedGroup)
    return found ? new Set(found.task_ids) : null
  }, [insight, emphasis.emphasizedGroup])
  const { emphasizedId } = emphasis
  const lit = (id: string) => (emphasizedId ? emphasizedId === id : groupIds ? groupIds.has(id) : true)
  const shown = emphasizedId ? model.spurs.find((spur) => spur.task.id === emphasizedId) : undefined
  const labelChars = Math.floor((gutterRight - 26) / 6.4)

  // A line with keyboard focus draws a ring, so focus stays visible.
  const [focusedId, setFocusedId] = useState<string | null>(null)
  const hovered = useRef<Target>(null)
  const focused = useRef<Target>(null)
  const apply = (target: Target) => {
    if (target && 'task' in target) emphasis.emphasize(target.task)
    else if (target && 'group' in target) emphasis.emphasizeGroup(target.group)
    else emphasis.emphasize(null)
  }
  const holds = (target: Target) =>
    target !== null &&
    ('task' in target ? emphasis.emphasizedId === target.task : emphasis.emphasizedGroup === target.group)
  const enter = (target: Target) => {
    hovered.current = target
    apply(target)
  }
  const leave = (target: Target) => {
    hovered.current = null
    if (holds(target)) apply(focused.current)
  }
  const focus = (target: Target) => {
    focused.current = target
    apply(target)
  }
  const blur = (target: Target) => {
    focused.current = null
    if (holds(target)) apply(hovered.current)
  }

  const cardBelow = shown ? shown.y1 + 12 + CARD_ROOM <= model.height : true
  // Below 680 px the map scrolls sideways, so a line that takes keyboard
  // focus brings its card into view. A pointer never scrolls the map.
  const card = useRef<HTMLDivElement>(null)
  const focusedCard = focusedId !== null && shown?.task.id === focusedId
  useEffect(() => {
    if (focusedCard) card.current?.scrollIntoView?.({ block: 'nearest', inline: 'nearest' })
  }, [focusedCard, focusedId])

  return (
    <section aria-label="Where the work is" data-branch-map className="min-w-0 rounded-lg border border-border bg-card">
      <style>{mapMotion}</style>
      <header className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-b border-border px-4 py-2.5">
        <h2 className="text-sm font-medium">Where the work is</h2>
        {/* The served groups, left to right (AC-2.4). */}
        <ol className="flex flex-wrap items-center gap-1" aria-label="Tasks by where their work is">
          {insight.whereabouts.map((item, index) => {
            const key = whereaboutsKey(item)
            const target = { group: key }
            return (
              <li key={key} className="flex items-center gap-1">
                {index > 0 && <ArrowRight aria-hidden className="size-3 text-faint" />}
                <button
                  type="button"
                  data-whereabouts={item.key}
                  data-whereabouts-branch={item.branch ?? undefined}
                  aria-pressed={emphasis.emphasizedGroup === key}
                  onMouseEnter={() => enter(target)}
                  onMouseLeave={() => leave(target)}
                  onFocus={() => focus(target)}
                  onBlur={() => blur(target)}
                  className={cn(
                    'flex items-baseline gap-1.5 rounded-md border px-2 py-1 text-xs transition-colors focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-primary',
                    emphasis.emphasizedGroup === key
                      ? 'border-edge bg-raised'
                      : 'border-border bg-card hover:bg-raised',
                    item.task_ids.length === 0 && 'text-faint',
                  )}
                >
                  <span
                    data-count
                    className={cn(
                      'font-semibold tabular-nums',
                      item.key === 'merged' && item.task_ids.length > 0 && 'text-positive',
                    )}
                  >
                    {item.task_ids.length}
                  </span>
                  <span data-whereabouts-label className="text-muted">
                    {item.key === 'merged' ? (
                      <>
                        merged into <span className="font-mono text-[11px]">{item.branch}</span>
                      </>
                    ) : (
                      whereaboutsLabel(item)
                    )}
                  </span>
                </button>
              </li>
            )
          })}
        </ol>
      </header>

      <div ref={wrapper} data-branch-map-scroll className="relative overflow-x-auto">
        {/* biome-ignore lint/a11y/useSemanticElements: an SVG element cannot be a fieldset, and the group names the lines it holds */}
        <svg
          width={width}
          height={model.height}
          viewBox={`0 0 ${width} ${model.height}`}
          role="group"
          aria-label={`Branch map of ${insight.tasks.length} ${insight.tasks.length === 1 ? 'task' : 'tasks'} on ${insight.branches.join(' and ')}`}
          className="block"
        >
          {model.ticks.map((tick) => (
            <g key={tick} aria-hidden>
              <line
                x1={model.x(tick)}
                x2={model.x(tick)}
                y1={TOP - 14}
                y2={model.height - 24}
                stroke="var(--color-border)"
                strokeWidth={1}
              />
              <text
                data-tick
                x={model.x(tick)}
                y={model.height - 9}
                textAnchor="middle"
                className="fill-faint text-[10px]"
              >
                {dayLabel.format(new Date(tick))}
              </text>
            </g>
          ))}
          <g aria-hidden>
            <line
              x1={model.xNow}
              x2={model.xNow}
              y1={TOP - 14}
              y2={model.height - 24}
              stroke="var(--color-edge)"
              strokeDasharray="3 3"
            />
            <text
              data-now
              x={model.xNow}
              y={model.height - 9}
              textAnchor="middle"
              className="fill-muted text-[10px] font-medium"
            >
              Now
            </text>
          </g>

          {model.lanes.map((lane) => (
            <g key={lane.branch} data-lane={lane.branch}>
              <line
                x1={GUTTER_LEFT - 8}
                x2={model.xNow + 10}
                y1={lane.y}
                y2={lane.y}
                stroke="var(--color-foreground)"
                strokeWidth={2.5}
                strokeLinecap="round"
                opacity={0.72}
              />
              <foreignObject x={0} y={lane.y - 11} width={GUTTER_LEFT - 12} height={22}>
                <div className="flex h-full items-center justify-end">
                  <span
                    data-lane-name
                    className="inline-flex max-w-full items-center gap-1 rounded-md bg-foreground px-1.5 py-0.5 font-mono text-[11px] font-medium leading-4 text-card"
                  >
                    <GitBranch aria-hidden className="size-3 shrink-0" />
                    <span className="truncate">{lane.branch}</span>
                  </span>
                </div>
              </foreignObject>
            </g>
          ))}

          {model.spurs.map((spur, index) => {
            const { task, row } = spur
            const color = phaseMeta[task.phase].color
            const on = lit(task.id)
            const emphasized = emphasizedId === task.id
            const dashed = spur.notStarted || spur.ends === 'closed'
            const working = task.phase === 'building' || task.phase === 'planning' || task.phase === 'review'
            const title = row?.title ?? task.id
            const target = { task: task.id }
            return (
              // biome-ignore lint/a11y/useSemanticElements: an SVG group cannot be a button element
              <g
                key={task.id}
                role="button"
                tabIndex={0}
                aria-label={`${title}: ${row?.next_action.label ?? phaseMeta[task.phase].label}, base ${task.base_branch}`}
                opacity={on ? 1 : 0.22}
                className="cursor-pointer outline-none transition-opacity"
                onMouseEnter={() => enter(target)}
                onMouseLeave={() => leave(target)}
                onFocus={(event) => {
                  // Only keyboard focus draws the ring and scrolls the card
                  // into view. A click must not move the line under the
                  // pointer before the click lands.
                  if (event.currentTarget.matches(':focus-visible')) setFocusedId(task.id)
                  focus(target)
                }}
                onBlur={() => {
                  setFocusedId(null)
                  blur(target)
                }}
                onClick={() => onSelect(task.id)}
                onKeyDown={(event) => {
                  if (event.key !== 'Enter' && event.key !== ' ') return
                  // Space would scroll the page.
                  event.preventDefault()
                  onSelect(task.id)
                }}
                data-branch-task={task.id}
                data-ends={spur.ends}
                data-not-started={spur.notStarted || undefined}
                data-map-emphasized={emphasized || undefined}
                data-dimmed={on ? undefined : true}
              >
                {focusedId === task.id && (
                  <path
                    data-focus-ring
                    d={spur.path}
                    fill="none"
                    stroke="var(--color-primary)"
                    strokeOpacity={0.35}
                    strokeWidth={9}
                    strokeLinecap="round"
                  />
                )}
                <path
                  d={spur.path}
                  fill="none"
                  stroke={color}
                  strokeWidth={emphasized ? 3.5 : 2.25}
                  strokeLinecap="round"
                  strokeDasharray={dashed ? '2 5' : undefined}
                  pathLength={dashed || reducedMotion ? undefined : 1}
                  style={
                    reducedMotion
                      ? undefined
                      : {
                          animation: dashed
                            ? 'crate-map-fade 600ms ease 500ms both'
                            : `crate-map-draw 900ms cubic-bezier(0.3, 0.7, 0.2, 1) ${120 + index * 55}ms both`,
                        }
                  }
                />
                {/* A wide, unseen stroke and end target make a short line
                    easy to point at. */}
                <path d={spur.path} fill="none" stroke="transparent" strokeWidth={16} />
                <circle cx={spur.xe} cy={spur.ends === 'merged' ? spur.y0 : spur.y1} r={9} fill="transparent" />
                {spur.changes.map((cx, at) => (
                  <circle
                    key={`${task.changes_requested_at[at]}-${at}`}
                    data-change-mark
                    cx={cx}
                    cy={spur.y1}
                    r={3.25}
                    fill="var(--color-card)"
                    stroke="var(--color-attention-dot)"
                    strokeWidth={2}
                  />
                ))}
                {spur.ends === 'merged' && (
                  <circle
                    data-merge-mark
                    cx={spur.xe}
                    cy={spur.y0}
                    r={4.25}
                    fill={color}
                    stroke="var(--color-card)"
                    strokeWidth={1.75}
                  />
                )}
                {spur.ends === 'closed' && (
                  <path
                    data-close-mark
                    d={`M${spur.xe - 3.5},${spur.y1 - 3.5} l7,7 m0,-7 l-7,7`}
                    stroke="var(--color-faint)"
                    strokeWidth={1.75}
                    strokeLinecap="round"
                  />
                )}
                {spur.ends === 'open' && (
                  <>
                    {working && !reducedMotion && (
                      <circle r={2.4} fill={color}>
                        <animateMotion dur="2.6s" repeatCount="indefinite" path={spur.path} begin={`${index * 0.3}s`} />
                      </circle>
                    )}
                    {task.phase === 'needs_you' && !reducedMotion && (
                      <circle
                        cx={spur.xe}
                        cy={spur.y1}
                        r={5}
                        fill="none"
                        stroke={color}
                        style={{ animation: 'crate-map-ping 1.6s ease-out infinite' }}
                      />
                    )}
                    <circle
                      cx={spur.xe}
                      cy={spur.y1}
                      r={4.75}
                      fill={spur.notStarted ? 'var(--color-card)' : color}
                      stroke={spur.notStarted ? color : 'var(--color-card)'}
                      strokeWidth={1.75}
                    />
                    <text data-open-label x={spur.xe + 12} y={spur.y1 + 3.5} className="text-[11px]">
                      <tspan className={cn('fill-foreground', emphasized && 'font-medium')}>
                        {shortTitle(title, Math.max(10, labelChars - phaseMeta[task.phase].label.length - 3))}
                      </tspan>
                      <tspan className="fill-muted"> · {phaseMeta[task.phase].label}</tspan>
                    </text>
                  </>
                )}
              </g>
            )
          })}
        </svg>

        {/* The task's card while it is pointed at or holds focus (AC-2.5). */}
        {shown && (
          <div
            ref={card}
            data-branch-card={shown.task.id}
            className="pointer-events-none absolute z-10 w-max max-w-64 rounded-md border border-border bg-card px-2.5 py-1.5 shadow-md"
            style={{
              left: Math.min(Math.max((shown.xs + Math.min(shown.xe, model.xNow)) / 2, 120), width - 140),
              top: cardBelow ? shown.y1 + 12 : shown.y1 - 12,
              transform: cardBelow ? 'translateX(-50%)' : 'translate(-50%, -100%)',
            }}
          >
            <p className="text-xs font-medium leading-snug text-foreground">
              {shortTitle(shown.row?.title ?? shown.task.id, 64)}
            </p>
            <p className="mt-0.5 text-[11px] leading-4 text-muted">
              {shown.row && <span data-branch-card-label>{shown.row.next_action.label}</span>}
              {shown.row?.pull_request?.number ? (
                <span data-branch-card-pull>{` · #${shown.row.pull_request.number}`}</span>
              ) : null}
              <span data-branch-card-rounds>
                {`${shown.row ? ' · ' : ''}${shown.task.review_rounds} review ${shown.task.review_rounds === 1 ? 'round' : 'rounds'}`}
              </span>
            </p>
            <p data-branch-card-branch className="font-mono text-[11px] leading-4 text-faint">
              {shown.ends === 'merged'
                ? `in ${shown.task.base_branch} since ${timeLabel.format(new Date(shown.task.merged_at as string))}`
                : `${shown.row?.branch ?? 'task branch'} → ${shown.task.base_branch}`}
            </p>
          </div>
        )}
      </div>

      <footer className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-border px-4 py-2 text-[11px] text-muted">
        <span data-legend="merged" className="inline-flex items-center gap-1.5">
          <svg width="22" height="10" aria-hidden="true">
            <circle cx="16" cy="5" r="4" fill="var(--color-positive)" />
            <line x1="0" x2="12" y1="5" y2="5" stroke="var(--color-positive)" strokeWidth="2" />
          </svg>
          Merged into the branch
        </span>
        <span data-legend="changes" className="inline-flex items-center gap-1.5">
          <svg width="10" height="10" aria-hidden="true">
            <circle cx="5" cy="5" r="3.25" fill="none" stroke="var(--color-attention-dot)" strokeWidth="2" />
          </svg>
          Review asked for changes
        </span>
        <span data-legend="not-started" className="inline-flex items-center gap-1.5">
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
      </footer>
    </section>
  )
}

// The map's motion: a line draws in once, a broken line fades in, and a task
// that waits on a person pulses. None runs under reduced motion, where the
// map renders no animation at all.
const mapMotion = `
@keyframes crate-map-draw {
  from { stroke-dasharray: 1; stroke-dashoffset: 1; }
  to { stroke-dasharray: 1; stroke-dashoffset: 0; }
}
@keyframes crate-map-fade {
  from { opacity: 0; }
  to { opacity: 1; }
}
@keyframes crate-map-ping {
  from { r: 5px; opacity: 0.9; }
  to { r: 13px; opacity: 0; }
}
`
