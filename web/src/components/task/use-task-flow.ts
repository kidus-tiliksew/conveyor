import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useSyncExternalStore } from 'react'
import { fetchTaskFlow } from '../../lib/api'
import { taskDetailQueryKey } from '../../lib/query-keys'
import { useWorkspaceSelection } from '../app-shell'

export function taskFlowQueryKey(workspace: string, taskId: string) {
  return ['task-flow', workspace, taskId] as const
}

// The flow has two dashboard readers: the opt-in Board stage stripe
// (req-operator-next-action AC-3.3) and the task-detail Flow tab's chart, which
// mounts only while that tab is shown (AC-7.4; feature-operator-next-action
// NA-7). The flow refreshes when the task detail does. It doesn't refresh on its own
// clock: SSE invalidation, blocker polling and mutations all refresh the
// detail entry. This hook watches that cache entry without adding an observer
// to it (an observer without the detail's queryFn would take over its
// refetches), and refetches the flow each time the entry updates. A reader
// that is not mounted reads nothing: no stripe while the preference is off, and
// no chart while task detail shows its Timeline tab.
export function useTaskFlow(taskId: string) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const subscribe = useCallback(
    (onChange: () => void) => queryClient.getQueryCache().subscribe(onChange),
    [queryClient],
  )
  const dataUpdatedAt = useSyncExternalStore(
    subscribe,
    () => queryClient.getQueryState(taskDetailQueryKey(workspace, taskId))?.dataUpdatedAt ?? 0,
  )
  const flow = useQuery({
    queryKey: taskFlowQueryKey(workspace, taskId),
    queryFn: ({ signal }) => fetchTaskFlow(workspace, taskId, signal),
    enabled: Boolean(workspace && taskId),
  })
  // The flow's own first fetch already reads the detail as it stood when the
  // reader mounted, so only later detail updates trigger a refetch.
  const seen = useRef(dataUpdatedAt)
  useEffect(() => {
    if (dataUpdatedAt === seen.current) return
    seen.current = dataUpdatedAt
    void queryClient.invalidateQueries({ queryKey: taskFlowQueryKey(workspace, taskId), exact: true })
  }, [dataUpdatedAt, queryClient, workspace, taskId])
  return flow
}
