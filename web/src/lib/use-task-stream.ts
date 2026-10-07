import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'

// Waits before reopening the stream, indexed by consecutive connections that
// ended without an activity frame; the last entry caps the delay.
const taskStreamRetryDelaysMs = [1_000, 2_000, 4_000, 8_000, 16_000, 30_000] as const

// Live updates: the per-task SSE stream, read with `fetch`, feeds the Query
// cache by invalidation. Every `activity` frame schedules a debounced refetch
// of the task detail and the feed. `fetch` does not reconnect on its own, so
// one serial loop per effect reopens the stream after it ends, errors, returns
// a non-OK status, or has no body (including 204), waiting a bounded backoff.
// Events committed while the stream was down produce no frame, so every usable
// connection opened by a retry refetches both families once
// (component-web-dashboard Server-state discipline).
export function useTaskStream(taskId: string, workspace: string) {
  const queryClient = useQueryClient()
  useEffect(() => {
    if (!workspace) return
    const controller = new AbortController()
    const { signal } = controller
    const url = `/v1/tasks/${encodeURIComponent(taskId)}/events/stream?workspace_id=${encodeURIComponent(workspace)}`
    let refresh: number | undefined
    const invalidate = () => {
      // An aborted effect belongs to another task, workspace, or an unmounted view.
      if (signal.aborted) return
      void queryClient.invalidateQueries({ queryKey: ['task', workspace, taskId] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    }
    const refreshQueries = () => {
      window.clearTimeout(refresh)
      refresh = window.setTimeout(invalidate, 250)
    }
    const wait = (ms: number) =>
      new Promise<void>((resolve) => {
        if (signal.aborted) return resolve()
        const finish = () => {
          window.clearTimeout(timer)
          signal.removeEventListener('abort', finish)
          resolve()
        }
        const timer = window.setTimeout(finish, ms)
        signal.addEventListener('abort', finish)
      })

    // Reads one connection. Resolves whether it delivered an activity frame;
    // the decoder and partial-frame buffer never outlive their response.
    const readConnection = async (reconnecting: boolean) => {
      let framed = false
      try {
        const response = await fetch(url, { signal })
        // A 204 No Content carries no stream even when the browser exposes an
        // empty body, so it counts as a missing body rather than a connection.
        if (!response.ok || response.status === 204 || !response.body) {
          void response.body?.cancel().catch(() => undefined)
          return false
        }
        if (reconnecting) invalidate()
        const reader = response.body.getReader(),
          decoder = new TextDecoder()
        let buffer = ''
        for (;;) {
          const { value, done } = await reader.read()
          if (done || signal.aborted) break
          buffer += decoder.decode(value, { stream: true })
          const frames = buffer.split('\n\n')
          buffer = frames.pop() ?? ''
          for (const frame of frames) {
            if (!frame.startsWith('event: activity')) continue
            framed = true
            refreshQueries()
          }
        }
      } catch {
        // A network, read, or abort failure ends this connection like EOF.
      }
      return framed
    }

    void (async () => {
      let failures = 0
      for (let attempt = 0; !signal.aborted; attempt++) {
        if (attempt > 0) {
          await wait(taskStreamRetryDelaysMs[Math.min(failures, taskStreamRetryDelaysMs.length) - 1])
          if (signal.aborted) return
        }
        const framed = await readConnection(attempt > 0)
        // A frame proves the stream worked, so the next wait starts over at
        // the shortest delay; a connection that ended without one backs off.
        failures = framed ? 1 : failures + 1
      }
    })()

    return () => {
      window.clearTimeout(refresh)
      controller.abort()
    }
  }, [queryClient, taskId, workspace])
}
