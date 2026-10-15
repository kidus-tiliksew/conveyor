import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'
import { fetchTaskHistory, TaskHistoryError } from '../../lib/api'
import { taskHistoryQueryKey } from '../../lib/query-keys'
import type { TaskHistorySelection } from '../../lib/types'
import { withQuerySignal } from './use-task-detail'

// One history page of a task (component-web-task-surfaces, History pages). A
// component mounts this only while its disclosure is open, so a closed
// disclosure issues no request. The page is read in the explicit workspace of
// its key with the query's abort signal, never retried, never kept once
// unused, and never refetched on focus, reconnect, or an interval. The task
// stream, dependency polling, and mutations never invalidate this family.
// Unmounting, or moving to another cursor, task, or workspace, cancels the read
// in flight, so a response for a superseded key never renders.
export function useTaskHistory(workspace: string, taskId: string, selection: TaskHistorySelection, cursor: string) {
  const queryClient = useQueryClient()
  useEffect(() => {
    const queryKey = taskHistoryQueryKey(workspace, taskId, selection, cursor)
    return () => {
      // Cancel once no observer remains. Deferring the check lets the
      // StrictMode development rehearsal remount without aborting the read.
      queueMicrotask(() => {
        const query = queryClient.getQueryCache().find({ queryKey, exact: true })
        if (query && query.getObserversCount() === 0) void query.cancel()
      })
    }
  }, [queryClient, workspace, taskId, selection, cursor])
  return useQuery({
    queryKey: taskHistoryQueryKey(workspace, taskId, selection, cursor),
    queryFn: (context) =>
      withQuerySignal(context, (signal) =>
        fetchTaskHistory(workspace, taskId, { selection, cursor: cursor || undefined }, signal),
      ),
    enabled: Boolean(taskId),
    retry: false,
    staleTime: 0,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    refetchInterval: false,
  })
}

// A 409 `history_restart` means the cursor expired, came from another server
// process, or the boundary's history changed: the client starts again from the
// newest page (component-http-api, Task history pages).
export function isHistoryRestart(error: unknown) {
  return error instanceof TaskHistoryError && error.status === 409 && error.code === 'history_restart'
}
