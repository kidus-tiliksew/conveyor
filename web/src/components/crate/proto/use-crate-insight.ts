// PROTOTYPE. Reads what the crate page's new sections need: every recorded
// activity event of the crate and each member task's own read, then derives
// the insight from them. The sample crate supplies the same shapes from
// memory.
import { useQueries, useQuery } from '@tanstack/react-query'
import { useMemo } from 'react'
import type { CrateActivityEvent, CrateActivityPage, CrateDetail } from '../../../lib/types'
import { buildInsight, type CrateInsight, type Promotion, type TaskExtra } from './model'

export interface SampleData {
  events: CrateActivityEvent[]
  extras: Record<string, TaskExtra>
  promotions: Promotion[]
}

async function getJSON<T>(url: string, signal?: AbortSignal) {
  const response = await fetch(url, { signal })
  if (!response.ok) throw new Error(`${response.status} ${response.statusText}`)
  return response.json() as Promise<T>
}

async function allActivity(workspace: string, crateId: string, signal?: AbortSignal) {
  const events: CrateActivityEvent[] = []
  let cursor = ''
  // Up to 2,000 events: enough for any crate a person follows on one page.
  for (let page = 0; page < 10; page++) {
    const params = new URLSearchParams({ limit: '200', workspace_id: workspace })
    if (cursor) params.set('cursor', cursor)
    const body = await getJSON<CrateActivityPage>(
      `/v1/crates/${encodeURIComponent(crateId)}/activity?${params.toString()}`,
      signal,
    )
    events.push(...body.events)
    if (!body.next_cursor) break
    cursor = body.next_cursor
  }
  return events
}

export function useCrateInsight(
  workspace: string,
  crate: CrateDetail,
  sample?: SampleData,
): { insight: CrateInsight | undefined; events: CrateActivityEvent[]; loading: boolean } {
  const live = !sample
  const activity = useQuery({
    queryKey: ['crates', workspace, crate.id, 'proto-activity'],
    queryFn: ({ signal }) => allActivity(workspace, crate.id, signal),
    enabled: live && Boolean(workspace),
  })
  const extras = useQueries({
    queries: (live ? crate.tasks : []).map((task) => ({
      queryKey: ['crates', workspace, crate.id, 'proto-task', task.id, task.state],
      queryFn: ({ signal }: { signal: AbortSignal }) =>
        getJSON<TaskExtra>(
          `/v1/tasks/${encodeURIComponent(task.id)}?workspace_id=${encodeURIComponent(workspace)}`,
          signal,
        ),
      staleTime: 60_000,
    })),
  })
  const extrasReady = extras.every((query) => !query.isPending)
  const extrasKey = extras.map((query) => query.dataUpdatedAt).join(',')

  // biome-ignore lint/correctness/useExhaustiveDependencies: extrasKey stands for the task reads
  const insight = useMemo(() => {
    if (sample) return buildInsight(crate, sample.events, sample.extras, sample.promotions)
    if (!activity.data || !extrasReady) return undefined
    const byTask: Record<string, TaskExtra | undefined> = {}
    crate.tasks.forEach((task, index) => {
      byTask[task.id] = extras[index]?.data
    })
    return buildInsight(crate, activity.data, byTask, [])
  }, [crate, sample, activity.data, extrasReady, extrasKey])

  return {
    insight,
    events: sample ? sample.events : (activity.data ?? []),
    loading: live && (activity.isPending || !extrasReady),
  }
}
