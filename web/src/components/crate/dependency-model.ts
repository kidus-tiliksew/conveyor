// The crate page's reading of the served dependencies (req-crate-insights
// REQ-6; component-crates invariants 38 and 39). The server computes every
// dependency value: which entries wait, `wait_depth`, and `waited_on_by`.
// This file reads `depends_on` only to choose which members to leave
// undimmed and where to draw a link, and derives no state, count, or order
// (component-crates invariant 21; DEC-151(5)).
import type { CrateInsightTask } from '../../lib/types'

// One link between two members: `from` is the member that is waited on, and
// `to` is the member whose `waiting` entry names it.
export interface WaitLink {
  from: string
  to: string
}

// Each served `waiting` entry that names a current member, as a link from
// that member to the member that waits on it, in the served task order and
// then the served entry order.
export function waitLinks(tasks: CrateInsightTask[]): WaitLink[] {
  const members = new Set(tasks.map((task) => task.id))
  return tasks.flatMap((task) =>
    task.depends_on
      .filter((entry) => entry.waiting && entry.member && members.has(entry.id))
      .map((entry) => ({ from: entry.id, to: task.id })),
  )
}

// The emphasized member and every member that `depends_on` entries connect
// to it in either direction, through any number of members. A merged
// dependency counts, so the chain keeps its history lit. A sibling that only
// shares a dependency with the emphasized member is not connected to it in
// one direction and stays out (req-crate-insights AC-6.6; component-crates
// invariant 33). Null when no task is emphasized.
export function relatedMembers(tasks: CrateInsightTask[], id: string | null): Set<string> | null {
  if (id === null) return null
  const members = new Map(tasks.map((task) => [task.id, task]))
  const up = new Map<string, string[]>()
  const down = new Map<string, string[]>()
  for (const task of tasks)
    for (const entry of task.depends_on) {
      if (!entry.member || !members.has(entry.id)) continue
      up.set(task.id, [...(up.get(task.id) ?? []), entry.id])
      down.set(entry.id, [...(down.get(entry.id) ?? []), task.id])
    }
  const related = new Set([id])
  for (const edges of [up, down]) {
    const seen = new Set([id])
    const stack = [id]
    while (stack.length > 0) {
      for (const next of edges.get(stack.pop() as string) ?? []) {
        if (seen.has(next)) continue
        seen.add(next)
        related.add(next)
        stack.push(next)
      }
    }
  }
  return related
}

// A key that changes only when the links change, so a refetch that serves
// the same dependencies keeps the scene's placement.
export function linksKey(links: WaitLink[]) {
  return links.map((link) => `${link.from}>${link.to}`).join('|')
}
