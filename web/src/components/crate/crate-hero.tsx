// The top of a crate's page (req-crates AC-5.2): the name, how far the work
// has landed, where each member task stands, and the run toward the target
// date. The open, merged, and total counts and `shipped` are the server's
// (component-crates invariant 9); the stage breakdown groups the member rows
// the same read returns.
import { Archive, CalendarClock, CircleCheck, Flag, Package, Target } from 'lucide-react'
import type { CSSProperties, ReactNode } from 'react'
import { taskStateLabels } from '../../lib/contracts'
import type { CrateDetail, TaskState } from '../../lib/types'
import { cn } from '../../lib/utils'
import { daysUntil, formatTargetDate } from './crate-parts'

type Tone = 'positive' | 'approved' | 'attention' | 'info' | 'queued' | 'parked' | 'closed'

// Furthest along first, so the bar reads left to right as work landing.
const buckets: { states: TaskState[]; tone: Tone }[] = [
  { states: ['merged'], tone: 'positive' },
  { states: ['approved'], tone: 'approved' },
  { states: ['awaiting_human'], tone: 'attention' },
  { states: ['running', 'claiming'], tone: 'info' },
  { states: ['queued'], tone: 'queued' },
  { states: ['parked'], tone: 'parked' },
  { states: ['closed'], tone: 'closed' },
]

const toneFill: Record<Tone, string> = {
  positive: 'bg-positive',
  approved: 'bg-positive/45',
  attention: 'bg-attention',
  info: 'bg-info',
  queued: 'bg-edge',
  parked: 'bg-failure/70',
  closed: 'bg-raised bg-[repeating-linear-gradient(135deg,transparent_0_3px,var(--color-edge)_3px_4px)]',
}

type Mood = 'shipped' | 'closed' | 'late' | 'active'

function moodOf(crate: CrateDetail): Mood {
  if (crate.state === 'closed') return 'closed'
  if (crate.shipped) return 'shipped'
  if (crate.target_date && daysUntil(crate.target_date) < 0) return 'late'
  return 'active'
}

const moodGlow: Record<Mood, string> = {
  shipped: 'var(--color-positive)',
  closed: 'var(--color-edge)',
  late: 'var(--color-attention)',
  active: 'var(--color-primary)',
}

export function CrateHero({
  crate,
  canAct,
  onSetDate,
}: {
  crate: CrateDetail
  canAct: boolean
  onSetDate: () => void
}) {
  const mood = moodOf(crate)
  const total = crate.total_task_count
  const merged = crate.merged_task_count
  const closedCount = Math.max(0, total - crate.open_task_count - merged)
  const uncovered = crate.requirements.filter((requirement) => requirement.task_count === 0).length

  return (
    <section
      aria-label="Crate summary"
      className="relative isolate overflow-hidden rounded-xl border border-border bg-card"
      style={{ '--glow': moodGlow[mood] } as CSSProperties}
    >
      {/* A soft wash in the crate's mood colour, top right. */}
      <div
        aria-hidden
        className="pointer-events-none absolute -top-24 -right-16 -z-10 size-72 rounded-full opacity-[0.13] blur-3xl"
        style={{ background: 'var(--glow)' }}
      />
      <div className="grid gap-5 px-4 pt-5 sm:px-6 sm:pt-6 md:grid-cols-[minmax(0,1fr)_18rem] md:items-center md:gap-8">
        <div className="flex min-w-0 items-center gap-4 sm:gap-5">
          <ProgressRing merged={merged} closed={closedCount} total={total} mood={mood} />
          <div className="min-w-0 flex-1">
            <MoodPill mood={mood} />
            <h1 className="mt-1.5 truncate text-xl font-semibold tracking-tight sm:text-2xl">{crate.name}</h1>
            <p className="mt-1 text-sm text-muted">
              {mood === 'shipped' ? (
                <span data-shipped-statement className="font-medium text-positive">
                  Shipped. Every task in this crate is merged or closed.
                </span>
              ) : mood === 'closed' ? (
                <>
                  Closed{crate.closed_at ? ` on ${formatDay(crate.closed_at)}` : ''}. Its tasks and history stay
                  readable.
                </>
              ) : total === 0 ? (
                'No tasks yet. Add one to start the crate moving.'
              ) : (
                <>
                  <Num>{merged}</Num> of <Num>{total}</Num> {total === 1 ? 'task' : 'tasks'} merged,{' '}
                  <Num>{crate.open_task_count}</Num> still open
                </>
              )}
            </p>
          </div>
        </div>
        <Schedule crate={crate} mood={mood} canAct={canAct} onSetDate={onSetDate} />
      </div>

      <div className="px-4 pt-5 pb-5 sm:px-6">
        <StageBreakdown crate={crate} />
      </div>

      <dl className="grid grid-cols-3 divide-x divide-border border-t border-border bg-surface/60 text-center sm:grid-cols-4">
        <Fact label="Open" value={crate.open_task_count} />
        <Fact label="Merged" value={merged} tone={merged > 0 ? 'positive' : undefined} />
        <Fact label="Total" value={total} />
        <div className="col-span-3 flex items-center justify-center gap-1.5 border-t border-border px-3 py-2.5 text-xs text-muted sm:col-span-1 sm:border-t-0">
          <Target aria-hidden className="size-3.5 shrink-0" />
          {crate.requirements.length === 0 ? (
            'No declared requirements'
          ) : uncovered === 0 ? (
            <span>
              {crate.requirements.length === 1
                ? 'Its requirement has tasks'
                : `All ${crate.requirements.length} requirements have tasks`}
            </span>
          ) : (
            <span>
              <span className="font-medium text-attention">{uncovered} uncovered</span> of {crate.requirements.length}{' '}
              requirements
            </span>
          )}
        </div>
      </dl>
    </section>
  )
}

