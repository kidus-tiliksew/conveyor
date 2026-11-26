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
//
// Dependencies (req-crate-insights AC-6.3, AC-6.4; component-crates invariant
// 39; DEC-151(2)): a lane's open lines take rows by the served `wait_depth`
// first, a link joins the end of an open member's line to the end of each
// open line whose `waiting` entry names it, and a mark on a member's line
// stands at the served merge time of each member it depends on. Every line
// and mark stays at its recorded time: the map places nothing after the
// present.
import { ArrowRight, GitBranch, RotateCw } from 'lucide-react'
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { CrateDetail, CrateInsight, CrateInsightTask, CrateTask, CrateWhereabouts } from '../../lib/types'
import { useMediaQuery } from '../../lib/use-media-query'
import { cn } from '../../lib/utils'
import { Button } from '../ui/button'
import { Skeleton } from '../ui/skeleton'
import type { CrateEmphasis, EmphasisTarget } from './crate-emphasis'
import { DependencyDetails, dependencyLineCount } from './dependency-details'
import { relatedMembers } from './dependency-model'
import { phaseMeta } from './line-model'

const DAY = 86_400_000
const ROW = 22
const LANE_HEAD = 20
const LANE_GAP = 26
const TOP = 30
const GUTTER_LEFT = 92
// Below this width the map keeps it and scrolls sideways inside its card.
const MIN_WIDTH = 680
// The hover card's height before it is measured, used to keep the card
// inside the map, and the height each dependency line adds to it.
const CARD_ROOM = 64
const CARD_DEPENDENCY_LINE = 16
// The space a card keeps from the edge of what a person can see, and the
// least room in which a card shows at all: one line of text and its border.
const CARD_EDGE = 4
const CARD_LEAST_ROOM = 20
// How long a card that scrolls stays after the pointer leaves its line or
// the card, so the pointer can cross the gap between them.
const CARD_REACH_MS = 250
// The order in which a lane's open lines take rows, nearest the merge first.
const OPEN_ORDER = ['gate', 'review', 'build', 'plan', 'intake', 'crate', 'aside']

// One task line's shape: it leaves the lane (y0) at xs, bends down to its
// own row (y1) over `bend` px, runs along the row, and for a merged task bends
// back up to the lane at xe. The path and the marks on it both come from this
// one shape.
interface LineShape {
  xs: number
  xe: number
  y0: number
  y1: number
  bend: number
  returns: boolean
}

interface Spur extends LineShape {
  task: CrateInsightTask
  row: CrateTask | undefined
  ends: 'merged' | 'closed' | 'open'
  notStarted: boolean
  path: string
  // Each change-request mark: its served x and the line's y there.
  changes: Array<{ x: number; y: number }>
}

// A link from the open member that is waited on (`from`) to the open member
// that waits on it (`to`). The arrowhead is at the task that waits.
interface DependencyLink {
  from: string
  to: string
  d: string
}

// The merge of member `from` on the line of member `to`, which depends on
// it: a dotted drop from the merge to the dependent line, and a ring where it
// meets the line.
interface DependencyMergeMark {
  from: string
  to: string
  x: number
  y: number
  d: string | null
}

// A bend is the cubic curve from (x0, ya) to (x0 + bend, yb) whose control
// points sit at 0.7 and 0.3 of the bend on the start and end heights. Its x
// and y, as shares of the bend, at curve parameter t:
const bendX = (t: number) => 3 * (1 - t) ** 2 * t * 0.7 + 3 * (1 - t) * t ** 2 * 0.3 + t ** 3
const bendY = (t: number) => 3 * (1 - t) * t ** 2 + t ** 3

export function linePath(shape: LineShape) {
  const { xs, xe, y0, y1, bend } = shape
  const out = `M${xs},${y0} C${xs + bend * 0.7},${y0} ${xs + bend * 0.3},${y1} ${xs + bend},${y1}`
  return shape.returns
    ? `${out} L${xe - bend},${y1} C${xe - bend * 0.3},${y1} ${xe - bend * 0.7},${y0} ${xe},${y0}`
    : `${out} L${xe},${y1}`
}

