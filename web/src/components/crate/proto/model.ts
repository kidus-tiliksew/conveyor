// PROTOTYPE. The crate page's derived view of a crate: where each member task
// is on the line and on its branches, and the crate's delivery numbers.
//
// The browser derives all of this here from the crate read, its activity, and
// each task's read. The shipped feature would serve it from the server, as
// requirement insights are served, so the page never computes a number of its
// own (DEC-61).
import type {
  CrateActivityEvent,
  CrateDetail,
  CrateTask,
  RequirementInsightProgress,
  RequirementInsightWeek,
  Stage,
} from '../../../lib/types'
import { statusOf } from '../../board/map/graph-model'

export const DAY = 86_400_000

// What the task is doing, which picks its colour everywhere on the page.
export type Phase = 'queued' | 'planning' | 'building' | 'review' | 'needs_you' | 'blocked' | 'merged' | 'closed'

// Where the task sits on the line, left to right.
// `lane` is merged work that waits on its branch for a release into the next.
export type Station = 'intake' | 'plan' | 'build' | 'review' | 'gate' | 'lane' | 'crate' | 'aside'

export const phaseMeta: Record<Phase, { label: string; color: string; soft: string }> = {
  queued: { label: 'Queued', color: 'var(--color-faint)', soft: 'var(--color-raised)' },
  planning: { label: 'Planning', color: 'var(--color-info)', soft: 'var(--color-info-soft)' },
  building: { label: 'Building', color: 'var(--color-primary)', soft: 'var(--color-primary-soft)' },
  review: { label: 'In review', color: 'var(--color-document)', soft: 'var(--color-document-soft)' },
  needs_you: { label: 'Needs you', color: 'var(--color-attention-dot)', soft: 'var(--color-attention-soft)' },
  blocked: { label: 'Blocked', color: 'var(--color-failure)', soft: 'var(--color-failure-soft)' },
  merged: { label: 'Merged', color: 'var(--color-positive)', soft: 'var(--color-positive-soft)' },
  closed: { label: 'Closed', color: 'var(--color-edge)', soft: 'var(--color-raised)' },
}

export const stations: Array<{ id: Station; label: string }> = [
  { id: 'intake', label: 'Queued' },
  { id: 'plan', label: 'Plan' },
  { id: 'build', label: 'Build' },
  { id: 'review', label: 'Review' },
  { id: 'gate', label: 'Merge gate' },
  { id: 'lane', label: 'Merged, waiting for a release' },
  { id: 'crate', label: 'Merged' },
]

// What each task's own read adds to the crate read.
export interface TaskExtra {
  base_branch: string
  created_at: string
  next_stage?: Stage
}

// A long-lived branch taking in another, such as develop released to master.
// Conveyor does not record these today: live crates carry none, and the sample
// crate shows what the page does once the forge reports them.
export interface Promotion {
  at: number
  from: string
  to: string
}

export interface ReviewMark {
  at: number
  verdict: string
}

export interface TaskTrack {
  task: CrateTask
  id: string
  title: string
  phase: Phase
  station: Station
  baseBranch: string
  createdAt: number
  pullRequestAt: number[]
  reviews: ReviewMark[]
  mergedAt?: number
  closedAt?: number
  // The long-lived branches that hold this task's work, base branch first.
  landedOn: string[]
  // The branch a merged task waits on, when a release has yet to take it on.
  lane?: string
}

export interface CrateInsight {
  tracks: TaskTrack[]
  byId: Map<string, TaskTrack>
  branches: string[]
  promotions: Promotion[]
  start: number
  now: number
  progress: RequirementInsightProgress
  // Share of reviewed tasks whose first review round approved.
  firstPassRate: number | null
  reviewedTasks: number
  // Tasks whose work is on a task branch only, then per long-lived branch the
  // tasks whose work has reached it and gone no further.
  whereabouts: Array<{ key: string; label: string; branch?: string; tracks: TaskTrack[] }>
}

const trunkNames = ['master', 'main', 'production', 'release']

