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
 * The context a task-detail mutation records in `onMutate`: the workspace the
 * operator acted in. Its `onSuccess` refreshes that workspace's detail even when
 * the selection changed while the request was in flight.
 */
export interface MutationWorkspace {
  workspace: string
}
