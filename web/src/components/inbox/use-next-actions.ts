import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useSyncExternalStore } from 'react'
import { fetchNextAction, fetchNextActions } from '../../lib/api'
import { taskDetailQueryKey } from '../../lib/query-keys'
import type { InboxItem, NextActionsPage } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'

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

// Bounds the page walk so a total that keeps growing cannot loop forever.
const INBOX_MAX_PAGES = 20

function inboxKey(item: InboxItem) {
  return item.proposal
    ? `${item.proposal.tier}:${item.proposal.id}:${item.proposal.version ?? 0}`
    : (item.task?.id ?? '')
}

// Reads every page of the inbox in server order, so the band counts cover the
// whole inbox and not its first page. An item an act moved between two page
// reads can arrive twice; the first occurrence keeps its place.
async function fetchWholeInbox(workspace: string, signal?: AbortSignal): Promise<NextActionsPage> {
  const first = await fetchNextActions(workspace, { limit: INBOX_PAGE_SIZE, offset: 0 }, signal)
  const items = [...first.items]
  let offset = first.items.length
  for (let page = 1; page < INBOX_MAX_PAGES && offset < first.total; page++) {
    const next = await fetchNextActions(workspace, { limit: INBOX_PAGE_SIZE, offset }, signal)
    if (next.items.length === 0) break
    items.push(...next.items)
    offset += next.items.length
  }
  const seen = new Set<string>()
  const unique = items.filter((item) => {
    const key = inboxKey(item)
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
  const subscribe = useCallback(
    (onChange: () => void) => queryClient.getQueryCache().subscribe(onChange),
    [queryClient],
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
