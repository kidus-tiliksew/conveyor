import type { Query, QueryClient, QueryKey } from '@tanstack/react-query'
import { nextActionsQueryKey } from '../components/inbox/use-next-actions'
import { workspaceChangeStreamURL } from './api'

// Waits before reopening the stream, indexed by consecutive connections that
// ended before `ready`; the last entry caps the delay at 30 seconds.
export const workspaceChangeRetryDelaysMs = [1_000, 2_000, 4_000, 8_000, 16_000, 30_000] as const

// The delay a frame waits before its refresh; frames inside it join it.
export const workspaceChangeRefreshDelayMs = 250

export type WorkspaceChangeRefreshOptions = {
  // Runs WorkspaceProvider's activity pass: cold after a connect, warm after a
  // change. The pass queues a follow-up when one is already running.
  refreshActivity: (cold: boolean) => void
  // Whether the route is a Blueprints route, the only place blueprints refresh.
  blueprintsActive: () => boolean
}

type Batch = { connected: boolean; allTasks: boolean; taskIds: Set<string> }

const emptyBatch = (): Batch => ({ connected: false, allTasks: false, taskIds: new Set() })

// The frame's task ID, or undefined for a notice that folded several tasks.
function frameTaskId(data: unknown) {
  if (typeof data !== 'string') return undefined
  try {
    const parsed = JSON.parse(data) as { task_id?: unknown }
    return typeof parsed?.task_id === 'string' && parsed.task_id !== '' ? parsed.task_id : undefined
  } catch {
    return undefined
  }
}

// The task IDs a bounded detail entry lists as blocking it.
function blockingTaskIds(query: Query) {
  const data = query.state.data as { task?: { blocking_task_ids?: string[] } } | undefined
  return data?.task?.blocking_task_ids ?? []
}

/**
 * Drives the shell's query refresh from the workspace change stream instead of
 * timers (component-web-dashboard, Workspace change stream;
 * feature-quiet-idle-runtime, "Pushed task streams and `await_review`"). An
 * open tab whose workspace does not change sends no request after its first
 * reads (req-quiet-idle-runtime AC-1.1).
 *
 * The stream is open only while the tab is visible. A browser `EventSource`
 * reads it; each `error` closes that source, so the browser's own retry never
 * runs, and this loop reconnects after a bounded backoff. Every `ready` frame
 * refreshes every target once, so a change committed between a reader's first
 * read and the stream's subscription is never missed. Every target carries the
 * workspace captured here. Returns the disposer.
 */
export function startWorkspaceChangeRefresh(
  queryClient: QueryClient,
  workspace: string,
  options: WorkspaceChangeRefreshOptions,
): () => void {
  let disposed = false
  let source: EventSource | undefined
  let reconnect: number | undefined
  let refresh: number | undefined
  // Consecutive connections that ended before `ready`.
  let failures = 0
  let batch = emptyBatch()
  // Entries that were fetching when a change arrived; each reads once more
  // when that fetch settles, because it may have started before the change.
  const followUps = new Set<string>()

  const unsubscribe = queryClient.getQueryCache().subscribe((event) => {
    if (event.type !== 'updated' || (event.action.type !== 'success' && event.action.type !== 'error')) return
    if (!followUps.delete(event.query.queryHash)) return
    const { queryKey } = event.query
    // Invalidate after the cache finishes dispatching this update.
    queueMicrotask(() => {
      if (!disposed) void queryClient.invalidateQueries({ queryKey, exact: true })
    })
  })

  // Invalidates each matching entry: an active one reads at once and an
  // inactive one reads when a reader next mounts. An entry already fetching
  // is never cancelled, so a burst of frames cannot starve a slow read.
  const refreshQueries = (queryKey: QueryKey, predicate?: (query: Query) => boolean) => {
    for (const query of queryClient.getQueryCache().findAll({ queryKey, predicate })) {
      if (query.state.fetchStatus === 'fetching') followUps.add(query.queryHash)
      else void queryClient.invalidateQueries({ queryKey: query.queryKey, exact: true })
    }
  }

  const flush = () => {
    refresh = undefined
    const { connected, allTasks, taskIds } = batch
    batch = emptyBatch()
    if (disposed) return
    options.refreshActivity(connected)
    refreshQueries(nextActionsQueryKey(workspace))
    refreshQueries(['pending-proposals', workspace])
    refreshQueries(['workspace', workspace])
    refreshQueries(['task-map', workspace])
    // Off the Blueprints routes a task sheet keeps the five-minute slow read.
    if (options.blueprintsActive()) refreshQueries(['blueprints', workspace])
    // `['task', workspace]` is the prefix of every taskDetailQueryKey entry.
    if (connected || allTasks) {
      refreshQueries(['task', workspace])
    } else if (taskIds.size > 0) {
      // A dependency can change without an event on its dependent, so a
      // detail that lists a named task as blocking refreshes too.
      refreshQueries(['task', workspace], (query) => {
        const taskId = query.queryKey[2]
        if (typeof taskId === 'string' && taskIds.has(taskId)) return true
        return blockingTaskIds(query).some((blocker) => taskIds.has(blocker))
      })
    }
  }

  const schedule = () => {
    if (refresh === undefined) refresh = window.setTimeout(flush, workspaceChangeRefreshDelayMs)
  }

  const connect = () => {
    reconnect = undefined
    if (disposed || source || document.visibilityState !== 'visible') return
    const current = new EventSource(workspaceChangeStreamURL(workspace))
    source = current
    current.addEventListener('ready', () => {
      if (source !== current) return
      failures = 0
      batch.connected = true
      schedule()
    })
    current.addEventListener('activity', (event) => {
      if (source !== current) return
      const taskId = frameTaskId((event as MessageEvent).data)
      if (taskId === undefined) batch.allTasks = true
      else batch.taskIds.add(taskId)
      schedule()
    })
    current.addEventListener('error', () => {
      if (source !== current) return
      current.close()
      source = undefined
      failures++
      reconnect = window.setTimeout(
        connect,
        workspaceChangeRetryDelaysMs[Math.min(failures, workspaceChangeRetryDelaysMs.length) - 1],
      )
    })
  }

  // Opens a stream now with the backoff reset, unless one is open.
  const connectNow = () => {
    if (source) return
    window.clearTimeout(reconnect)
    failures = 0
    connect()
  }

  // A hidden tab holds no stream. The pending reconnect and refresh are
  // dropped; the connect refresh after the tab is visible again covers them.
  const disconnect = () => {
    source?.close()
    source = undefined
    window.clearTimeout(reconnect)
    reconnect = undefined
    window.clearTimeout(refresh)
    refresh = undefined
    batch = emptyBatch()
  }

  const onVisibility = () => {
    if (document.visibilityState === 'visible') connectNow()
    else disconnect()
  }
  const onOnline = () => {
    if (reconnect !== undefined) connectNow()
  }
  document.addEventListener('visibilitychange', onVisibility)
  window.addEventListener('online', onOnline)
  connect()

  return () => {
    disposed = true
    disconnect()
    document.removeEventListener('visibilitychange', onVisibility)
    window.removeEventListener('online', onOnline)
    unsubscribe()
    followUps.clear()
  }
}
