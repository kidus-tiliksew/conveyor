import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'
import { fetchTaskTranscriptCapture } from '../../lib/api'
import { taskTranscriptQueryKey } from '../../lib/query-keys'
import { withQuerySignal } from './use-task-detail'

// One transcript capture's content (component-web-task-surfaces, Transcript
// disclosures). Only an open disclosure's body mounts this hook, so a closed
// disclosure issues no request. The read uses the key's explicit workspace and
// the query's abort signal, is never retried or kept once unused, and is never
// refetched on focus, reconnect, or an interval. The task stream, dependency
// polling, and mutations never invalidate this family. Unmounting, or moving
// to another task, workspace, work order, or attempt, cancels the read in
// flight, so a response for a superseded key never renders.
export function useTaskTranscript(workspace: string, taskId: string, workOrderId: string, attemptId: string) {
  const queryClient = useQueryClient()
  useEffect(() => {
    const queryKey = taskTranscriptQueryKey(workspace, taskId, workOrderId, attemptId)
    return () => {
      // Cancel once no observer remains. Deferring the check lets the
      // StrictMode development rehearsal remount without aborting the read.
      queueMicrotask(() => {
        const query = queryClient.getQueryCache().find({ queryKey, exact: true })
        if (query && query.getObserversCount() === 0) void query.cancel()
      })
    }
  }, [queryClient, workspace, taskId, workOrderId, attemptId])
  return useQuery({
    queryKey: taskTranscriptQueryKey(workspace, taskId, workOrderId, attemptId),
    queryFn: (context) =>
      withQuerySignal(context, (signal) =>
        fetchTaskTranscriptCapture(workspace, taskId, workOrderId, attemptId, signal),
      ),
    enabled: Boolean(workspace && taskId && workOrderId && attemptId),
    retry: false,
    staleTime: 0,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    refetchInterval: false,
  })
}
