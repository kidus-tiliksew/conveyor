import { type Query, type QueryKey, useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'
import { nextActionQueryKey, nextActionsQueryKey } from '../components/inbox/use-next-actions'
import { taskDetailQueryKey } from './query-keys'

// Waits before reopening the stream, indexed by consecutive connections that
// ended without an activity frame; the last entry caps the delay.
const taskStreamRetryDelaysMs = [1_000, 2_000, 4_000, 8_000, 16_000, 30_000] as const

// The delay a ready or activity frame waits before its one refresh; frames
// arriving inside it join that refresh.
const taskStreamRefreshDelayMs = 250

// Live updates: the per-task notify stream (`mode=notify`), read with `fetch`,
// feeds the Query cache by invalidation (component-web-task-surfaces, Live
// updates). The server never replays history in this mode: it sends `ready`
// once it has read the task's history fingerprint and `activity` whenever the
// fingerprint changes. Each frame schedules one refresh of the task detail, the
// workspace's activity family, the task's next action, and the workspace inbox,
// all in the workspace captured when the effect started; it never touches
// history pages, audit records, or verification families. A batch holding an
// `activity` frame invalidates every target, however fresh. A batch of `ready`
// frames alone refreshes only stale targets that are not already fetching and
// never cancels an in-flight read, so the first detail read of a just-opened
// sheet is not restarted; a reconnect after the stale window still refreshes,
// covering events committed during an outage. `fetch` does not reconnect on its
// own, so one serial loop per effect reopens the stream after it ends, errors,
// returns a non-OK status, or has no body (including 204), waiting a bounded
// backoff.
// A ready-only refresh skips entries still fresh by their own observers'
// stale time and entries already fetching (component-web-task-surfaces).
function staleAndIdle(query: Query) {
  return query.isStale() && query.state.fetchStatus === 'idle'
}

export function useTaskStream(taskId: string, workspace: string) {
  const queryClient = useQueryClient()
  useEffect(() => {
    if (!workspace) return
    const controller = new AbortController()
    const { signal } = controller
    const url = `/v1/tasks/${encodeURIComponent(taskId)}/events/stream?mode=notify&workspace_id=${encodeURIComponent(workspace)}`
    let refresh: number | undefined
    // Whether an activity frame joined the scheduled refresh.
    let changed = false
    const invalidate = () => {
      refresh = undefined
      const activity = changed
      changed = false
      // An aborted effect belongs to another task, workspace, or an unmounted view.
      if (signal.aborted) return
      const targets: QueryKey[] = [
        taskDetailQueryKey(workspace, taskId),
        ['activity', workspace],
        // The task's next action and the inbox listing it move with its events.
        nextActionQueryKey(workspace, taskId),
        nextActionsQueryKey(workspace),
      ]
      for (const queryKey of targets) {
        if (activity) void queryClient.invalidateQueries({ queryKey })
        else void queryClient.invalidateQueries({ queryKey, predicate: staleAndIdle }, { cancelRefetch: false })
      }
    }
    const refreshQueries = (activity: boolean) => {
      if (signal.aborted) return
      if (activity) changed = true
      // A frame inside the delay joins the refresh already scheduled.
      if (refresh !== undefined) return
      refresh = window.setTimeout(invalidate, taskStreamRefreshDelayMs)
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
    const readConnection = async () => {
      let framed = false
      try {
        const response = await fetch(url, { signal })
        // A 204 No Content carries no stream even when the browser exposes an
        // empty body, so it counts as a missing body rather than a connection.
        if (!response.ok || response.status === 204 || !response.body) {
          void response.body?.cancel().catch(() => undefined)
          return false
        }
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
            const name = /^event: *(\S+)/m.exec(frame)?.[1]
            if (name === 'activity') {
              // Only an activity frame proves the stream carried a change.
              framed = true
              refreshQueries(true)
            } else if (name === 'ready') {
              refreshQueries(false)
            }
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
        const framed = await readConnection()
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
