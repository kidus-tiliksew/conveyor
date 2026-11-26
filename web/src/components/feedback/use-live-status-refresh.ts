import { hashKey, onlineManager, type QueryKey, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useSyncExternalStore } from 'react'

/**
 * While a page shows at least one status line or crate line and its tab is
 * visible, it reads again every 60 seconds (component-production-feedback
 * invariant 23; component-web-dashboard invariant 15).
 */
export const LIVE_STATUS_REFRESH_MS = 60_000

/**
 * While a task or crate answer carries `environments_pending` true, the page
 * reads again after 10 seconds, at most six times in a row
 * (component-production-feedback invariant 23; req-production-feedback
 * AC-3.2).
 */
export const LIVE_STATUS_PENDING_MS = 10_000
export const LIVE_STATUS_PENDING_READS = 6

/**
 * The query options of a live-status read while its served answer is
 * pending. A focus, reconnect, or automatic retry read would skip the
 * pending budget, so none runs while the answer is pending. Otherwise the
 * query keeps its own options.
 */
export function pendingReadOptions(pending: boolean | undefined) {
  return pending ? { retry: false, refetchOnWindowFocus: false, refetchOnReconnect: false } : {}
}

function subscribeVisibility(onChange: () => void) {
  document.addEventListener('visibilitychange', onChange)
  return () => document.removeEventListener('visibilitychange', onChange)
}

function tabVisible() {
  return document.visibilityState !== 'hidden'
}

// TanStack Query's own online state. In its default online network mode a
// read that starts while it is false is paused before the query function
// runs, and the paused read resumes by itself on a later focus or reconnect.
function subscribeOnline(onChange: () => void) {
  return onlineManager.subscribe(onChange)
}

function online() {
  return onlineManager.isOnline()
}

/** What a live-status query served and where its fetch stands. */
export interface LiveStatusRead {
  /** The key of the query that the timer reads again. A new key starts a new run. */
  queryKey: QueryKey
  /** The number of served status lines or crate lines. */
  shown: number
  /** The served `environments_pending`. An absent flag is false. */
  pending: boolean
  /** True when the query holds a served answer. */
  hasAnswer: boolean
  /** The time of the newest successful answer. */
  dataUpdatedAt: number
  /** The time of the newest failed read. */
  errorUpdatedAt: number
  isFetching: boolean
  /** True while a read of the query waits in TanStack Query's paused queue. */
  isPaused: boolean
  refetch: () => Promise<unknown>
}

type ReadKind = 'pending' | 'regular'

/**
 * The one timed refresh of the task live-status read and the crate read
 * (component-production-feedback invariant 23). The page derives nothing: it
 * reads the served `environments_pending` and the number of served lines.
 *
 * - A served pending answer schedules one read after 10 seconds. A run holds
 *   at most six such reads, counted when each read starts. The first read of
 *   the page is not one of them.
 * - Without a pending read left, a page that shows at least one line reads
 *   again after 60 seconds, and a page whose answer is empty sets no timer.
 * - A pending answer from a 60-second read starts no new run of 10-second
 *   reads. Only a successful answer that is not pending, or a new query
 *   identity, starts the count again. A failed read, a hidden tab, and a
 *   render do not.
 * - No timer runs while the tab is hidden, while TanStack Query reports the
 *   browser offline, or while a read is in flight or paused. A tab that
 *   becomes visible again, or a browser that comes back online, waits the
 *   whole delay, so it makes no catch-up read. The timer stops when the
 *   query key changes and when the page unmounts.
 * - A timed read never waits in TanStack Query's paused queue, which would
 *   resume it on a later focus or reconnect. The timer starts a read only
 *   while the browser is online. A read that pauses all the same is
 *   cancelled and does not count.
 */
export function useLiveStatusRefresh(read: LiveStatusRead) {
  const queryClient = useQueryClient()
  const visible = useSyncExternalStore(subscribeVisibility, tabVisible, () => true)
  const isOnline = useSyncExternalStore(subscribeOnline, online, () => true)
  const identity = hashKey(read.queryKey)
  const run = useRef({ identity, pendingReads: 0, spent: false, seenAt: read.dataUpdatedAt })
  // The read that the timer started and that has not settled yet.
  const started = useRef<ReadKind | null>(null)
  const latest = useRef(read)
  latest.current = read

  const { shown, pending, hasAnswer, dataUpdatedAt, errorUpdatedAt, isFetching, isPaused } = read
  useEffect(() => {
    if (run.current.identity !== identity) {
      // A new query starts a new run. Its served answer, cached or not,
      // counts as the first read.
      run.current = { identity, pendingReads: 0, spent: false, seenAt: dataUpdatedAt }
      started.current = null
    }
    const state = run.current
    // A read in flight or paused sets no timer. Its answer or its failure
    // runs this effect again.
    if (isFetching || isPaused) return
    if (dataUpdatedAt !== state.seenAt) {
      state.seenAt = dataUpdatedAt
      if (!pending) {
        state.pendingReads = 0
        state.spent = false
      } else if (started.current === 'regular') {
        state.spent = true
      }
    }
    // errorUpdatedAt reruns this effect after a failed read. The failure
    // keeps the count and the last served answer.
    void errorUpdatedAt
    started.current = null
    if (!hasAnswer || !visible || !isOnline) return
    let kind: ReadKind | null = null
    if (pending && !state.spent && state.pendingReads < LIVE_STATUS_PENDING_READS) kind = 'pending'
    else if (shown > 0) kind = 'regular'
    if (!kind) return
    const timer = window.setTimeout(
      () => {
        const current = latest.current
        if (!tabVisible() || !online() || current.isFetching || current.isPaused) return
        started.current = kind
        void current.refetch()
        // The refetch sets the fetch status at once. A paused read would
        // resume later by itself, so it is cancelled and is not counted.
        if (queryClient.getQueryState(current.queryKey)?.fetchStatus === 'paused') {
          started.current = null
          void queryClient.cancelQueries({ queryKey: current.queryKey, exact: true })
          return
        }
        if (kind === 'pending') state.pendingReads += 1
      },
      kind === 'pending' ? LIVE_STATUS_PENDING_MS : LIVE_STATUS_REFRESH_MS,
    )
    return () => window.clearTimeout(timer)
  }, [
    identity,
    shown,
    pending,
    hasAnswer,
    dataUpdatedAt,
    errorUpdatedAt,
    isFetching,
    isPaused,
    visible,
    isOnline,
    queryClient,
  ])
}