function Num({ children }: { children: ReactNode }) {
  return <span className="font-medium tabular-nums text-foreground">{children}</span>
}

function Fact({ label, value, tone }: { label: string; value: number; tone?: 'positive' }) {
  return (
    <div className="px-3 py-2.5">
      <dt className="text-[11px] uppercase tracking-wide text-faint">{label}</dt>
      <dd
        data-stat={label.toLowerCase()}
        className={cn('text-lg font-semibold tabular-nums leading-tight', tone === 'positive' && 'text-positive')}
      >
        {value}
      </dd>
    </div>
  )
}

function MoodPill({ mood }: { mood: Mood }) {
  const pill: Record<Mood, { text: string; className: string; icon: ReactNode }> = {
    active: {
      text: 'Crate in progress',
      className: 'bg-primary-soft text-primary',
      icon: <Package aria-hidden className="size-3" />,
    },
    late: {
      text: 'Past its target date',
      className: 'bg-attention-soft text-attention',
      icon: <CalendarClock aria-hidden className="size-3" />,
    },
    shipped: {
      text: 'Shipped',
      className: 'bg-positive-soft text-positive',
      icon: <CircleCheck aria-hidden className="size-3" />,
    },
    closed: { text: 'Closed', className: 'bg-raised text-muted', icon: <Archive aria-hidden className="size-3" /> },
  }
  const { text, className, icon } = pill[mood]
  return (
    <span className={cn('inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium', className)}>
      {icon}
      {text}
    </span>
  )
}

// A ring: merged in green, closed-without-merging in grey, the rest empty.
function ProgressRing({ merged, closed, total, mood }: { merged: number; closed: number; total: number; mood: Mood }) {
  const radius = 42
  const circumference = 2 * Math.PI * radius
  const share = (count: number) => (total > 0 ? count / total : 0)
  const mergedLength = share(merged) * circumference
  const closedLength = share(closed) * circumference
  const percent = Math.round(share(merged) * 100)
  return (
    <div
      role="img"
      aria-label={total > 0 ? `${percent}% of tasks merged` : 'No tasks yet'}
      className="relative size-20 shrink-0 sm:size-24"
    >
      <svg aria-hidden="true" viewBox="0 0 100 100" className="size-full -rotate-90">
        <circle cx="50" cy="50" r={radius} fill="none" strokeWidth="9" className="stroke-raised" />
        {closedLength > 0 && (
          <circle
            cx="50"
            cy="50"
            r={radius}
            fill="none"
            strokeWidth="9"
            className="stroke-edge"
            strokeDasharray={`${closedLength} ${circumference}`}
            strokeDashoffset={-mergedLength}
          />
        )}
        {mergedLength > 0 && (
          <circle
            cx="50"
            cy="50"
            r={radius}
            fill="none"
            strokeWidth="9"
            strokeLinecap={merged === total ? 'butt' : 'round'}
            className="stroke-positive transition-[stroke-dasharray] duration-700"
            strokeDasharray={`${mergedLength} ${circumference}`}
          />
        )}
      </svg>
      <div className="absolute inset-0 flex flex-col items-center justify-center">
        {mood === 'shipped' ? (
          <CircleCheck aria-hidden className="size-8 text-positive" />
        ) : (
          <>
            <span className="text-lg font-semibold tabular-nums leading-none sm:text-xl">
              {percent}
              <span className="text-xs font-medium text-muted">%</span>
            </span>
            <span className="mt-0.5 text-[10px] text-faint">merged</span>
          </>
        )}
      </div>
    </div>
  )
}

