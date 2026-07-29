export type TaskRouteVariant = 'sheet' | 'full'

export function relatedTaskRoute(variant: TaskRouteVariant) {
  return variant === 'full' ? '/tasks/$taskId/full' as const : '/tasks/$taskId' as const
}
