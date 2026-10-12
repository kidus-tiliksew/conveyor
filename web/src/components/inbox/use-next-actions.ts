import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useSyncExternalStore } from 'react'
import { fetchNextAction, fetchNextActions } from '../../lib/api'
import { taskDetailQueryKey } from '../../lib/query-keys'
import type { InboxBand } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'

export const INBOX_PAGE_SIZE = 100

export function nextActionsQueryKey(workspace: string) {
  return ['next-actions', workspace] as const
}

export function nextActionQueryKey(workspace: string, taskId: string) {
  return ['next-action', workspace, taskId] as const
}

// The operator inbox (NA-6). The server orders it; the page renders that order.
// No workspace event stream exists, so it refreshes on a poll and on its own acts.
export function useNextActions(band?: InboxBand) {
  const { workspace } = useWorkspaceSelection()
  return useQuery({
    queryKey: [...nextActionsQueryKey(workspace), { band: band ?? null }],
    queryFn: ({ signal }) => fetchNextActions(workspace, { band, limit: INBOX_PAGE_SIZE, offset: 0 }, signal),
    enabled: Boolean(workspace),
    refetchInterval: 15_000,
  })
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
