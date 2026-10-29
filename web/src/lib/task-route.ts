export type TaskRouteVariant = 'sheet' | 'full'

export function relatedTaskRoute(variant: TaskRouteVariant) {
  return variant === 'full'
    ? ('/w/$workspaceId/tasks/$taskId/full' as const)
    : ('/w/$workspaceId/tasks/$taskId' as const)
}
