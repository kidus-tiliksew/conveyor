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

/** What a live-status query served and where its fetch stands. */
export interface LiveStatusRead {
  /**
   * The query identity, such as its serialized key. A new identity starts a
   * new run.
   */
  identity: string
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
 * - No timer runs while the tab is hidden or a read is in flight. A tab that
 *   becomes visible again waits the whole delay, so it makes no catch-up
 *   read. The timer stops when the identity changes and when the page
 *   unmounts.
 */
export function useLiveStatusRefresh(read: LiveStatusRead) {
  const visible = useSyncExternalStore(subscribeVisibility, tabVisible, () => true)
  const run = useRef({ identity: read.identity, pendingReads: 0, spent: false, seenAt: read.dataUpdatedAt })
  // The read that the timer started and that has not settled yet.
  const started = useRef<ReadKind | null>(null)
  const latest = useRef(read)
  latest.current = read

  const { identity, shown, pending, hasAnswer, dataUpdatedAt, errorUpdatedAt, isFetching } = read
  useEffect(() => {
    if (run.current.identity !== identity) {
      // A new query starts a new run. Its served answer, cached or not,
      // counts as the first read.
      run.current = { identity, pendingReads: 0, spent: false, seenAt: dataUpdatedAt }
      started.current = null
    }
    const state = run.current
    // A read in flight sets no timer. Its answer or its failure runs this
    // effect again.
    if (isFetching) return
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
    if (!hasAnswer || !visible) return
    let kind: ReadKind | null = null
    if (pending && !state.spent && state.pendingReads < LIVE_STATUS_PENDING_READS) kind = 'pending'
    else if (shown > 0) kind = 'regular'
    if (!kind) return
    const timer = window.setTimeout(
      () => {
        if (!tabVisible() || latest.current.isFetching) return
        if (kind === 'pending') state.pendingReads += 1
        started.current = kind
        void latest.current.refetch()
      },
      kind === 'pending' ? LIVE_STATUS_PENDING_MS : LIVE_STATUS_REFRESH_MS,
    )
    return () => window.clearTimeout(timer)
  }, [identity, shown, pending, hasAnswer, dataUpdatedAt, errorUpdatedAt, isFetching, visible])
}