// Trunk first, then develop, then the rest by name.
function branchOrder(a: string, b: string) {
  const rank = (name: string) => (trunkNames.includes(name) ? 0 : name === 'develop' || name === 'dev' ? 1 : 2)
  return rank(a) - rank(b) || a.localeCompare(b)
}

function stationOf(task: CrateTask, phase: Phase, extra: TaskExtra | undefined, hasPullRequest: boolean): Station {
  if (phase === 'merged') return 'crate'
  if (phase === 'closed') return 'aside'
  const kind = task.next_action.kind
  if (kind === 'plan_approval') return 'plan'
  if (kind === 'merge_approval') return 'gate'
  switch (extra?.next_stage) {
    case 'triage':
    case 'spec':
      return phase === 'queued' || phase === 'blocked' ? 'intake' : 'plan'
    case 'implement':
      return 'build'
    case 'review':
    case 'verify':
    case 'risk':
      return 'review'
    case 'gate':
    case 'merge':
      return 'gate'
    default:
      return hasPullRequest ? 'review' : phase === 'queued' || phase === 'blocked' ? 'intake' : 'build'
  }
}

function phaseOf(task: CrateTask, extra: TaskExtra | undefined, hasPullRequest: boolean): Phase {
  const status = statusOf(task)
  if (status === 'merged') return 'merged'
  if (status === 'closed') return 'closed'
  if (status === 'blocked') return 'blocked'
  if (status === 'plan_gate' || status === 'merge_gate' || status === 'needs_you') return 'needs_you'
  const working = status === 'running'
  const stage = extra?.next_stage
  if (stage === 'review' || stage === 'verify' || stage === 'risk') return 'review'
  if (stage === 'implement') return working || hasPullRequest ? 'building' : 'queued'
  if (stage === 'triage' || stage === 'spec') return working ? 'planning' : 'queued'
  if (hasPullRequest) return 'review'
  return working ? 'building' : 'queued'
}

// The long-lived branches that hold work merged into `base` at `mergedAt`, as
// of `until`: the base branch, then each branch a later release carried it to.
function landing(base: string, mergedAt: number, promotions: Promotion[], until: number) {
  const landedOn = [base]
  let reached = mergedAt
  let from = base
  for (const promotion of promotions.slice().sort((a, b) => a.at - b.at)) {
    if (promotion.from === from && promotion.at >= reached && promotion.at <= until) {
      landedOn.push(promotion.to)
      from = promotion.to
      reached = promotion.at
    }
  }
  return landedOn
}

// A merged task's place: on its branch's belt while a release can still take
// it further, in the crate once it has reached the last branch.
function mergedPlace(landedOn: string[], promotions: Promotion[]): Pick<TaskTrack, 'station' | 'lane'> {
  const last = landedOn[landedOn.length - 1]
  return promotions.some((promotion) => promotion.from === last)
    ? { station: 'lane', lane: last }
    : { station: 'crate' }
}

const utcDay = (time: number) => Math.floor(time / DAY) * DAY

function isoWeek(time: number) {
  const date = new Date(time)
  const day = (date.getUTCDay() + 6) % 7
  const monday = utcDay(time) - day * DAY
  const thursday = new Date(monday + 3 * DAY)
  const january4 = new Date(Date.UTC(thursday.getUTCFullYear(), 0, 4))
  const days = (thursday.getTime() - january4.getTime()) / DAY
  const week = 1 + Math.round((days - 3 + ((january4.getUTCDay() + 6) % 7)) / 7)
  return { monday, year: thursday.getUTCFullYear(), week }
}

