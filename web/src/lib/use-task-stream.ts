import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'

// Live updates: the per-task SSE stream feeds the Query
// cache by invalidation — every `activity` event schedules a debounced
// refetch of the task detail and the feed. EventSource reconnects itself.
export function useTaskStream(taskId: string, workspace: string) {
  const queryClient = useQueryClient()
  useEffect(() => {
    if (!workspace) return
    const controller = new AbortController()
    let refresh: number | undefined,
      buffer = ''
    const refreshQueries = () => {
      window.clearTimeout(refresh)
      refresh = window.setTimeout(() => {
        void queryClient.invalidateQueries({ queryKey: ['task', workspace, taskId] })
        void queryClient.invalidateQueries({ queryKey: ['activity'] })
      }, 250)
    }
    const token = sessionStorage.getItem('conveyor-token') ?? ''
    void fetch(`/v1/tasks/${encodeURIComponent(taskId)}/events/stream?workspace_id=${encodeURIComponent(workspace)}`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
      signal: controller.signal,
    })
      .then(async (response) => {
        if (!response.ok || !response.body) return
        const reader = response.body.getReader(),
          decoder = new TextDecoder()
        for (;;) {
          const { value, done } = await reader.read()
          if (done) break
          buffer += decoder.decode(value, { stream: true })
          const frames = buffer.split('\n\n')
          buffer = frames.pop() ?? ''
          for (const frame of frames) if (frame.startsWith('event: activity')) refreshQueries()
        }
      })
      .catch(() => undefined)
    return () => {
      window.clearTimeout(refresh)
      controller.abort()
    }
  }, [queryClient, taskId, workspace])
}
