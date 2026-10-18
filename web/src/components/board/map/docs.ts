// The requirements tasks deliver, with what each one's open work looks like
// (req-board-map REQ-1).
import type { TaskMapRequirement } from '../../../lib/types'
import { DONE, type GraphTask, type TaskGraphModel } from './graph-model'
import type { DocStats } from './hub-node'

export type Grouping = 'dependencies' | 'requirements'
export type DocKind = 'requirement'

export interface DocIndex {
  kind: DocKind
  docs: Map<string, { id: string; title: string }>
  stats: Map<string, DocStats>
  members: Map<string, Set<string>>
  docsOf: (task: GraphTask) => Array<{ id: string; title: string }>
}

// Delivery progress is the read's workspace-wide count for each requirement
// (req-board-map AC-1.3): merged over open plus merged, closed in neither.
export function buildDocIndex(model: TaskGraphModel, kind: DocKind, requirements: TaskMapRequirement[]): DocIndex {
  const raw = (task: GraphTask) => task.requirements
  const docs = new Map<string, { id: string; title: string }>()
  const members = new Map<string, Set<string>>()
  const openUse = new Map<string, number>()
  for (const task of model.tasks.values()) {
    if (task.stub) continue
    const open = !DONE.has(task.status)
    for (const doc of raw(task)) {
      docs.set(doc.id, doc)
      members.set(doc.id, (members.get(doc.id) ?? new Set()).add(task.id))
      if (open) openUse.set(doc.id, (openUse.get(doc.id) ?? 0) + 1)
    }
  }
  const progress = new Map(requirements.map((entry) => [entry.id, entry]))
  const stats = new Map<string, DocStats>()
  for (const [id, ids] of members) {
    const counts = progress.get(id)
    const entry: DocStats = {
      total: counts?.total_task_count ?? 0,
      merged: counts?.merged_task_count ?? 0,
      open: counts?.open_task_count ?? 0,
      byStatus: {},
    }
    for (const taskId of ids) {
      const task = model.tasks.get(taskId)
      if (!task || DONE.has(task.status)) continue
      entry.byStatus[task.status] = (entry.byStatus[task.status] ?? 0) + 1
    }
    stats.set(id, entry)
  }
  // Most specific first: the document with the least open work.
  const docsOf = (task: GraphTask) => [...raw(task)].sort((a, b) => (openUse.get(a.id) ?? 0) - (openUse.get(b.id) ?? 0))
  return { kind, docs, stats, members, docsOf }
}

export const hubId = (docId: string) => `doc:${docId}`

export const UNATTACHED = '__none'

export interface MapCluster {
  id: string
  hubId?: string
  taskIds: string[]
  edges: Array<{ id: string; source: string; target: string }>
}

// For the map every open task is placed once, with its neighbours: the
// document most shared with its direct dependencies and dependents, then the
// most specific one.
export function homeDoc(task: GraphTask, model: TaskGraphModel, index: DocIndex) {
  const candidates = index.docsOf(task)
  if (candidates.length === 0) return UNATTACHED
  const neighbours = [...task.dependsOn, ...(model.dependents.get(task.id) ?? [])]
  let best = candidates[0].id
  let bestScore = -1
  for (const candidate of candidates) {
    let score = 0
    for (const id of neighbours) {
      const neighbour = model.tasks.get(id)
      if (neighbour && index.docsOf(neighbour).some((doc) => doc.id === candidate.id)) score++
    }
    if (score > bestScore) {
      best = candidate.id
      bestScore = score
    }
  }
  return best
}

export function mapClusters(visible: Set<string>, model: TaskGraphModel, index: DocIndex) {
  const home = new Map<string, string>()
  const groups = new Map<string, string[]>()
  for (const id of visible) {
    const task = model.tasks.get(id)
    if (!task || task.stub || DONE.has(task.status)) continue
    const doc = homeDoc(task, model, index)
    home.set(id, doc)
    groups.set(doc, [...(groups.get(doc) ?? []), id])
  }
  // Finished tasks, when shown, join the cluster their home names if it holds
  // open work; they never make a cluster of their own (AC-3.4).
  for (const id of visible) {
    const task = model.tasks.get(id)
    if (!task || task.stub || !DONE.has(task.status)) continue
    const doc = homeDoc(task, model, index)
    const ids = groups.get(doc)
    if (!ids) continue
    home.set(id, doc)
    ids.push(id)
  }
  const clusters: MapCluster[] = []
  const crossEdges: Array<{ id: string; source: string; target: string }> = []
  for (const [doc, ids] of groups) {
    const members = new Set(ids)
    const hub = doc === UNATTACHED ? undefined : hubId(doc)
    const edges: MapCluster['edges'] = []
    const feedsOnward = new Set<string>()
    for (const id of ids) {
      for (const dependency of model.tasks.get(id)?.dependsOn ?? []) {
        if (members.has(dependency)) {
          edges.push({ id: `${dependency}->${id}`, source: dependency, target: id })
          feedsOnward.add(dependency)
        } else if (home.has(dependency)) {
          crossEdges.push({ id: `${dependency}->${id}`, source: dependency, target: id })
        }
      }
    }
    if (hub)
      for (const id of ids) if (!feedsOnward.has(id)) edges.push({ id: `${id}=>${hub}`, source: id, target: hub })
    clusters.push({ id: doc, hubId: hub, taskIds: ids, edges })
  }
  return { clusters, home, crossEdges }
}
