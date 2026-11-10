import { Link } from '@tanstack/react-router'
import { ArrowDown } from 'lucide-react'
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
import { ExecutionFactRows, ExecutionStatusLine, ExecutionTitle } from './execution-summary-details'
import { GateChoices } from './merge-at-gate'
import { CheckStatusList } from './next-action-checks'
import { NextActionParkReason } from './next-action-park-reason'
import { NextActionDeadline, NextActionIDs } from './next-action-parts'
import { PassiveNextActionBanner } from './passive-next-action-banner'
import { gateAnchorId, isReviewable, useGateSurfaceMounted } from './review-panel'
import type { TaskActivityView } from './task-activity-tabs'
import { claimedWorkOrder, WorkOrderPreemptControl } from './work-order-preempt-card'

// States where nothing is asked of anyone reading the page. On the Timeline
// they take a quiet one-row banner, so the box that asks for nothing never
// outweighs a decision and the timeline starts high on the page.
const passiveKinds = new Set(['working', 'queued'])

// The server's next action, directly under the task's description on the Board
// sheet and the full task route, ahead of its reference data
// (req-operator-next-action AC-7.1; component-web-task-surfaces invariant 32;
// component-next-action invariant 33). It is the one box on task detail that says
// where the task stands: the summary is its only headline, and the current
// order's execution state from deriveCurrentExecutionState adds just the lines
// its `facts` mark as saying more than that summary (DEC-61; AC-1.7). One
// decision has one surface per view: on the Timeline the human gate carries the
// statement and choices beside the evidence, and this card shrinks to one line
// pointing at it. On Flow no gate is mounted, so this card carries them itself.
export function NextActionCard({
  item,
  routeVariant,
  view,
}: {
  item: ActivityItem
  routeVariant: TaskRouteVariant
  view: TaskActivityView
}) {
  const gateOpen = view === 'timeline' && isReviewable(item)
  const gateSurface = useGateSurfaceMounted(item)
  const { data: action, isLoading } = useNextAction(item.task.id)
  const canRecover = useWorkspaceCapability('recover_work')
  const { workspace } = useWorkspaceSelection()
  // A claimed order keeps its stop affordance whether or not an execution
  // state maps (a claimed refresh-review round does not), and it renders once.
  const preemptOrder = canRecover ? claimedWorkOrder(item) : undefined
  if (isLoading) return <Skeleton className={gateOpen && gateSurface ? 'h-11' : 'h-20'} />
  if (!action || action.kind === 'terminal') {
    return preemptOrder ? <WorkOrderPreemptControl order={preemptOrder} /> : null
  }
  const execution = deriveCurrentExecutionState(item, action)
  const operator = action.actor === 'operator'
  const paused = execution?.status === 'paused'
  const actionBadge = (
    <Badge variant={operator ? 'attention' : action.kind === 'merging' ? 'positive' : 'default'}>{action.label}</Badge>
  )
  if (gateOpen && gateSurface) {
    return (
      <section aria-label="Next action" className="min-w-0 space-y-2">
        <div className="flex min-w-0 flex-wrap items-center gap-x-2.5 gap-y-1 rounded-lg border border-attention/40 bg-card px-3 py-2">
          {actionBadge}
          <span className="text-xs capitalize text-faint" data-actor={action.actor}>
            {action.actor}
          </span>
          <h2
            className="min-w-0 text-sm font-medium max-sm:order-last max-sm:basis-full sm:flex-[1_1_14rem] sm:truncate"
            title={action.summary}
          >
            {action.summary}
          </h2>
          <button
            type="button"
            data-go-to-gate
            onClick={() => document.getElementById(gateAnchorId(item.task.id))?.scrollIntoView({ block: 'end' })}
            className="inline-flex shrink-0 items-center gap-1 rounded text-xs font-medium text-primary hover:underline focus-visible:outline-2 focus-visible:outline-primary max-sm:ml-auto pointer-coarse:-my-1.5 pointer-coarse:min-h-10"
          >
            Go to decision
            <ArrowDown aria-hidden="true" className="size-3.5" />
          </button>
        </div>
        {preemptOrder && <WorkOrderPreemptControl order={preemptOrder} />}
      </section>
    )
  }
  if (view === 'timeline' && passiveKinds.has(action.kind) && action.choices.length === 0 && !paused) {
    // Keyed so a menu panel never carries over to another task, workspace, or order.
    return (
      <PassiveNextActionBanner
        key={`${workspace}:${item.task.id}:${action.current_order?.id ?? ''}`}
        item={item}
        action={action}
        preemptOrder={preemptOrder}
      />
    )
  }
  return (
    <section aria-label="Next action">
      <Card className={cn('overflow-hidden', (paused || (!execution && operator)) && 'border-attention/40')}>
        <CardContent className="space-y-2.5 py-3">
          {/* A state waiting on a person tints only its header line, as the
              timeline's gate does: a whole brown card is more alarm than it carries. */}
          <div
            className={cn(
              'flex flex-wrap items-center gap-2',
              (paused || operator) && '-mx-4 -mt-3 mb-3 border-b border-attention/25 bg-attention-soft px-4 py-2',
            )}
          >
            {actionBadge}
            <span className="text-xs capitalize text-faint" data-actor={action.actor}>
              {action.actor}
            </span>
          </div>
          {execution && <ExecutionStatusLine state={execution} />}
          <h2 className="text-sm font-semibold leading-6">{action.summary}</h2>
          {execution && execution.title !== action.label && <ExecutionTitle state={execution} />}
          {action.kind === 'parked' && <NextActionParkReason item={item} />}
          {action.question && <p className="text-sm text-muted">{action.question}</p>}
          <CheckStatusList action={action} />
          {execution && <ExecutionFactRows state={execution} routeVariant={routeVariant} />}
          {!gateOpen &&
            (action.kind === 'context_proposal' && action.details?.context_proposals?.length ? (
              <ContextProposalDecision key={`${workspace}:${item.task.id}`} taskId={item.task.id} action={action} />
            ) : (
              <GateChoices taskId={item.task.id} action={action} routeVariant={routeVariant} />
            ))}
          {action.if_ignored && <p className="text-xs text-muted">If ignored: {action.if_ignored}</p>}
          <NextActionDeadline deadline={action.deadline} />
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

// Task detail's form of the context_proposal checklist: compact rows, the
// same Apply and Dismiss all as the Inbox detail, and a link to the decision
// in the Inbox (component-next-action invariant 31; component-web-task-surfaces
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
