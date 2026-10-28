import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo } from 'react'
import { boardColumnFor } from '../../lib/activity'
import { fetchTaskActivity, fetchTaskAudit, fetchVerificationPage, fetchVerificationSummary } from '../../lib/api'
import { isBlueprintAnchor } from '../../lib/blueprint'
import { stageGroups } from '../../lib/contracts'
import { taskDetailQueryKey } from '../../lib/query-keys'
import type { ActivityItem, TaskAuditKind, VerificationCollection } from '../../lib/types'
import { useTaskStream } from '../../lib/use-task-stream'
import { useBoardActivityPage, useWorkspaceSelection } from '../app-shell'

// Reads a query's abort signal one microtask after the query function starts.
// React StrictMode mounts, unmounts, and remounts every effect once in
// development, and TanStack Query cancels an in-flight read whose signal was
// already read when its last observer leaves, so a synchronous read would
// abort and repeat every first read. Deferring it lets the rehearsal pass;
// a real unmount, task change, or workspace change still aborts the read.
export async function withQuerySignal<T>(
  context: { signal: AbortSignal },
  read: (signal: AbortSignal) => Promise<T>,
): Promise<T> {
  await Promise.resolve()
  return read(context.signal)
}

// Shared by the sheet and the full page: the task-detail query plus the SSE
// stream that keeps it (and the board) fresh.
export function useTaskDetail(taskId: string) {
  const { workspace } = useWorkspaceSelection()
  const query = useQuery({
    queryKey: taskDetailQueryKey(workspace, taskId),
    queryFn: (context) => withQuerySignal(context, (signal) => fetchTaskActivity(workspace, taskId, signal)),
    // A dependency can merge without emitting an event on this task. Poll only
    // while the task reports active blockers; SSE remains the fast path.
    refetchInterval: (current) => ((current.state.data?.task.blocking_task_ids?.length ?? 0) > 0 ? 15_000 : false),
    // A bounded view can weigh more than a megabyte, and the task stream keeps
    // it current, so returning to the tab sends no detail read.
    refetchOnWindowFocus: false,
  })
  useTaskStream(taskId, workspace)
  return query
}

const verificationQueryOptions = {
  retry: false,
  staleTime: Infinity,
  gcTime: 0,
  refetchOnWindowFocus: false,
  refetchOnReconnect: false,
  refetchInterval: false,
} as const

// Whether the task reads verification at all: the predicate
// VerificationVerdictLine applies before it queries the summary.
export function taskHasVerifyStage(item: ActivityItem) {
  return Boolean(
    item.task.policy_contract?.verify_stage ||
      (item.work_orders ?? []).some((order) => order.stage === 'verify') ||
      item.task.next_stage === 'verify',
  )
}

// These families are independent of task activity and SSE invalidation.
export function useTaskVerification(taskId: string, enabled = true) {
  const { workspace } = useWorkspaceSelection()
  return useInfiniteQuery({
    queryKey: ['verification-summary', workspace, taskId],
    queryFn: ({ pageParam, signal }) => fetchVerificationSummary(workspace, taskId, pageParam, signal),
    initialPageParam: '',
    // A body without contexts (an older or malformed answer) has no next page.
    getNextPageParam: (page) => page.contexts?.next_cursor || undefined,
    enabled: enabled && Boolean(workspace && taskId),
    ...verificationQueryOptions,
  })
}

export function useVerificationPages(
  taskId: string,
  contextId: string,
  collection: VerificationCollection,
  limit?: number,
) {
  const { workspace } = useWorkspaceSelection()
  return useInfiniteQuery({
    queryKey: ['verification-page', workspace, taskId, contextId, collection, limit ?? 0],
    queryFn: ({ pageParam, signal }) =>
      fetchVerificationPage(workspace, taskId, contextId, collection, pageParam, signal, limit),
    initialPageParam: '',
    getNextPageParam: (page) => page.next_cursor || undefined,
    enabled: Boolean(workspace && taskId && contextId),
    ...verificationQueryOptions,
  })
}

// Enabled only by an open individual disclosure (component-web-dashboard).
// The separate family is never invalidated by task SSE or dependency polling.
export function useTaskAudit(taskId: string, kind: TaskAuditKind, recordId: string, open: boolean) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  useEffect(() => {
    if (!open) return
    return () => {
      void queryClient.cancelQueries({ queryKey: ['task-audit', workspace, taskId, kind, recordId], exact: true })
    }
  }, [queryClient, workspace, taskId, kind, recordId, open])
  return useQuery({
    queryKey: ['task-audit', workspace, taskId, kind, recordId],
    queryFn: ({ signal }) => fetchTaskAudit(workspace, taskId, kind, recordId, signal),
    enabled: Boolean(open && workspace && taskId && recordId),
    retry: false,
    staleTime: 0,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    refetchInterval: false,
  })
}

// Prev/next follow the visual order of the Board page the Kanban view shows:
// columns left to right, blueprint anchors skipped, newest creation first with
// the task ID breaking ties, exactly as the Board sorts. The page comes from
// the cache, so no activity query is mounted here; navigation stays on that
// page (req-operator-next-action AC-6.12), and without one (the full route,
// the Board map, a pending read) there are no neighbours
// (component-web-task-surfaces, Board navigation).
export function useTaskOrder(taskId: string) {
  const activity = useBoardActivityPage()
  return useMemo(() => {
    const byGroup = new Map<string, Array<{ id: string; created: number }>>()
    for (const summary of activity?.items ?? []) {
      if (isBlueprintAnchor(summary.task)) continue
      const key = boardColumnFor(summary)
      byGroup.set(key, [
        ...(byGroup.get(key) ?? []),
        { id: summary.task.id, created: new Date(summary.task.created_at).getTime() },
      ])
    }
    const order = stageGroups.flatMap(({ key }) =>
      (byGroup.get(key) ?? [])
        .sort((a, b) => b.created - a.created || a.id.localeCompare(b.id))
        .map((entry) => entry.id),
    )
    const index = order.indexOf(taskId)
    const edges: string[] = []
    if (activity && index < 0) {
      edges.push('This task is outside the loaded Board window.')
    } else if (activity) {
      if (index === 0 && activity.offset > 0) edges.push('The previous task is on an earlier Board page.')
      if (index === order.length - 1 && activity.offset + activity.items.length < activity.total) {
        edges.push('The next task is on a later Board page.')
      }
    }
    return {
      previousId: index > 0 ? order[index - 1] : undefined,
      nextId: index >= 0 && index < order.length - 1 ? order[index + 1] : undefined,
      windowEdge: edges.join(' '),
    }
  }, [activity, taskId])
}
