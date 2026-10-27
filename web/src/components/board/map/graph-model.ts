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
// separate dependency mark, never a status, so GraphStatus holds exactly what
// this returns. A requirement insight row, which
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
    for (const id of task.dependsOn) {
      const list = dependents.get(id)
      if (list) list.push(task.id)
      else dependents.set(id, [task.id])
    }
  }
  return { tasks, dependents, unblocks: openDownstreamCounts(tasks, dependents).counts }
}

// Each task's count of distinct open tasks downstream, in one pass per build
// rather than a walk from every task (component-web-task-surfaces, Board map
// Model). Tarjan's algorithm follows dependents; as each strongly connected
// component closes, every component it reaches has closed already, so its
// reachable open tasks are its own open members joined with theirs, kept as a
// bitset over the open tasks. Each edge between components is relaxed once,
// and `relaxed` counts those unions. A task counts the rest of its component,
// and itself only on a cycle, as the walk it replaces does.
export function openDownstreamCounts(
  tasks: Map<string, GraphTask>,
  dependents: Map<string, string[]>,
): { counts: Map<string, number>; relaxed: number } {
  const ids = [...tasks.keys()]
  const slot = new Map(ids.map((id, index) => [id, index]))
  const next = ids.map((id) =>
    (dependents.get(id) ?? []).flatMap((dependent) => {
      const index = slot.get(dependent)
      return index === undefined ? [] : [index]
    }),
  )
  // Open tasks get consecutive bits.
  const bit = new Int32Array(ids.length).fill(-1)
  let open = 0
  ids.forEach((id, index) => {
    const status = tasks.get(id)?.status
    if (status && !DONE.has(status)) bit[index] = open++
  })
  const words = (open + 31) >>> 5
  const order = new Int32Array(ids.length).fill(-1)
  const low = new Int32Array(ids.length)
  const component = new Int32Array(ids.length).fill(-1)
  const onStack = new Uint8Array(ids.length)
  const stack: number[] = []
  const reach: Uint32Array[] = []
  const counts = new Map<string, number>()
  let visited = 0
  let relaxed = 0
  for (let root = 0; root < ids.length; root++) {
    if (order[root] !== -1) continue
    order[root] = low[root] = visited++
    stack.push(root)
    onStack[root] = 1
    // Each frame is a task and the next of its dependents to follow.
    const frames: Array<[number, number]> = [[root, 0]]
    while (frames.length > 0) {
      const frame = frames[frames.length - 1]
      const [node, edge] = frame
      if (edge < next[node].length) {
        frame[1]++
        const dependent = next[node][edge]
        if (order[dependent] === -1) {
          order[dependent] = low[dependent] = visited++
          stack.push(dependent)
          onStack[dependent] = 1
          frames.push([dependent, 0])
        } else if (onStack[dependent]) low[node] = Math.min(low[node], order[dependent])
        continue
      }
      frames.pop()
      if (frames.length > 0) {
        const parent = frames[frames.length - 1][0]
        low[parent] = Math.min(low[parent], low[node])
      }
      if (low[node] !== order[node]) continue
      const id = reach.length
      const members: number[] = []
      let member: number
      do {
        member = stack.pop() as number
        onStack[member] = 0
        component[member] = id
        members.push(member)
      } while (member !== node)
      const set = new Uint32Array(words)
      let cyclic = members.length > 1
      for (const index of members) {
        if (bit[index] >= 0) set[bit[index] >>> 5] |= 1 << (bit[index] & 31)
        for (const dependent of next[index]) {
          if (component[dependent] === id) {
            if (dependent === index) cyclic = true
            continue
          }
          const other = reach[component[dependent]]
          for (let word = 0; word < words; word++) set[word] |= other[word]
          relaxed++
        }
      }
      reach.push(set)
      let size = 0
      for (const word of set) size += popcount(word)
      for (const index of members) counts.set(ids[index], size - (!cyclic && bit[index] >= 0 ? 1 : 0))
    }
  }
  return { counts, relaxed }
}

function popcount(word: number) {
  let value = word - ((word >>> 1) & 0x55555555)
  value = (value & 0x33333333) + ((value >>> 2) & 0x33333333)
  return (((value + (value >>> 4)) & 0x0f0f0f0f) * 0x01010101) >>> 24
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
  'parked',
  'merged',
  'closed',
]