export function buildInsight(
  crate: CrateDetail,
  events: CrateActivityEvent[],
  extras: Record<string, TaskExtra | undefined>,
  promotions: Promotion[],
  now = Date.now(),
): CrateInsight {
  const perTask = new Map<string, CrateActivityEvent[]>()
  for (const event of events) {
    const list = perTask.get(event.task_id)
    if (list) list.push(event)
    else perTask.set(event.task_id, [event])
  }

  const tracks = crate.tasks.map((task): TaskTrack => {
    const own = (perTask.get(task.id) ?? []).slice().sort((a, b) => Date.parse(a.at) - Date.parse(b.at))
    const at = (kind: string) => own.filter((event) => event.kind === kind).map((event) => Date.parse(event.at))
    const extra = extras[task.id]
    const created = extra?.created_at ? Date.parse(extra.created_at) : (at('task.created')[0] ?? now)
    const pullRequestAt = at('pull_request.opened')
    const hasPullRequest = pullRequestAt.length > 0 || task.pull_request != null
    const phase = phaseOf(task, extra, hasPullRequest)
    const merged = [...at('merge.confirmed'), ...at('merge.reconciled')].sort()[0]
    const cancelled = at('task.cancelled')[0]
    const lastEvent = own.length ? Date.parse(own[own.length - 1].at) : created
    const baseBranch = extra?.base_branch || 'main'
    const mergedAt = phase === 'merged' ? (merged ?? lastEvent) : undefined
    const landedOn = mergedAt === undefined ? [] : landing(baseBranch, mergedAt, promotions, now)
    return {
      task,
      id: task.id,
      title: task.title,
      phase,
      ...(mergedAt === undefined
        ? { station: stationOf(task, phase, extra, hasPullRequest) }
        : mergedPlace(landedOn, promotions)),
      baseBranch,
      createdAt: created,
      pullRequestAt,
      reviews: own
        .filter((event) => event.kind === 'review.round_completed')
        .map((event) => ({ at: Date.parse(event.at), verdict: event.verdict ?? '' })),
      mergedAt,
      closedAt: phase === 'closed' ? (cancelled ?? lastEvent) : undefined,
      landedOn,
    }
  })

  const branches = [
    ...new Set([...tracks.map((track) => track.baseBranch), ...promotions.flatMap((p) => [p.from, p.to])]),
  ].sort(branchOrder)
  const start = tracks.length ? Math.min(...tracks.map((track) => track.createdAt)) : now

  // Daily series on UTC days, from the first task to today.
  const firstDay = utcDay(start)
  const today = utcDay(now)
  const daily: RequirementInsightProgress['daily'] = []
  for (let day = firstDay; day <= today; day += DAY) {
    const end = day + DAY
    const scope = tracks.filter((track) => track.createdAt < end).length
    const delivered = tracks.filter((track) => track.mergedAt !== undefined && track.mergedAt < end).length
    const closed = tracks.filter((track) => track.closedAt !== undefined && track.closedAt < end).length
    daily.push({ day: new Date(day).toISOString(), scope: scope - closed, open: scope - delivered - closed, delivered })
  }

  const merged = tracks.filter((track) => track.mergedAt !== undefined)
  const open = tracks.filter((track) => track.phase !== 'merged' && track.phase !== 'closed')
  const recent = merged.filter((track) => (track.mergedAt as number) >= now - 14 * DAY).length
  const ratePerDay = recent / 14
  const forecast =
    open.length > 0 && ratePerDay > 0
      ? { day: new Date(utcDay(now + (open.length / ratePerDay) * DAY)).toISOString(), rate_per_day: ratePerDay }
      : null

  const weekly: RequirementInsightWeek[] = []
  const thisWeek = isoWeek(now)
  for (let index = 7; index >= 0; index--) {
    const info = isoWeek(thisWeek.monday - index * 7 * DAY)
    weekly.push({
      week_start: new Date(info.monday).toISOString(),
      iso_year: info.year,
      iso_week: info.week,
      delivered: merged.filter((track) => {
        const at = track.mergedAt as number
        return at >= info.monday && at < info.monday + 7 * DAY
      }).length,
      in_progress: index === 0,
    })
  }

  const cycles = merged.map((track) => ((track.mergedAt as number) - track.createdAt) / 1000).sort((a, b) => a - b)
  const median =
    cycles.length === 0
      ? null
      : cycles.length % 2
        ? cycles[(cycles.length - 1) / 2]
        : (cycles[cycles.length / 2 - 1] + cycles[cycles.length / 2]) / 2

  const actors = new Map<string, number>()
  for (const track of open)
    actors.set(track.task.next_action.actor, (actors.get(track.task.next_action.actor) ?? 0) + 1)

  const reviewed = tracks.filter((track) => track.reviews.length > 0)
  const firstPass = reviewed.filter((track) => track.reviews[0].verdict === 'approve').length

  // Where each task's work is: the furthest long-lived branch it has reached.
  const whereabouts: CrateInsight['whereabouts'] = [
    { key: 'not-started', label: 'Not started', tracks: open.filter((track) => track.station === 'intake') },
    { key: 'task-branch', label: 'On task branches', tracks: open.filter((track) => track.station !== 'intake') },
  ]
  for (const branch of branches.slice().reverse()) {
    whereabouts.push({
      key: `branch:${branch}`,
      label: `In ${branch}`,
      branch,
      tracks: merged.filter((track) => track.landedOn[track.landedOn.length - 1] === branch),
    })
  }

  return {
    tracks,
    byId: new Map(tracks.map((track) => [track.id, track])),
    branches,
    promotions,
    start,
    now,
    progress: {
      daily,
      forecast,
      weekly,
      delivered: merged.length,
      total: tracks.length - tracks.filter((track) => track.phase === 'closed').length,
      open: open.length,
      throughput_per_week: ratePerDay * 7,
      median_cycle_seconds: median,
      status_mix: {
        delivered: merged.length,
        closed: tracks.filter((track) => track.phase === 'closed').length,
        open: [...actors].map(([actor, count]) => ({ actor: actor as never, count })),
      },
    },
    firstPassRate: reviewed.length ? firstPass / reviewed.length : null,
    reviewedTasks: reviewed.length,
    whereabouts: whereabouts.filter((group) => group.tracks.length > 0 || group.branch),
  }
}

