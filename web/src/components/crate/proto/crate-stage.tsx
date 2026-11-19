// PROTOTYPE. The top of the crate page: the crate's name, counts, and target
// date over the 3D line, with the generated briefing beneath it.
import { Archive, CalendarPlus, CircleCheck, Pause, Play } from 'lucide-react'
import { Component, lazy, type ReactNode, Suspense, useEffect, useMemo, useRef, useState } from 'react'
import type { CrateDetail } from '../../../lib/types'
import { useMediaQuery } from '../../../lib/use-media-query'
import { cn } from '../../../lib/utils'
import { Button } from '../../ui/button'
import { formatTargetDate, targetDateHint } from '../crate-parts'
import { type CrateInsight, phaseMeta, stations, type TaskTrack, tracksAt } from './model'

const LineScene = lazy(() => import('./line-scene'))

// A browser without WebGL keeps the page: the stations below still say where
// every task is.
class SceneBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false }
  static getDerivedStateFromError() {
    return { failed: true }
  }
  render() {
    return this.state.failed ? null : this.props.children
  }
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
  canAct,
  onSetDate,
  hoveredId,
  onHover,
  onSelect,
  children,
}: {
  crate: CrateDetail
  insight: CrateInsight | undefined
  canAct: boolean
  onSetDate: () => void
  hoveredId: string | null
  onHover: (id: string | null) => void
  onSelect: (id: string) => void
  // The briefing, shown under the line.
  children?: ReactNode
}) {
  const open = crate.state === 'open'
  const compact = useMediaQuery('(max-width: 767px)')
  const reducedMotion = useMediaQuery('(prefers-reduced-motion: reduce)')
  const dark = useDarkTheme()
  const hint = targetDateHint(crate)
  const total = crate.total_task_count
  const share = total > 0 ? Math.round((crate.merged_task_count / total) * 100) : 0

  // The scene renders only while it is on screen.
  const band = useRef<HTMLDivElement>(null)
  const [visible, setVisible] = useState(true)
  useEffect(() => {
    if (!band.current) return
    const observer = new IntersectionObserver(([entry]) => setVisible(entry.isIntersecting), { rootMargin: '120px' })
    observer.observe(band.current)
    return () => observer.disconnect()
  }, [])

  // Branches whose merged work waits for a release get a side belt, and the
  // crate stands for the branch the releases reach last.
  const lanes = useMemo(() => [...new Set((insight?.promotions ?? []).map((promotion) => promotion.from))], [insight])
  const trunk = useMemo(() => {
    const sources = new Set(lanes)
    return insight?.promotions.map((promotion) => promotion.to).find((branch) => !sources.has(branch))
  }, [insight, lanes])

  // Replay: the line as it stood at an earlier time, played forward to now.
  const [replayAt, setReplayAt] = useState<number | null>(null)
  const [playing, setPlaying] = useState(false)
  const start = insight ? insight.start - (insight.now - insight.start) * 0.03 : 0
  const end = insight?.now ?? 0
  useEffect(() => {
    if (!playing || !insight) return
    let frame = 0
    let last = performance.now()
    const tick = (now: number) => {
      const step = ((now - last) / 9000) * (end - start)
      last = now
      setReplayAt((current) => {
        const next = (current ?? start) + step
        if (next >= end) {
          setPlaying(false)
          return null
        }
        return next
      })
      frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
  }, [playing, insight, start, end])
  // The scene gets a new list only when a task changes place.
  const shown = useRef<{ key: string; tracks: TaskTrack[] }>({ key: '', tracks: [] })
  const tracks = useMemo(() => {
    if (!insight) return []
    const next = replayAt === null ? insight.tracks : tracksAt(insight, replayAt)
    const key = next.map((track) => `${track.id}:${track.station}:${track.phase}`).join('|')
    if (key !== shown.current.key) shown.current = { key, tracks: next }
    return shown.current.tracks
  }, [insight, replayAt])

  return (
    <section aria-label="Crate summary" className="overflow-hidden rounded-xl border border-border bg-card">
      <div
        ref={band}
        className={cn('relative', lanes.length > 0 ? 'h-[21rem] md:h-[27rem]' : 'h-[19rem] md:h-[23rem]')}
        style={{
          background:
            'radial-gradient(120% 90% at 78% 100%, color-mix(in oklab, var(--color-primary) 9%, transparent), transparent 62%), linear-gradient(180deg, var(--color-card), var(--color-surface))',
        }}
      >
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
          {insight && visible && (
            <SceneBoundary>
              <Suspense fallback={null}>
                <LineScene
                  tracks={tracks}
                  lanes={lanes}
                  trunk={trunk}
                  shipped={crate.shipped && replayAt === null}
                  closed={!open}
                  compact={compact}
                  dark={dark}
                  reducedMotion={reducedMotion}
                  hoveredId={hoveredId}
                  onHover={onHover}
                  onSelect={onSelect}
                  padTop={compact ? 100 : 78}
                  padBottom={compact ? 44 : 22}
                />
              </Suspense>
            </SceneBoundary>
          )}
        </div>

        <div className="pointer-events-none absolute inset-x-0 top-0 grid grid-cols-[minmax(0,1fr)_auto] items-start gap-x-4 px-5 pt-4 sm:px-6">
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
                <span className="ml-2 rounded-full bg-positive-soft px-1.5 py-px text-xs font-medium tabular-nums text-positive">
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
                  Closed
                </p>
              )
            )}
          </div>
          <div className="pointer-events-auto text-right">
            {crate.target_date ? (
              <>
                <p className="text-xs text-faint">Target date</p>
                <p data-target-date className="text-sm font-medium">
                  {formatTargetDate(crate.target_date)}
                </p>
                {hint && (
                  <p className={cn('text-xs', hint.late ? 'font-medium text-attention' : 'text-muted')}>{hint.text}</p>
                )}
              </>
            ) : (
              canAct &&
              open && (
                <Button variant="ghost" size="sm" onClick={onSetDate} className="-mr-2.5">
                  <CalendarPlus aria-hidden />
                  Set a target date
                </Button>
              )
            )}
          </div>
        </div>

        {insight && insight.tracks.length > 0 && end - start > 60_000 && (
          <div className="absolute bottom-3 left-4 flex items-center gap-2 rounded-full border border-border bg-card/90 py-1 pr-3 pl-1 shadow-xs backdrop-blur sm:left-5">
            <button
              type="button"
              aria-label={playing ? 'Pause the replay' : 'Replay this crate from its first task'}
              title={playing ? 'Pause' : 'Replay this crate from its first task'}
              onClick={() => {
                if (!playing && replayAt === null) setReplayAt(start)
                setPlaying(!playing)
              }}
              className="grid size-6 place-items-center rounded-full bg-foreground text-card hover:opacity-85 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
            >
              {playing ? <Pause aria-hidden className="size-3" /> : <Play aria-hidden className="ml-px size-3" />}
            </button>
            <input
              type="range"
              aria-label="Point in the crate's history"
              min={start}
              max={end}
              step={(end - start) / 400}
              value={replayAt ?? end}
              onChange={(event) => {
                const value = Number(event.target.value)
                setPlaying(false)
                setReplayAt(value >= end ? null : value)
              }}
              className="h-1 w-24 cursor-pointer accent-primary sm:w-36"
            />
            <span className="w-[4.5rem] text-[11px] tabular-nums text-muted">
              {replayAt === null ? 'Now' : replayLabel.format(new Date(replayAt))}
            </span>
          </div>
        )}

        {/* The same picture in words, for a screen reader and for a browser
            that cannot draw the scene. */}
        {insight && (
          <ul className="sr-only" aria-label="Where the tasks are">
            {stations.map((station) => (
              <li key={station.id}>
                {station.label}: {insight.tracks.filter((track) => track.station === station.id).length}
              </li>
            ))}
          </ul>
        )}
      </div>

      {insight && insight.tracks.length > 0 && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-border px-5 py-2 text-[11px] text-muted sm:px-6">
          {(['queued', 'planning', 'building', 'review', 'needs_you', 'blocked', 'merged'] as const)
            .filter((phase) => insight.tracks.some((track) => track.phase === phase))
            .map((phase) => (
              <span key={phase} className="inline-flex items-center gap-1.5">
                <span className="size-2 rounded-[3px]" style={{ background: phaseMeta[phase].color }} />
                {phaseMeta[phase].label}
              </span>
            ))}
        </div>
      )}

      {children && <div className="border-t border-border">{children}</div>}
    </section>
  )
}
