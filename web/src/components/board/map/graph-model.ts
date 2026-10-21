// The Board map's model (req-board-map; component-web-task-surfaces, Board
// map), built in the browser from the one task-map read. Every status comes
// from the stated next action (DEC-61).
import type { TaskMapItem } from '../../../lib/types'

export type GraphStatus =
  | 'blocked'
  | 'plan_gate'
  | 'merge_gate'
  | 'needs_you'
  | 'running'
  | 'waiting'
  | 'ready'
  | 'parked'
  | 'merged'
  | 'closed'

export interface GraphTask {
  id: string
  title: string
  repo: string
  status: GraphStatus
  state: string
  actionLabel?: string
  actionSummary?: string
  lastEventAt: string
  dependsOn: string[]
  // The unmerged dependencies, for the dependency mark beside the stated
  // status (req-board-map AC-2.2). A closed one cannot be satisfied.
  blocking: Array<{ id: string; title: string; unsatisfiable: boolean }>
  requirements: Array<{ id: string; title: string }>
  // Present only for tasks the read named as a dependency but did not return
  // as an item of its own.
  stub?: boolean
}

export interface TaskGraphModel {
  tasks: Map<string, GraphTask>
  dependents: Map<string, string[]>
  // Open tasks transitively waiting on each task.
  unblocks: Map<string, number>
}

// The treatment follows only the stated actor, judgment flag, and kind
// (req-board-map AC-2.1; req-operator-next-action AC-1.7). Merged and closed
// tasks keep their terminal presentation. Waiting on another task is a
// separate dependency mark, never a status. A requirement insight row, which
// lists only open tasks, passes its next action alone.
export function statusOf(item: Pick<TaskMapItem, 'next_action'> & { state?: TaskMapItem['state'] }): GraphStatus {
  const action = item.next_action
  if (item.state === 'merged') return 'merged'
  if (item.state === 'closed') return 'closed'
  if (action.actor === 'dependency') return 'blocked'
  if (action.judgment) {
    if (action.kind === 'plan_approval') return 'plan_gate'
    if (action.kind === 'merge_approval') return 'merge_gate'
    return 'needs_you'
  }
  if (action.actor === 'agent') return action.kind === 'working' ? 'running' : 'ready'
  // The factory handling it, or a mechanical operator step.
  return 'parked'
}

export const DONE: ReadonlySet<GraphStatus> = new Set(['merged', 'closed'])

export function buildModel(items: TaskMapItem[]): TaskGraphModel {
  const tasks = new Map<string, GraphTask>()
  for (const item of items) {
    tasks.set(item.id, {
      id: item.id,
      title: item.title || item.id,
      repo: item.repo,
      status: statusOf(item),
      state: item.state,
      actionLabel: item.next_action?.label,
      actionSummary: item.next_action?.summary,
      lastEventAt: item.last_event_at,
      dependsOn: item.depends_on ?? [],
      blocking: [],
      requirements: (item.requirements ?? []).map(({ id, title }) => ({ id, title })),
    })
  }
  // A dependency the read did not carry (the repository filter hid it) still
  // deserves a node, named by what the dependent's next action states. An
  // edge the server does not list as blocking is satisfied: it merged.
  for (const item of items) {
    for (const id of item.depends_on ?? []) {
      if (tasks.has(id)) continue
      const named = item.next_action?.blocked_by?.find((blocker) => blocker.task_id === id)
      const state = item.unsatisfiable_task_ids?.includes(id)
        ? 'closed'
        : item.blocking_task_ids?.includes(id)
          ? (named?.state ?? '')
          : 'merged'
      tasks.set(id, {
        id,
        title: named?.title || id,
        repo: '',
        status: state === 'merged' ? 'merged' : state === 'closed' ? 'closed' : 'parked',
        state,
        lastEventAt: '',
        dependsOn: [],
        blocking: [],
        requirements: [],
        stub: true,
      })
    }
  }
  for (const item of items) {
    const task = tasks.get(item.id)
    if (!task) continue
    task.blocking = (item.blocking_task_ids ?? []).map((id) => ({
      id,
      title: tasks.get(id)?.title ?? id,
      unsatisfiable: item.unsatisfiable_task_ids?.includes(id) ?? false,
    }))
  }
  const dependents = new Map<string, string[]>()
  for (const task of tasks.values()) {
    for (const id of task.dependsOn) dependents.set(id, [...(dependents.get(id) ?? []), task.id])
  }
  const unblocks = new Map<string, number>()
  for (const id of tasks.keys()) {
    let open = 0
    for (const downstream of walk(id, 'down', tasks, dependents)) {
      const status = tasks.get(downstream)?.status
      if (status && !DONE.has(status)) open++
    }
    unblocks.set(id, open)
  }
  return { tasks, dependents, unblocks }
}

export function walk(
  start: string,
  direction: 'up' | 'down',
  tasks: Map<string, GraphTask>,
  dependents: Map<string, string[]>,
): Set<string> {
  const seen = new Set<string>()
  const stack = [start]
  while (stack.length > 0) {
    const id = stack.pop() as string
    const next = direction === 'up' ? (tasks.get(id)?.dependsOn ?? []) : (dependents.get(id) ?? [])
    for (const neighbour of next) {
      if (seen.has(neighbour)) continue
      seen.add(neighbour)
      stack.push(neighbour)
    }
  }
  return seen
}

export const statusMeta: Record<GraphStatus, { label: string; tone: string; ring: string; dot: string }> = {
  blocked: { label: 'Blocked', tone: 'text-failure', ring: 'border-failure', dot: 'bg-failure' },
  plan_gate: { label: 'Plan gate', tone: 'text-attention', ring: 'border-attention-dot', dot: 'bg-attention-dot' },
  merge_gate: { label: 'Merge gate', tone: 'text-attention', ring: 'border-attention-dot', dot: 'bg-attention-dot' },
  needs_you: { label: 'Needs you', tone: 'text-attention', ring: 'border-attention-dot', dot: 'bg-attention-dot' },
  running: { label: 'Running', tone: 'text-primary', ring: 'border-primary', dot: 'bg-primary' },
  waiting: { label: 'Waiting on deps', tone: 'text-faint', ring: 'border-edge', dot: 'bg-faint' },
  ready: { label: 'Ready', tone: 'text-positive', ring: 'border-positive', dot: 'bg-positive' },
  parked: { label: 'Parked', tone: 'text-muted', ring: 'border-muted', dot: 'bg-muted' },
  merged: { label: 'Merged', tone: 'text-positive', ring: 'border-positive', dot: 'bg-positive' },
  closed: { label: 'Closed', tone: 'text-faint', ring: 'border-edge', dot: 'bg-faint' },
}

// What a task shows as its status text: the stated next action's label,
// verbatim. Only a dependency the read did not carry has none: it shows its
// terminal state, or that the repository filter hides it.
export const labelOf = (task: GraphTask) =>
  task.actionLabel ??
  (task.stub && !DONE.has(task.status) ? 'Hidden by the repository filter' : statusMeta[task.status].label)

export const statusOrder: GraphStatus[] = [
  'blocked',
  'plan_gate',
  'merge_gate',
  'needs_you',
  'running',
  'ready',
  'waiting',
  'parked',
  'merged',
  'closed',
]