// A short form of a task title for tight places.
export function shortTitle(title: string, max = 34) {
  const clean = title.replace(/\s+/g, ' ').trim()
  return clean.length <= max ? clean : `${clean.slice(0, max - 1).trimEnd()}…`
}

// The line as it stood at an earlier time, rebuilt from the recorded events:
// a task is queued, then planned and built until its first pull request, in
// review while a pull request waits, back in build after a review asks for
// changes, and merged from its merge. A task that has not started yet stays
// where it is now.
export function tracksAt(insight: CrateInsight, time: number): TaskTrack[] {
  if (time >= insight.now) return insight.tracks
  const result: TaskTrack[] = []
  for (const track of insight.tracks) {
    if (track.createdAt > time) continue
    const at = (phase: Phase, station: Station) => result.push({ ...track, phase, station, lane: undefined })
    if (track.mergedAt !== undefined && track.mergedAt <= time) {
      const landedOn = landing(track.baseBranch, track.mergedAt, insight.promotions, time)
      result.push({
        ...track,
        phase: 'merged',
        landedOn,
        lane: undefined,
        ...mergedPlace(landedOn, insight.promotions),
      })
    } else if (track.closedAt !== undefined && track.closedAt <= time) at('closed', 'aside')
    else if (track.pullRequestAt.length === 0 && track.mergedAt === undefined && track.closedAt === undefined)
      result.push(track)
    else {
      const opened = track.pullRequestAt.filter((moment) => moment <= time)
      const lastOpened = opened[opened.length - 1]
      if (lastOpened === undefined) {
        const firstWork = track.pullRequestAt[0] ?? track.mergedAt ?? track.closedAt ?? insight.now
        const share = (time - track.createdAt) / Math.max(1, firstWork - track.createdAt)
        if (share < 0.15) at('queued', 'intake')
        else if (share < 0.35) at('planning', 'plan')
        else at('building', 'build')
      } else {
        const review = track.reviews.filter((mark) => mark.at <= time && mark.at > lastOpened).pop()
        if (review?.verdict === 'changes_requested') at('building', 'build')
        else if (review?.verdict === 'approve') at('review', 'gate')
        else at('review', 'review')
      }
    }
  }
  return result
}
