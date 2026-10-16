import { deriveCurrentExecutionState } from '../../lib/activity'
import type { TaskRouteVariant } from '../../lib/task-route'
import type { ActivityItem } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceCapability } from '../app-shell'
import { useNextAction } from '../inbox/use-next-actions'
import { Badge } from '../ui/badge'
import { Card, CardContent } from '../ui/card'
import { Skeleton } from '../ui/skeleton'
import { ExecutionFactRows, ExecutionStatusLine, ExecutionTitle, RecentActivity } from './execution-summary-details'
import { GateChoices } from './merge-at-gate'
import { NextActionDeadline, NextActionIDs } from './next-action-parts'
import { claimedWorkOrder, WorkOrderPreemptControl } from './work-order-preempt-card'

// The server's next action, leading task detail on the Board sheet and the full
// task route, directly above the timeline (feature-operator-next-action NA-6;
// req-operator-next-action AC-7.1). It is the one box on task detail that says
// where the task stands: the summary is its only headline, and the current
// order's execution state from deriveCurrentExecutionState adds just the lines
// its `facts` mark as saying more than that summary (DEC-61; AC-1.7). While the
// human gate is open on the same page it carries the choices beside the
// evidence, so the card does not repeat them (`gateOpen`).
export function NextActionCard({
  item,
  routeVariant,
  gateOpen,
}: {
  item: ActivityItem
  routeVariant: TaskRouteVariant
  gateOpen: boolean
}) {
  const { data: action, isLoading } = useNextAction(item.task.id)
  const canRecover = useWorkspaceCapability('recover_work')
  // A claimed order keeps its stop affordance whether or not an execution
  // state maps (a claimed refresh-review round does not), and it renders once.
  const preemptOrder = canRecover ? claimedWorkOrder(item) : undefined
  if (isLoading) return <Skeleton className="h-20" />
  if (!action || action.kind === 'terminal') {
    return preemptOrder ? <WorkOrderPreemptControl order={preemptOrder} /> : null
  }
  const execution = deriveCurrentExecutionState(item, action)
  const operator = action.actor === 'operator'
  const paused = execution?.status === 'paused'
  return (
    <section aria-label="Next action">
      <Card
        className={cn(
          execution
            ? paused
              ? 'border-attention/55 bg-attention-soft'
              : 'border-primary/25 bg-primary-soft/35'
            : operator && 'border-attention/40',
        )}
      >
        <CardContent className="space-y-2.5 py-3">
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant={operator ? 'attention' : action.kind === 'merging' ? 'positive' : 'mono'}>
              {action.label}
            </Badge>
            <Badge variant="outline" data-actor={action.actor}>
              {action.actor}
            </Badge>
          </div>
          {execution && <ExecutionStatusLine state={execution} />}
          <h2 className="text-sm font-semibold leading-6">{action.summary}</h2>
          {execution && <ExecutionTitle state={execution} />}
          {action.question && <p className="text-sm text-muted">{action.question}</p>}
          {execution && <ExecutionFactRows state={execution} routeVariant={routeVariant} />}
          {!gateOpen && <GateChoices taskId={item.task.id} action={action} />}
          {action.if_ignored && <p className="text-xs text-muted">If ignored: {action.if_ignored}</p>}
          <NextActionDeadline deadline={action.deadline} />
          {execution?.order.state === 'claimed' && execution.order.activity_snapshot && (
            <RecentActivity snapshot={execution.order.activity_snapshot} />
          )}
          {preemptOrder && (
            <div className="border-t border-border/60 pt-2.5">
              <WorkOrderPreemptControl order={preemptOrder} />
            </div>
          )}
          <NextActionIDs value={{ details: action.details, current_order: action.current_order }} />
        </CardContent>
      </Card>
    </section>
  )
}
