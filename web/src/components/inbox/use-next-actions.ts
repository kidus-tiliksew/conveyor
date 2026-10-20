import { hashKey, useQuery, useQueryClient } from '@tanstack/react-query'
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

// One task's full next action. On task detail it refreshes whenever the
// detail entry does, so the card never disagrees with the timeline under it.
export function useNextAction(taskId: string, enabled = true) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  // Only events of this task's detail entry wake the hook, and they wake it
  // after the current render: another component that creates or observes the
  // entry while rendering emits a cache event synchronously, and acting on it
  // there would update this component mid-render.
  const subscribe = useCallback(
    (onChange: () => void) => {
      const detailHash = hashKey(taskDetailQueryKey(workspace, taskId))
      let active = true
      const unsubscribe = queryClient.getQueryCache().subscribe((event) => {
        if (event.query.queryHash === detailHash) queueMicrotask(() => active && onChange())
      })
      return () => {
        active = false
        unsubscribe()
      }
    },
    [queryClient, workspace, taskId],
  )
  const dataUpdatedAt = useSyncExternalStore(
    subscribe,
    () => queryClient.getQueryState(taskDetailQueryKey(workspace, taskId))?.dataUpdatedAt ?? 0,
  )
  const query = useQuery({
    queryKey: nextActionQueryKey(workspace, taskId),
    queryFn: ({ signal }) => fetchNextAction(workspace, taskId, signal),
    enabled: enabled && Boolean(workspace && taskId),
  })
  const seen = useRef(dataUpdatedAt)
  useEffect(() => {
    if (dataUpdatedAt === seen.current) return
    seen.current = dataUpdatedAt
    void queryClient.invalidateQueries({ queryKey: nextActionQueryKey(workspace, taskId), exact: true })
  }, [dataUpdatedAt, queryClient, workspace, taskId])
  return query
}
