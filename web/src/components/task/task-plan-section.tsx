import { planStateLabels } from '../../lib/contracts'
import type { TaskRouteVariant } from '../../lib/task-route'
import type { ActivityItem } from '../../lib/types'
import { cn } from '../../lib/utils'
import { SpecCard } from './spec-card'
import { TaskPlanActs } from './task-plan-acts'

// Task detail's plan section, the same on the Board sheet and the full route
// (component-web-task-surfaces invariants 32, 60). It always renders, headed by
// the served plan state's label. The current plan version, when one exists,
// follows with its title, version, and approval; without one the label stands
// alone. The operator's plan acts sit beside the label
// (invariants 61, 62; req-task-plan-state AC-1.4).
export function TaskPlanSection({
  item,
  variant,
  className,
}: {
  item: ActivityItem
  variant: TaskRouteVariant
  className?: string
}) {
  const planState = item.task.plan_state
  const label = planStateLabels[planState] ?? planState
  return (
    <section aria-label="Execution plan" className={cn('space-y-2', className)} data-plan-state={planState}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold text-foreground" data-plan-state-label>
          {label}
        </h2>
        <TaskPlanActs item={item} />
      </div>
      {item.spec && (
        <SpecCard
          key={`${item.spec.task_id}-${item.spec.version}`}
          spec={item.spec}
          collapsible={variant !== 'full'}
          overflowExpandable
          routeVariant={variant}
          planState={planState}
        />
      )}
    </section>
  )
}