// Where the member tasks stand, as one segmented bar with a legend.
function StageBreakdown({ crate }: { crate: CrateDetail }) {
  const groups = buckets
    .map((bucket) => ({
      ...bucket,
      label: taskStateLabels[bucket.states[0]] ?? bucket.states[0],
      count: crate.tasks.filter((task) => bucket.states.includes(task.state)).length,
    }))
    .filter((group) => group.count > 0)
  const total = crate.tasks.length

  return (
    <div className="min-w-0">
      <h2 className="text-xs font-medium text-muted">Where the tasks are</h2>
      {total === 0 ? (
        <div className="mt-2.5 h-3 rounded-full border border-dashed border-edge" />
      ) : (
        <div
          role="img"
          aria-label={groups.map((g) => `${g.count} ${g.label}`).join(', ')}
          className="mt-2.5 flex gap-1"
        >
          {groups.map((group) => (
            <span
              key={group.tone}
              className={cn('h-3 min-w-3 rounded-[4px] first:rounded-l-full last:rounded-r-full', toneFill[group.tone])}
              style={{ flexGrow: group.count, flexBasis: 0 }}
            />
          ))}
        </div>
      )}
      <ul className="mt-3 flex flex-wrap gap-x-4 gap-y-1.5 text-xs">
        {groups.map((group) => (
          <li key={group.tone} className="flex items-center gap-1.5 text-muted">
            <span aria-hidden className={cn('size-2.5 rounded-[3px]', toneFill[group.tone])} />
            {group.label}
            <span className="font-medium tabular-nums text-foreground">{group.count}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}

// The run from the crate's start to its target date, with today marked.
function Schedule({
  crate,
  mood,
  canAct,
  onSetDate,
}: {
  crate: CrateDetail
  mood: Mood
  canAct: boolean
  onSetDate: () => void
}) {
  if (!crate.target_date)
    return (
      <div className="flex flex-col justify-center rounded-lg border border-dashed border-edge px-4 py-3">
        <p className="text-sm font-medium">No target date</p>
        <p className="mt-0.5 text-xs text-muted">Started {formatDay(crate.created_at)}</p>
        {canAct && crate.state === 'open' && (
          <button
            type="button"
            onClick={onSetDate}
            className="mt-2 self-start text-xs font-medium text-primary hover:underline"
          >
            Set a target date
          </button>
        )}
      </div>
    )

  const days = daysUntil(crate.target_date)
  const start = startOfDay(new Date(crate.created_at))
  const end = new Date(`${crate.target_date}T00:00:00Z`).getTime()
  const now = Date.now()
  const span = Math.max(1, end - start)
  const todayAt = Math.min(1, Math.max(0, (now - start) / span))
  const late = mood === 'late'
  const done = mood === 'shipped' || mood === 'closed'

  // A finished crate carries no ship date, so only say it beat the target
  // when the target is still ahead.
  const headline = done
    ? mood === 'shipped' && days >= 0
      ? 'Shipped before target'
      : 'Target date'
    : days === 0
      ? 'Due today'
      : days > 0
        ? `${days} ${days === 1 ? 'day' : 'days'} left`
        : `${-days} ${days === -1 ? 'day' : 'days'} late`

  return (
    <div
      className={cn(
        'rounded-lg border px-4 py-3',
        late ? 'border-attention/30 bg-attention-soft/60' : 'border-border bg-surface/70',
      )}
    >
      <div className="flex items-baseline justify-between gap-3">
        <p className={cn('text-base font-semibold tabular-nums', late && 'text-attention')}>{headline}</p>
        <p className="shrink-0 text-xs text-muted" data-target-date>
          {formatTargetDate(crate.target_date)}
        </p>
      </div>
      <div className="mt-3.5 mb-1 flex items-center gap-2">
        <div className="relative h-1.5 flex-1 rounded-full bg-raised">
          <div
            className={cn(
              'absolute inset-y-0 left-0 rounded-full',
              late ? 'bg-attention' : mood === 'closed' ? 'bg-edge' : done ? 'bg-positive' : 'bg-primary',
            )}
            style={{ width: `${(done ? 1 : todayAt) * 100}%` }}
          />
          {!done && (
            <span
              aria-hidden
              className={cn(
                'absolute top-1/2 size-3 -translate-x-1/2 -translate-y-1/2 rounded-full border-2 border-card shadow-sm',
                late ? 'bg-attention' : 'bg-primary',
              )}
              style={{ left: `${todayAt * 100}%` }}
            />
          )}
        </div>
        <Flag aria-hidden className={cn('size-3.5 shrink-0', late ? 'text-attention' : 'text-muted')} />
      </div>
      <div className="relative mt-2 mr-5.5 h-4 text-[11px] text-faint">
        <span className="absolute left-0">Started {formatDay(crate.created_at)}</span>
        {!done && todayAt > 0.32 && (
          <span
            className={cn('absolute font-medium', late ? 'text-attention' : 'text-primary')}
            style={todayAt > 0.85 ? { right: 0 } : { left: `${todayAt * 100}%`, transform: 'translateX(-50%)' }}
          >
            Today
          </span>
        )}
      </div>
    </div>
  )
}

function startOfDay(date: Date) {
  return Date.UTC(date.getFullYear(), date.getMonth(), date.getDate())
}

function formatDay(value: string) {
  return new Date(value).toLocaleDateString('en', { month: 'short', day: 'numeric' })
}
