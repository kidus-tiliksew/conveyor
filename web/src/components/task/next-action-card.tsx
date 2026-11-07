import { Link } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { deriveCurrentExecutionState } from '../../lib/activity'
import type { TaskRouteVariant } from '../../lib/task-route'
import type { ActivityItem, NextAction } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import {
  ContextProposalChecklist,
  ContextProposalFooter,
  MonitorOriginLine,
  useContextProposalSelection,
  useContextProposalSends,
} from '../inbox/context-proposal-rows'
import { useNextAction } from '../inbox/use-next-actions'
import { Badge } from '../ui/badge'
import { Card, CardContent } from '../ui/card'
import { Skeleton } from '../ui/skeleton'
import { CheckStatusList } from './next-action-checks'
import { ExecutionFactRows, ExecutionStatusLine, ExecutionTitle } from './execution-summary-details'
import { GateChoices } from './merge-at-gate'
import { NextActionDeadline, NextActionIDs } from './next-action-parts'
import { gateToneClasses } from './review-panel'
import { claimedWorkOrder, WorkOrderPreemptControl } from './work-order-preempt-card'

// The server's next action, leading task detail on the Board sheet and the full
// task route, directly above the timeline (feature-operator-next-action NA-6;
// req-operator-next-action AC-7.1). It is the one box on task detail that says
// where the task stands: the summary is its only headline, and the current
// order's execution state from deriveCurrentExecutionState adds just the lines
// its `facts` mark as saying more than that summary (DEC-61; AC-1.7). While the
// human gate is open on the same page it carries the choices beside the
// evidence, so the card does not repeat them (`gateOpen`), and it renders
// without a box of its own as the top of the decision block (`joined`). While
// someone else acts, nothing is asked of the reader, so the card stays small.
export function NextActionCard({
  item,
  routeVariant,
  gateOpen,
  joined = false,
  footer,
}: {
  item: ActivityItem
  routeVariant: TaskRouteVariant
  gateOpen: boolean
  joined?: boolean
  // An action on this same state, such as re-enqueueing a queued task.
  footer?: ReactNode
}) {
  const { data: action, isLoading } = useNextAction(item.task.id)
  const canRecover = useWorkspaceCapability('recover_work')
  const { workspace } = useWorkspaceSelection()
  // A claimed order keeps its stop affordance whether or not an execution
  // state maps (a claimed refresh-review round does not), and it renders once.
  const preemptOrder = canRecover ? claimedWorkOrder(item) : undefined
  if (isLoading) return <Skeleton className="h-20" />
  if (!action || action.kind === 'terminal') {
    if (footer) return <>{footer}</>
    return preemptOrder ? <WorkOrderPreemptControl order={preemptOrder} /> : null
  }
  const execution = deriveCurrentExecutionState(item, action)
  const operator = action.actor === 'operator'
  const paused = execution?.status === 'paused'
  const preempt = preemptOrder && <WorkOrderPreemptControl order={preemptOrder} />
  const ids = <NextActionIDs value={{ details: action.details, current_order: action.current_order }} />
  const choices =
    !gateOpen &&
    (action.kind === 'context_proposal' && action.details?.context_proposals?.length ? (
      <ContextProposalDecision key={`${workspace}:${item.task.id}`} taskId={item.task.id} action={action} />
    ) : (
      <GateChoices taskId={item.task.id} action={action} routeVariant={routeVariant} />
    ))
  if (joined) {
    const tone = gateToneClasses(action.kind)
    return (
      <section aria-label="Next action" className={cn('space-y-1.5 px-4 py-3', tone.header)}>
        <div className="flex flex-wrap items-center justify-between gap-x-3">
          <p className={cn('text-xs font-semibold', tone.title)}>{action.label}</p>
          <div className="text-right open:basis-full">{ids}</div>
        </div>
        <span className="sr-only" data-actor={action.actor}>
          {action.actor}
        </span>
        <h2 className="text-sm font-semibold leading-6">{action.summary}</h2>
        {execution && <ExecutionStatusLine state={execution} />}
        {execution && <ExecutionTitle state={execution} />}
        <CheckStatusList action={action} />
        {execution && <ExecutionFactRows state={execution} routeVariant={routeVariant} />}
        {action.if_ignored && <p className="text-xs text-muted">If ignored: {action.if_ignored}</p>}
        <NextActionDeadline deadline={action.deadline} />
        {preempt}
      </section>
    )
  }
  if (!operator && !paused) {
    return (
      <section aria-label="Next action" className="space-y-1.5 rounded-lg border border-border bg-surface px-4 py-2.5">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-muted">
          <span className="font-medium text-foreground">{action.label}</span>
          <span className="sr-only" data-actor={action.actor}>
            {action.actor}
          </span>
          {execution && <ExecutionStatusLine state={execution} />}
          {action.deadline && (
            <span className="ml-auto">
              <NextActionDeadline deadline={action.deadline} />
            </span>
          )}
        </div>
        <h2 className="text-sm leading-6 font-medium">{action.summary}</h2>
        {execution && <ExecutionTitle state={execution} />}
        {action.question && <p className="text-sm text-muted">{action.question}</p>}
        <CheckStatusList action={action} />
        {execution && <ExecutionFactRows state={execution} routeVariant={routeVariant} />}
        {choices}
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="min-w-0 flex-1">{ids}</div>
          {preempt}
        </div>
        {footer}
      </section>
    )
  }
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
          <CheckStatusList action={action} />
          {execution && <ExecutionFactRows state={execution} routeVariant={routeVariant} />}
          {choices}
          {action.if_ignored && <p className="text-xs text-muted">If ignored: {action.if_ignored}</p>}
          <NextActionDeadline deadline={action.deadline} />
          {preemptOrder && (
            <div className="border-t border-border/60 pt-2.5">
              <WorkOrderPreemptControl order={preemptOrder} />
            </div>
          )}
          {ids}
          {footer}
        </CardContent>
      </Card>
    </section>
  )
}

// Task detail's form of the context_proposal checklist: compact rows, the
// same Apply and Dismiss all as the Inbox detail, and a link to the decision
// in the Inbox (feature-operator-next-action NA-6; component-web-task-surfaces
// Next-action card).
function ContextProposalDecision({ taskId, action }: { taskId: string; action: NextAction }) {
  const { workspace } = useWorkspaceSelection()
  const sends = useContextProposalSends(taskId)
  const selection = useContextProposalSelection(taskId)
  return (
    <div data-context-decision className="min-w-0 space-y-2.5">
      <MonitorOriginLine action={action} />
      <ContextProposalChecklist action={action} selection={selection} sends={sends} compact />
      <div className="flex min-w-0 flex-wrap items-start justify-between gap-2">
        <ContextProposalFooter action={action} selection={selection} sends={sends} />
        <Link
          to="/w/$workspaceId/inbox"
          params={{ workspaceId: workspace }}
          search={{ selected: taskId }}
          data-review-in-inbox
          className="text-xs font-medium text-primary hover:underline"
        >
          Review in Inbox →
        </Link>
      </div>
    </div>
  )
}
