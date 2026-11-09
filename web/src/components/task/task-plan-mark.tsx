import { ClipboardCheck, ClipboardClock, ClipboardPen } from 'lucide-react'
import { planStateLabels } from '../../lib/contracts'
import type { PlanState } from '../../lib/types'
import { cn } from '../../lib/utils'

const planMarkIcons: Record<Exclude<PlanState, 'not_required'>, typeof ClipboardPen> = {
  needed: ClipboardPen,
  awaiting_approval: ClipboardClock,
  completed: ClipboardCheck,
}

// The Board card's plan-state mark: a small icon beside the title, named by
// the served plan state's label. A task that needs no plan carries none, and
// an unknown or absent value marks nothing rather than a guess. The mark
// orders nothing and never stands in for the next-action label
// (component-web-task-surfaces invariants 7, 57, 58; req-task-plan-state
// AC-1.4).
export function TaskPlanMark({ planState, className }: { planState?: PlanState; className?: string }) {
  if (!planState || planState === 'not_required' || !(planState in planMarkIcons)) return null
  const Icon = planMarkIcons[planState]
  const label = planStateLabels[planState]
  return (
    <span
      role="img"
      aria-label={label}
      title={label}
      data-plan-state-mark={planState}
      className={cn(
        'mt-0.5 inline-flex shrink-0',
        planState === 'completed'
          ? 'text-positive'
          : planState === 'awaiting_approval'
            ? 'text-attention'
            : 'text-faint',
        className,
      )}
    >
      <Icon aria-hidden className="size-[15px]" />
    </span>
  )
}
