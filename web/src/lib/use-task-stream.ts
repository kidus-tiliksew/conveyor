import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'

// Live updates (spec §13.3, §17.3): the per-task SSE stream feeds the Query
// cache by invalidation — every `activity` event schedules a debounced
// refetch of the task detail and the feed. EventSource reconnects itself.
export function useTaskStream(taskId: string) {
  const queryClient = useQueryClient()
  useEffect(() => {
    const stream = new EventSource(`/v1/tasks/${encodeURIComponent(taskId)}/events/stream`)
    let refresh: number | undefined
    stream.addEventListener('activity', () => {
      window.clearTimeout(refresh)
      refresh = window.setTimeout(() => {
        void queryClient.invalidateQueries({ queryKey: ['task', taskId] })
        void queryClient.invalidateQueries({ queryKey: ['activity'] })
      }, 250)
    })
    return () => {
      window.clearTimeout(refresh)
      stream.close()
    }
  }, [queryClient, taskId])
}
