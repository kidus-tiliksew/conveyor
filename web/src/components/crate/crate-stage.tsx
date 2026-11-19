// The top of a crate's page: the crate's name, counts, shipped or closed
// statement, and target date (req-crates AC-5.2) over the line of stations
// and its replay (req-crate-insights REQ-1; component-crates invariants 33
// and 34). Every count, `shipped`, station, phase, and replay step is the
// server's (component-crates invariants 9 and 21; DEC-143(3)). The page
// formats them and derives none.
import { Archive, CalendarPlus, CircleCheck, Pause, Play, RotateCw } from 'lucide-react'
import { Component, lazy, type ReactNode, Suspense, useEffect, useMemo, useRef, useState } from 'react'
import type { CrateDetail, CrateInsight } from '../../lib/types'
import { useMediaQuery } from '../../lib/use-media-query'
import { cn } from '../../lib/utils'
import { Button } from '../ui/button'
import type { CrateEmphasis } from './crate-emphasis'
import { formatTargetDate, targetDateHint } from './crate-parts'
import { type LineBlock, lineStations, phaseMeta, phasesInUse, placesAt, replaySpan, stationCounts } from './line-model'

// three.js and its React bindings load with this chunk only (DEC-143(2)).
const LineScene = lazy(() => import('./line-scene'))

// A replay plays the crate's whole history in this many milliseconds.
const REPLAY_MS = 9000

// Reports a scene that fails to draw, so the page shows the station counts
// as text in its place (req-crate-insights AC-1.5).
class SceneBoundary extends Component<{ children: ReactNode; onFail: () => void }, { failed: boolean }> {
  state = { failed: false }
  static getDerivedStateFromError() {
    return { failed: true }
  }
  componentDidCatch() {
    this.props.onFail()
  }
  render() {
    return this.state.failed ? null : this.props.children
  }
}

// Whether this browser can create a WebGL context. The answer holds for the
// page's life.
let webglAnswer: boolean | undefined
function webglAvailable() {
  if (webglAnswer !== undefined) return webglAnswer
  try {
    const canvas = document.createElement('canvas')
    const context = canvas.getContext('webgl2') ?? canvas.getContext('webgl')
    webglAnswer = context != null
    context?.getExtension('WEBGL_lose_context')?.loseContext()
  } catch {
    webglAnswer = false
  }
  return webglAnswer
}

const replayLabel = new Intl.DateTimeFormat('en', { month: 'short', day: 'numeric', hour: 'numeric' })

function useDarkTheme() {
  const [dark, setDark] = useState(() => document.documentElement.dataset.theme === 'dark')
  useEffect(() => {
    const observer = new MutationObserver(() => setDark(document.documentElement.dataset.theme === 'dark'))
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
    return () => observer.disconnect()
  }, [])
  return dark
}

