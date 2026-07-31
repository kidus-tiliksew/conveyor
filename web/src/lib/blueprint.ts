import { taskStateLabels } from './contracts'
import type { BlueprintDelivery, BlueprintView, Task } from './types'

// Blueprint vocabulary (spec §21.49). An anchor is a contract with a progress
// bar, not a card waiting for a worker, so every label here speaks about
// delivery rather than the pipeline. Centralized so no surface has to invent
// its own phrasing — or leak a raw task state.

// The client mirror of core.BlueprintAnchor: children exist only through
// blueprint materialization from an approved §4.1 decomposition, so the
// parent/child relation is the classification. No stored flag, no new entity.
export function isBlueprintAnchor(task: Pick<Task, 'children'>): boolean {
  return (task.children?.length ?? 0) > 0
}

// "In delivery — 1 of 4" counts children that actually merged. A child that
// closed without merging did not deliver, so it never inflates the numerator.
export function deliveryLabel(delivery: BlueprintDelivery): string {
  switch (delivery.state) {
    case 'completed':
      return 'Completed'
    case 'cancelled':
      return 'Cancelled'
    default:
      return `In delivery — ${delivery.merged} of ${delivery.total}`
  }
}

export function deliveryTone(delivery: BlueprintDelivery): 'positive' | 'mono' | 'outline' {
  if (delivery.state === 'completed') return 'positive'
  if (delivery.state === 'cancelled') return 'mono'
  return 'outline'
}

// The rollup keeps merged and closed as separate quantities — folding them
// into one "done" count would report abandoned work as delivered.
export function childRollup(delivery: BlueprintDelivery): string {
  const parts = [
    delivery.merged > 0 ? `${delivery.merged} merged` : '',
    delivery.closed > 0 ? `${delivery.closed} closed without merging` : '',
    delivery.open > 0 ? `${delivery.open} in progress` : '',
  ].filter(Boolean)
  return parts.length > 0 ? parts.join(' · ') : 'No child tasks yet'
}

// Every child state renders through taskStateLabels; the raw state is only
// ever a fallback for a value the label table has not learned yet.
export function childStateLabel(state: string): string {
  return taskStateLabels[state] ?? state.replaceAll('_', ' ')
}

export function findBlueprint(views: BlueprintView[] | undefined, taskId: string): BlueprintView | undefined {
  return views?.find((view) => view.task.id === taskId)
}
