import { hashKey, queryOptions, useQuery, useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useSyncExternalStore } from 'react'
import { fetchNextAction, fetchNextActions } from '../../lib/api'
import { taskDetailQueryKey } from '../../lib/query-keys'
import type { NextActionsPage } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { inboxItemKey } from './inbox-selection'

export const INBOX_PAGE_SIZE = 100

export function nextActionsQueryKey(workspace: string) {
  return ['next-actions', workspace] as const
}

export function nextActionQueryKey(workspace: string, taskId: string) {
  return ['next-action', workspace, taskId] as const
}

// The operator inbox (NA-6). The server orders it; the page renders that order.
// It is read once without a band so every band's count comes from one
// response; the page filters by band in the browser. No workspace event
// stream exists, so it refreshes on a poll and on its own acts.
export function useNextActions() {
  const { workspace } = useWorkspaceSelection()
  return useQuery({
    queryKey: nextActionsQueryKey(workspace),
    queryFn: ({ signal }) => fetchWholeInbox(workspace, signal),
    enabled: Boolean(workspace),
    refetchInterval: 15_000,
  })
}

// Reads every page of the inbox in server order, so the band counts cover the
// whole inbox and not its first page. The first page's total is the walk's
// finite target; a page that returns nothing ends the walk early, and the
// query's abort signal cancels it. An item an act moved between two page reads
// can arrive twice; the first occurrence keeps its place.
async function fetchWholeInbox(workspace: string, signal?: AbortSignal): Promise<NextActionsPage> {
  const first = await fetchNextActions(workspace, { limit: INBOX_PAGE_SIZE, offset: 0 }, signal)
  const items = [...first.items]
  let offset = first.items.length
  while (offset < first.total) {
    const next = await fetchNextActions(workspace, { limit: INBOX_PAGE_SIZE, offset }, signal)
    if (next.items.length === 0) break
    items.push(...next.items)
    offset += next.items.length
  }
  const seen = new Set<string>()
  const unique = items.filter((item) => {
    const key = inboxItemKey(item)
    if (seen.has(key)) return false
    seen.add(key)
    return true
  })
  return { ...first, items: unique, total: unique.length, offset: 0, limit: unique.length }
}

// The task's next action: one key, request, and stale time shared by
// useNextAction and the task sheet's mount-time prefetch. Sixty seconds lets
// the card that mounts when a slow detail read returns reuse the prefetched
// statement; the task stream and detail updates keep it current
// (feature-operator-next-action NA-6).
export function nextActionQueryOptions(workspace: string, taskId: string) {
  return queryOptions({
    queryKey: nextActionQueryKey(workspace, taskId),
    queryFn: ({ signal }) => fetchNextAction(workspace, taskId, signal),
    staleTime: 60_000,
  })
}

// One task's full next action. On task detail it refreshes when the detail
// entry updates, so the card never disagrees with the timeline under it,
// unless a next-action read started after the detail read that produced the
// update started: that read already observes what the detail read saw. The
// task stream invalidates both entries in one batch, so it causes one
// next-action read rather than two; a dependency poll or a mutation's detail
// refresh still re-reads the next action once (NA-6).
export function useNextAction(taskId: string, enabled = true) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  // Start order of this hook's two reads, from the cache's fetch events. They
  // reset with the task or workspace; a failed detail read changes no data.
  const starts = useRef({ key: '', sequence: 0, detail: 0, nextAction: 0 })
  const startsKey = `${workspace}\u0000${taskId}`
  if (starts.current.key !== startsKey) starts.current = { key: startsKey, sequence: 0, detail: 0, nextAction: 0 }
  // Only events of this task's two entries wake the hook, and they wake it
  // after the current render: another component that creates or observes the
  // entry while rendering emits a cache event synchronously, and acting on it
  // there would update this component mid-render. Fetch starts are recorded
  // synchronously, so their order is the order the reads began.
  const subscribe = useCallback(
    (onChange: () => void) => {
      const detailHash = hashKey(taskDetailQueryKey(workspace, taskId))
      const nextActionHash = hashKey(nextActionQueryKey(workspace, taskId))
      let active = true
      const unsubscribe = queryClient.getQueryCache().subscribe((event) => {
        const hash = event.query.queryHash
        if (hash !== detailHash && hash !== nextActionHash) return
        if (event.type === 'updated' && event.action.type === 'fetch' && starts.current.key === startsKey) {
          const sequence = ++starts.current.sequence
          if (hash === detailHash) starts.current.detail = sequence
          else starts.current.nextAction = sequence
        }
        if (hash === detailHash) queueMicrotask(() => active && onChange())
      })
      return () => {
        active = false
        unsubscribe()
      }
    },
    [queryClient, workspace, taskId, startsKey],
  )
  const dataUpdatedAt = useSyncExternalStore(
    subscribe,
    () => queryClient.getQueryState(taskDetailQueryKey(workspace, taskId))?.dataUpdatedAt ?? 0,
  )
  const query = useQuery({
    ...nextActionQueryOptions(workspace, taskId),
    enabled: enabled && Boolean(workspace && taskId),
  })
  const seen = useRef(dataUpdatedAt)
  useEffect(() => {
    if (dataUpdatedAt === seen.current) return
    seen.current = dataUpdatedAt
    // A next-action read that began after this detail read began already
    // covers the update; invalidating would cancel or repeat it.
    if (starts.current.nextAction > starts.current.detail) return
    void queryClient.invalidateQueries({ queryKey: nextActionQueryKey(workspace, taskId), exact: true })
  }, [dataUpdatedAt, queryClient, workspace, taskId])
  return query
}