// The y of the line at x, on the same curves linePath draws. A bend's x
// grows with t, so t is found by bisection.
export function lineYAt(shape: LineShape, x: number) {
  const { xs, xe, y0, y1, bend, returns } = shape
  const onBend = (from: number, ya: number, yb: number) => {
    const target = (x - from) / bend
    let low = 0
    let high = 1
    for (let step = 0; step < 32; step += 1) {
      const mid = (low + high) / 2
      if (bendX(mid) < target) low = mid
      else high = mid
    }
    return ya + (yb - ya) * bendY((low + high) / 2)
  }
  if (bend <= 0) return x <= xs ? y0 : y1
  if (x <= xs) return y0
  if (x < xs + bend) return onBend(xs, y0, y1)
  if (returns && x >= xe) return y0
  if (returns && x > xe - bend) return onBend(xe - bend, y1, y0)
  return y1
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
            // A task takes a row below every task of the lane it waits on
            // (req-crate-insights AC-6.3).
            a.wait_depth - b.wait_depth ||
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
        const shape: LineShape = {
          xs,
          xe,
          y0: laneY,
          y1,
          bend: Math.min(16, (xe - xs) / 2),
          returns: ends === 'merged',
        }
        // Each round that asked for changes, at its served time and on the
        // line there, also inside a bend (AC-2.3).
        const changes = task.changes_requested_at
          .map(served)
          .flatMap((time) => (time === null ? [] : [{ x: x(time), y: lineYAt(shape, x(time)) }]))
        spurs.push({
          ...shape,
          task,
          row: rows.get(task.id),
          ends,
          notStarted: ends === 'open' && task.station === 'intake',
          path: linePath(shape),
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
    const spurs = lanes.flatMap((lane) => lane.spurs)

    // The dependency links and merge marks (AC-6.3, AC-6.4), from the
    // served entries only.
    const byId = new Map(spurs.map((spur) => [spur.task.id, spur]))
    const links: DependencyLink[] = []
    const marks: DependencyMergeMark[] = []
    // Brackets that leave the same end step left, so they stay apart.
    const bracketDepth = new Map<number, number>()
    for (const waiter of spurs) {
      for (const entry of waiter.task.depends_on) {
        if (!entry.member) continue
        const source = byId.get(entry.id)
        if (!source) continue
        if (entry.waiting && source.ends === 'open' && waiter.ends === 'open') {
          // A bracket left of the two ends, from the task waited on to the
          // task that waits, with the arrowhead at the task that waits.
          const fx = source.xe
          const fy = source.y1
          const tx = waiter.xe
          const ty = waiter.y1
          const level = bracketDepth.get(Math.round(fx)) ?? 0
          bracketDepth.set(Math.round(fx), level + 1)
          const bx = Math.min(fx, tx) - 11 - (level % 3) * 4
          const dir = ty > fy ? 1 : -1
          links.push({
            from: source.task.id,
            to: waiter.task.id,
            d: `M${fx - 6},${fy} H${bx + 4} Q${bx},${fy} ${bx},${fy + 4 * dir} V${ty - 4 * dir} Q${bx},${ty} ${bx + 4},${ty} H${tx - 6}`,
          })
          continue
        }
        // The merge it depends on, when that merge falls after the task's
        // creation and before its merge or close, and no later than the
        // present. The bounds compare served times, never positions.
        const merged = served(source.task.merged_at)
        const created = served(waiter.task.created_at)
        const finished = served(waiter.task.merged_at) ?? served(waiter.task.closed_at)
        if (merged === null || created === null || merged <= created || merged > now) continue
        if (finished !== null && merged >= finished) continue
        const xm = x(merged)
        const yLine = lineYAt(waiter, xm)
        const yFrom = source.y0
        const dir = yLine > yFrom ? 1 : -1
        marks.push({
          from: source.task.id,
          to: waiter.task.id,
          x: xm,
          y: yLine,
          d: Math.abs(yLine - yFrom) > 12 ? `M${xm},${yFrom + 5 * dir} V${yLine - 4 * dir}` : null,
        })
      }
    }
    return { lanes, height, x, xNow, ticks, spurs, links, marks }
  }, [insight, width, gutterRight, rows])

  // Pointing at a count emphasizes the lines of its served task IDs (AC-2.4).
  const groupIds = useMemo(() => {
    const found = insight.whereabouts.find((item) => whereaboutsKey(item) === emphasis.emphasizedGroup)
    return found ? new Set(found.task_ids) : null
  }, [insight, emphasis.emphasizedGroup])
  const { emphasizedId } = emphasis
  // An emphasized task keeps the members its dependencies connect it to lit,
  // in either direction and through any number of members (AC-6.6).
  const related = useMemo(() => relatedMembers(insight.tasks, emphasizedId), [insight.tasks, emphasizedId])
  const lit = (id: string) => (related ? related.has(id) : groupIds ? groupIds.has(id) : true)
  // The card's task: the emphasized one, or for a moment after the pointer
  // leaves, the task of a card that scrolls, so the pointer can reach it.
  // The emphasis itself clears at once, so no late clear undoes what
  // another part of the page emphasizes.
  const [lingering, setLingering] = useState<string | null>(null)
  const cardId = emphasizedId ?? lingering
  const shown = cardId ? model.spurs.find((spur) => spur.task.id === cardId) : undefined
  const labelChars = Math.floor((gutterRight - 26) / 6.4)

  // A line with keyboard focus draws a ring, so focus stays visible.
  const [focusedId, setFocusedId] = useState<string | null>(null)
  // The pointer and focus targets stay apart in the shared emphasis, so
  // leaving a line or count clears only the pointer's target and a line or
  // count that still has focus keeps its emphasis (AC-2.5).
  // Leaving clears the pointer's target. A card that scrolls stays a
  // moment longer, and entering it emphasizes its task again. On its way,
  // the pointer can cross another line or count: that one takes the
  // emphasis only if the pointer still rests on it when the moment ends.
  const leaving = useRef<number | undefined>(undefined)
  const heldCard = useRef<string | null>(null)
  const held = useRef<string | null>(null)
  const hovered = useRef<EmphasisTarget | null>(null)
  // A line can report its leave after the pointer has reached the card.
  // While the pointer is on the card, such a late leave changes nothing.
  const onCard = useRef(false)
  useEffect(() => () => window.clearTimeout(leaving.current), [])
  const show = (target: EmphasisTarget) =>
    'task' in target ? emphasis.emphasize(target.task) : emphasis.emphasizeGroup(target.group)
  const enter = (target: EmphasisTarget) => {
    hovered.current = target
    if (held.current !== null && !('task' in target && target.task === held.current)) return
    window.clearTimeout(leaving.current)
    held.current = null
    setLingering(null)
    show(target)
  }
  const leave = (target?: EmphasisTarget) => {
    const left = hovered.current
    if (
      target &&
      left &&
      ('task' in target ? 'task' in left && left.task === target.task : 'group' in left && left.group === target.group)
    )
      hovered.current = null
    if (held.current !== null || (target && onCard.current)) return
    window.clearTimeout(leaving.current)
    emphasis.emphasize(null)
    const stays = heldCard.current
    if (!stays) return
    held.current = stays
    setLingering(stays)
    leaving.current = window.setTimeout(() => {
      held.current = null
      setLingering(null)
      const next = hovered.current
      if (next) show(next)
    }, CARD_REACH_MS)
  }
  // The pointer reaches the card: its task takes the emphasis again.
  const enterCard = () => {
    onCard.current = true
    window.clearTimeout(leaving.current)
    const task = held.current
    held.current = null
    hovered.current = null
    if (task === null) return
    setLingering(null)
    emphasis.emphasize(task)
  }
  const focus = (target: EmphasisTarget) => emphasis.focus(target)
  const blur = (target: EmphasisTarget) => emphasis.blur(target)

  // Below 680 px the map scrolls sideways, so a line that takes keyboard
  // focus brings its card into view. A pointer never scrolls the map.
  const card = useRef<HTMLDivElement>(null)
  // The card's full height, measured after each render, also while it
  // scrolls. Its placement moves it and does not change this height. Until
  // it is measured, the estimate counts the dependency lines.
  const [cardHeight, setCardHeight] = useState<number | null>(null)
  const [cardWidth, setCardWidth] = useState<number | null>(null)
  useLayoutEffect(() => {
    const height = card.current ? card.current.scrollHeight + 2 : null
    const cardWide = card.current ? card.current.offsetWidth : null
    setCardHeight((current) => (current === height ? current : height))
    setCardWidth((current) => (current === cardWide ? current : cardWide))
  })
  const cardRoom =
    cardHeight ?? (shown ? CARD_ROOM + dependencyLineCount(shown.task) * CARD_DEPENDENCY_LINE : CARD_ROOM)
  // The part of the map a person can see: inside the crate page's scroll
  // area and the window, in the map's own rows, and inside the map's own
  // sideways scroll. It is read again on each scroll and resize while a
  // card shows.
  const shownId = shown?.task.id
  const [seen, setSeen] = useState<{ top: number; bottom: number; left: number; right: number } | null>(null)
  useLayoutEffect(() => {
    const element = wrapper.current
    if (!element || !shownId) return
    const scroller = element.closest('[data-crate-scroll]')
    const read = () => {
      const frame = element.getBoundingClientRect()
      const port = scroller?.getBoundingClientRect()
      const top = Math.max(port?.top ?? 0, 0) - frame.top
      const bottom = Math.min(port?.bottom ?? window.innerHeight, window.innerHeight) - frame.top
      const left = element.scrollLeft
      const right = element.scrollLeft + element.clientWidth
      setSeen((current) =>
        current && current.top === top && current.bottom === bottom && current.left === left && current.right === right
          ? current
          : { top, bottom, left, right },
      )
    }
    read()
    const target = scroller ?? window
    target.addEventListener('scroll', read, { passive: true })
    element.addEventListener('scroll', read, { passive: true })
    window.addEventListener('resize', read)
    return () => {
      target.removeEventListener('scroll', read)
      element.removeEventListener('scroll', read)
      window.removeEventListener('resize', read)
    }
  }, [shownId, wrapper])
  // The card stands below the line when it fits there, and above it when it
  // fits there instead. When it fits on neither side it stands below, in
  // room the map keeps under its last row: above, its top would be cut off.
  // A card that the pointer shows keeps its whole box inside the part a
  // person can see (req-crate-insights AC-6.1). Above the line, it runs from
  // the seen part's top, or the map's top, down to the line. Below the line,
  // it runs from the line down to the seen part's foot. When it fits there
  // on neither side, it takes the side with more room, keeps to exactly
  // that room, and scrolls, and only then takes the pointer, which can move
  // onto it from the line. It opens scrolled just far enough to show its
  // first waiting task. With less than one line of room on both sides, it
  // hides. A card that keyboard focus shows stays whole, and the page
  // scrolls to it while the focus stays on the line.
  const focusedCard = focusedId !== null && shown?.task.id === focusedId
  let cardBelow = shown ? shown.y1 + 12 + cardRoom <= model.height || shown.y1 - 12 - cardRoom < 0 : true
  // The card's top edge when it stands below, or its foot when above.
  let cardEdge = shown ? (cardBelow ? shown.y1 + 12 : shown.y1 - 12) : 0
  let cardCap: number | null = null
  let cardHidden = false
  if (shown && seen && !focusedCard) {
    const overTop = Math.max(seen.top, 0) + CARD_EDGE
    const overFoot = Math.min(shown.y1 - 12, seen.bottom - CARD_EDGE)
    const underTop = Math.max(shown.y1 + 12, seen.top + CARD_EDGE)
    const underFoot = seen.bottom - CARD_EDGE
    const roomAbove = overFoot - overTop
    const roomBelow = underFoot - underTop
    const fitsBelow = cardRoom <= roomBelow
    const fitsAbove = cardRoom <= roomAbove
    if (cardBelow ? !fitsBelow : !fitsAbove) {
      if (fitsBelow || fitsAbove) cardBelow = fitsBelow
      else {
        cardBelow = roomBelow >= roomAbove
        cardCap = Math.floor(cardBelow ? roomBelow : roomAbove)
        cardHidden = cardCap < CARD_LEAST_ROOM
      }
    }
    cardEdge = cardBelow ? underTop : overFoot
  }
  // The map keeps that room for the tallest such card from the start, so
  // pointing at a line does not move the page. The shown card's measure
  // adds any height the estimate missed.
  const reserved = useMemo(
    () =>
      Math.max(
        0,
        ...model.spurs.map((spur) => {
          const room = CARD_ROOM + dependencyLineCount(spur.task) * CARD_DEPENDENCY_LINE
          const fits = spur.y1 + 12 + room <= model.height || spur.y1 - 12 - room >= 0
          return fits ? 0 : spur.y1 + 12 + room - model.height
        }),
      ),
    [model],
  )
  // The card's centre, kept so the whole card stands inside the part of the
  // map's sideways scroll that a person can see.
  let cardLeft = shown ? Math.min(Math.max((shown.xs + Math.min(shown.xe, model.xNow)) / 2, 120), width - 140) : 0
  if (seen && cardWidth !== null && seen.right - seen.left >= cardWidth + 2 * CARD_EDGE)
    cardLeft = Math.min(
      Math.max(cardLeft, seen.left + cardWidth / 2 + CARD_EDGE),
      seen.right - cardWidth / 2 - CARD_EDGE,
    )
  heldCard.current = shown && cardCap !== null && !cardHidden ? shown.task.id : null
  // A card that scrolls opens scrolled just far enough to show its first
  // waiting task, once for each task it shows.
  const openedFor = useRef<string | null>(null)
  const capped = cardCap !== null
  useLayoutEffect(() => {
    const element = card.current
    if (!element || !capped || !shownId) {
      if (!capped) openedFor.current = null
      return
    }
    if (openedFor.current === shownId) return
    openedFor.current = shownId
    const first = element.querySelector('[data-waits-on]')
    if (!first) return
    const end =
      first.getBoundingClientRect().bottom - element.getBoundingClientRect().top - element.clientTop + element.scrollTop
    element.scrollTop = Math.max(0, end - element.clientHeight)
  }, [capped, shownId])
  const below = shown && cardBelow ? shown.y1 + 12 + (cardCap ?? cardRoom) - model.height : 0
  const mapHeight = model.height + Math.max(reserved, below)
  useEffect(() => {
    if (focusedCard) card.current?.scrollIntoView?.({ block: 'nearest', inline: 'nearest' })
  }, [focusedCard, focusedId])
  // A map that scrolls sideways opens scrolled to the present, where the
  // open lines and their links end. It does so once, after its first
  // measure, and later refreshes keep the user's scroll.
  const openedAtPresent = useRef(false)
  useLayoutEffect(() => {
    const element = wrapper.current
    if (openedAtPresent.current || !element || measured === 0) return
    openedAtPresent.current = true
    if (element.scrollWidth > element.clientWidth) element.scrollLeft = element.scrollWidth
  }, [measured, wrapper])

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

      <div ref={wrapper} data-branch-map-scroll className="relative overflow-x-auto" style={{ minHeight: mapHeight }}>
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

          {(model.links.length > 0 || model.marks.length > 0) && (
            <g data-dependency-links aria-hidden>
              <defs>
                <marker
                  id="crate-dependency-arrow"
                  viewBox="0 0 8 8"
                  refX="7"
                  refY="4"
                  markerWidth="7"
                  markerHeight="7"
                  orient="auto-start-reverse"
                >
                  <path
                    d="M1,1 L7,4 L1,7"
                    fill="none"
                    stroke="var(--color-muted)"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                </marker>
              </defs>
              {model.links.map((link) => {
                const on = lit(link.from) && lit(link.to)
                const strong = emphasizedId !== null && on
                return (
                  <path
                    key={`${link.from}>${link.to}`}
                    data-dependency-link={`${link.from}>${link.to}`}
                    data-dimmed={on ? undefined : true}
                    d={link.d}
                    fill="none"
                    stroke={strong ? 'var(--color-foreground)' : 'var(--color-muted)'}
                    strokeWidth={strong ? 1.75 : 1.25}
                    strokeLinecap="round"
                    opacity={on ? 0.9 : 0.12}
                    markerEnd="url(#crate-dependency-arrow)"
                    className="transition-opacity"
                  />
                )
              })}
              {model.marks.map((mark) => {
                const on = lit(mark.from) && lit(mark.to)
                const strong = emphasizedId !== null && on
                const stroke = strong ? 'var(--color-foreground)' : 'var(--color-muted)'
                return (
                  <g
                    key={`${mark.from}>${mark.to}`}
                    data-dependency-merge={`${mark.from}>${mark.to}`}
                    data-dimmed={on ? undefined : true}
                    opacity={on ? (strong ? 0.95 : 0.7) : 0.12}
                    className="transition-opacity"
                  >
                    {mark.d && (
                      <path
                        d={mark.d}
                        fill="none"
                        stroke={stroke}
                        strokeWidth={1.25}
                        strokeDasharray="1 3"
                        strokeLinecap="round"
                      />
                    )}
                    <circle
                      cx={mark.x}
                      cy={mark.y}
                      r={2.75}
                      fill="var(--color-card)"
                      stroke={stroke}
                      strokeWidth={1.5}
                    />
                  </g>
                )
              })}
            </g>
          )}

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
                  data-line-path
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
                {spur.changes.map((mark, at) => (
                  <circle
                    key={`${task.changes_requested_at[at]}-${at}`}
                    data-change-mark
                    cx={mark.x}
                    cy={mark.y}
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
        {shown && !cardHidden && (
          // biome-ignore lint/a11y/noStaticElementInteractions: the pointer that rests on a card that scrolls keeps the card; the card holds no control
          <div
            ref={card}
            data-branch-card={shown.task.id}
            data-branch-card-scrolls={cardCap !== null}
            className={cn(
              'absolute z-10 w-max max-w-64 overflow-y-auto overscroll-contain rounded-md border border-border bg-card px-2.5 py-1.5 shadow-md',
              cardCap === null && 'pointer-events-none',
            )}
            onMouseEnter={enterCard}
            onMouseLeave={() => {
              onCard.current = false
              leave()
            }}
            style={{
              maxHeight: cardCap ?? undefined,
              left: cardLeft,
              top: cardEdge,
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
            {/* What the task waits on and how many members wait on it
                (AC-6.1, AC-6.2). */}
            <DependencyDetails task={shown.task} className="mt-1 border-t border-border pt-1" />
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
        {/* Each dependency mark is named only while the map draws one. */}
        {model.links.length > 0 && (
          <span data-legend="waits" className="inline-flex items-center gap-1.5">
            <svg width="22" height="12" aria-hidden="true">
              <path d="M2,1 V5 Q2,9 6,9 H19" fill="none" stroke="var(--color-muted)" strokeWidth="1.25" />
              <path d="M16,6.5 L19,9 L16,11.5" fill="none" stroke="var(--color-muted)" strokeWidth="1.25" />
            </svg>
            Waits for that task to merge
          </span>
        )}
        {model.marks.length > 0 && (
          <span data-legend="dependency-merged" className="inline-flex items-center gap-1.5">
            <svg width="10" height="14" aria-hidden="true">
              <path
                d="M5,1 V7"
                stroke="var(--color-muted)"
                strokeWidth="1.25"
                strokeDasharray="1 3"
                strokeLinecap="round"
              />
              <circle cx="5" cy="10.5" r="2.5" fill="var(--color-card)" stroke="var(--color-muted)" strokeWidth="1.5" />
            </svg>
            A task it waited on merged
          </span>
        )}
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
