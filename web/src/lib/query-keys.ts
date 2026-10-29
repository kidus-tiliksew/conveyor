// Cache keys shared across components (component-web-dashboard). Every key
// carries the workspace, so a read or invalidation can never address another
// workspace's entry.

/**
 * The one task-detail key: `useTaskDetail` reads it, and every mutation that
 * refreshes task detail invalidates it with the workspace captured when the
 * mutation started (component-web-task-surfaces).
 */
export function taskDetailQueryKey(workspace: string, taskId: string) {
  return ['task', workspace, taskId] as const
}

/**
 * The one task-history page key (component-web-task-surfaces, History pages).
 * The empty cursor is the newest page. The task stream, dependency polling, and
 * mutations never invalidate this family.
 */
export function taskHistoryQueryKey(workspace: string, taskId: string, selection: string, cursor: string) {
  return ['task-history', workspace, taskId, selection, cursor] as const
}

/**
 * The workspace's decision list, the one read behind the Decisions surface,
 * each decision page, and the System Design attention surface's proposed
 * decisions (component-web-decision-surfaces).
 */
export function decisionsQueryKey(workspace: string) {
  return ['decisions', workspace] as const
}

/**
 * The workspace's decision citations from `GET /v1/decision-citations`, the
 * one read behind every citing-document count and list
 * (component-web-decision-surfaces).
 */
export function decisionCitationsQueryKey(workspace: string) {
  return ['decision-citations', workspace] as const
}

/**
 * Placeholder data that never crosses workspaces. A query keeps its previous
 * result on screen only when the previous query's key names the same
 * workspace, so after a switch it shows its first-load state instead of the
 * previous workspace's data (component-web-dashboard, Shell and workspace
 * scoping). Each call returns a new function, so TanStack never reuses a
 * memoized placeholder from another workspace.
 */
export function sameWorkspacePlaceholder(workspace: string) {
  return <T>(previous: T | undefined, previousQuery?: { queryKey: readonly unknown[] }) =>
    previousQuery?.queryKey[1] === workspace ? previous : undefined
}

/**
 * One transcript capture's content (component-web-task-surfaces, Transcript
 * disclosures). The family sits outside the task-detail and task-history
 * prefixes, so the task stream, dependency polling, and mutations never
 * invalidate it.
 */
export function taskTranscriptQueryKey(workspace: string, taskId: string, workOrderId: string, attemptId: string) {
  return ['task-transcript', workspace, taskId, workOrderId, attemptId] as const
}

/**
 * The context a task-detail mutation records in `onMutate`: the workspace the
 * operator acted in. Its `onSuccess` refreshes that workspace's detail even when
 * the selection changed while the request was in flight.
 */
export interface MutationWorkspace {
  workspace: string
}
