// The crate line's presentation of served values: the colour and name of each
// phase, the six stations in line order, and the line at one moment of the
// replay. Every phase, station, and step comes from the crate insight view;
// this file derives none of them from tasks or events (component-crates
// invariant 21; DEC-143(3)).
import type { CrateInsight, CratePhase, CrateStation } from '../../lib/types'

// One block on the line: a member task where the insight view places it,
// with the title and the verbatim next-action label from the crate read.
export interface LineBlock {
  id: string
  phase: CratePhase
  station: CrateStation
  baseBranch: string
  // From the crate read, joined by task ID. Absent while the two reads
  // disagree about the task, and then the card shows only what it has.
  title?: string
  label?: string
}

// The colour of each phase and the name the legend gives it
// (req-crate-insights AC-1.2).
export const phaseMeta: Record<CratePhase, { label: string; color: string }> = {
  queued: { label: 'Queued', color: 'var(--color-faint)' },
  planning: { label: 'Planning', color: 'var(--color-info)' },
  building: { label: 'Building', color: 'var(--color-primary)' },
  review: { label: 'In review', color: 'var(--color-document)' },
  needs_you: { label: 'Waits on a person', color: 'var(--color-attention-dot)' },
  blocked: { label: 'Blocked by a task', color: 'var(--color-failure)' },
  merged: { label: 'Merged', color: 'var(--color-positive)' },
  closed: { label: 'Closed', color: 'var(--color-edge)' },
}

// The legend's order.
export const phaseOrder: CratePhase[] = [
  'queued',
  'planning',
  'building',
  'review',
  'needs_you',
  'blocked',
  'merged',
  'closed',
]

// The six stations of the line, in line order (req-crate-insights AC-1.1).
// Closed tasks sit aside, off the line, at no station.
export type LineStation = Exclude<CrateStation, 'aside'>

export const lineStations: Array<{ id: LineStation; label: string }> = [
  { id: 'intake', label: 'Queued' },
  { id: 'plan', label: 'Plan' },
  { id: 'build', label: 'Build' },
  { id: 'review', label: 'Review' },
  { id: 'gate', label: 'Merge gate' },
  { id: 'crate', label: 'Merged' },
]

// The replay's span in milliseconds: the first member's creation to the read
// instant. `usable` is false when the span is empty, so the page offers no
// replay and never divides by zero.
export function replaySpan(insight: CrateInsight) {
  const start = Date.parse(insight.replay.start)
  const end = Date.parse(insight.replay.end)
  const usable = insight.tasks.length > 0 && Number.isFinite(start) && Number.isFinite(end) && end > start
  return { start, end, usable }
}

// Where each task is at `at`, in the served task order. With `at` null the
// line is the present: the served `tasks`. Otherwise each task is at its last
// served step at or before `at`, and a task with no such step is not on the
// line yet (req-crate-insights AC-1.4; component-crates invariant 25).
export function placesAt(
  insight: CrateInsight,
  at: number | null,
): Array<{ id: string; phase: CratePhase; station: CrateStation; baseBranch: string }> {
  if (at === null)
    return insight.tasks.map((task) => ({
      id: task.id,
      phase: task.phase,
      station: task.station,
      baseBranch: task.base_branch,
    }))
  const last = new Map<string, { phase: CratePhase; station: CrateStation }>()
  // The served steps are in time order, so the first later step ends the walk.
  for (const step of insight.replay.steps) {
    if (Date.parse(step.at) > at) break
    last.set(step.task_id, { phase: step.phase, station: step.station })
  }
  const result: Array<{ id: string; phase: CratePhase; station: CrateStation; baseBranch: string }> = []
  for (const task of insight.tasks) {
    const place = last.get(task.id)
    if (place) result.push({ id: task.id, ...place, baseBranch: task.base_branch })
  }
  return result
}

// The number of blocks at each station of the line.
export function stationCounts(blocks: Array<{ station: CrateStation }>) {
  const counts = Object.fromEntries(lineStations.map((station) => [station.id, 0])) as Record<LineStation, number>
  for (const block of blocks) if (block.station !== 'aside') counts[block.station] += 1
  return counts
}

// The phases the line shows, in legend order.
export function phasesInUse(blocks: Array<{ phase: CratePhase }>) {
  return phaseOrder.filter((phase) => blocks.some((block) => block.phase === phase))
}
