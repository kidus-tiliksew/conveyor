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
// by recency within each column.
export function useTaskOrder(taskId: string) {
  const { data: activity } = useActivity()
  return useMemo(() => {
    const byGroup = new Map<string, Array<{ id: string; at: string }>>()
    for (const summary of activity ?? []) {
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
    return {
      previousId: index > 0 ? order[index - 1] : undefined,
      nextId: index >= 0 && index < order.length - 1 ? order[index + 1] : undefined,
    }
  }, [activity, taskId])
}