export function CrateStage({
  crate,
  insight,
  insightError,
  retryingInsight,
  onRetryInsight,
  canAct,
  onSetDate,
  emphasis,
  onSelect,
}: {
  crate: CrateDetail
  // The insight view. It loads and fails apart from the crate read, so the
  // header facts and the rest of the page never wait on it.
  insight: CrateInsight | undefined
  insightError: unknown
  retryingInsight: boolean
  onRetryInsight: () => void
  canAct: boolean
  onSetDate: () => void
  emphasis: CrateEmphasis
  onSelect: (id: string) => void
}) {
  const open = crate.state === 'open'
  const compact = useMediaQuery('(max-width: 767px)')
  const reducedMotion = useMediaQuery('(prefers-reduced-motion: reduce)')
  const dark = useDarkTheme()
  // Without WebGL, or once the scene fails, the counts are text.
  const [sceneFailed, setSceneFailed] = useState(() => !webglAvailable())

  // The scene renders only while it is on screen.
  const band = useRef<HTMLDivElement>(null)
  const [visible, setVisible] = useState(true)
  useEffect(() => {
    if (!band.current || typeof IntersectionObserver === 'undefined') return
    const observer = new IntersectionObserver(([entry]) => setVisible(entry.isIntersecting), { rootMargin: '120px' })
    observer.observe(band.current)
    return () => observer.disconnect()
  }, [])

  // Replay: the line as it stood at an earlier moment, played forward to
  // the present. `cursor` null is the present.
  const span = insight ? replaySpan(insight) : { start: 0, end: 0, usable: false }
  const { start, end, usable } = span
  const [cursor, setCursor] = useState<number | null>(null)
  const [playing, setPlaying] = useState(false)
  // A refresh can move the span: the cursor stays inside it, and a cursor
  // at or past the end is the present.
  const at = cursor === null || !usable || cursor >= end ? null : Math.max(cursor, start)
  const atRef = useRef(at)
  atRef.current = at
  useEffect(() => {
    if (!playing || !usable) return
    let frame = 0
    let last = performance.now()
    let value = atRef.current ?? start
    const tick = (now: number) => {
      value += ((now - last) / REPLAY_MS) * (end - start)
      last = now
      if (value >= end) {
        // The replay ends at the present (req-crate-insights AC-1.4).
        setPlaying(false)
        setCursor(null)
        return
      }
      setCursor(value)
      frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
  }, [playing, usable, start, end])

  // The blocks at the shown moment, joined by task ID to the crate read's
  // title and verbatim next-action label. The scene gets a new list only
  // when a task changes place.
  const rows = useMemo(() => new Map(crate.tasks.map((task) => [task.id, task])), [crate.tasks])
  const shown = useRef<{ key: string; blocks: LineBlock[] }>({ key: '', blocks: [] })
  const blocks = useMemo(() => {
    const next: LineBlock[] = (insight ? placesAt(insight, at) : []).map((place) => {
      const row = rows.get(place.id)
      return { ...place, title: row?.title, label: row?.next_action.label }
    })
    const key = next
      .map((block) => `${block.id}:${block.station}:${block.phase}:${block.baseBranch}:${block.title}:${block.label}`)
      .join('|')
    if (key !== shown.current.key) shown.current = { key, blocks: next }
    return shown.current.blocks
  }, [insight, at, rows])
  const counts = useMemo(() => stationCounts(blocks), [blocks])
  const legend = useMemo(() => phasesInUse(blocks), [blocks])

  const textMode = sceneFailed
  const sceneShown = Boolean(insight) && !textMode

  return (
    <section aria-label="Crate summary" className="overflow-hidden rounded-xl border border-border bg-card">
      <div
        ref={band}
        data-line-mode={textMode ? 'text' : 'scene'}
        data-motion={reducedMotion ? 'reduced' : 'full'}
        className={cn('relative', !textMode && 'h-[19rem] md:h-[23rem]')}
        style={
          textMode
            ? undefined
            : {
                background:
                  'radial-gradient(120% 90% at 78% 100%, color-mix(in oklab, var(--color-primary) 9%, transparent), transparent 62%), linear-gradient(180deg, var(--color-card), var(--color-surface))',
              }
        }
      >
        {!textMode && (
          <>
            {/* A faint floor grid gives the line something to stand on. */}
            <div
              aria-hidden
              className="absolute inset-x-0 bottom-0 h-3/5 opacity-60"
              style={{
                backgroundImage:
                  'linear-gradient(var(--color-border) 1px, transparent 1px), linear-gradient(90deg, var(--color-border) 1px, transparent 1px)',
                backgroundSize: '34px 34px',
                maskImage: 'linear-gradient(180deg, transparent, black 55%, transparent)',
                transform: 'perspective(600px) rotateX(58deg)',
                transformOrigin: 'bottom',
              }}
            />
            <div className="absolute inset-0">
              {sceneShown && visible && (
                <SceneBoundary onFail={() => setSceneFailed(true)}>
                  <Suspense fallback={null}>
                    <LineScene
                      blocks={blocks}
                      counts={counts}
                      shipped={crate.shipped && at === null}
                      closed={!open}
                      compact={compact}
                      dark={dark}
                      reducedMotion={reducedMotion}
                      emphasizedId={emphasis.emphasizedId}
                      onEmphasize={emphasis.emphasize}
                      onSelect={onSelect}
                      padTop={compact ? 100 : 78}
                      padBottom={compact ? 44 : 22}
                    />
                  </Suspense>
                </SceneBoundary>
              )}
            </div>
          </>
        )}

        <HeaderFacts crate={crate} canAct={canAct} onSetDate={onSetDate} overlay={!textMode} />

        {insightError != null && !insight && (
          <div
            data-line-error
            className={cn(
              'flex flex-wrap items-center gap-2 text-sm text-muted',
              textMode ? 'px-5 pb-4 sm:px-6' : 'absolute inset-x-0 bottom-3 justify-center px-4',
            )}
          >
            <span>
              The line of stations did not load.
              {insightError instanceof Error && insightError.message ? ` ${insightError.message}` : ''}
            </span>
            <Button variant="outline" size="sm" onClick={onRetryInsight} disabled={retryingInsight}>
              <RotateCw aria-hidden />
              Try again
            </Button>
          </div>
        )}

        {/* The same picture in words. Assistive technology always reads this
            list in place of the canvas, and without WebGL every reader sees
            it (req-crate-insights AC-1.5; DEC-143(5)). */}
        {insight && (
          <ul
            aria-label="Tasks at each station"
            data-line-counts
            className={textMode ? 'grid grid-cols-3 gap-2 px-5 pb-4 sm:grid-cols-6 sm:px-6' : 'sr-only'}
          >
            {lineStations.map((station) => (
              <li
                key={station.id}
                data-station={station.id}
                className={textMode ? 'rounded-lg border border-border bg-surface px-2.5 py-2' : undefined}
              >
                <span className={textMode ? 'block truncate text-xs text-muted' : undefined}>{station.label}</span>
                <span className="sr-only">: </span>
                <span
                  data-count
                  className={textMode ? 'block text-lg font-semibold tabular-nums text-foreground' : undefined}
                >
                  {counts[station.id]}
                </span>
              </li>
            ))}
          </ul>
        )}

        {usable && (
          <div
            data-replay
            className={cn(
              'flex w-max items-center gap-2 rounded-full border border-border bg-card/90 py-1 pr-3 pl-1 shadow-xs backdrop-blur',
              textMode ? 'mx-5 mb-4 sm:mx-6' : 'absolute bottom-3 left-4 sm:left-5',
            )}
          >
            <button
              type="button"
              aria-label={
                playing
                  ? 'Pause the replay'
                  : at === null
                    ? 'Replay this crate from its first task'
                    : 'Continue the replay'
              }
              title={playing ? 'Pause' : at === null ? 'Replay this crate from its first task' : 'Continue the replay'}
              onClick={() => {
                if (!playing && at === null) setCursor(start)
                setPlaying(!playing)
              }}
              className="grid size-6 place-items-center rounded-full bg-foreground text-card hover:opacity-85 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
            >
              {playing ? <Pause aria-hidden className="size-3" /> : <Play aria-hidden className="ml-px size-3" />}
            </button>
            <input
              type="range"
              aria-label="Moment in the crate's history"
              aria-valuetext={at === null ? 'Now' : replayLabel.format(new Date(at))}
              min={start}
              max={end}
              step={(end - start) / 400}
              value={at ?? end}
              onChange={(event) => {
                const value = Number(event.target.value)
                setPlaying(false)
                setCursor(value >= end ? null : value)
              }}
              className="h-1 w-24 cursor-pointer accent-primary sm:w-36"
            />
            <span data-replay-time className="w-[5.5rem] whitespace-nowrap text-[11px] tabular-nums text-muted">
              {at === null ? 'Now' : replayLabel.format(new Date(at))}
            </span>
          </div>
        )}
      </div>

      {/* The colours the line uses, each named (req-crate-insights AC-1.2). */}
      {sceneShown && legend.length > 0 && (
        <div
          data-line-legend
          className="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-border px-5 py-2 text-[11px] text-muted sm:px-6"
        >
          {legend.map((phase) => (
            <span key={phase} data-phase={phase} className="inline-flex items-center gap-1.5">
              <span className="size-2 rounded-[3px]" style={{ background: phaseMeta[phase].color }} />
              {phaseMeta[phase].label}
            </span>
          ))}
        </div>
      )}
    </section>
  )
}

// The crate's name, its open, merged, and total counts, the shipped or
// closed statement, and its target date (req-crates AC-5.2). Over the scene
// they float on the line; with the counts as text they stand above them.
function HeaderFacts({
  crate,
  canAct,
  onSetDate,
  overlay,
}: {
  crate: CrateDetail
  canAct: boolean
  onSetDate: () => void
  overlay: boolean
}) {
  const open = crate.state === 'open'
  const total = crate.total_task_count
  const share = total > 0 ? Math.round((crate.merged_task_count / total) * 100) : 0
  return (
    <div
      className={cn(
        'grid grid-cols-[minmax(0,1fr)_auto] items-start gap-x-4 px-5 pt-4 sm:px-6',
        overlay ? 'pointer-events-none absolute inset-x-0 top-0' : 'pb-4',
      )}
    >
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
          {total > 0 && open && !crate.shipped && (
            <span
              data-merged-share
              className="ml-2 rounded-full bg-positive-soft px-1.5 py-px text-xs font-medium tabular-nums text-positive"
            >
              {share}%
            </span>
          )}
        </p>
        {crate.shipped ? (
          <p data-shipped-statement className="mt-1.5 flex items-center gap-1.5 text-sm font-medium text-positive">
            <CircleCheck aria-hidden className="size-4 shrink-0" />
            Shipped. Every task in this crate is merged or closed.
          </p>
        ) : (
          !open && (
            <p className="mt-1.5 flex items-center gap-1.5 text-sm text-muted">
              <Archive aria-hidden className="size-3.5 shrink-0" />
              Closed{crate.closed_at ? ` on ${formatDay(crate.closed_at)}` : ''}
            </p>
          )
        )}
      </div>
      {/* Every crate shows its saved target date; only an open crate offers
          the hint and "Set a target date". */}
      <div className="pointer-events-auto">
        <Due crate={crate} canAct={canAct && open} onSetDate={onSetDate} />
      </div>
    </div>
  )
}

// The target date and, for an open crate, how far off it is, worded as on the
// crates list. A closed crate shows its saved date read-only: targetDateHint
// gives it no hint, and the page passes canAct false so it offers no
// "Set a target date".
function Due({ crate, canAct, onSetDate }: { crate: CrateDetail; canAct: boolean; onSetDate: () => void }) {
  // When a date is first set, "Set a target date" goes away. If that left
  // focus on the page body, move it to the new date so keyboard users keep
  // their place.
  const dateRef = useRef<HTMLDivElement>(null)
  const hadDate = useRef(Boolean(crate.target_date))
  useEffect(() => {
    const active = document.activeElement
    if (crate.target_date && !hadDate.current && (!active || active === document.body)) dateRef.current?.focus()
    hadDate.current = Boolean(crate.target_date)
  }, [crate.target_date])

  if (!crate.target_date)
    return canAct ? (
      <Button variant="ghost" size="sm" onClick={onSetDate} className="-mr-2.5">
        <CalendarPlus aria-hidden />
        Set a target date
      </Button>
    ) : null
  const hint = targetDateHint(crate)
  return (
    <div
      ref={dateRef}
      tabIndex={-1}
      className="rounded-md text-right outline-none focus-visible:ring-2 focus-visible:ring-primary/40"
    >
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
