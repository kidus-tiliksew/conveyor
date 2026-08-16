import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { fetchTaskActivity } from '../../lib/api'
import { groupForSummary } from '../../lib/activity'
import { stageGroups } from '../../lib/contracts'
import { useTaskStream } from '../../lib/use-task-stream'
import { useActivity, useWorkspaceSelection } from '../app-shell'

// Shared by the sheet and the full page: the task-detail query plus the SSE
// stream that keeps it (and the board) fresh.
export function useTaskDetail(taskId: string) {
  const { workspace } = useWorkspaceSelection()
  const query = useQuery({
    queryKey: ['task', workspace, taskId],
    queryFn: () => fetchTaskActivity(taskId),
    // A dependency can merge without emitting an event on this task. Poll only
    // while the task reports active blockers; SSE remains the fast path.
    refetchInterval: (current) => ((current.state.data?.task.blocking_task_ids?.length ?? 0) > 0 ? 15_000 : false),
  })
  useTaskStream(taskId, workspace)
  return query
}

// Prev/next follow the board's visual order: columns left to right, cards
// by recency within each column. A surface that already knows the order it is
// showing passes `enabled: false` and supplies its own. Otherwise navigation is
// deliberately limited to the current bounded Board page (AC-2.3).
export function useTaskOrder(taskId: string, enabled = true) {
  const { data: activity } = useActivity(undefined, enabled)
  return useMemo(() => {
    const byGroup = new Map<string, Array<{ id: string; at: string }>>()
    for (const summary of activity?.items ?? []) {
      const key = groupForSummary(summary)
      byGroup.set(key, [
        ...(byGroup.get(key) ?? []),
        { id: summary.task.id, at: summary.last_event_at || summary.task.created_at },
      ])
    }
    const order = stageGroups.flatMap(({ key }) =>
      (byGroup.get(key) ?? [])
        .sort((a, b) => new Date(b.at).getTime() - new Date(a.at).getTime())
        .map((entry) => entry.id),
    )
    const index = order.indexOf(taskId)
    const edges: string[] = []
    if (activity && index < 0) {
      edges.push('This task is outside the loaded Board window.')
    } else if (activity) {
      if (index === 0 && activity.offset > 0) edges.push('The previous task is on an earlier Board page.')
      if (index === order.length - 1 && activity.offset + activity.items.length < activity.total) {
        edges.push('The next task is on a later Board page.')
      }
    }
    return {
      previousId: index > 0 ? order[index - 1] : undefined,
      nextId: index >= 0 && index < order.length - 1 ? order[index + 1] : undefined,
      windowEdge: edges.join(' '),
    }
  }, [activity, taskId])
}
